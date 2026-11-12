package host

import (
	"os"
	"strings"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/controlplane"
)

// nodeConfigPathEnv is the node daemon's existing test-fixture override for
// the node configuration path. It changes only where the same real node.json is
// read; node/state-dir agreement and the public host-key summary are still
// required.
const nodeConfigPathEnv = "EREGION_NODE_CONFIG"

// LocalRegistryIdentity observes the physical main host's public registry
// identity. The bootstrap-owned node.json and the managed node name/state-dir
// settings must agree; the public SSH host key summary supplies the physical
// discriminator. It never reads a private key or trusts a role/name alone.
func LocalRegistryIdentity() (controlplane.RegistryHostIdentity, error) {
	path := DefaultNodeConfigPath()
	if override := strings.TrimSpace(os.Getenv(nodeConfigPathEnv)); override != "" {
		path = override
	}
	return localRegistryIdentityAt(path)
}

// localRegistryIdentityAt is the testable form of LocalRegistryIdentity. The
// production path is fixed; tests use the actual node.json written by the
// preceding host steps.
func localRegistryIdentityAt(configPath string) (controlplane.RegistryHostIdentity, error) {
	return localRegistryIdentityAtSummary(configPath, HostKeySummary)
}

func localRegistryIdentityAtSummary(configPath string, hostKeySummary func() (string, error)) (controlplane.RegistryHostIdentity, error) {
	config, err := LoadNodeFile(configPath)
	if err != nil {
		return controlplane.RegistryHostIdentity{}, contract.NewFault(contract.PreconditionFailed,
			"the bootstrap node configuration is not readable", err.Error())
	}
	nodeName := strings.TrimSpace(os.Getenv("EREGION_NODE_NAME"))
	stateDir := strings.TrimSpace(os.Getenv("EREGION_NODE_STATE_DIR"))
	if nodeName == "" || stateDir == "" {
		return controlplane.RegistryHostIdentity{}, contract.NewFault(contract.PreconditionFailed,
			"the managed node name or state directory is missing",
			"Start the node with its managed EREGION_NODE_NAME and EREGION_NODE_STATE_DIR settings; registry mutation never guesses them.")
	}
	canonicalStateDir, err := controlplane.CanonicalRegistryHostStateDir(stateDir)
	if err != nil {
		return controlplane.RegistryHostIdentity{}, err
	}
	configStateDir, err := controlplane.CanonicalRegistryHostStateDir(config.StateDir)
	if err != nil {
		return controlplane.RegistryHostIdentity{}, err
	}
	if nodeName != config.Node || canonicalStateDir != configStateDir {
		return controlplane.RegistryHostIdentity{}, contract.NewFault(contract.PreconditionFailed,
			"the managed node settings do not agree with the bootstrap node configuration",
			"The managed node name and state directory must exactly match node.json; a different local path or name is not the bound registry host.")
	}
	summary, summaryErr := hostKeySummary()
	if summaryErr != nil {
		return controlplane.RegistryHostIdentity{}, contract.NewFault(contract.PreconditionFailed,
			"the public SSH host key summary is unavailable", summaryErr.Error())
	}
	hostKeyType, fingerprint, err := parsePublicHostKeySummary(summary)
	if err != nil {
		return controlplane.RegistryHostIdentity{}, err
	}
	identity := controlplane.RegistryHostIdentity{
		NodeName: nodeName, HostKeyType: hostKeyType, HostKeyFingerprint: fingerprint,
		StateDir: configStateDir,
	}
	if err := identity.Validate(); err != nil {
		return controlplane.RegistryHostIdentity{}, err
	}
	return identity, nil
}

// BootstrapRegistryIdentity builds the same public descriptor from the
// bootstrap config during first workspace assignment, before service
// environment variables exist.
func BootstrapRegistryIdentity(config Config) (controlplane.RegistryHostIdentity, error) {
	stateDir, err := controlplane.CanonicalRegistryHostStateDir(config.Layout.NodeStateDir)
	if err != nil {
		return controlplane.RegistryHostIdentity{}, err
	}
	hostKeyType, fingerprint, err := publicHostKeySummary()
	if err != nil {
		return controlplane.RegistryHostIdentity{}, err
	}
	identity := controlplane.RegistryHostIdentity{
		NodeName: strings.TrimSpace(config.Node), HostKeyType: hostKeyType, HostKeyFingerprint: fingerprint, StateDir: stateDir,
	}
	if err := identity.Validate(); err != nil {
		return controlplane.RegistryHostIdentity{}, err
	}
	return identity, nil
}

func publicHostKeySummary() (string, string, error) {
	summary, err := HostKeySummary()
	if err != nil {
		return "", "", contract.NewFault(contract.PreconditionFailed,
			"the public SSH host key summary is unavailable", err.Error())
	}
	return parsePublicHostKeySummary(summary)
}

func parsePublicHostKeySummary(summary string) (string, string, error) {
	keyType, fingerprint, ok := strings.Cut(strings.TrimSpace(summary), " ")
	if !ok || strings.TrimSpace(keyType) == "" || strings.TrimSpace(fingerprint) == "" {
		return "", "", contract.NewFault(contract.PreconditionFailed,
			"the public SSH host key summary is invalid", "Record the installed host key type and SHA256 fingerprint.")
	}
	return strings.TrimSpace(keyType), strings.TrimSpace(fingerprint), nil
}
