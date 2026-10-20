package resources

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	credentialSchema       = "eregion/resource-credential/v1"
	credentialExportSchema = "eregion/resource-credentials/v1"
)

// storedCredential is the protected on-disk form. It is never serialized in a
// Result, event, log line, or job argument.
type storedCredential struct {
	Schema   string `json:"schema"`
	Resource string `json:"resource"`
	Host     string `json:"host"`
	Username string `json:"username"`
	Database string `json:"database"`
	Password string `json:"password"`
}

func (s *Service) resourceDir(name string) string {
	return filepath.Join(s.config.StateDir, "resources", name)
}

func (s *Service) credentialPath(name string) string {
	return filepath.Join(s.resourceDir(name), "credential.json")
}

func (s *Service) passwordPath(name string) string {
	return filepath.Join(s.resourceDir(name), "password")
}

func (s *Service) restoreJournalPath(name string) string {
	return filepath.Join(s.resourceDir(name), "restore.json")
}

func (s *Service) credentialReference(spec Spec) *CredentialReference {
	return &CredentialReference{
		Username:     spec.Username,
		Database:     spec.Database,
		PasswordFile: s.passwordPath(spec.Name),
		Mode:         "0600",
	}
}

// credentialExport is the protected-file payload. It is written only to the
// explicit destination and is never returned in a Result or event.
type credentialExport struct {
	Schema      string    `json:"schema"`
	Resource    string    `json:"resource"`
	Host        string    `json:"host"`
	Engine      Engine    `json:"engine"`
	OperationID string    `json:"operation_id,omitempty"`
	Endpoint    *Endpoint `json:"endpoint,omitempty"`
	Username    string    `json:"username"`
	Database    string    `json:"database"`
	SSLMode     string    `json:"sslmode"`
	Password    string    `json:"password"`
	DSN         string    `json:"dsn"`
}

// validateExportDestination checks only the requested path shape. The executor
// additionally enforces create-once semantics so a retry cannot overwrite an
// unrelated file.
func validateExportDestination(path string) error {
	if !filepath.IsAbs(path) || strings.ContainsAny(path, "\x00\r\n") || strings.Contains(path, "..") {
		return invalidResource("credential destination must be a safe absolute path", "Name a new file in a protected directory; use an absolute path without '..'.")
	}
	if info, err := os.Stat(filepath.Dir(path)); err != nil || !info.IsDir() {
		return precondition("credential destination directory is unavailable", "Create the protected destination directory before exporting credentials.")
	}
	return nil
}

func (s *Service) exportCredentials(ctx context.Context, spec Spec, request *ExportRequest, effect effectRecord) (Result, error) {
	result := baseResult(ActionCredentials, spec, StateUnknown)
	if request == nil {
		return result, invalidResource("credential export destination is required", "Name one new absolute destination file.")
	}
	if err := validateExportDestination(request.Destination); err != nil {
		return result, err
	}
	credential, err := s.loadCredential(spec)
	if err != nil {
		return result, err
	}
	endpoint, err := s.endpoint(ctx, spec)
	if err != nil {
		return result, err
	}
	dsn := ""
	if endpoint != nil {
		user := url.UserPassword(spec.Username, credential.Password)
		dsn = (&url.URL{
			Scheme: "postgres", User: user,
			Host: endpoint.Address + ":" + strconv.Itoa(endpoint.Port),
			Path: "/" + spec.Database, RawQuery: "sslmode=" + endpoint.SSLMode,
		}).String()
	}
	export := credentialExport{
		Schema: credentialExportSchema, Resource: spec.Name, Host: spec.Host, Engine: spec.Engine,
		OperationID: effect.OperationID, Endpoint: endpoint, Username: spec.Username,
		Database: spec.Database, SSLMode: "disable", Password: credential.Password, DSN: dsn,
	}
	encoded, err := json.MarshalIndent(export, "", "  ")
	if err != nil {
		return result, err
	}
	if err := writeCredentialExport(request.Destination, append(encoded, '\n')); err != nil {
		return result, err
	}
	result.Changed = true
	result.Endpoint = endpoint
	result.Credential = s.credentialReference(spec)
	result.Export = &CredentialExport{
		Destination: request.Destination, Mode: "0600",
		Username: spec.Username, Database: spec.Database,
	}
	if state, stateErr := s.observeState(ctx, spec); stateErr == nil {
		result.State = state
	}
	return result, nil
}

