// Package vault stores revisioned secrets encrypted at rest in PostgreSQL and
// resolves exact revisions for transient node injection.
//
// Plaintext exists only in memory: it is never written to operation events,
// River arguments, node payloads, journals, logs, or error messages. Ciphertext
// is AES-256-GCM under a master key from a protected file outside the database.
package vault

import (
	"regexp"
	"strings"

	"github.com/mbvlabs/eregion/internal/contract"
)

const (
	// MaxValueBytes bounds one secret value. The environment-injection total is
	// bounded separately by MaxResolveBytes.
	MaxValueBytes = 32 * 1024
	// MaxResolveBytes bounds all plaintext values returned by one Resolve call.
	MaxResolveBytes = 256 * 1024
	// MaxSecretsPerEnvironment bounds one environment's injectable secret set.
	MaxSecretsPerEnvironment = 128
	// MaxListLimit bounds one metadata page.
	MaxListLimit = 200
)

var (
	appPattern         = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,38}[a-z0-9])?$`)
	environmentPattern = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,38}[a-z0-9])?$`)
	namePattern        = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,127}$`)
)

// Namespace is the canonical identity of one secret. Environment and name are
// independently validated; they are never combined into an unescaped string.
type Namespace struct {
	App         string `json:"app"`
	Environment string `json:"environment"`
	Name        string `json:"name"`
}

// NewNamespace validates and returns a canonical namespace.
func NewNamespace(app, environment, name string) (Namespace, error) {
	ns := Namespace{App: app, Environment: environment, Name: name}
	if err := ns.Validate(); err != nil {
		return Namespace{}, err
	}
	return ns, nil
}

// Validate checks each namespace component with an independent strict pattern.
func (n Namespace) Validate() error {
	if err := ValidateApp(n.App); err != nil {
		return err
	}
	if err := ValidateEnvironment(n.Environment); err != nil {
		return err
	}
	return ValidateName(n.Name)
}

func ValidateApp(app string) error {
	if !appPattern.MatchString(app) {
		return contract.NewFault(contract.ValidationFailed, "invalid secret app name", "Use 1–40 lowercase letters, digits, and inner hyphens; start with a letter and end with a letter or digit.")
	}
	return nil
}

func ValidateEnvironment(environment string) error {
	if !environmentPattern.MatchString(environment) {
		return contract.NewFault(contract.ValidationFailed, "invalid secret environment name", "Use 1–40 lowercase letters, digits, and inner hyphens; start with a letter and end with a letter or digit.")
	}
	return nil
}

func ValidateName(name string) error {
	if !namePattern.MatchString(name) {
		return contract.NewFault(contract.ValidationFailed, "invalid secret name", "Use 1–128 uppercase ASCII letters, digits, and underscores; start with a letter. The name becomes one environment variable during injection.")
	}
	return nil
}

// ValidateValue checks one plaintext value without inspecting its contents in an
// error or log. Values must fit environment injection and cannot carry NUL.
func ValidateValue(value []byte) error {
	if len(value) == 0 {
		return contract.NewFault(contract.ValidationFailed, "secret value is empty", "Provide a nonempty value; delete or archive a secret instead of storing an empty one.")
	}
	if len(value) > MaxValueBytes {
		return contract.NewFault(contract.ValidationFailed, "secret value is too large", "Limit one secret value to 32768 bytes.")
	}
	for _, b := range value {
		if b == 0 {
			return contract.NewFault(contract.ValidationFailed, "secret value contains NUL", "Environment injection cannot represent NUL; encode binary data first.")
		}
	}
	return nil
}

// Key returns a stable, non-secret label for diagnostics.
func (n Namespace) Key() string {
	return n.App + "/" + n.Environment + "/" + n.Name
}

func (n Namespace) String() string { return n.Key() }

func validateActor(actor contract.Actor) error {
	if err := actor.Validate(); err != nil {
		return err
	}
	if strings.TrimSpace(actor.Subject) != actor.Subject {
		return contract.NewFault(contract.ValidationFailed, "invalid actor subject", "Resolve a trimmed identity of at most 256 bytes from authentication.")
	}
	return nil
}
