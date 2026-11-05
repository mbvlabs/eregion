// Package api contains authenticated HTTP adapters, not operation business logic.
package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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

// Auth authenticates bearer tokens against an immutable snapshot of the
// credential file. An Auth loaded from a path re-reads that file when its
// identity changes, so minting or revoking a token needs no service restart.
type Auth struct {
	path     string
	snapshot atomic.Pointer[authSnapshot]
	reload   sync.Mutex
}

// authSnapshot is immutable once published. err is non-nil when the file
// changed but could not be accepted; every token is then denied until a valid
// version loads, never falling back to the previous, possibly revoked set.
type authSnapshot struct {
	principals map[[32]byte]Principal
	stamp      authFileStamp
	err        error
}

// authFileStamp identifies one version of the credential file. Size, mode and
// mtime catch ordinary rewrites; os.SameFile catches an atomic rename whose
// content happens to match the old metadata; ownership and ctime catch a
// chown/chmod that changes whether the service account may read it.
type authFileStamp struct {
	present            bool
	size               int64
	mode               os.FileMode
	modTime            time.Time
	info               os.FileInfo
	uid, gid           uint32
	ctimeSec, ctimeNS  int64
	ownershipAvailable bool
}

func stampFor(info os.FileInfo) authFileStamp {
	if info == nil {
		return authFileStamp{}
	}
	stamp := authFileStamp{present: true, size: info.Size(), mode: info.Mode(), modTime: info.ModTime(), info: info}
	stamp.uid, stamp.gid, stamp.ctimeSec, stamp.ctimeNS, stamp.ownershipAvailable = authOwnership(info)
	return stamp
}

func statAuthFile(path string) authFileStamp {
	info, err := os.Stat(path)
	if err != nil {
		return authFileStamp{}
	}
	return stampFor(info)
}

func (s authFileStamp) equal(other authFileStamp) bool {
	if s.present != other.present {
		return false
	}
	if !s.present {
		return true
	}
	if s.size != other.size || s.mode != other.mode || !s.modTime.Equal(other.modTime) {
		return false
	}
	if s.ownershipAvailable != other.ownershipAvailable {
		return false
	}
	if s.ownershipAvailable && (s.uid != other.uid || s.gid != other.gid || s.ctimeSec != other.ctimeSec || s.ctimeNS != other.ctimeNS) {
		return false
	}
	return s.info != nil && other.info != nil && os.SameFile(s.info, other.info)
}

// LoadAuth reads explicit hashes only, never plaintext API tokens. Permissions
// are checked on the opened file to avoid a stat/open race. Later rewrites are
// observed by stat on each authentication.
func LoadAuth(path string) (*Auth, error) {
	principals, stamp, err := readAuthFile(path)
	if err != nil {
		return nil, err
	}
	auth := &Auth{path: path}
	auth.snapshot.Store(&authSnapshot{principals: principals, stamp: stamp})
	return auth, nil
}

func readAuthFile(path string) (map[[32]byte]Principal, authFileStamp, error) {
	return readAuthFileWith(path, nil)
}

// readAuthFileWith lets a test mutate the file after a failed open or between
// the content read and the stability check. Production always passes nil.
func readAuthFileWith(path string, beforeStat func()) (map[[32]byte]Principal, authFileStamp, error) {
	file, err := os.Open(path)
	if err != nil {
		if beforeStat != nil {
			beforeStat()
		}
		// The opened file, not the path, is the version that was validated.
		// A permission or ownership repair between the failed open and this
		// return must leave the failure retryable, never stamped as the
		// repaired file.
		return nil, authFileStamp{}, contract.NewFault(contract.ValidationFailed, "could not open API credential file", "Set EREGION_AUTH_FILE to a private regular JSON file.")
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil || !stat.Mode().IsRegular() || stat.Mode().Perm()&0077 != 0 {
		return nil, stampFor(stat), contract.NewFault(contract.ValidationFailed, "API credential file permissions are unsafe", "Use a regular file readable only by its owner (0600 or 0400).")
	}
	raw, err := io.ReadAll(io.LimitReader(file, 65537))
	if err != nil || len(raw) > 65536 {
		return nil, stampFor(stat), contract.NewFault(contract.ValidationFailed, "API credential file is unreadable or too large", "Limit the file to 64 KiB.")
	}
	var principals []Principal
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&principals); err != nil || decoder.Decode(new(any)) != io.EOF {
		return nil, stampFor(stat), contract.NewFault(contract.ValidationFailed, "invalid API credential file", "Use an array of actor, token_sha256, and scopes objects; unknown fields are rejected.")
	}
	if beforeStat != nil {
		beforeStat()
	}
	// Refuse content that was modified while it was read: a torn document must
	// not become the authenticated snapshot. The returned zero stamp makes the
	// next authentication retry instead of caching the final file version as
	// permanently invalid.
	after, err := file.Stat()
	if err != nil || !stampFor(stat).equal(stampFor(after)) {
		return nil, authFileStamp{}, contract.NewFault(contract.ValidationFailed, "API credential file changed while it was being read", "Rewrite it atomically and retry.")
	}
	indexed, err := indexPrincipals(principals)
	if err != nil {
		return nil, stampFor(after), err
	}
	return indexed, stampFor(after), nil
}

