package cli

import (
	"bytes"
	"context"
	"encoding/json"

	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/bundle"
	"github.com/mbvlabs/eregion/internal/cli/ui"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/controlplane"
	"github.com/mbvlabs/eregion/internal/host"
	"github.com/mbvlabs/eregion/internal/telemetry"
	"github.com/mbvlabs/eregion/internal/transport"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
)

// loginSpecs is `eregion login`, as vaire login: decrypt the transfer bundle
// from server init's secrets handoff with its passphrase and turn it into a
// working workspace access on this machine.
func (a *app) loginSpecs() []spec {
	meta := readMeta("Log in to a main node with its transfer bundle",
		[]string{"Paste the transfer bundle from server init's secrets handoff (or the whole copied handoff), then its passphrase."},
		[]string{"eregion login", "eregion login --file main-secrets.md --passphrase-file pass.txt --json"})
	meta.Mutating, meta.Prompts, meta.Remote = true, true, true
	meta.DryRunReason = "Login only writes the private workspace store and identity files."
	meta.InputFlags = []string{"file", "passphrase-file", "replace"}
	meta.ErrorCodes = append(meta.ErrorCodes, contract.ValidationFailed, contract.PreconditionFailed, contract.Conflict)
	return []spec{{
		use:  "login",
		meta: meta,
		flags: func(cmd *cobra.Command) {
			cmd.Flags().String("file", "", "File with the transfer bundle (or the whole handoff); otherwise it is read from standard input")
			cmd.Flags().String("passphrase-file", "", "File whose first line is the bundle passphrase; asked for in a terminal")
			cmd.Flags().String("replace", "", "Replace the saved local access for this verified workspace; must match its canonical name")
		},
		run: a.login,
	}}
}

// loginSeams replace the network probe and readiness check in tests. The zero
// value uses the real WireGuard probe and node RPC.
type loginSeams struct {
	probe  func(context.Context, *host.WorkspaceSSH, string) (ssh.PublicKey, error)
	verify func(context.Context, *host.WorkspaceSSH) (rpcNodeInfoResult, error)
	ready  func(context.Context, *host.WorkspaceSSH) error
}

// handoffRecord is the parsed secrets record: section title → label → value.
type handoffRecord map[string]map[string]string

func parseHandoffRecord(text string) handoffRecord {
	record := handoffRecord{}
	section := ""
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	for i := 0; i < len(lines); i++ {
		line := strings.TrimRight(lines[i], " \t")
		if title, ok := strings.CutPrefix(line, "## "); ok {
			section = strings.TrimSpace(title)
			record[section] = map[string]string{}
			continue
		}
		item, ok := strings.CutPrefix(line, "- ")
		if section == "" || !ok {
			continue
		}
		label, value, ok := strings.Cut(item, ":")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		if value == "" {
			// A multi-line value is the fenced block that follows.
			j := i + 1
			for j < len(lines) && strings.TrimSpace(lines[j]) == "" {
				j++
			}
			if j < len(lines) && strings.TrimSpace(lines[j]) == "```" {
				var block []string
				for j++; j < len(lines) && strings.TrimSpace(lines[j]) != "```"; j++ {
					block = append(block, strings.TrimRight(lines[j], " \t"))
				}
				value, i = strings.Join(block, "\n"), j
			}
		}
		record[section][strings.TrimSpace(label)] = value
	}
	return record
}

// field finds label in the first section whose title starts with prefix.
func (h handoffRecord) field(prefix, label string) string {
	for title, fields := range h {
		if strings.HasPrefix(title, prefix) {
			return fields[label]
		}
	}
	return ""
}

