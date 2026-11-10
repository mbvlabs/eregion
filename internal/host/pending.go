package host

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/doctor"
	"github.com/mbvlabs/eregion/internal/engine"
)

// pendingSchema versions the local unfinished-init state. It may change between
// alphas; a binary that does not understand it refuses rather than guessing.
const pendingSchema = 1

// PendingInitFile is the unfinished init's answers and completed phases.
const PendingInitFile = "init.json"

// PendingKnownHostsFile is the host-key pin of the init's public first-contact
// address. The saved workspace access pins the WireGuard address in its own
// known_hosts, so --discard drops trust learned by an abandoned attempt.
const PendingKnownHostsFile = "known_hosts"

// PendingInit is one unfinished `server init`, saved under the private workspace store's
// directory with mode 0600. It holds answers and progress, never the external
// DSN or a generated key: the DSN stays in the operator's protected file and
// the operator identity lives in servers/<name>/ (already 0600).
type PendingInit struct {
	Schema int    `json:"schema"`
	Server string `json:"server"`
	Host   string `json:"host"`
	User   string `json:"user"`
	Port   int    `json:"port"`
	// Identity is the first-contact private key path, not its content.
	Identity           string `json:"identity,omitempty"`
	HostKeyFingerprint string `json:"host_key_fingerprint,omitempty"`
	Node               string `json:"node"`
	// Workspace is the requested portable identity; WorkspaceID is filled from
	// the authoritative database read before the handoff. Neither is guessed
	// from the context or node name.
	Workspace       string `json:"workspace,omitempty"`
	WorkspaceID     string `json:"workspace_id,omitempty"`
	Context         string `json:"context"`
	Hostname        string `json:"hostname"`
	DatabaseURLFile string `json:"database_url_file,omitempty"`
	AdminUser       string `json:"admin_user"`
	// Facts learned during init, so --resume does not need first contact.
	PublicIPv4    string `json:"public_ipv4,omitempty"`
	Platform      string `json:"platform,omitempty"`
	Arch          string `json:"arch,omitempty"`
	NodePublicKey string `json:"node_public_key,omitempty"`
	// HostKey is the pinned SSH host key ("type SHA256:…"), so a resume that
	// skips first contact can still build the handoff.
	HostKey string `json:"host_key,omitempty"`
	// SecretsOut is the record written by an earlier attempt, if any.
	SecretsOut string `json:"secrets_out,omitempty"`
	// Doctor is the report that passed the gate. A resumed handoff after the
	// gate therefore shows the same summary without rerunning doctor.
	Doctor *doctor.Report `json:"doctor,omitempty"`
	// DoctorSkipped records an explicit --skip-doctor pass so a resumed
	// handoff says skipped rather than inventing a report.
	DoctorSkipped bool              `json:"doctor_skipped,omitempty"`
	Phases        map[string]string `json:"phases"`
}

// PendingRoot is the directory of unfinished inits, beside the context file.
func PendingRoot() (string, error) {
	contexts, err := DefaultWorkspacePath()
	if err != nil {
		return "", err
	}
	return PendingRootAt(contexts)
}

// PendingRootAt is PendingRoot for one resolved private store path.
func PendingRootAt(storePath string) (string, error) {
	if strings.TrimSpace(storePath) == "" {
		return PendingRoot()
	}
	return filepath.Join(filepath.Dir(storePath), "pending"), nil
}

// PendingDir is pending/<server>. The name is validated here so every caller
// joins only a plain server name, never a path.
func PendingDir(name string) (string, error) {
	contexts, err := DefaultWorkspacePath()
	if err != nil {
		return "", err
	}
	return PendingDirAt(contexts, name)
}

// PendingDirAt is PendingDir for one resolved private store path.
func PendingDirAt(storePath, name string) (string, error) {
	if !accessNamePattern.MatchString(name) {
		return "", invalidPendingName()
	}
	root, err := PendingRootAt(storePath)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, name), nil
}

// PendingKnownHosts is the init's private known_hosts file.
func PendingKnownHosts(name string) (string, error) {
	contexts, err := DefaultWorkspacePath()
	if err != nil {
		return "", err
	}
	return PendingKnownHostsAt(contexts, name)
}

// PendingKnownHostsAt is PendingKnownHosts for one resolved private store path.
func PendingKnownHostsAt(storePath, name string) (string, error) {
	dir, err := PendingDirAt(storePath, name)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, PendingKnownHostsFile), nil
}

// PendingExists reports whether an unfinished init is saved for name.
func PendingExists(name string) bool {
	contexts, err := DefaultWorkspacePath()
	if err != nil {
		return false
	}
	return PendingExistsAt(contexts, name)
}

// PendingExistsAt is PendingExists for one resolved private store path.
func PendingExistsAt(storePath, name string) bool {
	dir, err := PendingDirAt(storePath, name)
	if err != nil {
		return false
	}
	_, err = os.Stat(filepath.Join(dir, PendingInitFile))
	return err == nil
}

// PendingNames lists unfinished inits, sorted.
func PendingNames() ([]string, error) {
	contexts, err := DefaultWorkspacePath()
	if err != nil {
		return nil, err
	}
	return PendingNamesAt(contexts)
}

