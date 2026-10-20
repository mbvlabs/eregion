package vault

import (
	"context"
	"math"
	"strconv"
	"strings"

	"github.com/mbvlabs/eregion/internal/contract"
)

// Reference is a secret-free, immutable revision identity. References deliberately
// carry the namespace so accidental cross-environment injection is rejected.
// The only accepted spelling is vault:<app>/<environment>/<NAME>@<revision>.
type Reference struct {
	Namespace Namespace
	Revision  uint64
}

func ParseReference(text string) (Reference, error) {
	bad := func() (Reference, error) {
		return Reference{}, contract.NewFault(contract.ValidationFailed, "invalid pinned secret reference", "Use vault:<app>/<environment>/<NAME>@<positive revision>; never supply a value.")
	}
	if !strings.HasPrefix(text, "vault:") {
		return bad()
	}
	path, revisionText, ok := strings.Cut(strings.TrimPrefix(text, "vault:"), "@")
	parts := strings.Split(path, "/")
	if !ok || len(parts) != 3 {
		return bad()
	}
	ns, err := NewNamespace(parts[0], parts[1], parts[2])
	if err != nil {
		return bad()
	}
	revision, err := strconv.ParseUint(revisionText, 10, 63)
	if err != nil || revision == 0 || revision > math.MaxInt64 || strconv.FormatUint(revision, 10) != revisionText {
		return bad()
	}
	return Reference{Namespace: ns, Revision: revision}, nil
}

// EnvironmentResolver implements the core's legacy interface only to fail
// closed. Production must call ResolveEnvironment with authoritative placement.
type EnvironmentResolver struct{ service *Service }

func NewEnvironmentResolver(service *Service) *EnvironmentResolver {
	return &EnvironmentResolver{service: service}
}

func (*EnvironmentResolver) Resolve(_ context.Context, refs map[string]string) (map[string]string, error) {
	if len(refs) == 0 {
		return map[string]string{}, nil
	}
	return nil, contract.NewFault(contract.PreconditionFailed, "secret resolution requires an authoritative environment", "Call ResolveEnvironment with the deployment's app and environment; namespace-free resolution is refused.")
}

func (r *EnvironmentResolver) ResolveEnvironment(ctx context.Context, app, environment string, refs map[string]string) (map[string]string, error) {
	if r == nil || r.service == nil {
		return nil, contract.NewFault(contract.Unavailable, "secret storage is not configured", "Set EREGION_VAULT_KEY_FILE on every control-plane worker.")
	}
	if len(refs) > 64 {
		return nil, contract.NewFault(contract.ValidationFailed, "too many secret references", "Pin at most 64 environment secrets.")
	}
	pins := RevisionMap{}
	names := map[string]string{}
	for key, text := range refs {
		if err := ValidateName(key); err != nil {
			return nil, err
		}
		ref, err := ParseReference(text)
		if err != nil {
			return nil, err
		}
		if ref.Namespace.App != app || ref.Namespace.Environment != environment {
			return nil, contract.NewFault(contract.Forbidden, "secret reference belongs to another environment", "Pin a secret in the destination environment; cross-environment resolution is forbidden.")
		}
		if revision, ok := pins[ref.Namespace.Name]; ok && revision != ref.Revision {
			return nil, contract.NewFault(contract.ValidationFailed, "one secret name is pinned to different revisions", "Use a single immutable revision per secret.")
		}
		pins[ref.Namespace.Name] = ref.Revision
		names[key] = ref.Namespace.Name
	}
	resolved, err := r.service.Resolve(ctx, contract.Actor{Kind: "system", Subject: "environment-secret-injection"}, app, environment, pins)
	if err != nil {
		return nil, err
	}
	values := make(map[string]string, len(names))
	for key, name := range names {
		value := resolved.Values[name]
		if strings.ContainsRune(value, '\x00') {
			return nil, contract.NewFault(contract.PreconditionFailed, "secret cannot be injected into a process environment", "Rotate the secret to a value without NUL bytes.")
		}
		values[key] = value
	}
	return values, nil
}