func (a *app) login(cmd *cobra.Command, _ []string) (result, error) {
	path, _ := cmd.Flags().GetString("file")
	passFile, _ := cmd.Flags().GetString("passphrase-file")
	interactive := humanAtTerminal(cmd)
	var armored []byte
	var err error
	if path != "" {
		var raw []byte
		if raw, err = os.ReadFile(expandHome(path)); err == nil { //nolint:gosec // G304: the operator's bundle.
			armored, err = bundle.Read(bytes.NewReader(raw))
		}
	} else {
		if interactive {
			fmt.Fprintln(cmd.ErrOrStderr(), "\n"+ui.Header("eregion login", 72))
			fmt.Fprintln(cmd.ErrOrStderr(), "Paste the transfer bundle (from "+bundle.Header+" to "+bundle.Footer+"), or the whole copied handoff:")
		}
		armored, err = bundle.Read(cmd.InOrStdin())
	}
	if err != nil {
		return result{}, contract.NewFault(contract.InputRequired, "could not read the transfer bundle", err.Error())
	}
	var pass string
	switch {
	case passFile != "":
		raw, err := os.ReadFile(expandHome(passFile)) //nolint:gosec // G304: the operator's passphrase file.
		if err != nil {
			return result{}, contract.NewFault(contract.InputRequired, "could not read the passphrase file", err.Error())
		}
		pass = strings.TrimSpace(strings.SplitN(string(raw), "\n", 2)[0])
	case interactive:
		if pass, err = ui.Secret("Bundle passphrase"); err != nil {
			return result{}, err
		}
	default:
		return result{}, contract.NewFault(contract.InputRequired, "--passphrase-file is required without a terminal", "Pass the bundle passphrase in a file.")
	}
	a.secrets.Add(pass)
	plain, err := bundle.Decrypt(armored, pass)
	if err != nil {
		return result{}, contract.NewFault(contract.ValidationFailed, err.Error(), "Check the passphrase and the bundle, then retry.")
	}
	rec := parseHandoffRecord(string(plain))
	get := func(section, label string) string { return rec.field(section, label) }
	historicalName := strings.TrimSpace(get("Main node", "Context"))
	nodeName := get("Main node", "Node name")
	sshKey := get("Operator", "SSH private key")
	wgKey := get("Operator", "WireGuard private key")
	wireGuardAddress := get("Main node", "WireGuard address")
	adminUser := get("Operator", "Admin user")
	workspaceName := strings.TrimSpace(get("Main node", "Workspace"))
	workspaceID := strings.TrimSpace(get("Main node", "Workspace ID"))
	workspaceDeclared := workspaceName != "" || workspaceID != ""
	required := map[string]string{
		"SSH private key": sshKey, "WireGuard private key": wgKey,
		"WireGuard address":    wireGuardAddress,
		"admin user":           adminUser,
		"WireGuard endpoint":   get("Main node", "WireGuard endpoint"),
		"WireGuard public key": get("Main node", "WireGuard public key"),
		"SSH host key":         get("Main node", "SSH host key"),
	}
	for label, value := range required {
		if value == "" {
			return result{}, contract.NewFault(contract.ValidationFailed, "the transfer bundle has no "+label, "Use the bundle from eregion server init's secrets handoff.")
		}
	}
	if workspaceDeclared {
		if workspaceName == "" || workspaceID == "" {
			return result{}, contract.NewFault(contract.ValidationFailed, "the transfer bundle has an incomplete workspace identity", "Use a current protected export with both the workspace name and ID.")
		}
		if err := controlplane.ValidateWorkspaceName(workspaceName); err != nil {
			return result{}, err
		}
		if err := controlplane.ValidateWorkspaceID(workspaceID); err != nil {
			return result{}, err
		}
	}
	for _, v := range []string{sshKey, wgKey, get("Operator", "Sudo password")} {
		a.secrets.Add(v)
	}

	if err := rejectWorkspaceSelection(cmd, "login"); err != nil {
		return result{}, err
	}
	store, err := loadWorkspaceStore(cmd)
	if err != nil {
		return result{}, err
	}
	replace, _ := cmd.Flags().GetString("replace")
	replace = strings.TrimSpace(replace)
	privateName := workspaceName
	if workspaceDeclared {
		if err := validateLoginWorkspaceTarget(store, workspaceName, workspaceID, replace); err != nil {
			return result{}, err
		}
	} else {
		if replace != "" {
			return result{}, contract.NewFault(contract.Usage, "--replace names a verified workspace; this bundle has no workspace identity", "Refresh the protected export from the node and use its Workspace/Workspace ID fields.")
		}
		if len(store.Groups()) > 0 {
			return result{}, contract.NewFault(contract.Conflict,
				"this machine already has a verified workspace; this old bundle carries no portable identity",
				"Refresh the protected export from the node and log in with its Workspace and Workspace ID fields.")
		}
		if historicalName == "" {
			return result{}, contract.NewFault(contract.ValidationFailed, "the transfer bundle has neither a workspace identity nor a private access name", "Use a current protected export from eregion secrets export.")
		}
		if err := host.ValidateAccessName(historicalName); err != nil {
			return result{}, err
		}
		privateName = historicalName
	}
	autoUpgrade := boolFlag(cmd, "auto-upgrade-nodes")
	for _, group := range store.Groups() {
		if workspaceDeclared && group.ID == workspaceID {
			for _, access := range group.Access {
				autoUpgrade = autoUpgrade || access.AutoUpgradeNodes
			}
		}
	}
	credentialDirName, err := store.CredentialDirName(workspaceID)
	if err != nil {
		return result{}, contract.NewFault(contract.PreconditionFailed, "could not allocate a private workspace credential directory", err.Error())
	}
	if !workspaceDeclared {
		credentialDirName, err = store.CredentialDirName("legacy-" + historicalName)
		if err != nil {
			return result{}, contract.NewFault(contract.PreconditionFailed, "could not allocate a private workspace credential directory", err.Error())
		}
	}
	dir := filepath.Join(filepath.Dir(store.Path), credentialDirName)
	if err := os.MkdirAll(filepath.Dir(dir), 0o700); err != nil {
		return result{}, err
	}
	// Build and prove the identity in a sibling directory first; the existing
	// workspace access is swapped only after probe and ready both succeed.
	tempDir, err := os.MkdirTemp(filepath.Dir(dir), ".eregion-login-workspace-")
	if err != nil {
		return result{}, err
	}
	defer func() { _ = os.RemoveAll(tempDir) }()
	sshPath, wgPath := filepath.Join(tempDir, "ssh_ed25519"), filepath.Join(tempDir, "wireguard.key")
	for path, content := range map[string]string{sshPath: sshKey + "\n", wgPath: wgKey + "\n", filepath.Join(tempDir, "sudo-password"): get("Operator", "Sudo password") + "\n"} {
		if err := writePrivate(path, []byte(content)); err != nil {
			return result{}, err
		}
	}
	if password, err := restoreMetricsCredential(tempDir, rec); err != nil {
		return result{}, err
	} else if password != "" {
		a.secrets.Add(password)
	}
	if _, err := transport.ReadWireGuardKey(wgPath); err != nil {
		return result{}, contract.NewFault(contract.ValidationFailed, "the record's WireGuard key is not usable", err.Error())
	}
	tempKnownHosts := filepath.Join(tempDir, "known_hosts")
	route := &host.WorkspaceSSH{
		Address: net.JoinHostPort(wireGuardAddress, "22"), User: adminUser,
		Identity: sshPath, KnownHosts: tempKnownHosts,
		WireGuard: &host.WorkspaceWireGuard{
			PrivateKeyFile: wgPath, Address: get("Operator", "WireGuard address"),
			NodeAddress: wireGuardAddress, NodePublicKey: get("Main node", "WireGuard public key"),
			Endpoint: get("Main node", "WireGuard endpoint"),
		},
	}
	// The import is fresh, so always prove the key through the tunnel against
	// the record before writing anything the workspace access will trust.
	want := get("Main node", "SSH host key")
	keyType, _, _ := strings.Cut(want, " ")
	probe := a.loginSeams.probe
	if probe == nil {
		probe = probeOverWireGuard
	}
	key, err := probe(cmd.Context(), route, keyType)
	if err != nil {
		return result{}, contract.NewFault(contract.Unavailable, "the node does not answer over WireGuard", err.Error())
	}
	if got := key.Type() + " " + ssh.FingerprintSHA256(key); got != want {
		return result{}, contract.NewFault(contract.PreconditionFailed, "the node's host key "+got+" does not match the record", "Do not continue until the server is checked.")
	}
	if err := pinHostKey(tempKnownHosts, route.Address, key); err != nil {
		return result{}, err
	}
	stderr := cmd.ErrOrStderr()
	// A portable-workspace bundle is proved against the node's authoritative
	// identity before any local file is published. An old bundle has no
	// workspace fields and remains an explicitly unconfigured access artifact.
	if workspaceDeclared {
		verify := a.loginSeams.verify
		if verify == nil {
			verify = func(ctx context.Context, route *host.WorkspaceSSH) (rpcNodeInfoResult, error) {
				return infoRoute(ctx, route, autoUpgrade, stderr)
			}
		}
		info, err := verify(cmd.Context(), route)
		if err != nil {
			return result{}, err
		}
		if info.WorkspaceError != "" {
			return result{}, contract.NewFault(contract.Unavailable,
				"the node workspace identity could not be verified: "+info.WorkspaceError,
				"Check the control database and its migrations on the node, then retry login.")
		}
		if !info.Workspace.Configured {
			return result{}, contract.NewFault(contract.PreconditionFailed,
				"the node's workspace identity is not configured",
				"Run eregion apply --assign-workspace NAME on the node, then refresh the protected export with eregion secrets export.")
		}
		if nodeName != "" && info.Node != "" && info.Node != nodeName {
			fault := contract.NewFault(contract.Conflict,
				"the bundle node does not match the node identity",
				"Export again from the correct node; do not hand-edit the bundle.")
			fault.Details = map[string]any{"bundle_node": nodeName, "node": info.Node}
			return result{}, fault
		}
		if info.Workspace.Name != workspaceName || info.Workspace.ID != workspaceID {
			fault := contract.NewFault(contract.Conflict,
				"the bundle workspace identity does not match the node",
				"The workspace name and ID are immutable. Export again from the correct node; do not edit the bundle.")
			fault.Details = map[string]any{
				"bundle_workspace": workspaceName, "bundle_workspace_id": workspaceID,
				"node_workspace": info.Workspace.Name, "node_workspace_id": info.Workspace.ID,
			}
			return result{}, fault
		}
	}
	ready := a.loginSeams.ready
	if ready == nil {
		ready = func(ctx context.Context, route *host.WorkspaceSSH) error {
			return readyRoute(ctx, route, autoUpgrade, stderr)
		}
	}
	if err := ready(cmd.Context(), route); err != nil {
		return result{}, err
	}

	if err := os.Rename(tempDir, dir); err != nil {
		return result{}, err
	}
	savedRoute := *route
	savedRoute.Identity = filepath.Join(dir, "ssh_ed25519")
	savedRoute.KnownHosts = filepath.Join(dir, "known_hosts")
	if route.WireGuard != nil {
		wg := *route.WireGuard
		wg.PrivateKeyFile = filepath.Join(dir, "wireguard.key")
		savedRoute.WireGuard = &wg
	}
	saved := host.WorkspaceAccess{
		Name: privateName, SSH: &savedRoute, CredentialDir: credentialDirName,
		AutoUpgradeNodes: autoUpgrade,
	}
	if workspaceDeclared {
		_, err = store.PublishWorkspace(workspaceID, workspaceName, saved, replace != "")
	} else {
		_, err = store.PublishUnconfiguredAccess(saved)
	}
	if err != nil {
		// The published directory is new and never replaced an old credential
		// directory. Removing it cannot touch existing workspace files.
		_ = os.RemoveAll(dir)
		return result{}, err
	}
	routeText := route.User + "@" + route.Address + " over WireGuard"
	data := loginSuccessData(nodeName, routeText, workspaceName, workspaceID, workspaceDeclared)
	title, rows, message := loginSuccessPresentation(nodeName, routeText, workspaceName, workspaceID, workspaceDeclared)
	if interactive {
		fmt.Fprintln(cmd.ErrOrStderr(), ui.Card(title, rows, nil))
		return result{data: data, streamed: true}, nil
	}
	return result{data: data, plain: message}, nil
}