// PendingNamesAt lists unfinished inits for one resolved private store path.
func PendingNamesAt(storePath string) ([]string, error) {
	root, err := PendingRootAt(storePath)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() && PendingExistsAt(storePath, entry.Name()) {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	return names, nil
}

// LoadPending reads pending/<name>/init.json.
func LoadPending(name string) (*PendingInit, error) {
	contexts, err := DefaultWorkspacePath()
	if err != nil {
		return nil, err
	}
	return LoadPendingAt(contexts, name)
}

// LoadPendingAt reads one unfinished init from a resolved private store path.
func LoadPendingAt(storePath, name string) (*PendingInit, error) {
	if !accessNamePattern.MatchString(name) {
		return nil, invalidPendingName()
	}
	dir, err := PendingDirAt(storePath, name)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, PendingInitFile)
	raw, mode, exists, err := fileState(path)
	if err != nil {
		return nil, err
	}
	if !exists {
		fault := contract.NewFault(contract.NotFound, "no unfinished init is saved for "+name, "Start one with eregion server init.")
		fault.Details = map[string]any{"reason": "nothing_to_resume", "pending_path": dir}
		return nil, fault
	}
	if mode&0o077 != 0 {
		fault := contract.NewFault(contract.PreconditionFailed, "the unfinished init is readable by group or others", "Run chmod 600 "+path+" before resuming.")
		fault.Details = map[string]any{"pending_path": dir}
		return nil, fault
	}
	var pending PendingInit
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&pending); err != nil || pending.Schema != pendingSchema || pending.Server != name {
		fault := contract.NewFault(contract.PreconditionFailed, "the unfinished init is invalid", "Discard it with eregion server init --discard --workspace-name "+name+" and start again.")
		fault.Details = map[string]any{"pending_path": dir}
		return nil, fault
	}
	if pending.Phases == nil {
		pending.Phases = map[string]string{}
	}
	return &pending, nil
}

// Save writes the unfinished init atomically, owner-only.
func (p *PendingInit) Save() error {
	contexts, err := DefaultWorkspacePath()
	if err != nil {
		return err
	}
	return p.SaveAt(contexts)
}

// SaveAt writes the unfinished init under one resolved private store path.
func (p *PendingInit) SaveAt(storePath string) error {
	if !accessNamePattern.MatchString(p.Server) {
		return invalidPendingName()
	}
	if p.Phases == nil {
		p.Phases = map[string]string{}
	}
	p.Schema = pendingSchema
	dir, err := PendingDirAt(storePath, p.Server)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return engine.WriteFileAtomic(filepath.Join(dir, PendingInitFile), append(raw, '\n'), 0o600)
}

// Done reports whether phase completed.
func (p *PendingInit) Done(phase string) bool {
	_, ok := p.Phases[phase]
	return ok
}

// Mark records phase as completed now.
func (p *PendingInit) Mark(phase string) {
	if p.Phases == nil {
		p.Phases = map[string]string{}
	}
	p.Phases[phase] = time.Now().UTC().Format(time.RFC3339)
}

// SavePendingFile writes one protected helper file (for example the vault
// keyring captured for the handoff) under pending/<name>/.
func SavePendingFile(name, file string, data []byte) error {
	contexts, err := DefaultWorkspacePath()
	if err != nil {
		return err
	}
	return SavePendingFileAt(contexts, name, file, data)
}

// SavePendingFileAt is SavePendingFile for one resolved private store path.
func SavePendingFileAt(storePath, name, file string, data []byte) error {
	if !accessNamePattern.MatchString(name) || strings.ContainsAny(file, "/\\\x00\r\n") {
		return invalidPendingName()
	}
	dir, err := PendingDirAt(storePath, name)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return engine.WriteFileAtomic(filepath.Join(dir, file), data, 0o600)
}

// LoadPendingFile reads a helper file saved by SavePendingFile.
func LoadPendingFile(name, file string) ([]byte, error) {
	contexts, err := DefaultWorkspacePath()
	if err != nil {
		return nil, err
	}
	return LoadPendingFileAt(contexts, name, file)
}

// LoadPendingFileAt is LoadPendingFile for one resolved private store path.
func LoadPendingFileAt(storePath, name, file string) ([]byte, error) {
	if !accessNamePattern.MatchString(name) || strings.ContainsAny(file, "/\\\x00\r\n") {
		return nil, invalidPendingName()
	}
	dir, err := PendingDirAt(storePath, name)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, file)
	raw, mode, exists, err := fileState(path)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, contract.NewFault(contract.PreconditionFailed, "the unfinished init is missing "+file, "Discard it and start again.")
	}
	if mode&0o077 != 0 {
		return nil, contract.NewFault(contract.PreconditionFailed, file+" in the unfinished init is not owner-only", "Run chmod 600 "+path+".")
	}
	return raw, nil
}

// RemovePending deletes pending/<name>.
func RemovePending(name string) error {
	contexts, err := DefaultWorkspacePath()
	if err != nil {
		return err
	}
	return RemovePendingAt(contexts, name)
}

// RemovePendingAt deletes pending/<name> under one resolved private store path.
func RemovePendingAt(storePath, name string) error {
	dir, err := PendingDirAt(storePath, name)
	if err != nil {
		return err
	}
	return os.RemoveAll(dir)
}

func invalidPendingName() error {
	return contract.NewFault(contract.ValidationFailed, "invalid server name", "Use lowercase letters, digits, hyphens, or underscores.")
}
