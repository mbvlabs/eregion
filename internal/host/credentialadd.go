package host

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
)

// CredentialAddRequest adds one scoped automation credential to an existing
// server auth file and hands the plaintext token to an explicitly requested
// owner-only file.
type CredentialAddRequest struct {
	AuthFile string
	Actor    contract.Actor
	Scopes   []string
	TokenOut string
}

// CredentialAddResult contains no credential.
type CredentialAddResult struct {
	AuthFile    string   `json:"auth_file"`
	TokenOut    string   `json:"token_out"`
	ActorKind   string   `json:"actor_kind"`
	Actor       string   `json:"actor_subject"`
	Scopes      []string `json:"scopes"`
	Fingerprint string   `json:"token_fingerprint"`
	Created     bool     `json:"created"`
}

func (r CredentialAddRequest) Validate() error {
	if !filepath.IsAbs(r.AuthFile) {
		return contract.NewFault(contract.ValidationFailed, "the auth file path must be absolute", "Use a path such as /etc/eregion/auth.json.")
	}
	if !filepath.IsAbs(r.TokenOut) {
		return contract.NewFault(contract.ValidationFailed, "the token output path must be absolute", "Use a private absolute path such as /root/eregion-ci.token.")
	}
	if filepath.Clean(r.AuthFile) == filepath.Clean(r.TokenOut) {
		return contract.NewFault(contract.ValidationFailed, "the token output must differ from the auth file", "The auth file stores only hashes.")
	}
	if err := r.Actor.Validate(); err != nil {
		return err
	}
	if len(r.Scopes) == 0 {
		return contract.NewFault(contract.ValidationFailed, "at least one scope is required", "Use --scope operations:submit and other explicit scopes.")
	}
	for _, scope := range r.Scopes {
		if !knownScope(scope) {
			return contract.NewFault(contract.ValidationFailed, "unknown automation scope", "Use operations:submit, operations:read, operations:cancel, or system:read.")
		}
	}
	return nil
}

func (r CredentialAddRequest) inputs() ([]byte, error) {
	scopes := append([]string(nil), r.Scopes...)
	sortStrings(scopes)
	return canonicalInput(struct {
		AuthFile string         `json:"auth_file"`
		Actor    contract.Actor `json:"actor"`
		Scopes   []string       `json:"scopes"`
		TokenOut string         `json:"token_out"`
	}{r.AuthFile, r.Actor, scopes, r.TokenOut})
}

// Confirmation issues the plan-bound token for a credential addition.
func (r CredentialAddRequest) Confirmation(now time.Time) (Confirmation, error) {
	if err := r.Validate(); err != nil {
		return Confirmation{}, err
	}
	base, err := authRevision(r.AuthFile)
	if err != nil {
		return Confirmation{}, err
	}
	inputs, err := r.inputs()
	if err != nil {
		return Confirmation{}, err
	}
	return issueConfirmation("host.credential.add", r.AuthFile, base, inputs, now)
}

// Apply adds the credential. The token is written first; if the auth file
// update fails, a newly created token file is removed so a dangling secret is
// not left behind.
func (r CredentialAddRequest) Apply(confirmation string, now time.Time) (CredentialAddResult, error) {
	if err := r.Validate(); err != nil {
		return CredentialAddResult{}, err
	}
	base, err := authRevision(r.AuthFile)
	if err != nil {
		return CredentialAddResult{}, err
	}
	inputs, err := r.inputs()
	if err != nil {
		return CredentialAddResult{}, err
	}
	if err := verifyConfirmation(confirmation, "host.credential.add", r.AuthFile, base, inputs, now); err != nil {
		return CredentialAddResult{}, err
	}
	token, err := generateToken()
	if err != nil {
		return CredentialAddResult{}, err
	}
	created, err := writeSecretOnce(r.TokenOut, []byte(token+"\n"), "root", "root")
	if err != nil {
		return CredentialAddResult{}, err
	}
	principals, err := ReadAuthFile(r.AuthFile)
	if err == nil {
		principals = append(principals, principal(r.Actor, token, r.Scopes))
		err = WriteAuthFile(r.AuthFile, principals, "root", "root")
	}
	if err != nil {
		if created {
			_ = os.Remove(r.TokenOut)
		}
		return CredentialAddResult{}, err
	}
	return CredentialAddResult{
		AuthFile: r.AuthFile, TokenOut: r.TokenOut, ActorKind: r.Actor.Kind, Actor: r.Actor.Subject,
		Scopes: append([]string(nil), r.Scopes...), Fingerprint: tokenFingerprint(token), Created: created,
	}, nil
}

func authRevision(path string) (string, error) {
	raw, mode, exists, err := fileState(path)
	if err != nil {
		return "", err
	}
	if !exists {
		return "absent", nil
	}
	if mode&0o077 != 0 {
		return "", contract.NewFault(contract.PreconditionFailed, "the auth file is readable by group or others", "Fix its permissions to 0600 before adding a credential.")
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// AuthRevision exposes the non-secret base revision for plan output.
func AuthRevision(path string) (string, error) { return authRevision(path) }

// NewAutomationPrincipal is a small exported helper for callers that need the
// same actor shape without constructing an api.Principal themselves.
func NewAutomationPrincipal(actor contract.Actor, token string, scopes []string) (api.Principal, error) {
	if err := actor.Validate(); err != nil {
		return api.Principal{}, err
	}
	for _, scope := range scopes {
		if !knownScope(scope) {
			return api.Principal{}, fmt.Errorf("unknown scope %s", scope)
		}
	}
	return principal(actor, token, scopes), nil
}

func normalizeScopes(scopes []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, scope := range scopes {
		scope = strings.TrimSpace(scope)
		if scope != "" && !seen[scope] {
			seen[scope] = true
			out = append(out, scope)
		}
	}
	sortStrings(out)
	return out
}