func validateLoginWorkspaceTarget(store *host.WorkspaceStore, workspaceName, workspaceID, replace string) error {
	foundID := false
	for _, access := range store.Access {
		if access.Workspace == nil {
			continue
		}
		if access.Workspace.ID == workspaceID {
			foundID = true
			if access.Workspace.Name != workspaceName {
				fault := contract.NewFault(contract.Conflict,
					"workspace "+workspaceID+" is saved with another name",
					"The workspace name and ID are immutable. Refresh the correct protected export; same ID with another name is refused even with --replace.")
				fault.Details = map[string]any{"workspace_id": workspaceID, "saved_workspace": access.Workspace.Name, "bundle_workspace": workspaceName}
				return fault
			}
		}
		if access.Workspace.ID != workspaceID && access.Workspace.Name == workspaceName {
			fault := contract.NewFault(contract.Conflict,
				"workspace name "+workspaceName+" is already used by workspace "+access.Workspace.ID,
				"A workspace name identifies one immutable ID. Import the correct export, or remove the conflicting local access explicitly.")
			fault.Details = map[string]any{"workspace": workspaceName, "workspace_id": access.Workspace.ID, "bundle_workspace_id": workspaceID}
			return fault
		}
	}
	if !foundID {
		if replace != "" && replace != workspaceName {
			return contract.NewFault(contract.Conflict,
				"--replace must name the verified workspace ("+workspaceName+")",
				"Pass --replace "+workspaceName+", or omit --replace.")
		}
		return nil
	}
	if replace == "" {
		fault := contract.NewFault(contract.Conflict,
			"workspace "+workspaceName+" is already saved on this machine",
			"Pass --replace "+workspaceName+" to replace its local access records.")
		fault.Details = map[string]any{"workspace": workspaceName, "workspace_id": workspaceID}
		return fault
	}
	if replace != workspaceName {
		fault := contract.NewFault(contract.Conflict,
			"--replace must name the verified workspace ("+workspaceName+")",
			"Pass --replace "+workspaceName+".")
		fault.Details = map[string]any{"workspace": workspaceName}
		return fault
	}
	return nil
}

