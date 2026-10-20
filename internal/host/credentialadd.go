package host

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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
	// Grants bind the credential to explicit platform action kinds and
	// canonical targets. They use the same api.ActionGrant semantics the server
	// enforces; an empty grant list keeps the diagnostic-only automation actor.
	Grants   []api.ActionGrant
	TokenOut string
}

// CredentialAddResult contains no credential.
type CredentialAddResult struct {
	AuthFile    string            `json:"auth_file"`
	TokenOut    string            `json:"token_out"`
	ActorKind   string            `json:"actor_kind"`
	Actor       string            `json:"actor_subject"`
	Scopes      []string          `json:"scopes"`
	Grants      []api.ActionGrant `json:"action_grants,omitempty"`
	Fingerprint string            `json:"token_fingerprint"`
	Created     bool              `json:"created"`
}

// ParseActionGrant parses the repeatable --action-grant kind:target form and
// validates it with the server's authorization rules.
func ParseActionGrant(raw string) (api.ActionGrant, error) {
	kind, target, ok := strings.Cut(strings.TrimSpace(raw), ":")
	kind, target = strings.TrimSpace(kind), strings.TrimSpace(target)
	if !ok || kind == "" || target == "" {
		return api.ActionGrant{}, contract.NewFault(contract.ValidationFailed, "invalid action grant", "Use kind:target, for example platform.deploy:env/api/production.")
	}
	grant := api.ActionGrant{Kind: kind, Target: target}
	if err := validateActionGrants([]api.ActionGrant{grant}); err != nil {
		return api.ActionGrant{}, err
	}
	return grant, nil
}

func validateActionGrants(grants []api.ActionGrant) error {
	if len(grants) == 0 {
		return nil
	}
	// NewAuth applies the server's exact grant validation, including wildcard,
	// target, and duplicate rejection.
	_, err := api.NewAuth([]api.Principal{{
		Actor:        contract.Actor{Kind: "ci", Subject: "grant-validation"},
		TokenSHA256:  strings.Repeat("0", 64),
		Scopes:       []string{"operations:submit"},
		ActionGrants: grants,
	}})
	if err != nil {
		return contract.NewFault(contract.ValidationFailed, "invalid action grant", contract.Classify(err).Message)
	}
	return nil
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
	if err := validateActionGrants(r.Grants); err != nil {
		return err
	}
	return nil
}

func (r CredentialAddRequest) inputs() ([]byte, error) {
	scopes := append([]string(nil), r.Scopes...)
	sortStrings(scopes)
	grants := append([]api.ActionGrant(nil), r.Grants...)
	sort.Slice(grants, func(i, j int) bool {
		if grants[i].Kind != grants[j].Kind {
			return grants[i].Kind < grants[j].Kind
		}
		return grants[i].Target < grants[j].Target
	})
	return canonicalInput(struct {
		AuthFile string            `json:"auth_file"`
		Actor    contract.Actor    `json:"actor"`
		Scopes   []string          `json:"scopes"`
		Grants   []api.ActionGrant `json:"action_grants,omitempty"`
		TokenOut string            `json:"token_out"`
	}{r.AuthFile, r.Actor, scopes, grants, r.TokenOut})
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
		added := principal(r.Actor, token, r.Scopes)
		added.ActionGrants = append([]api.ActionGrant(nil), r.Grants...)
		principals = append(principals, added)
		err = WriteAuthFile(r.AuthFile, principals, "root", "root")
	}
	if err != nil {
		if created {
			_ = os.Remove(r.TokenOut)
		}
		return CredentialAddResult{}, err
	}
	grants := append([]api.ActionGrant(nil), r.Grants...)
	sort.Slice(grants, func(i, j int) bool {
		if grants[i].Kind != grants[j].Kind {
			return grants[i].Kind < grants[j].Kind
		}
		return grants[i].Target < grants[j].Target
	})
	return CredentialAddResult{
		AuthFile: r.AuthFile, TokenOut: r.TokenOut, ActorKind: r.Actor.Kind, Actor: r.Actor.Subject,
		Scopes: append([]string(nil), r.Scopes...), Grants: grants, Fingerprint: tokenFingerprint(token), Created: created,
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
