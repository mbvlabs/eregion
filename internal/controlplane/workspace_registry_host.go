package controlplane

import (
	"context"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

var registryHostKeyTypePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9@._+-]{0,127}$`)
var registryHostFingerprintPattern = regexp.MustCompile(`^SHA256:[A-Za-z0-9+/=]{20,}$`)

// RegistryHostIdentity is the immutable public identity of the physical main
// host that owns the private registry and shared host lock. It is installation
// metadata, not a fleet or node-role claim. No private key material is stored or
// compared.
type RegistryHostIdentity struct {
	NodeName           string `json:"node_name"`
	HostKeyType        string `json:"host_key_type"`
	HostKeyFingerprint string `json:"host_key_fingerprint"`
	StateDir           string `json:"state_dir"`
}

// Validate checks the public, all-or-none binding shape.
func (i RegistryHostIdentity) Validate() error {
	if err := platform.ValidateName(strings.TrimSpace(i.NodeName)); err != nil {
		return contract.NewFault(contract.ValidationFailed, "invalid registry host node name", "Use the configured main node name.")
	}
	if !registryHostKeyTypePattern.MatchString(strings.TrimSpace(i.HostKeyType)) {
		return contract.NewFault(contract.ValidationFailed, "invalid registry host key type", "Use the public SSH host key type summary recorded by bootstrap.")
	}
	if !registryHostFingerprintPattern.MatchString(strings.TrimSpace(i.HostKeyFingerprint)) {
		return contract.NewFault(contract.ValidationFailed, "invalid registry host fingerprint", "Use the SHA256 public SSH host-key fingerprint recorded by bootstrap.")
	}
	stateDir, err := CanonicalRegistryHostStateDir(i.StateDir)
	if err != nil {
		return err
	}
	if stateDir != i.StateDir {
		return contract.NewFault(contract.ValidationFailed, "registry host state directory is not canonical", "Use an absolute, clean node state directory recorded by bootstrap.")
	}
	return nil
}

// CanonicalRegistryHostStateDir returns the absolute clean state directory, or
// fails closed. It never falls back to the default node state directory.
func CanonicalRegistryHostStateDir(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || strings.ContainsRune(value, 0) || !filepath.IsAbs(value) {
		return "", contract.NewFault(contract.ValidationFailed, "invalid registry host state directory", "Use the absolute node state directory recorded by bootstrap.")
	}
	return filepath.Clean(value), nil
}

func (i RegistryHostIdentity) Equal(other RegistryHostIdentity) bool {
	return i.NodeName == other.NodeName &&
		i.HostKeyType == other.HostKeyType &&
		i.HostKeyFingerprint == other.HostKeyFingerprint &&
		i.StateDir == other.StateDir
}

// VerifyWorkspaceRegistryHost reads the singleton binding and requires the
// observed local public identity to match it exactly. Missing bindings and
// mismatches fail closed; there is no name-only or role/default fallback.
func VerifyWorkspaceRegistryHost(ctx context.Context, pool workspaceQuerier, observed RegistryHostIdentity) (RegistryHostIdentity, error) {
	if err := observed.Validate(); err != nil {
		return RegistryHostIdentity{}, err
	}
	identity, err := readWorkspaceSchema(ctx, pool)
	if err != nil {
		return RegistryHostIdentity{}, err
	}
	if !identity.Configured {
		return RegistryHostIdentity{}, contract.NewFault(contract.PreconditionFailed,
			"the workspace is not configured",
			"Initialize the workspace on the physical main host before mutating registry state.")
	}
	if identity.RegistryHost == nil {
		return RegistryHostIdentity{}, contract.NewFault(contract.PreconditionFailed,
			"the workspace has no verified registry host binding",
			"This workspace predates the registry-host binding; it remains unverified and cannot upload or finalize a delete. Use a fresh installation.")
	}
	if !identity.RegistryHost.Equal(observed) {
		fault := contract.NewFault(contract.Conflict,
			"this physical host is not the workspace's registry host",
			"Registry upload and final metadata deletion must run on the exact bound main host; names alone are not sufficient.")
		fault.Details = map[string]any{
			"bound_node":  identity.RegistryHost.NodeName,
			"actual_node": observed.NodeName,
		}
		return RegistryHostIdentity{}, fault
	}
	return *identity.RegistryHost, nil
}