// loginSuccessData keeps the public login payload independent of the human
// card. The local private access key is not a public identity.
func loginSuccessData(nodeName, route string, workspaceName, workspaceID string, workspaceDeclared bool) map[string]any {
	data := map[string]any{
		"route": route, "workspace_configured": workspaceDeclared,
	}
	if nodeName != "" {
		data["node"] = nodeName
	}
	if workspaceDeclared {
		data["workspace"] = map[string]string{"name": workspaceName, "id": workspaceID}
	}
	return data
}

// loginSuccessPresentation leads with the verified portable workspace, not
// the local private access key. An old unconfigured bundle says so explicitly and
// never presents the alias as a workspace.
func loginSuccessPresentation(nodeName, route, workspaceName, workspaceID string, workspaceDeclared bool) (title string, rows [][2]string, message string) {
	rows = make([][2]string, 0, 4)
	if workspaceDeclared {
		title = "Logged in to workspace"
		rows = append(rows,
			[2]string{"workspace", workspaceName + " (current)"},
			[2]string{"workspace id", workspaceID},
		)
		message = "logged in to workspace " + workspaceName + " (" + workspaceID + ")"
	} else {
		title = "Logged in"
		rows = append(rows, [2]string{"workspace", "not configured (older bundle)"})
		message = "logged in; workspace identity not configured; refresh the protected export from a current node"
	}
	if nodeName != "" {
		rows = append(rows, [2]string{"node", nodeName})
	}
	rows = append(rows, [2]string{"route", route})
	return title, rows, message
}

