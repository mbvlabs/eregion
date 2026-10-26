// Package tokens manages per-use deploy credentials in the node's auth file.
// Only hashes are stored; the plaintext token is returned once by Create.
package tokens

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/host"
	"github.com/mbvlabs/eregion/internal/platform"
)

const (
	// Prefix starts every eregion-managed token. It fits the API token shape.
	Prefix = "ert_"
	// DefaultTTL is how long a new token works without --expires.
	DefaultTTL = 24 * time.Hour
	// MinTTL and MaxTTL bound an explicit --expires.
	MinTTL = time.Minute
	MaxTTL = 90 * 24 * time.Hour
	// Scopes are what a deploy token needs: submit one deploy and read its
	// operation.
	submitScope = "operations:submit"
	readScope   = "operations:read"
)

var namePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,63}$`)

// CreateRequest asks for one deploy token bound to exactly one environment.
type CreateRequest struct {
	App         string
	Environment string
	// Name is the stable identifier used by list and revoke. Empty generates
	// a name from the app, environment and a random suffix.
	Name string
	TTL  time.Duration
}

// Metadata is the non-secret listing form. It never contains the token or its
// full hash.
type Metadata struct {
	Name         string            `json:"name"`
	ActorKind    string            `json:"actor_kind"`
	ActorSubject string            `json:"actor_subject"`
	App          string            `json:"app"`
	Environment  string            `json:"environment"`
	Scopes       []string          `json:"scopes"`
	ActionGrants []api.ActionGrant `json:"action_grants"`
	TokenPrefix  string            `json:"token_prefix"`
	Fingerprint  string            `json:"fingerprint"`
	CreatedAt    time.Time         `json:"created_at"`
	ExpiresAt    time.Time         `json:"expires_at"`
}

// Issued is Create's one-time result.
type Issued struct {
	Token    string   `json:"token"`
	Metadata Metadata `json:"metadata"`
}

// Create mints a token, writes only its hash to path and returns the token.
func Create(path string, request CreateRequest, now time.Time) (Issued, error) {
	if err := validateRequest(&request); err != nil {
		return Issued{}, err
	}
	if now.IsZero() {
		now = time.Now()
	}
	token, err := generate()
	if err != nil {
		return Issued{}, err
	}
	if request.Name == "" {
		request.Name = defaultName(request.App, request.Environment)
	}
	if err := ValidateName(request.Name); err != nil {
		return Issued{}, err
	}
	principals, err := host.ReadAuthFile(path)
	if err != nil {
		return Issued{}, err
	}
	for _, existing := range principals {
		if existing.Name == request.Name {
			return Issued{}, contract.NewFault(contract.Conflict, "a token named "+request.Name+" already exists", "Choose another --name or revoke the existing token first.")
		}
	}
	created := now.UTC()
	expires := created.Add(request.TTL)
	grant := api.ActionGrant{Kind: platform.KindDeploy, Target: "env/" + request.App + "/" + request.Environment}
	principal := api.Principal{
		Actor:        contract.Actor{Kind: "ci", Subject: request.Name},
		TokenSHA256:  hash(token),
		Scopes:       []string{submitScope, readScope},
		ActionGrants: []api.ActionGrant{grant},
		Name:         request.Name,
		TokenPrefix:  prefixOf(token),
		CreatedAt:    &created,
		ExpiresAt:    &expires,
	}
	// NewAuth applies the server's exact validation before the file is written.
	if _, err := api.NewAuth([]api.Principal{principal}); err != nil {
		return Issued{}, err
	}
	if err := host.WriteAuthFile(path, append(principals, principal), host.DefaultLayout().User, host.DefaultLayout().Group); err != nil {
		return Issued{}, contract.NewFault(contract.Unavailable, "could not write the token store", err.Error())
	}
	return Issued{Token: token, Metadata: metadataOf(principal, request.App, request.Environment)}, nil
}

// List returns managed tokens ordered by creation time, then name.
func List(path string) ([]Metadata, error) {
	principals, err := host.ReadAuthFile(path)
	if err != nil {
		return nil, err
	}
	out := make([]Metadata, 0, len(principals))
	for _, principal := range principals {
		if principal.Name == "" {
			continue
		}
		app, environment := targetOf(principal)
		out = append(out, metadataOf(principal, app, environment))
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].Name < out[j].Name
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

// Revoke removes one managed token by name and returns the removed metadata.
func Revoke(path, name string) (Metadata, error) {
	if err := ValidateName(name); err != nil {
		return Metadata{}, err
	}
	principals, err := host.ReadAuthFile(path)
	if err != nil {
		return Metadata{}, err
	}
	var removed api.Principal
	kept := principals[:0]
	for _, principal := range principals {
		if principal.Name == name {
			removed = principal
			continue
		}
		kept = append(kept, principal)
	}
	if removed.Name == "" {
		return Metadata{}, contract.NewFault(contract.NotFound, "token "+name+" not found", "List tokens with eregion token list.")
	}
	if err := host.WriteAuthFile(path, kept, host.DefaultLayout().User, host.DefaultLayout().Group); err != nil {
		return Metadata{}, contract.NewFault(contract.Unavailable, "could not write the token store", err.Error())
	}
	app, environment := targetOf(removed)
	return metadataOf(removed, app, environment), nil
}

// ValidateName checks the stable token identifier.
func ValidateName(name string) error {
	if !namePattern.MatchString(name) {
		return contract.NewFault(contract.ValidationFailed, "invalid token name", "Use 2–64 lowercase letters, digits, dots, underscores, or hyphens.")
	}
	return nil
}

func validateRequest(request *CreateRequest) error {
	if err := platform.ValidateName(request.App); err != nil {
		return err
	}
	if err := platform.ValidateName(request.Environment); err != nil {
		return err
	}
	if request.Name != "" {
		if err := ValidateName(request.Name); err != nil {
			return err
		}
	}
	if request.TTL == 0 {
		request.TTL = DefaultTTL
	}
	if request.TTL < MinTTL || request.TTL > MaxTTL {
		return contract.NewFault(contract.ValidationFailed, "token lifetime out of range", "Use --expires from 1m to 2160h (90 days).")
	}
	return nil
}

func generate() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", contract.NewFault(contract.Internal, "could not generate a token", "Retry the command.")
	}
	return Prefix + base64.RawURLEncoding.EncodeToString(raw[:]), nil
}

func defaultName(app, environment string) string {
	var raw [3]byte
	_, _ = rand.Read(raw[:])
	base := "deploy-" + app + "-" + environment
	if len(base) > 55 {
		base = base[:55]
	}
	return base + "-" + hex.EncodeToString(raw[:])
}

func hash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func prefixOf(token string) string {
	if len(token) <= 12 {
		return token
	}
	return token[:12]
}

func metadataOf(principal api.Principal, app, environment string) Metadata {
	created := time.Time{}
	if principal.CreatedAt != nil {
		created = principal.CreatedAt.UTC()
	}
	expires := time.Time{}
	if principal.ExpiresAt != nil {
		expires = principal.ExpiresAt.UTC()
	}
	fingerprint := ""
	if len(principal.TokenSHA256) >= 12 {
		fingerprint = principal.TokenSHA256[:12]
	}
	return Metadata{
		Name: principal.Name, ActorKind: principal.Actor.Kind, ActorSubject: principal.Actor.Subject,
		App: app, Environment: environment, Scopes: slices.Clone(principal.Scopes),
		ActionGrants: slices.Clone(principal.ActionGrants), TokenPrefix: principal.TokenPrefix,
		Fingerprint: fingerprint, CreatedAt: created, ExpiresAt: expires,
	}
}

func targetOf(principal api.Principal) (string, string) {
	for _, grant := range principal.ActionGrants {
		if grant.Kind != platform.KindDeploy {
			continue
		}
		parts := strings.Split(grant.Target, "/")
		if len(parts) == 3 && parts[0] == "env" {
			return parts[1], parts[2]
		}
	}
	return "", ""
}

// FormatExpiry is the plain listing form of an expiry time.
func FormatExpiry(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return t.UTC().Format(time.RFC3339)
}
