package host

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
)

var tokenPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{32,256}$`)

var scopes = []string{"operations:submit", "operations:read", "operations:cancel", "system:read"}

func knownScope(scope string) bool {
	for _, known := range scopes {
		if scope == known {
			return true
		}
	}
	return false
}

// KnownScopes returns a copy of the server's accepted API scopes.
func KnownScopes() []string { return append([]string(nil), scopes...) }

// Credentials is the secret handoff for one bootstrap. Empty tokens mean this
// is a read-only plan and the credential has not been generated yet.
type Credentials struct {
	Operator          string
	Automation        string
	AutomationSubject string
	AutomationScopes  []string
}

// LoadCredentials reads or creates the two bootstrap credentials under the
// bootstrap state directory. Plan (apply=false) never creates a credential.
func LoadCredentials(cfg Config, apply bool) (Credentials, error) {
	role, err := cfg.RoleConfig()
	if err != nil {
		return Credentials{}, err
	}
	if !role.ControlPlane {
		return Credentials{}, nil
	}
	credentials := Credentials{}
	credentials.Operator, err = loadToken(filepath.Join(cfg.Layout.StateDir, "secrets", "operator.token"), cfg.OperatorTokenFile, apply)
	if err != nil {
		return Credentials{}, err
	}
	if len(cfg.AutomationScopes) > 0 {
		credentials.AutomationSubject = cfg.AutomationSubject
		credentials.AutomationScopes = append([]string(nil), cfg.AutomationScopes...)
		credentials.Automation, err = loadToken(filepath.Join(cfg.Layout.StateDir, "secrets", "automation.token"), "", apply)
		if err != nil {
			return Credentials{}, err
		}
	}
	return credentials, nil
}

func loadToken(statePath, inputPath string, create bool) (string, error) {
	raw, mode, exists, err := fileState(statePath)
	if err != nil {
		return "", err
	}
	if exists {
		if mode != 0o600 {
			return "", fmt.Errorf("the bootstrap credential at %s must be mode 0600", statePath)
		}
		token := strings.TrimSpace(string(raw))
		if !tokenPattern.MatchString(token) {
			return "", fmt.Errorf("the bootstrap credential at %s is invalid; restore it or remove the bootstrap state deliberately", statePath)
		}
		if inputPath != "" {
			provided, err := readTokenFile(inputPath)
			if err != nil {
				return "", err
			}
			if provided != token {
				return "", fmt.Errorf("the supplied credential differs from the existing bootstrap credential; refusing to rotate it implicitly")
			}
		}
		return token, nil
	}
	if inputPath != "" {
		token, err := readTokenFile(inputPath)
		if err != nil {
			return "", err
		}
		if create {
			if err := writeReplacingSecret(statePath, []byte(token+"\n"), "", ""); err != nil {
				return "", err
			}
		}
		return token, nil
	}
	if !create {
		return "", nil
	}
	token, err := generateToken()
	if err != nil {
		return "", err
	}
	if err := writeReplacingSecret(statePath, []byte(token+"\n"), "", ""); err != nil {
		return "", err
	}
	return token, nil
}

func readTokenFile(path string) (string, error) {
	raw, err := readPrivateFile(path, 1024)
	if err != nil {
		return "", fmt.Errorf("read the credential file %s: %w", path, err)
	}
	token := strings.TrimSpace(string(raw))
	if !tokenPattern.MatchString(token) {
		return "", fmt.Errorf("the credential file %s does not contain a valid API token", path)
	}
	return token, nil
}

func generateToken() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

// Hash returns the lowercase SHA-256 digest the server stores.
func tokenHash(token string) string {
	hash := sha256.Sum256([]byte(token))
	return hex.EncodeToString(hash[:])
}

// Principal returns the server credential for a token. The token itself is
// never stored in the auth file.
func principal(actor contract.Actor, token string, scopes []string) api.Principal {
	copied := append([]string(nil), scopes...)
	return api.Principal{Actor: actor, TokenSHA256: tokenHash(token), Scopes: copied}
}

// AuthPrincipals builds the deterministic auth file contents.
func (c Credentials) AuthPrincipals(node string) ([]api.Principal, error) {
	if c.Operator == "" {
		return nil, fmt.Errorf("the operator credential has not been generated")
	}
	operator := principal(contract.Actor{Kind: "operator", Subject: "operator@" + node}, c.Operator, scopes)
	principals := []api.Principal{operator}
	if c.Automation != "" {
		subject := c.AutomationSubject
		if subject == "" {
			subject = "automation@" + node
		}
		principals = append(principals, principal(contract.Actor{Kind: "ci", Subject: subject}, c.Automation, c.AutomationScopes))
	}
	if _, err := api.NewAuth(principals); err != nil {
		return nil, contract.NewFault(contract.ValidationFailed, "the generated API credentials are invalid", err.Error())
	}
	return principals, nil
}

// AuthFile encodes the principals in a stable order.
func (c Credentials) AuthFile(node string) ([]byte, error) {
	principals, err := c.AuthPrincipals(node)
	if err != nil {
		return nil, err
	}
	raw, err := json.MarshalIndent(principals, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

// TokenHashes is the only credential representation allowed in step inputs.
func (c Credentials) TokenHashes() map[string]string {
	hashes := map[string]string{}
	if c.Operator != "" {
		hashes["operator"] = engine.HashSecret(c.Operator)
	}
	if c.Automation != "" {
		hashes["automation"] = engine.HashSecret(c.Automation)
	}
	return hashes
}

// CredentialFingerprint is safe to show. It is a digest, not the credential.
func CredentialFingerprint(token string) string {
	if token == "" {
		return ""
	}
	return tokenHash(token)[:12]
}

func exportToken(path, token, owner, group string) (bool, error) {
	if token == "" {
		return false, fmt.Errorf("cannot export an empty credential")
	}
	return writeSecretOnce(path, []byte(token+"\n"), owner, group)
}

// ReadAuthFile reads the existing principal list for an additive credential
// command. It enforces the same owner-only mode as api.LoadAuth.
func ReadAuthFile(path string) ([]api.Principal, error) {
	raw, mode, exists, err := fileState(path)
	if err != nil {
		return nil, err
	}
	if !exists {
		return nil, nil
	}
	if mode&0o077 != 0 {
		return nil, fmt.Errorf("%s must be readable only by its owner", path)
	}
	var principals []api.Principal
	if err := json.Unmarshal(raw, &principals); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if len(principals) == 0 {
		return nil, fmt.Errorf("%s contains no credentials", path)
	}
	if _, err := api.NewAuth(principals); err != nil {
		return nil, err
	}
	return principals, nil
}

// WriteAuthFile validates and atomically replaces an auth file with 0600.
func WriteAuthFile(path string, principals []api.Principal, owner, group string) error {
	if _, err := api.NewAuth(principals); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(principals, "", "  ")
	if err != nil {
		return err
	}
	return writeFile(path, append(raw, '\n'), 0o600, owner, group)
}

// ReadTokenFile is the CLI adapter for explicit token inputs.
func ReadTokenFile(path string) (string, error) { return readTokenFile(path) }

// ValidToken reports whether a handed-off value has the API token shape. It
// does not prove the token is authorized; the first authenticated request does.
func ValidToken(token string) bool { return tokenPattern.MatchString(strings.TrimSpace(token)) }

// GenerateToken returns a new 256-bit API credential.
func GenerateToken() (string, error) { return generateToken() }

// Ensure private state directories exist with root-only access.
func ensureState(cfg Config) error {
	if err := ensureDir(cfg.Layout.StateDir, 0o700); err != nil {
		return err
	}
	if err := ensureDir(filepath.Join(cfg.Layout.StateDir, "secrets"), 0o700); err != nil {
		return err
	}
	return ensureDir(cfg.Layout.NodeStateDir, 0o700)
}

// secretsForRedaction returns every secret bootstrap holds in memory. Callers
// register them before producing any output.
func secretsForRedaction(cfg Config, credentials Credentials) []string {
	values := []string{cfg.DatabaseURL, credentials.Operator, credentials.Automation}
	if strings.Contains(cfg.DatabaseURL, ":") {
		// The URL password is often quoted or embedded in an error; register
		// the individual password too.
		if parsed, err := parseDatabaseURL(cfg.DatabaseURL); err == nil {
			values = append(values, parsed.password)
		}
	}
	return values
}