// restoreMetricsCredential writes the handoff's Node metrics credential into
// the new identity directory. It returns the password for redaction, or empty
// when the record predates the authenticated endpoint.
func restoreMetricsCredential(dir string, record handoffRecord) (string, error) {
	username := strings.TrimSpace(record.field("Node metrics", "Username"))
	password := strings.TrimSpace(record.field("Node metrics", "Password"))
	if username == "" && password == "" {
		return "", nil
	}
	credential := telemetry.Credential{Username: username, Password: password}
	if err := credential.Validate(); err != nil {
		return "", contract.NewFault(contract.ValidationFailed, "the record's telemetry credential is not usable", err.Error())
	}
	raw, err := json.MarshalIndent(credential, "", "  ")
	if err != nil {
		return "", err
	}
	if err := writePrivate(filepath.Join(dir, metricsCredentialFile), append(raw, '\n')); err != nil {
		return "", err
	}
	return password, nil
}

// readyRoute checks a not-yet-saved route without touching the workspace store.
func readyRoute(ctx context.Context, route *host.WorkspaceSSH, autoUpgrade bool, stderr io.Writer) error {
	client, err := routeNodeClient(route, autoUpgrade, stderr)
	if err != nil {
		return err
	}
	return client.Ready(ctx)
}

// infoRoute reads the node's authoritative identity over a not-yet-saved route.
func infoRoute(ctx context.Context, route *host.WorkspaceSSH, autoUpgrade bool, stderr io.Writer) (rpcNodeInfoResult, error) {
	client, err := routeNodeClient(route, autoUpgrade, stderr)
	if err != nil {
		return rpcNodeInfoResult{}, err
	}
	return client.NodeInfo(ctx)
}

