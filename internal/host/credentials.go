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

// Credentials is the bootstrap credential handoff. Setup mints no API token;
// CI/webhook tokens are added later with an explicit create command.
type Credentials struct{}

// LoadCredentials validates the role before bootstrap runs. It creates no
// credential.
func LoadCredentials(cfg Config, _ bool) (Credentials, error) {
	if _, err := cfg.RoleConfig(); err != nil {
		return Credentials{}, err
	}
	return Credentials{}, nil
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

// AuthPrincipals builds the deterministic auth file contents. Setup writes an
// empty list; the auth file is only the CI/webhook token path.
func (c Credentials) AuthPrincipals(string) ([]api.Principal, error) {
	principals := []api.Principal{}
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
func (c Credentials) TokenHashes() map[string]string { return map[string]string{} }

// CredentialFingerprint is safe to show. It is a digest, not the credential.
func CredentialFingerprint(token string) string {
	if token == "" {
		return ""
	}
	return tokenHash(token)[:12]
}

func tokenFingerprint(token string) string { return CredentialFingerprint(token) }

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
		return principals, nil
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
	values := []string{cfg.DatabaseURL}
	if strings.Contains(cfg.DatabaseURL, ":") {
		// The URL password is often quoted or embedded in an error; register
		// the individual password too.
		if parsed, err := parseDatabaseURL(cfg.DatabaseURL); err == nil {
			values = append(values, parsed.password)
		}
	}
	return values
}