// writeCredentialExport publishes one new 0600 regular file. If the same
// operation retries after a lost acknowledgement, an identical existing file is
// accepted as the durable effect; any different file is a conflict, never an
// overwrite.
func writeCredentialExport(path string, raw []byte) error {
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return precondition("credential destination is not a regular file", "Choose a new path; symlinks and special files are rejected.")
		}
		if info.Mode().Perm()&0o077 != 0 {
			return precondition("credential destination is group- or world-readable", "Set mode 0600 or choose a new destination.")
		}
		existing, readErr := readRegular(path, 32*1024)
		if readErr != nil {
			return unavailable("credential destination cannot be read safely", "Inspect the destination file and permissions.")
		}
		if bytes.Equal(existing, raw) {
			return nil
		}
		return conflict("credential destination already exists with different contents", "Choose a new destination file; credentials are never overwritten implicitly.")
	} else if !os.IsNotExist(err) {
		return unavailable("credential destination cannot be inspected", "Check permissions on the destination directory.")
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: explicit caller-requested destination.
	if err != nil {
		if os.IsExist(err) {
			// A concurrent attempt may have published the same export.
			return writeCredentialExport(path, raw)
		}
		return unavailable("credential destination could not be created", "Choose a new path in a writable protected directory.")
	}
	remove := true
	defer func() {
		if remove {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(raw); err != nil {
		_ = file.Close()
		return unavailable("credential export could not be written", "Check free space and directory permissions.")
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return unavailable("credential export could not be synced", "Check the filesystem.")
	}
	if err := file.Close(); err != nil {
		return unavailable("credential export could not be closed", "Check the filesystem.")
	}
	if err := syncDir(filepath.Dir(path)); err != nil {
		return unavailable("credential destination directory could not be synced", "Check the filesystem and directory permissions.")
	}
	remove = false
	return nil
}

// ensureCredential creates the password and reference once and returns the
// stored value. It repairs an interrupted creation before the container starts,
// but never silently rotates a password for an existing volume.
func (s *Service) ensureCredential(spec Spec) (storedCredential, error) {
	if err := ensurePrivateDir(s.resourceDir(spec.Name)); err != nil {
		return storedCredential{}, unavailable("could not create the protected credential directory", "Check permissions on the service state directory.")
	}
	credentialPath := s.credentialPath(spec.Name)
	passwordPath := s.passwordPath(spec.Name)
	credentialRaw, credentialErr := readProtectedFile(credentialPath, 16*1024)
	passwordRaw, passwordErr := readProtectedFile(passwordPath, 1024)

	switch {
	case credentialErr == nil && passwordErr == nil:
		// Both exist; validate and return below.
	case credentialErr == nil:
		// Interrupted creation after the JSON was published.
		var stored storedCredential
		if err := decodeCredential(credentialRaw, &stored); err != nil {
			return storedCredential{}, err
		}
		if _, err := writeOnce(passwordPath, []byte(stored.Password), 0o600); err != nil {
			return storedCredential{}, unavailable("could not restore the protected password file", "Check permissions on the service state directory.")
		}
		return s.ensureCredential(spec)
	case passwordErr == nil:
		// Interrupted creation after the password file was published.
		password := strings.TrimSuffix(string(passwordRaw), "\n")
		if len(password) < 32 {
			return storedCredential{}, precondition("protected credential file is incomplete", "Inspect the resource credential files; do not rotate silently.")
		}
		stored := storedCredential{
			Schema: credentialSchema, Resource: spec.Name, Host: spec.Host,
			Username: spec.Username, Database: spec.Database, Password: password,
		}
		encoded, err := json.MarshalIndent(stored, "", "  ")
		if err != nil {
			return storedCredential{}, err
		}
		if _, err := writeOnce(credentialPath, append(encoded, '\n'), 0o600); err != nil {
			return storedCredential{}, unavailable("could not write the protected credential file", "Check permissions on the service state directory.")
		}
		return s.ensureCredential(spec)
	default:
		if !os.IsNotExist(credentialErr) || !os.IsNotExist(passwordErr) {
			return storedCredential{}, precondition("protected credential files cannot be read safely", "Inspect permissions and regular-file state in the service state directory.")
		}
		password, err := randomPassword()
		if err != nil {
			return storedCredential{}, err
		}
		if _, err := writeOnce(passwordPath, []byte(password), 0o600); err != nil {
			return storedCredential{}, unavailable("could not write the protected password file", "Check permissions on the service state directory.")
		}
		stored := storedCredential{
			Schema: credentialSchema, Resource: spec.Name, Host: spec.Host,
			Username: spec.Username, Database: spec.Database, Password: password,
		}
		encoded, err := json.MarshalIndent(stored, "", "  ")
		if err != nil {
			return storedCredential{}, err
		}
		if _, err := writeOnce(credentialPath, append(encoded, '\n'), 0o600); err != nil {
			return storedCredential{}, unavailable("could not write the protected credential file", "Check permissions on the service state directory.")
		}
		return s.ensureCredential(spec)
	}

	var stored storedCredential
	if err := decodeCredential(credentialRaw, &stored); err != nil {
		return storedCredential{}, err
	}
	password := strings.TrimSuffix(string(passwordRaw), "\n")
	if stored.Password != password {
		return storedCredential{}, precondition("protected credential files disagree", "Inspect the credential and password files; do not silently rotate the database password.")
	}
	if stored.Schema != credentialSchema || stored.Resource != spec.Name || stored.Host != spec.Host ||
		stored.Username != spec.Username || stored.Database != spec.Database || stored.Password == "" {
		return storedCredential{}, conflict("existing credentials belong to a different resource identity", "Use the original resource identity or destroy and recreate the resource deliberately.")
	}
	return stored, nil
}

func (s *Service) loadCredential(spec Spec) (storedCredential, error) {
	credentialRaw, err := readProtectedFile(s.credentialPath(spec.Name), 16*1024)
	if err != nil {
		return storedCredential{}, precondition("protected credentials are missing for an existing database", "Restore the protected credential files before starting or replacing this resource; the password is never guessed or rotated silently.")
	}
	passwordRaw, err := readProtectedFile(s.passwordPath(spec.Name), 1024)
	if err != nil {
		return storedCredential{}, precondition("protected password file is missing for an existing database", "Restore the protected credential files before starting or replacing this resource; the password is never guessed or rotated silently.")
	}
	var stored storedCredential
	if err := decodeCredential(credentialRaw, &stored); err != nil {
		return storedCredential{}, err
	}
	password := strings.TrimSuffix(string(passwordRaw), "\n")
	if stored.Password != password {
		return storedCredential{}, precondition("protected credential files disagree", "Inspect the credential and password files; do not silently rotate the database password.")
	}
	if stored.Resource != spec.Name || stored.Host != spec.Host || stored.Username != spec.Username || stored.Database != spec.Database {
		return storedCredential{}, conflict("existing credentials belong to a different resource identity", "Use the original resource identity or destroy and recreate the resource deliberately.")
	}
	return stored, nil
}

func decodeCredential(raw []byte, stored *storedCredential) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(stored); err != nil {
		return precondition("protected credential file is corrupt", "Inspect the credential file; do not recreate the database over unknown credentials.")
	}
	if stored.Schema != credentialSchema || stored.Password == "" {
		return precondition("protected credential file is invalid", "Inspect the credential file; do not recreate the database over unknown credentials.")
	}
	return nil
}

func readProtectedFile(path string, max int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("%s is accessible by group or others", path)
	}
	return readRegular(path, max)
}

func randomPassword() (string, error) {
	value := make([]byte, 32)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(value), nil
}

func (s *Service) removeCredential(spec Spec) error {
	for _, path := range []string{s.credentialPath(spec.Name), s.passwordPath(spec.Name)} {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	if err := syncDir(s.resourceDir(spec.Name)); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Remove(s.resourceDir(spec.Name)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
