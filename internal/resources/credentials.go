package resources

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const credentialSchema = "eregion/resource-credential/v1"

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