func NewAuth(principals []Principal) (*Auth, error) {
	indexed, err := indexPrincipals(principals)
	if err != nil {
		return nil, err
	}
	auth := &Auth{}
	auth.snapshot.Store(&authSnapshot{principals: indexed})
	return auth, nil
}

func indexPrincipals(principals []Principal) (map[[32]byte]Principal, error) {
	if len(principals) > 128 {
		return nil, contract.NewFault(contract.ValidationFailed, "invalid number of API credentials", "Configure at most 128 scoped credentials.")
	}
	index := make(map[[32]byte]Principal, len(principals))
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
		if _, exists := index[key]; exists {
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
		index[key] = principal
	}
	return index, nil
}

// currentSnapshot returns the snapshot for this request, reloading it first
// when the credential file changed. A stat per request is cheap and keeps
// revocation latency at one request instead of a timer or a restart.
func (a *Auth) currentSnapshot() *authSnapshot {
	snapshot := a.snapshot.Load()
	if a.path == "" {
		return snapshot
	}
	stamp := statAuthFile(a.path)
	if snapshot != nil && stamp.equal(snapshot.stamp) {
		return snapshot
	}
	return a.reloadSnapshot()
}

func (a *Auth) reloadSnapshot() *authSnapshot {
	a.reload.Lock()
	defer a.reload.Unlock()
	snapshot := a.snapshot.Load()
	stamp := statAuthFile(a.path)
	if snapshot != nil && stamp.equal(snapshot.stamp) {
		return snapshot
	}
	principals, readStamp, err := readAuthFile(a.path)
	if err != nil {
		// Remember the file version that failed so requests over the same bad
		// file keep failing closed without re-reading or logging on every
		// request; a later rewrite changes the stamp and is retried.
		snapshot = &authSnapshot{stamp: readStamp, err: err}
		a.snapshot.Store(snapshot)
		slog.Error("API credential store changed but could not be reloaded; token authentication is denied until it is valid", "path", a.path, "error", err)
		return snapshot
	}
	snapshot = &authSnapshot{principals: principals, stamp: readStamp}
	a.snapshot.Store(snapshot)
	return snapshot
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
	snapshot := a.currentSnapshot()
	if snapshot == nil {
		return Principal{}, contract.NewFault(contract.Unauthenticated, "API authentication is not configured", "Configure scoped API credentials.")
	}
	if snapshot.err != nil {
		return Principal{}, contract.NewFault(contract.Unauthenticated, "API authentication is unavailable", "The API credential file is unreadable or invalid; token authentication stays disabled until it is fixed.")
	}
	principal, ok := snapshot.principals[sha256.Sum256([]byte(strings.TrimPrefix(header, prefix)))]
	if !ok {
		return Principal{}, contract.NewFault(contract.Unauthenticated, "API authentication failed", "Check the token and configured server credentials.")
	}
	if principal.ExpiresAt != nil && !principal.ExpiresAt.After(time.Now()) {
		return Principal{}, contract.NewFault(contract.Unauthenticated, "API credential expired", "Mint a new per-use token with eregion token create.")
	}
	return principal, nil
}
