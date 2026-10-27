package cli

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/host"
	"github.com/mbvlabs/eregion/internal/transport"
	"github.com/spf13/cobra"
)

// secretsSpecs registers the operator handoff re-export. It is separate from
// secretSpecs because it reproduces the server-init record, not a vault value.
func (a *app) secretsSpecs() []spec {
	exportMeta := operationMeta("Re-export the server handoff behind the human factor", "eregion secrets export --secrets-out ./prod-secrets.md")
	exportMeta.WhenToUse = []string{
		"Reproduce the server init handoff record: the operator keys, hostname, control-plane database URL and vault keyring, sealed with a new age passphrase. Requires a code from the authenticator app at a terminal.",
	}
	exportMeta.Mutating = false
	exportMeta.Prompts = true
	exportMeta.InputFlags = []string{"secrets-out"}
	exportMeta.Examples = []string{"eregion secrets export", "eregion secrets export --secrets-out ./prod-secrets.md"}
	return []spec{{
		use:  "secrets",
		meta: readMeta("Export the server handoff behind the human factor", []string{"Reproduce the protected server record after init."}, []string{"eregion secrets export"}),
		run:  func(cmd *cobra.Command, _ []string) (result, error) { return a.help(cmd), nil },
		children: []spec{
			{use: "export", meta: exportMeta, flags: func(cmd *cobra.Command) {
				cmd.Flags().String("secrets-out", "", "Write the full record to this new owner-only file as well as the terminal")
				cmd.Flags().Bool("yes", false, "Refused: the authenticator code cannot be replaced by an acknowledgement")
			}, run: a.secretsExport},
		},
	}}
}

func (a *app) secretsExport(cmd *cobra.Command, _ []string) (result, error) {
	if err := requireHuman(cmd, "eregion secrets export"); err != nil {
		return result{}, err
	}
	store, err := host.LoadContextStore("")
	if err != nil {
		return result{}, err
	}
	current, err := store.Get("")
	if err != nil {
		return result{}, err
	}
	if current.SSH == nil || current.SSH.WireGuard == nil {
		return result{}, contract.NewFault(contract.PreconditionFailed, "context "+current.Name+" has no WireGuard operator keys", "This export needs a server-init context with the operator identity.")
	}
	dir, err := identityDir(current.Name)
	if err != nil {
		return result{}, err
	}
	sshKey, err := readIdentityFile(filepath.Join(dir, "ssh_ed25519"))
	if err != nil {
		return result{}, contract.NewFault(contract.PreconditionFailed, "the operator SSH key is missing", "The context file references keys that are no longer on this machine; restore them from the stored handoff.")
	}
	wgKey, err := transport.ReadWireGuardKey(filepath.Join(dir, "wireguard.key"))
	if err != nil {
		return result{}, contract.NewFault(contract.PreconditionFailed, "the operator WireGuard key is missing", "The context file references keys that are no longer on this machine; restore them from the stored handoff.")
	}
	sudoPassword, err := readIdentityFile(filepath.Join(dir, "sudo-password"))
	if err != nil {
		return result{}, contract.NewFault(contract.PreconditionFailed, "the operator sudo password is missing", "The context file references credentials that are no longer on this machine; restore them from the stored handoff.")
	}
	code, err := readProtectCode(cmd, "Code from the authenticator app")
	if err != nil {
		return result{}, err
	}
	client, err := a.client(cmd)
	if err != nil {
		return result{}, err
	}
	exported, err := client.HandoffExport(cmd.Context(), code)
	if err != nil {
		return result{}, err
	}
	publicHost, _, _ := net.SplitHostPort(current.SSH.WireGuard.Endpoint)
	input := handoffRecordInput{
		NodeName: exported.Node, Context: current.Name, Hostname: exported.Hostname,
		PublicAddress: publicHost, Platform: exported.Platform, SSHHostKey: exported.SSHHostKey,
		WireGuardEndpoint: current.SSH.WireGuard.Endpoint, WireGuardNodeAddress: current.SSH.WireGuard.NodeAddress,
		WireGuardNodePublicKey: current.SSH.WireGuard.NodePublicKey,
		AdminUser:              current.SSH.User, SudoPassword: strings.TrimSpace(sudoPassword),
		OperatorWireGuardAddress: current.SSH.WireGuard.Address, OperatorWireGuardKey: wgKey.Encode(),
		OperatorSSHKey: strings.TrimSpace(sshKey),
		DatabaseURL:    exported.DatabaseURL, VaultKeyring: exported.VaultKeyring,
	}
	record, err := newSecretsHandoff(input.Context, handoffGroups(input))
	if err != nil {
		return result{}, err
	}
	// Register actual values, not labels, so ordinary output stays readable
	// while every credential in the record is redacted.
	a.secrets.Add(input.SudoPassword, input.OperatorWireGuardKey, input.OperatorSSHKey, input.DatabaseURL, input.VaultKeyring, record.passphrase)
	for _, group := range record.groups {
		for _, field := range group.fields {
			a.secrets.Add(field.value)
		}
	}
	outPath := flagString(cmd, "secrets-out")
	if outPath != "" {
		if err := record.writeRecord(outPath, false); err != nil {
			return result{}, err
		}
	}
	if err := record.show(os.Stderr); err != nil {
		return result{}, err
	}
	data := map[string]any{
		"node": exported.Node, "hostname": exported.Hostname, "context": current.Name, "exported": true,
	}
	plain := "Re-exported the server handoff to the terminal"
	if record.path != "" {
		data["secrets_out"] = record.path
		plain += " and wrote " + record.path
	}
	return result{data: data, plain: plain + "; eregion does not store it.\n"}, nil
}

func readIdentityFile(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s is not an owner-only regular file", path)
	}
	raw, err := os.ReadFile(path) //nolint:gosec // G304: eregion's own identity file.
	if err != nil {
		return "", err
	}
	return string(raw), nil
}
