// Package api contains authenticated HTTP adapters, not operation business logic.
package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
)

var tokenPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{32,256}$`)

var tokenNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,63}$`)

var knownScopes = []string{"operations:submit", "operations:read", "operations:cancel", "system:read"}

type Principal struct {
	Actor        contract.Actor `json:"actor"`
	TokenSHA256  string         `json:"token_sha256"`
	Scopes       []string       `json:"scopes"`
	ActionGrants []ActionGrant  `json:"action_grants,omitempty"`
	// Name identifies a managed per-use token for listing and revocation.
	// Operator and system principals leave it empty.
	Name string `json:"name,omitempty"`
	// TokenPrefix is the first few characters of the token, safe to list.
	TokenPrefix string     `json:"token_prefix,omitempty"`
	CreatedAt   *time.Time `json:"created_at,omitempty"`
	ExpiresAt   *time.Time `json:"expires_at,omitempty"`
}

type Auth struct{ principals map[[32]byte]Principal }

// LoadAuth reads explicit hashes only, never plaintext API tokens. Permissions are
// checked on the opened file to avoid a stat/open race. Reload requires restart.
func LoadAuth(path string) (*Auth, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, contract.NewFault(contract.ValidationFailed, "could not open API credential file", "Set EREGION_AUTH_FILE to a private regular JSON file.")
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil || !stat.Mode().IsRegular() || stat.Mode().Perm()&0077 != 0 {
		return nil, contract.NewFault(contract.ValidationFailed, "API credential file permissions are unsafe", "Use a regular file readable only by its owner (0600 or 0400).")
	}
	raw, err := io.ReadAll(io.LimitReader(file, 65537))
	if err != nil || len(raw) > 65536 {
		return nil, contract.NewFault(contract.ValidationFailed, "API credential file is unreadable or too large", "Limit the file to 64 KiB.")
	}
	var principals []Principal
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&principals); err != nil || decoder.Decode(new(any)) != io.EOF {
		return nil, contract.NewFault(contract.ValidationFailed, "invalid API credential file", "Use an array of actor, token_sha256, and scopes objects; unknown fields are rejected.")
	}
	return NewAuth(principals)
}

func NewAuth(principals []Principal) (*Auth, error) {
	if len(principals) > 128 {
		return nil, contract.NewFault(contract.ValidationFailed, "invalid number of API credentials", "Configure at most 128 scoped credentials.")
	}
	auth := &Auth{principals: make(map[[32]byte]Principal)}
	for _, principal := range principals {
		if err := principal.Actor.Validate(); err != nil {
			return nil, err
		}
		digest, err := hex.DecodeString(principal.TokenSHA256)
		if err != nil || len(digest) != sha256.Size || principal.TokenSHA256 != strings.ToLower(principal.TokenSHA256) || len(principal.Scopes) == 0 {
			return nil, contract.NewFault(contract.ValidationFailed, "invalid API credential hash or scopes", "Use a lowercase SHA-256 hash of a random token and explicit supported scopes.")
		}
		var key [32]byte
		copy(key[:], digest)
		if _, exists := auth.principals[key]; exists {
			return nil, contract.NewFault(contract.ValidationFailed, "duplicate API credential hash", "Each token must resolve to exactly one principal.")
		}
		if principal.Name != "" && !tokenNamePattern.MatchString(principal.Name) {
			return nil, contract.NewFault(contract.ValidationFailed, "invalid API credential name", "Use 2–64 lowercase letters, digits, dots, underscores, or hyphens.")
		}
		if principal.TokenPrefix != "" && len(principal.TokenPrefix) > 16 {
			return nil, contract.NewFault(contract.ValidationFailed, "invalid API credential prefix", "Store at most the first 16 characters for listing.")
		}
		if principal.CreatedAt != nil && principal.CreatedAt.IsZero() {
			return nil, contract.NewFault(contract.ValidationFailed, "invalid API credential creation time", "Omit created_at or use an RFC 3339 time.")
		}
		if principal.ExpiresAt != nil {
			if principal.ExpiresAt.IsZero() {
				return nil, contract.NewFault(contract.ValidationFailed, "invalid API credential expiry", "Omit expires_at or use an RFC 3339 time.")
			}
			if principal.CreatedAt != nil && !principal.ExpiresAt.After(*principal.CreatedAt) {
				return nil, contract.NewFault(contract.ValidationFailed, "invalid API credential expiry", "Expiry must be after creation.")
			}
		}
		for _, scope := range principal.Scopes {
			if !slices.Contains(knownScopes, scope) {
				return nil, contract.NewFault(contract.ValidationFailed, "unknown API scope", "Use operations:submit, operations:read, operations:cancel, or system:read.")
			}
		}
		principal.Scopes = slices.Clone(principal.Scopes)
		if err := validateGrants(principal.ActionGrants); err != nil {
			return nil, err
		}
		principal.ActionGrants = slices.Clone(principal.ActionGrants)
		auth.principals[key] = principal
	}
	return auth, nil
}

func (a *Auth) authenticate(header, scope string) (contract.Actor, error) {
	principal, err := a.resolvePrincipal(header)
	if err != nil {
		return contract.Actor{}, err
	}
	if !slices.Contains(principal.Scopes, scope) {
		return contract.Actor{}, contract.NewFault(contract.Forbidden, "API credential lacks the required scope", "Use a credential explicitly authorized for this action.")
	}
	return principal.Actor, nil
}

// authenticateAction resolves a credential and checks one action grant after
// normalization. CI handlers use it so a deploy token can only reach its own
// environment.
func (a *Auth) authenticateAction(header, scope, kind, target string) (Principal, error) {
	principal, err := a.resolvePrincipal(header)
	if err != nil {
		return Principal{}, err
	}
	if !slices.Contains(principal.Scopes, scope) {
		return Principal{}, contract.NewFault(contract.Forbidden, "API credential lacks the required scope", "Use a credential explicitly authorized for this action.")
	}
	if err := authorizePrincipalAction(principal, kind, target); err != nil {
		return Principal{}, err
	}
	return principal, nil
}

func (a *Auth) resolvePrincipal(header string) (Principal, error) {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) || !tokenPattern.MatchString(strings.TrimPrefix(header, prefix)) {
		return Principal{}, contract.NewFault(contract.Unauthenticated, "API authentication required", "Supply a valid Bearer token.")
	}
	if a == nil {
		return Principal{}, contract.NewFault(contract.Unauthenticated, "API authentication is not configured", "Configure scoped API credentials.")
	}
	principal, ok := a.principals[sha256.Sum256([]byte(strings.TrimPrefix(header, prefix)))]
	if !ok {
		return Principal{}, contract.NewFault(contract.Unauthenticated, "API authentication failed", "Check the token and configured server credentials.")
	}
	if principal.ExpiresAt != nil && !principal.ExpiresAt.After(time.Now()) {
		return Principal{}, contract.NewFault(contract.Unauthenticated, "API credential expired", "Mint a new per-use token with eregion token create.")
	}
	return principal, nil
}