func routeNodeClient(route *host.WorkspaceSSH, autoUpgrade bool, stderr io.Writer) (*nodeClient, error) {
	remote, err := routeRemote(route, false)
	if err != nil {
		return nil, contract.NewFault(contract.ValidationFailed, "the workspace SSH route is not usable", err.Error())
	}
	return &nodeClient{remote: remote, route: route, operator: route.User, autoUpgrade: autoUpgrade, stderr: stderr}, nil
}

// probeOverWireGuard reads the node's SSH host key of keyType through the
// tunnel; the node offers several types and the record names one.
func probeOverWireGuard(ctx context.Context, route *host.WorkspaceSSH, keyType string) (ssh.PublicKey, error) {
	dial, err := wireGuardDial(route.WireGuard)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, err := dial(ctx, "tcp", route.Address)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	var key ssh.PublicKey
	_, _, _, _ = ssh.NewClientConn(conn, route.Address, &ssh.ClientConfig{
		User:              "probe",
		HostKeyAlgorithms: hostKeyAlgorithms(keyType),
		HostKeyCallback: func(_ string, _ net.Addr, k ssh.PublicKey) error {
			key = k
			return fmt.Errorf("probed")
		},
	})
	if key == nil {
		return nil, fmt.Errorf("no host key from %s", route.Address)
	}
	return key, nil
}

// hostKeyAlgorithms are the algorithms that make the server present a key of
// keyType (an ssh-rsa key is signed with rsa-sha2-*).
func hostKeyAlgorithms(keyType string) []string {
	if keyType == ssh.KeyAlgoRSA {
		return []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256, ssh.KeyAlgoRSA}
	}
	return []string{keyType}
}
