package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mbvlabs/eregion/internal/access"
	"github.com/mbvlabs/eregion/internal/buildinfo"
	"github.com/mbvlabs/eregion/internal/cli/ui"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/host"
	"github.com/mbvlabs/eregion/internal/upgrade"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

// Init phases recorded in pending/<context>/init.json. --resume continues
// after the last completed one.
const (
	phaseContact     = "first_contact"
	phaseSecrets     = "operator_identity"
	phaseHost        = "host_bootstrapped"
	phaseAccessSetup = "access_setup"
	phaseAccess      = "wireguard_access"
	phaseContext     = "context_saved"
	phaseHandoff     = "handoff_stored"
)

// serverSpecs is the operator entry point: eregion server init walks a human
// through first contact in a terminal (preflight, answers, plan, apply,
// handoff), and takes the same answers as flags for agents.
func (a *app) serverSpecs() []spec {
	parent := readMeta("Set up and manage eregion servers",
		[]string{"Set up a fresh Debian 13 or Ubuntu 26.04 server as an eregion main node. The operator CLI runs on Linux or macOS; the node receives a matching linux binary."},
		[]string{"eregion server init", "eregion server init --host root@203.0.113.10 --hostname app.example.com --database-url-file /root/eregion-db.url --context-name prod --accept-new-host-key --secrets-out ./prod-secrets.md --confirm prod --json"})
	parent.Platforms = []string{"linux", "darwin"}
	meta := parent
	meta.Summary = "Set up a fresh server as the main node (control plane)"
	meta.WhenToUse = []string{"Run it bare in a terminal for the guided setup. Agents pass every answer, a host key decision, --secrets-out and --confirm <server>; JSON and no-TTY use never prompt."}
	meta.Mutating, meta.DryRun, meta.Prompts, meta.Remote = true, true, true, true
	meta.InputFlags = []string{"host", "identity", "password-stdin", "node", "hostname", "database-url-file", "context-name", "admin-user", "answers", "host-key-fingerprint", "accept-new-host-key", "secrets-out", "confirm"}
	meta.Environment = []string{"EREGION_DATABASE_URL", "EREGION_CONTEXT_FILE"}
	meta.NeedsRestart = "never"
	meta.ErrorCodes = append(meta.ErrorCodes, contract.ValidationFailed, contract.PreconditionFailed, contract.ConfirmationRequired, contract.MissingTool, contract.RemoteUnreachable, contract.NotFound)
	return []spec{{
		use:  "server",
		meta: parent,
		run:  func(cmd *cobra.Command, _ []string) (result, error) { return a.help(cmd), nil },
		children: []spec{{
			use:  "init",
			meta: meta,
			flags: func(cmd *cobra.Command) {
				f := cmd.Flags()
				f.String("host", "", "Server to set up: user@host, as root (SSH port 22)")
				f.String("identity", "", "SSH private key for first contact; default: ssh-agent and ~/.ssh keys")
				f.Bool("password-stdin", false, "Read the root SSH password from the first line of standard input")
				f.String("node", "", "Main node name (default main)")
				f.String("hostname", "", "Public hostname of the main node, used exactly as given (e.g. app.example.com)")
				f.String("database-url-file", "", "External PostgreSQL URL file; otherwise EREGION_DATABASE_URL")
				f.String("context-name", "", "Server and local context name (default: the node name)")
				f.String("admin-user", "admin", "Unprivileged operator account on the server")
				f.String("answers", "", "TOML file with the answers; flags override it")
				f.String("host-key-fingerprint", "", "Expected SHA256 host key fingerprint")
				f.Bool("accept-new-host-key", false, "Trust an unpinned host key without checking it")
				f.String("secrets-out", "", "Write the full secrets handoff to this new owner-only file; required without a terminal")
				f.String("confirm", "", "Confirm the plan; must name the server (--confirm <server>)")
				f.Bool("resume", false, "Continue an unfinished init")
				f.Bool("discard", false, "Drop an unfinished init's local state; the server is not touched")
				f.Bool("dns-ready", false, "The DNS record exists; don't wait for 'confirmed'")
				f.Bool("dry-run", false, "Show the plan after read-only preflight checks")
			},
			run: a.serverInit,
		}},
	}}
}

// serverInitAnswers are the answers from the answers file, flags and prompts.
// DatabaseURL is loaded from the protected file and never persisted.
type serverInitAnswers struct {
	Host            string
	User            string
	Port            string
	Identity        string
	PasswordStdin   bool
	Node            string
	Hostname        string
	DatabaseURLFile string
	DatabaseURL     string
	Context         string
	AdminUser       string
}

type serverInitSummary struct {
	Server        string   `json:"server"`
	Node          string   `json:"node"`
	Platform      string   `json:"platform"`
	Hostname      string   `json:"hostname"`
	Context       string   `json:"context"`
	AdminUser     string   `json:"admin_user"`
	PublicAddress string   `json:"public_address"`
	API           string   `json:"api"`
	NodeExporter  string   `json:"node_exporter,omitempty"`
	SecretsOut    string   `json:"secrets_out,omitempty"`
	Steps         []string `json:"steps,omitempty"`
	OperationID   string   `json:"operation_id,omitempty"`
	DryRun        bool     `json:"dry_run,omitempty"`
	Resumed       bool     `json:"resumed,omitempty"`
	Warnings      []string `json:"warnings,omitempty"`
}

type serverInit struct {
	a            *app
	cmd          *cobra.Command
	interactive  bool
	live         *ui.Live
	answers      serverInitAnswers
	pending      *host.PendingInit
	identity     *operatorIdentity
	knownHosts   string
	hostKey      string
	publicIPv4   string
	platform     host.Platform
	binaryPath   string
	remote       *host.Remote
	auth         []ssh.AuthMethod
	dryRun       bool
	dnsShown     bool
	session      *remoteSession
	hostPlan     host.Plan
	operationID  string
	vaultKeyring string
	warnings     []string
}

// hostname is the main node's public name, as the operator gave it.
func (a serverInitAnswers) hostname() string { return a.Hostname }

// address is the first-contact SSH address.
func (a serverInitAnswers) address() string { return net.JoinHostPort(a.Host, a.Port) }

func (a *app) serverInit(cmd *cobra.Command, _ []string) (res result, err error) {
	jsonMode, _ := cmd.Flags().GetBool("json")
	outputMode, _ := cmd.Flags().GetString("output")
	r := &serverInit{a: a, cmd: cmd}
	r.dryRun = boolFlag(cmd, "dry-run")
	r.interactive = !jsonMode && outputMode != "json" && term.IsTerminal(int(os.Stdin.Fd())) && ui.Enabled(os.Stderr) //nolint:gosec // G115: file descriptors fit in int.
	if r.interactive {
		r.live = ui.Start(os.Stderr)
		defer func() {
			ui.Finish(err != nil)
			if errors.Is(err, ui.ErrAborted) {
				err = contract.NewFault(contract.Cancelled, "server init cancelled", "Nothing was changed after the last finished step; run eregion server init --resume to continue.")
			}
		}()
		r.live.Println("")
		r.live.Println(ui.Header("eregion server init", r.live.Width()))
	}
	summary, err := r.run(cmd.Context())
	if err != nil {
		return result{}, err
	}
	if r.interactive {
		return result{data: summary, streamed: true}, nil
	}
	plain := "server " + summary.Server + " is ready; context " + summary.Context + " is current"
	if boolFlag(cmd, "discard") {
		plain = "dropped the unfinished init of " + summary.Server
		if summary.DryRun {
			plain = "would drop the unfinished init of " + summary.Server
		}
	}
	if summary.DryRun && !boolFlag(cmd, "discard") {
		plain = "would set up server " + summary.Server + " (" + summary.Platform + "); nothing was applied"
	}
	if summary.SecretsOut != "" {
		plain += "; secrets written to " + summary.SecretsOut
	}
	return result{data: summary, plain: plain}, nil
}

func (r *serverInit) run(ctx context.Context) (serverInitSummary, error) {
	if boolFlag(r.cmd, "discard") {
		if boolFlag(r.cmd, "resume") {
			return serverInitSummary{}, contract.NewFault(contract.Usage, "--resume and --discard cannot be combined", "Run one of them.")
		}
		return r.discard()
	}
	if err := r.prepare(); err != nil {
		return serverInitSummary{}, err
	}
	if r.pending.Done(phaseHandoff) {
		_ = host.RemovePending(r.answers.Context)
		return serverInitSummary{
			Server: r.answers.Context, Node: r.answers.Node, Context: r.answers.Context,
			Hostname: r.answers.Hostname, AdminUser: r.answers.AdminUser,
			PublicAddress: r.answers.Host, Platform: r.platform.String(), Resumed: boolFlag(r.cmd, "resume"),
		}, nil
	}
	summary := serverInitSummary{
		Server: r.answers.Context, Node: r.answers.Node, Context: r.answers.Context,
		Hostname: r.answers.Hostname, AdminUser: r.answers.AdminUser,
		PublicAddress: r.answers.Host, Resumed: boolFlag(r.cmd, "resume"),
	}
	if r.dryRun {
		if err := r.preview(ctx); err != nil {
			return summary, err
		}
		summary.DryRun, summary.Platform = true, r.platform.String()
		for _, step := range r.planRows(r.hostPlan) {
			summary.Steps = append(summary.Steps, step.Description)
		}
		return summary, nil
	}
	if err := r.requireHandoffStore(); err != nil {
		return summary, err
	}
	if err := r.execute(ctx, &summary); err != nil {
		return summary, err
	}
	summary.Platform = r.platform.String()
	summary.Warnings = r.warnings
	r.finish(summary)
	return summary, nil
}

// execute walks the remaining phases. Each phase is idempotent and recorded
// before the next one starts, so an interrupted init resumes without a second
// operator identity or a second lockdown.
func (r *serverInit) execute(ctx context.Context, summary *serverInitSummary) error {
	if !r.pending.Done(phaseContact) {
		if err := r.localPreflight(ctx); err != nil {
			return err
		}
		// Save only once the local preflight passed: a missing key or an
		// unreachable host must not leave an unfinished init behind.
		if err := r.savePending(); err != nil {
			return err
		}
		if err := r.contact(ctx); err != nil {
			return err
		}
		if err := r.mark(phaseContact); err != nil {
			return err
		}
	}
	// A resume skips first contact, so show the DNS record here unless this run
	// already did. --dns-ready treats it as created and never waits.
	if !r.dnsShown {
		if err := r.askDNS(); err != nil {
			return err
		}
	}
	if r.pending.Done(phaseAccessSetup) {
		// The node already trusts the generated SSH key. Regenerating it would
		// silently break the only operator login.
		if dir, dirErr := identityDir(r.answers.Context); dirErr == nil {
			for _, name := range []string{"ssh_ed25519", "wireguard.key"} {
				if _, statErr := os.Stat(filepath.Join(dir, name)); statErr != nil {
					return contract.NewFault(contract.PreconditionFailed, "the operator identity for "+r.answers.Context+" is missing ("+name+")", "The server already trusts the old keys. Restore "+dir+" or run server init --discard and rebuild.")
				}
			}
		}
	}
	id, err := loadOrCreateIdentity(r.answers.Context)
	if err != nil {
		return err
	}
	r.identity = id
	// The generated operator credentials are secrets in every later message.
	r.a.secrets.Add(id.sudoPassword, id.wg.Encode(), id.sshKeyPEM)
	if !r.pending.Done(phaseSecrets) {
		if err := r.mark(phaseSecrets); err != nil {
			return err
		}
	}

	// Plan, then confirm, before any host change.
	if !r.pending.Done(phaseHost) {
		if err := r.ensureRootConnection(ctx); err != nil {
			return err
		}
		if err := r.prepareHost(ctx); err != nil {
			return err
		}
		r.showPlan(r.hostPlan)
	} else {
		r.showResumePlan()
	}
	if err := r.confirm(); err != nil {
		r.removeSession(ctx)
		return err
	}
	if !r.pending.Done(phaseHost) {
		if err := r.applyHost(ctx); err != nil {
			return err
		}
	}
	summary.OperationID = r.operationID

	if !r.pending.Done(phaseAccessSetup) {
		if err := r.ensureRootConnection(ctx); err != nil {
			return err
		}
	}
	if err := r.readVaultKeyring(ctx); err != nil {
		return err
	}

	var route *host.ContextSSH
	if !r.pending.Done(phaseAccess) {
		if route, err = r.secureAccess(ctx, r.identity, r.answers.Host, r.knownHosts); err != nil {
			return err
		}
		globalKnownHosts, err := r.pinPendingKnownHosts()
		if err != nil {
			return err
		}
		route.KnownHosts = globalKnownHosts
		r.pending.NodePublicKey = route.WireGuard.NodePublicKey
		if err := r.savePending(); err != nil {
			return err
		}
		if err := r.mark(phaseAccess); err != nil {
			return err
		}
		r.remote = nil
	} else {
		if route, err = r.routeFromPending(); err != nil {
			return err
		}
	}
	summary.API = "ssh " + r.answers.AdminUser + "@" + access.NodeAddress + " over WireGuard"

	if !r.pending.Done(phaseContext) {
		if err := r.saveContext(route); err != nil {
			return err
		}
		if err := r.mark(phaseContext); err != nil {
			return err
		}
	} else if store, err := host.LoadContextStore(""); err == nil {
		if _, err := store.Use(r.answers.Context); err != nil {
			return err
		}
	}

	task := r.start("ready", "Checking that the node RPC answers")
	client, err := r.a.contextClient(r.cmd)
	if err == nil {
		err = client.Ready(ctx)
	}
	if err != nil {
		task.end(ui.EndFail, "")
		return err
	}
	task.end(ui.EndOK, "eregion node rpc over SSH in WireGuard")

	summary.NodeExporter = r.checkNodeExporter(ctx, route)
	r.checkDNS(ctx)
	r.waitForCertificate(ctx)

	if !r.pending.Done(phaseHandoff) {
		if err := r.handoff(ctx, summary); err != nil {
			return err
		}
		if err := r.mark(phaseHandoff); err != nil {
			return err
		}
	}
	if err := host.RemovePending(r.answers.Context); err != nil {
		r.warnings = append(r.warnings, "the local pending state could not be removed: "+err.Error())
	}
	return nil
}

// prepare collects and validates the answers, loads a pending init on
// --resume, and refuses a server that already has a context or an unfinished
// init.
func (r *serverInit) prepare() error {
	if boolFlag(r.cmd, "resume") {
		name := flagString(r.cmd, "context-name")
		if name == "" {
			names, err := host.PendingNames()
			if err != nil {
				return err
			}
			switch len(names) {
			case 0:
				return contract.NewFault(contract.PreconditionFailed, "there is no unfinished init to resume", "Start one with eregion server init.")
			case 1:
				name = names[0]
			default:
				fault := contract.NewFault(contract.ValidationFailed, "several unfinished inits exist", "Pass --context-name with one of details.pending.")
				fault.Details = map[string]any{"pending": names}
				return fault
			}
		}
		pending, err := host.LoadPending(name)
		if err != nil {
			return err
		}
		r.pending = pending
		r.answers = answersFromPending(pending)
		if v := flagString(r.cmd, "identity"); v != "" {
			r.answers.Identity = v
		}
		r.answers.PasswordStdin = boolFlag(r.cmd, "password-stdin")
		if err := r.loadDatabaseURL(); err != nil {
			return err
		}
		if v := flagString(r.cmd, "host-key-fingerprint"); v != "" {
			r.pending.HostKeyFingerprint = v
		}
		if v := flagString(r.cmd, "secrets-out"); v != "" {
			r.pending.SecretsOut = v
		}
		known, err := host.PendingKnownHosts(name)
		if err != nil {
			return err
		}
		r.knownHosts = known
		r.publicIPv4 = pending.PublicIPv4
		r.hostKey = pending.HostKey
		r.platform = host.Platform{PrettyName: pending.Platform, Arch: pending.Arch}
	} else {
		if err := r.collect(); err != nil {
			return err
		}
		name := r.answers.Context
		store, err := host.LoadContextStore("")
		if err != nil {
			return err
		}
		for _, saved := range store.Contexts {
			if saved.Name == name {
				fault := contract.NewFault(contract.PreconditionFailed, "a context named "+name+" already exists on this machine", "Use another --context-name, or remove it with eregion context remove "+name+" --confirm "+name+".")
				fault.Details = map[string]any{"reason": "context_exists", "context": name}
				return fault
			}
		}
		if host.PendingExists(name) {
			fault := contract.NewFault(contract.PreconditionFailed, "an unfinished init for "+name+" exists", "Continue it with eregion server init --resume --context-name "+name+", or drop it with eregion server init --discard --context-name "+name+".")
			fault.Details = map[string]any{"reason": "init_pending", "server": name}
			return fault
		}
		r.pending = &host.PendingInit{Server: name, Phases: map[string]string{}}
		known, err := host.PendingKnownHosts(name)
		if err != nil {
			return err
		}
		r.knownHosts = known
	}
	return r.validate()
}

func answersFromPending(p *host.PendingInit) serverInitAnswers {
	port := p.Port
	if port == 0 {
		port = 22
	}
	return serverInitAnswers{
		Host: p.Host, User: p.User, Port: strconv.Itoa(port), Identity: p.Identity,
		Node: p.Node, Context: p.Context, Hostname: p.Hostname,
		DatabaseURLFile: p.DatabaseURLFile, AdminUser: p.AdminUser,
	}
}

// collect reads the answers file, then flags, then prompts for a terminal.
func (r *serverInit) collect() error {
	cmd := r.cmd
	given := map[string]bool{}
	var ans serverInitAnswers
	filePort := 0
	if path := flagString(cmd, "answers"); path != "" {
		path = expandHome(path)
		info, statErr := os.Stat(path)
		if statErr != nil {
			return contract.NewFault(contract.ValidationFailed, "could not read the answers file", statErr.Error())
		}
		if info.Size() > 64<<10 {
			return contract.NewFault(contract.ValidationFailed, "the answers file is too large", "Keep --answers under 64 KiB.")
		}
		raw, err := os.ReadFile(path) //nolint:gosec // G304: the operator chose this answers file.
		if err != nil {
			return contract.NewFault(contract.ValidationFailed, "could not read the answers file", err.Error())
		}
		file, err := parseAnswersFile(raw)
		if err != nil {
			return contract.NewFault(contract.ValidationFailed, path+" does not parse", err.Error())
		}
		ans = file.answers()
		filePort = file.Port
		for key, present := range file.supplied() {
			given[key] = present
		}
	}
	take := func(name, flag string, dst *string) {
		if cmd.Flags().Changed(flag) {
			*dst = strings.TrimSpace(flagString(cmd, flag))
			given[name] = *dst != ""
		}
	}
	take("host", "host", &ans.Host)
	take("identity", "identity", &ans.Identity)
	take("node", "node", &ans.Node)
	take("hostname", "hostname", &ans.Hostname)
	take("database_url_file", "database-url-file", &ans.DatabaseURLFile)
	take("context", "context-name", &ans.Context)
	take("admin_user", "admin-user", &ans.AdminUser)
	ans.PasswordStdin = boolFlag(cmd, "password-stdin")

	if !given["host"] || ans.Host == "" {
		if !r.interactive {
			return contract.NewFault(contract.InputRequired, "--host is required without a terminal", "Pass --host root@203.0.113.10.")
		}
		v, err := ui.Input("Server to set up", "user@host, as root (SSH port 22)", "")
		if err != nil {
			return err
		}
		ans.Host = v
	}
	user, hostPart, port, err := parseTarget(ans.Host)
	if err != nil {
		return err
	}
	if filePort > 0 && !targetHasPort(ans.Host) {
		port = strconv.Itoa(filePort)
	}
	ans.User, ans.Host, ans.Port = user, hostPart, port

	if ans.Node == "" {
		ans.Node = "main"
	}
	if !given["context"] && r.interactive {
		v, err := ui.Input("Server and context name", "Names the server locally and in the secrets handoff", ans.Node)
		if err != nil {
			return err
		}
		ans.Context = v
	}
	if ans.Context == "" {
		ans.Context = ans.Node
	}
	if ans.AdminUser == "" {
		ans.AdminUser = access.AdminUser
	}
	if !given["hostname"] || ans.Hostname == "" {
		if !r.interactive {
			return contract.NewFault(contract.InputRequired, "--hostname is required without a terminal", "Pass --hostname app.example.com.")
		}
		v, err := ui.Input("Public hostname", "The main node's name, used exactly as given. Create one DNS record for it.", "")
		if err != nil {
			return err
		}
		ans.Hostname = v
	}
	ans.Hostname = strings.TrimSuffix(strings.TrimSpace(ans.Hostname), ".")
	if !given["database_url_file"] || ans.DatabaseURLFile == "" {
		if env := strings.TrimSpace(os.Getenv("EREGION_DATABASE_URL")); env != "" {
			ans.DatabaseURL = env
		} else if r.interactive {
			v, err := ui.Secret("External PostgreSQL URL (postgres://user:password@host:5432/db)")
			if err != nil {
				return err
			}
			ans.DatabaseURL = v
		} else {
			return contract.NewFault(contract.InputRequired, "the control plane needs an external PostgreSQL URL", "Pass --database-url-file with an owner-only file.")
		}
	}
	if ans.DatabaseURL == "" && ans.DatabaseURLFile != "" {
		raw, err := host.ReadPrivateInput(expandHome(ans.DatabaseURLFile))
		if err != nil {
			return contract.NewFault(contract.ValidationFailed, "could not read the database URL file", err.Error())
		}
		ans.DatabaseURL = strings.TrimSpace(string(raw))
	}
	if !given["node"] && r.interactive {
		v, err := ui.Input("Main node name", "This server becomes the main node: control plane, workers, builds and applications", ans.Node)
		if err != nil {
			return err
		}
		ans.Node = v
	}
	if !given["admin_user"] && r.interactive {
		v, err := ui.Input("Admin user", "The unprivileged operator account", ans.AdminUser)
		if err != nil {
			return err
		}
		ans.AdminUser = v
	}
	r.answers = ans
	if r.interactive {
		r.live.Println(ui.Answered("Server", ans.User+"@"+ans.address()))
		r.live.Println(ui.Answered("Node", ans.Node))
		r.live.Println(ui.Answered("Context", ans.Context))
		r.live.Println(ui.Answered("Public hostname", ans.Hostname))
		r.live.Println(ui.Answered("Admin user", ans.AdminUser))
	}
	return nil
}

// validate checks every answer before anything is generated or contacted.
func (r *serverInit) validate() error {
	ans := r.answers
	if ans.Host == "" || ans.User == "" {
		return contract.NewFault(contract.InputRequired, "the server must be user@host", "Pass --host root@203.0.113.10.")
	}
	if ans.Port != "" && ans.Port != "22" {
		return contract.NewFault(contract.ValidationFailed, "server init needs SSH on port 22, not "+ans.Port, "Connect on port 22 (a provider NAT or firewall rule is fine), or move sshd to 22, then run server init again. The WireGuard route and sshd drop-in use port 22.")
	}
	if ans.PasswordStdin && ans.Identity != "" {
		return contract.NewFault(contract.Usage, "--password-stdin and --identity cannot be combined", "Use one first-contact credential.")
	}
	if err := host.ValidateContextName(ans.Context); err != nil {
		return err
	}
	if err := host.ValidateNodeName(ans.Node); err != nil {
		return err
	}
	if err := host.ValidateAccountName(ans.AdminUser); err != nil {
		return err
	}
	if ans.AdminUser == "root" {
		return contract.NewFault(contract.ValidationFailed, "the admin user must be an unprivileged account", "Choose a new or existing non-root account, for example admin.")
	}
	if err := host.ValidateHostname(ans.Hostname); err != nil {
		return contract.NewFault(contract.ValidationFailed, "invalid hostname "+ans.Hostname, err.Error())
	}
	if ans.DatabaseURL == "" {
		return contract.NewFault(contract.InputRequired, "the control plane needs an external PostgreSQL URL", "Pass --database-url-file with an owner-only file.")
	}
	if err := checkIdentity(ans.Identity); err != nil {
		return err
	}
	r.a.secrets.Add(ans.DatabaseURL)
	return nil
}

// checkIdentity validates an explicit first-contact key before any prompt or
// connection; it accepts an encrypted key because first contact can ask for
// its passphrase in a terminal.
func checkIdentity(path string) error {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	raw, err := os.ReadFile(expandHome(path)) //nolint:gosec // G304: the operator's own key.
	if err != nil {
		return contract.NewFault(contract.ValidationFailed, "the SSH identity "+path+" is not readable", err.Error())
	}
	if _, err := ssh.ParsePrivateKey(raw); err != nil {
		var encrypted *ssh.PassphraseMissingError
		if !errors.As(err, &encrypted) {
			return contract.NewFault(contract.ValidationFailed, "the SSH identity "+path+" is not a usable private key", err.Error())
		}
	}
	return nil
}

// targetHasPort reports whether the host part carries an explicit :port.
func targetHasPort(target string) bool {
	if _, _, err := net.SplitHostPort(target); err != nil {
		return false
	}
	return true
}

func parseTarget(v string) (user, hostPart, port string, err error) {
	user = "root"
	if at := strings.LastIndex(v, "@"); at >= 0 {
		user, v = v[:at], v[at+1:]
	}
	hostPart, port = v, "22"
	if h, p, splitErr := net.SplitHostPort(v); splitErr == nil {
		hostPart, port = h, p
	}
	if hostPart == "" || user == "" {
		return "", "", "", contract.NewFault(contract.ValidationFailed, "the server must be user@host or user@host:port", "For example root@203.0.113.10.")
	}
	if n, convErr := strconv.Atoi(port); convErr != nil || n < 1 || n > 65535 {
		return "", "", "", contract.NewFault(contract.ValidationFailed, "the SSH port must be a number from 1 to 65535", "")
	}
	return user, hostPart, port, nil
}

// localPreflight is step 1: tools and reachability before the host key or any
// remote fact is touched.
func (r *serverInit) localPreflight(ctx context.Context) error {
	task := r.start("preflight", "Local preflight")
	if err := requireLocalTools(); err != nil {
		task.end(ui.EndFail, "")
		return err
	}
	task.detail("operator " + runtime.GOOS + "/" + runtime.GOARCH)
	auth, err := firstContactAuth(r.cmd, r.answers.Identity, r.interactive, r.a.secrets)
	if err != nil {
		task.end(ui.EndFail, "")
		return err
	}
	r.auth = auth
	task.end(ui.EndOK, "SSH credential ready")
	reach := r.start("reachable", "Checking that "+r.answers.address()+" answers")
	if err := checkReachable(ctx, r.answers.address(), 20*time.Second); err != nil {
		reach.end(ui.EndFail, "")
		return err
	}
	reach.end(ui.EndOK, "")
	return nil
}

// trustHostKey pins the server's key on first contact: a matching
// --host-key-fingerprint, --accept-new-host-key, or a yes in a terminal.
func (r *serverInit) trustHostKey(ctx context.Context) error {
	address := r.answers.address()
	want := flagString(r.cmd, "host-key-fingerprint")
	if want == "" {
		want = r.pending.HostKeyFingerprint
	}
	accept := boolFlag(r.cmd, "accept-new-host-key")
	trusted := func(key ssh.PublicKey) (bool, error) {
		fingerprint := ssh.FingerprintSHA256(key)
		r.hostKey = key.Type() + " " + fingerprint
		if want != "" && (want == fingerprint || "SHA256:"+want == fingerprint) {
			return true, nil
		}
		if accept {
			r.warnings = append(r.warnings, "trusted host key "+fingerprint+" without checking it")
			return true, nil
		}
		if !r.interactive {
			fault := contract.NewFault(contract.InputRequired, "first contact needs the host key "+fingerprint+" to be confirmed", "Pass --host-key-fingerprint "+fingerprint+" or --accept-new-host-key.")
			fault.Details = map[string]any{"flag": "--host-key-fingerprint", "fingerprint": fingerprint}
			return false, fault
		}
		ok, err := ui.YesNoDefault("Trust host key "+fingerprint+"?", "Compare it with ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub on the provider console.", true)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, contract.NewFault(contract.Cancelled, "the host key was not trusted", "Pass --host-key-fingerprint "+fingerprint+" once it is checked.")
		}
		return true, nil
	}

	if hostKeyPinned(r.knownHosts, address) {
		key, err := probeHostKey(ctx, address)
		if err != nil {
			return contract.NewFault(contract.RemoteUnreachable, "the server does not answer on "+address, err.Error())
		}
		if pinnedKeyMatches(r.knownHosts, address, key) {
			r.hostKey = key.Type() + " " + ssh.FingerprintSHA256(key)
			return nil
		}
		// A rebuilt server has new host keys. Replacing the pin is a decision.
		fingerprint := ssh.FingerprintSHA256(key)
		r.warn("the host key of " + address + " changed to " + key.Type() + " " + fingerprint)
		ok, err := trusted(key)
		if err != nil {
			return err
		}
		if !ok {
			return nil
		}
		for _, pinned := range []string{address, net.JoinHostPort(access.NodeAddress, "22")} {
			if err := unpinHostKey(r.knownHosts, pinned); err != nil {
				return err
			}
		}
	}

	task := r.start("hostkey", "Reading the host key of "+address)
	key, err := probeHostKey(ctx, address)
	if err != nil {
		task.end(ui.EndFail, "")
		return contract.NewFault(contract.RemoteUnreachable, "the server does not answer on "+address, err.Error())
	}
	fingerprint := ssh.FingerprintSHA256(key)
	if _, err := trusted(key); err != nil {
		task.end(ui.EndFail, "")
		return err
	}
	task.end(ui.EndOK, key.Type()+" "+fingerprint)
	return pinHostKey(r.knownHosts, address, key)
}

// contact pins the host key, authenticates as root and runs the read-only
// remote preflight.
func (r *serverInit) contact(ctx context.Context) error {
	if err := r.trustHostKey(ctx); err != nil {
		return err
	}
	auth := r.auth
	if len(auth) == 0 {
		resolved, err := firstContactAuth(r.cmd, r.answers.Identity, r.interactive, r.a.secrets)
		if err != nil {
			return err
		}
		auth = resolved
	}
	remote, err := host.NewRemote(host.RemoteConfig{
		Address: r.answers.address(), User: r.answers.User, KnownHosts: r.knownHosts, Auth: auth,
	})
	if err != nil {
		return contract.NewFault(contract.ValidationFailed, "the SSH target is not usable", err.Error())
	}
	task := r.start("contact", "Connecting to "+r.answers.User+"@"+r.answers.address())
	raw, err := remote.Output(ctx, remoteProbeScript)
	if err != nil {
		task.end(ui.EndFail, "")
		return contract.NewFault(contract.Unavailable, "could not log in over SSH", err.Error())
	}
	facts, err := parseRemoteFacts(string(raw))
	if err != nil {
		task.end(ui.EndFail, "")
		return err
	}
	if err := r.checkRemotePreflight(facts, boolFlag(r.cmd, "resume")); err != nil {
		task.end(ui.EndFail, "")
		return err
	}
	r.remote = remote
	r.platform = facts.platform()
	r.pending.Platform, r.pending.Arch = r.platform.PrettyName, r.platform.Arch
	task.end(ui.EndOK, r.platform.PrettyName+", "+r.platform.Arch)
	if r.publicIPv4, err = publicIPv4(ctx, r.answers.Host); err != nil {
		return err
	}
	r.pending.PublicIPv4 = r.publicIPv4
	if err := r.askDNS(); err != nil {
		return err
	}
	if r.dryRun {
		return nil
	}
	return r.savePending()
}

// prepareHost selects the node binary, uploads the planning bundle and reads
// the remote plan. It is the plan step, not an apply.
func (r *serverInit) prepareHost(ctx context.Context) error {
	if err := r.resolveNodeBinary(ctx); err != nil {
		return err
	}
	cfg, err := r.hostConfig()
	if err != nil {
		return err
	}
	task := r.start("upload", "Uploading the linux node binary and planning")
	session, err := newRemoteSession(ctx, r.remote, cfg, r.answers.address())
	if err != nil {
		task.end(ui.EndFail, "")
		return err
	}
	plan, err := remotePlan(ctx, session)
	if err != nil {
		r.session = session
		r.removeSession(ctx)
		task.end(ui.EndFail, "")
		return err
	}
	r.session, r.hostPlan = session, plan
	task.end(ui.EndOK, fmt.Sprintf("%d steps", len(plan.Steps)))
	if plan.ApplyBlocked != "" {
		r.removeSession(ctx)
		return contract.NewFault(contract.PreconditionFailed, "the server cannot be set up: "+plan.ApplyBlocked, "Fix the reported precondition and run eregion server init again.")
	}
	return nil
}

// resolveNodeBinary picks the linux binary that is installed at
// /usr/local/bin/eregion once. The running CLI is used when it already is
// linux/<node arch>; otherwise the verified release artifact for this CLI's
// version is fetched.
func (r *serverInit) resolveNodeBinary(ctx context.Context) error {
	if r.platform.Arch != "amd64" && r.platform.Arch != "arm64" {
		return contract.NewFault(contract.PreconditionFailed, "the server architecture "+r.platform.Arch+" is not supported", "Use an amd64 or arm64 server.")
	}
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	if runtime.GOOS == "linux" && runtime.GOARCH == r.platform.Arch {
		r.binaryPath = executable
		return nil
	}
	if buildinfo.Version == "" || buildinfo.Version == "dev" || buildinfo.Channel == "" || buildinfo.Channel == "dev" {
		return contract.NewFault(
			contract.PreconditionFailed,
			"this CLI cannot provide a linux/"+r.platform.Arch+" node binary",
			"Run server init from a linux/"+r.platform.Arch+" build of eregion, or install a released build whose channel publishes a linux/"+r.platform.Arch+" artifact.",
		)
	}
	task := r.start("binary", "Fetching the verified linux/"+r.platform.Arch+" node binary")
	_, path, err := upgrade.FetchNodeBinary(ctx, buildinfo.Channel, buildinfo.Version, r.platform.Arch)
	if err != nil {
		task.end(ui.EndFail, "")
		return err
	}
	task.end(ui.EndOK, buildinfo.Version)
	r.binaryPath = path
	return nil
}

func (r *serverInit) hostConfig() (host.Config, error) {
	cfg := host.DefaultConfig(host.RoleMain, r.answers.Node)
	cfg.DatabaseURL = r.answers.DatabaseURL
	cfg.Hostname = r.answers.Hostname
	cfg.BinaryPath = r.binaryPath
	if err := cfg.Validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// applyHost applies the exact plan the operator confirmed. The uploaded bundle
// and its stable paths are the same ones the plan used.
func (r *serverInit) applyHost(ctx context.Context) error {
	if r.session == nil {
		return contract.NewFault(contract.Internal, "the server init lost its uploaded plan", "Report the command and binary version.")
	}
	report, err := r.apply(ctx, r.session, r.hostPlan)
	if err != nil {
		return withResumeHint(err, r.answers.Context)
	}
	r.removeSession(ctx)
	r.operationID = report.OperationID
	return r.mark(phaseHost)
}

// removeSession drops the uploaded planning/apply bundle. A failed apply keeps
// it for resume; every path that never applies must remove it.
func (r *serverInit) removeSession(ctx context.Context) {
	if r.session == nil {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := r.session.Remove(cleanupCtx); err != nil {
		r.note("uploaded bundle cleanup failed: " + err.Error())
	}
	r.session = nil
}

// ensureRootConnection reopens first contact for a resume that already ran the
// host phase. The root account is still enabled until the access setup runs.
func (r *serverInit) ensureRootConnection(ctx context.Context) error {
	if r.remote != nil {
		return nil
	}
	auth := r.auth
	if len(auth) == 0 {
		resolved, err := firstContactAuth(r.cmd, r.answers.Identity, r.interactive, r.a.secrets)
		if err != nil {
			return err
		}
		auth = resolved
	}
	remote, err := host.NewRemote(host.RemoteConfig{
		Address: r.answers.address(), User: r.answers.User, KnownHosts: r.knownHosts, Auth: auth,
	})
	if err != nil {
		return contract.NewFault(contract.ValidationFailed, "the SSH target is not usable", err.Error())
	}
	r.remote = remote
	return nil
}

// readVaultKeyring reads the generated keyring for the handoff. It is the only
// server secret that is not otherwise on the operator's machine.
func (r *serverInit) readVaultKeyring(ctx context.Context) error {
	if r.vaultKeyring != "" {
		return nil
	}
	path := filepath.Join(host.DefaultLayout().ConfigDir, host.VaultKeyringName)
	task := r.start("keyring", "Reading the vault keyring for the handoff")
	var ring string
	if raw, err := host.LoadPendingFile(r.answers.Context, host.VaultKeyringName); err == nil {
		ring = strings.TrimSpace(string(raw))
	} else if r.remote == nil {
		task.end(ui.EndFail, "")
		return contract.NewFault(contract.PreconditionFailed, "the vault keyring was not captured before access switched to WireGuard", "Run server init --discard and start again; the keyring is on the server but the operator account cannot read it.")
	} else {
		raw, err := r.remote.Output(ctx, "cat "+shellPath(path))
		if err != nil {
			task.end(ui.EndFail, "")
			return contract.NewFault(contract.Unavailable, "could not read the vault keyring on the server", err.Error())
		}
		ring = strings.TrimSpace(string(raw))
		if err := host.SavePendingFile(r.answers.Context, host.VaultKeyringName, []byte(strings.TrimRight(ring, "\n")+"\n")); err != nil {
			task.end(ui.EndFail, "")
			return err
		}
	}
	if ring == "" {
		task.end(ui.EndFail, "")
		return contract.NewFault(contract.PreconditionFailed, "the vault keyring on the server is empty", "Fix the host-vault-keyring-v1 step before taking the handoff.")
	}
	task.end(ui.EndOK, path)
	r.vaultKeyring = ring
	r.a.secrets.Add(ring)
	return nil
}

// loadDatabaseURL reads the DSN again on a resume. It is never stored in the
// pending state, only the protected file path is; a URL typed at the prompt
// is asked for again.
func (r *serverInit) loadDatabaseURL() error {
	if r.answers.DatabaseURLFile != "" {
		raw, err := host.ReadPrivateInput(expandHome(r.answers.DatabaseURLFile))
		if err != nil {
			return contract.NewFault(contract.ValidationFailed, "could not read the database URL file", err.Error())
		}
		r.answers.DatabaseURL = strings.TrimSpace(string(raw))
		return nil
	}
	r.answers.DatabaseURL = strings.TrimSpace(os.Getenv("EREGION_DATABASE_URL"))
	if r.answers.DatabaseURL == "" && r.interactive {
		v, err := ui.Secret("External PostgreSQL URL (postgres://user:password@host:5432/db)")
		if err != nil {
			return err
		}
		r.answers.DatabaseURL = v
	}
	return nil
}

// routeFromPending rebuilds the context route on a resume that already locked
// the host down.
func (r *serverInit) routeFromPending() (*host.ContextSSH, error) {
	if r.identity == nil {
		id, err := loadOrCreateIdentity(r.answers.Context)
		if err != nil {
			return nil, err
		}
		r.identity = id
	}
	if r.pending.NodePublicKey == "" {
		return nil, contract.NewFault(contract.PreconditionFailed, "the pending init has no node WireGuard key", "Run server init --discard and start again; the host may need a rebuild.")
	}
	known, err := knownHostsPath()
	if err != nil {
		return nil, err
	}
	return r.identity.wireGuardRoute(r.answers.Host, r.pending.NodePublicKey, known, r.answers.AdminUser), nil
}

// pinPendingKnownHosts copies the init's pins into the context store's
// known_hosts so the saved context can verify the node after the pending state
// is gone.
func (r *serverInit) pinPendingKnownHosts() (string, error) {
	global, err := knownHostsPath()
	if err != nil {
		return "", err
	}
	raw, err := os.ReadFile(r.knownHosts)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if len(raw) == 0 {
		return global, nil
	}
	if err := os.MkdirAll(filepath.Dir(global), 0o700); err != nil {
		return "", err
	}
	file, err := os.OpenFile(global, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // eregion's own known_hosts.
	if err != nil {
		return "", err
	}
	if _, err := file.Write(raw); err != nil {
		_ = file.Close()
		return "", err
	}
	if err := file.Close(); err != nil {
		return "", err
	}
	return global, nil
}

func (r *serverInit) saveContext(route *host.ContextSSH) error {
	task := r.start("context", "Saving context "+r.answers.Context)
	store, err := host.LoadContextStore("")
	if err != nil {
		task.end(ui.EndFail, "")
		return err
	}
	if _, err := store.SaveContext(host.Context{Name: r.answers.Context, SSH: route}); err != nil {
		task.end(ui.EndFail, "")
		return err
	}
	task.end(ui.EndOK, "SSH over WireGuard, no API token")
	return nil
}

func (r *serverInit) savePending() error {
	p := r.pending
	p.Server, p.Context = r.answers.Context, r.answers.Context
	p.Host, p.User, p.Identity = r.answers.Host, r.answers.User, r.answers.Identity
	p.Port = 22
	if n, err := strconv.Atoi(r.answers.Port); err == nil {
		p.Port = n
	}
	p.Node, p.Hostname = r.answers.Node, r.answers.Hostname
	p.DatabaseURLFile, p.AdminUser = r.answers.DatabaseURLFile, r.answers.AdminUser
	if r.hostKey != "" {
		p.HostKey = r.hostKey
		if _, fingerprint, ok := strings.Cut(r.hostKey, " "); ok {
			p.HostKeyFingerprint = fingerprint
		}
	}
	if r.publicIPv4 != "" {
		p.PublicIPv4 = r.publicIPv4
	}
	if r.platform.PrettyName != "" {
		p.Platform = r.platform.PrettyName
	}
	if r.platform.Arch != "" {
		p.Arch = r.platform.Arch
	}
	return p.Save()
}

func (r *serverInit) mark(phase string) error {
	r.pending.Mark(phase)
	return r.pending.Save()
}

// discard drops the local pending state. It never touches the server.
func (r *serverInit) discard() (serverInitSummary, error) {
	name := flagString(r.cmd, "context-name")
	if name == "" {
		names, err := host.PendingNames()
		if err != nil {
			return serverInitSummary{}, err
		}
		if len(names) != 1 {
			fault := contract.NewFault(contract.InputRequired, "--discard needs --context-name", "Pass the server whose unfinished init to drop.")
			fault.Details = map[string]any{"flag": "--context-name", "pending": names}
			return serverInitSummary{}, fault
		}
		name = names[0]
	}
	if err := host.ValidateContextName(name); err != nil {
		return serverInitSummary{}, err
	}
	if !host.PendingExists(name) {
		fault := contract.NewFault(contract.PreconditionFailed, "there is no unfinished init for "+name, "Start one with eregion server init.")
		fault.Details = map[string]any{"reason": "nothing_to_discard", "pending": pendingNamesOrNil()}
		return serverInitSummary{}, fault
	}
	pending, err := host.LoadPending(name)
	if err != nil {
		// A corrupt pending state must still be discardable; that is the
		// recovery path for a bad file.
		r.warnings = append(r.warnings, "the unfinished init could not be read ("+err.Error()+"); it will be removed")
		pending = &host.PendingInit{Server: name, Phases: map[string]string{}}
	}
	summary := serverInitSummary{Server: name, Node: pending.Node, Context: name, Hostname: pending.Hostname}
	if pending.Done(phaseHost) || pending.Done(phaseAccessSetup) || pending.Done(phaseAccess) {
		r.warnings = append(r.warnings,
			"the server was already bootstrapped by this init; discarding drops the only operator credentials for it (SSH key, WireGuard key, sudo password). The server itself is not changed.")
	}
	summary.Warnings = r.warnings
	for _, warning := range r.warnings {
		r.warn(warning)
	}
	if dryRun(r.cmd) {
		summary.DryRun = true
		return summary, nil
	}
	if err := r.confirmDiscard(name); err != nil {
		return summary, err
	}
	if err := host.RemovePending(name); err != nil {
		return summary, err
	}
	// Keep the identity when a context already references it; dropping the
	// files would break that context.
	store, err := host.LoadContextStore("")
	switch {
	case err != nil:
		r.warnings = append(r.warnings, "the context store is unreadable, so the generated identity files were kept: "+err.Error())
	case contextSaved(store, name):
	default:
		if dir, err := identityDir(name); err == nil {
			_ = os.RemoveAll(dir)
		}
	}
	return summary, nil
}

func contextSaved(store *host.ContextStore, name string) bool {
	for _, saved := range store.Contexts {
		if saved.Name == name {
			return true
		}
	}
	return false
}

func pendingNamesOrNil() []string {
	names, _ := host.PendingNames()
	return names
}

func (r *serverInit) confirmDiscard(name string) error {
	if want := flagString(r.cmd, "confirm"); want != "" {
		if want != name {
			fault := contract.NewFault(contract.ConfirmationRequired, "--confirm must name the server", "Pass --confirm "+name+".")
			fault.Details = map[string]any{"confirm_token": name}
			return fault
		}
		return nil
	}
	if !r.interactive {
		return contract.NewFault(contract.ConfirmationRequired, "discarding an unfinished init needs confirmation", "Pass --confirm "+name+".")
	}
	if err := ui.TypeWord("Type 'discard' to drop the unfinished init of "+name, "The server is not touched; the pending state and its operator credentials are deleted.", "discard"); err != nil {
		if errors.Is(err, ui.ErrAborted) {
			return contract.NewFault(contract.Cancelled, "the unfinished init of "+name+" was kept", "Nothing was changed.")
		}
		return err
	}
	return nil
}

// requireHandoffStore is the early non-interactive gate: without a terminal the
// handoff must go to an explicit file before any change, or init fails.
func (r *serverInit) requireHandoffStore() error {
	if r.interactive {
		return nil
	}
	if flagString(r.cmd, "secrets-out") != "" || r.pending.SecretsOut != "" {
		return nil
	}
	return contract.NewFault(contract.InputRequired, "the secrets handoff needs --secrets-out without a terminal", "Pass --secrets-out with a new owner-only file path, or run in a terminal and confirm the handoff is stored.")
}

// confirm is the plan gate. Non-interactive runs need --confirm <server>.
func (r *serverInit) confirm() error {
	server := r.answers.Context
	if want := flagString(r.cmd, "confirm"); want != "" {
		if want != server {
			fault := contract.NewFault(contract.ConfirmationRequired, "--confirm must name the server", "Pass --confirm "+server+".")
			fault.Details = map[string]any{"confirm_token": server}
			return fault
		}
		return nil
	}
	if !r.interactive {
		return contract.NewFault(contract.ConfirmationRequired, "server init needs confirmation", "Review the plan with --dry-run, then pass --confirm "+server+".")
	}
	question := "Set up " + r.answers.User + "@" + r.answers.address() + " as server " + server + " now?"
	detail := "Hardens the host, installs the control plane and closes public SSH (reverted unless confirmed)."
	if r.pending.Done(phaseAccess) && r.pending.Done(phaseContext) {
		question = "Finish the handoff for server " + server + " now?"
		detail = "The host and access are already set up; this checks readiness and shows the secrets handoff."
	}
	ok, err := ui.YesNoDefault(question, detail, true)
	if err != nil {
		return err
	}
	if !ok {
		return contract.NewFault(contract.Cancelled, "server init was not confirmed", "Nothing was applied.")
	}
	return nil
}

// preview runs the read-only part of the flow for --dry-run: preflight,
// contact, remote preflight, binary selection and the remote plan. It leaves no
// pending init behind, but does pin the host key and upload a planning bundle
// as the implementation reference says.
func (r *serverInit) preview(ctx context.Context) error {
	if !boolFlag(r.cmd, "resume") {
		defer func() { _ = host.RemovePending(r.answers.Context) }()
	}
	if !r.pending.Done(phaseContact) {
		if err := r.localPreflight(ctx); err != nil {
			return err
		}
		// Save only once the local preflight passed: a missing key or an
		// unreachable host must not leave an unfinished init behind.
		if err := r.savePending(); err != nil {
			return err
		}
		if err := r.contact(ctx); err != nil {
			return err
		}
	}
	if r.pending.Done(phaseHost) {
		r.showResumePlan()
		return nil
	}
	if err := r.ensureRootConnection(ctx); err != nil {
		return err
	}
	if err := r.prepareHost(ctx); err != nil {
		return err
	}
	defer r.removeSession(ctx)
	r.showPlan(r.hostPlan)
	return nil
}

func (r *serverInit) showPlan(plan host.Plan) {
	var b strings.Builder
	fmt.Fprintf(&b, "server        %s (%s)\n", r.answers.Context, r.answers.Host)
	fmt.Fprintf(&b, "node          %s\n", r.answers.Node)
	fmt.Fprintf(&b, "hostname      %s\n", r.answers.Hostname)
	fmt.Fprintf(&b, "admin user    %s\n", r.answers.AdminUser)
	fmt.Fprintf(&b, "node binary   %s\n", r.binaryPath)
	if r.answers.DatabaseURLFile != "" {
		fmt.Fprintf(&b, "database      external PostgreSQL from %s\n", r.answers.DatabaseURLFile)
	} else if os.Getenv("EREGION_DATABASE_URL") != "" {
		b.WriteString("database      external PostgreSQL from EREGION_DATABASE_URL\n")
	} else {
		b.WriteString("database      external PostgreSQL entered at the prompt\n")
	}
	b.WriteString("\n")
	for _, step := range r.planRows(plan) {
		mark := ui.Dim("○")
		if step.Complete {
			mark = ui.Dim(ui.SymOK)
		}
		fmt.Fprintf(&b, "%s %s\n", mark, step.Description)
	}
	for _, risk := range plan.AccessRisks {
		b.WriteString(r.warningLine(risk) + "\n")
	}
	body := strings.TrimRight(b.String(), "\n")
	title := "Plan for server " + r.answers.Context + " (" + r.platform.String() + ")"
	if r.live == nil {
		fmt.Fprintln(r.cmd.ErrOrStderr(), "plan:\n"+body)
		return
	}
	r.live.Println(ui.PanelFit(title, body, r.live.Width()))
}

// showResumePlan lists the phases a resume will still run when the host phase
// is already behind us, so the confirmation always shows a plan.
func (r *serverInit) showResumePlan() {
	remaining := []string{}
	if !r.pending.Done(phaseAccessSetup) {
		remaining = append(remaining, "harden the host, create the admin user and bring up WireGuard")
	}
	remaining = append(remaining, "confirm access over WireGuard and close public SSH")
	if !r.pending.Done(phaseContext) {
		remaining = append(remaining, "save the local context")
	}
	remaining = append(remaining, "check node readiness", "show the secrets handoff")
	body := "host steps are already applied; remaining:\n- " + strings.Join(remaining, "\n- ")
	if r.live == nil {
		fmt.Fprintln(r.cmd.ErrOrStderr(), body)
		return
	}
	r.live.Println(ui.PanelFit("Resuming server "+r.answers.Context, body, r.live.Width()))
}

// apply runs the remote apply and turns its step log into live tasks. An
// access-changing apply is confirmed from a fresh connection right away:
// reaching the host again is the proof the new access path works.
func (r *serverInit) apply(ctx context.Context, session *remoteSession, plan host.Plan) (host.ApplyReport, error) {
	progress := newApplyProgress(r, plan.Steps)
	report, err := remoteApply(ctx, session, plan.Confirmation.Token, progress)
	var fault *contract.Fault
	if err != nil && errors.As(err, &fault) && fault.Code == contract.ConfirmationRequired {
		if id, _ := fault.Details["operation_id"].(string); id != "" {
			progress.close(ui.EndOK)
			task := r.start("confirm", "Confirming access change from a fresh connection")
			raw, runErr := session.run(ctx, "confirm", []string{id}, io.Discard)
			if runErr == nil {
				_, runErr = decodeRemoteEnvelope[host.ConfirmReport](raw, "confirm")
			}
			if runErr != nil {
				task.end(ui.EndFail, "")
				return report, runErr
			}
			task.end(ui.EndOK, "")
			return host.ApplyReport{Node: plan.Node, Role: plan.Role, OperationID: id}, nil
		}
	}
	if err != nil {
		progress.close(ui.EndFail)
		return report, err
	}
	progress.close(ui.EndOK)
	return report, nil
}

// applyProgress reads "bootstrap <step>: <message>" lines from the remote
// stderr and walks the plan in order: every plan step gets a line, also the
// ones that finish without output or were already done.
type applyProgress struct {
	r     *serverInit
	plan  []host.PlanEntry
	mu    sync.Mutex
	buf   []byte
	next  int // index of the first plan step without a line yet
	task  *initTask
	index map[string]int
}

func newApplyProgress(r *serverInit, plan []host.PlanEntry) *applyProgress {
	index := make(map[string]int, len(plan))
	for i, step := range plan {
		index[step.ID] = i
	}
	return &applyProgress{r: r, plan: plan, index: index}
}

func (p *applyProgress) Write(b []byte) (int, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.buf = append(p.buf, b...)
	for {
		i := strings.IndexByte(string(p.buf), '\n')
		if i < 0 {
			break
		}
		line := string(p.buf[:i])
		p.buf = p.buf[i+1:]
		p.line(line)
	}
	return len(b), nil
}

func (p *applyProgress) line(line string) {
	rest, ok := strings.CutPrefix(line, "bootstrap ")
	if !ok {
		return
	}
	step, message, _ := strings.Cut(rest, ": ")
	message = p.r.a.secrets.Redact(message)
	// Sub-step IDs such as host-eregion-api.service-apply belong to the
	// running plan step.
	if i, planned := p.index[step]; planned && i >= p.next {
		p.endTask(ui.EndOK)
		p.flush(i)
		p.task = p.r.start("step-"+step, p.plan[i].Description)
		p.next = i + 1
	}
	if p.task != nil && message != "" && message != step {
		p.task.detail(message)
	}
}

// flush prints a line for each plan step before upto that never reported.
func (p *applyProgress) flush(upto int) {
	for ; p.next < upto; p.next++ {
		step := p.plan[p.next]
		if step.Complete {
			p.r.line(ui.EndSkip, step.Description, "already done")
		} else {
			p.r.line(ui.EndOK, step.Description, "")
		}
	}
}

func (p *applyProgress) endTask(state string) {
	if p.task != nil {
		p.task.end(state, "")
		p.task = nil
	}
}

func (p *applyProgress) close(state string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.endTask(state)
	if state == ui.EndOK {
		p.flush(len(p.plan))
	}
}

func (r *serverInit) line(state, title, detail string) {
	if r.live != nil {
		r.live.Line(state, title, detail)
		return
	}
	fmt.Fprintf(r.cmd.ErrOrStderr(), "server init: %s: %s %s\n", title, state, detail)
}

// initTask is a live task in a terminal, or plain lines otherwise.
type initTask struct {
	r     *serverInit
	t     *ui.Task
	title string
}

func (r *serverInit) start(key, title string) *initTask {
	if r.live != nil {
		return &initTask{r: r, t: r.live.Start(key, title), title: title}
	}
	fmt.Fprintln(r.cmd.ErrOrStderr(), "server init: "+title)
	return &initTask{r: r, title: title}
}

func (t *initTask) detail(s string) {
	if t.t != nil {
		t.t.Detail(s)
		return
	}
	fmt.Fprintln(t.r.cmd.ErrOrStderr(), "  "+s)
}

func (t *initTask) end(state, detail string) {
	if t.t != nil {
		t.t.End(state, detail)
		return
	}
	if detail != "" {
		fmt.Fprintf(t.r.cmd.ErrOrStderr(), "server init: %s: %s %s\n", t.title, state, detail)
	}
}

// warn is a warning line: styled in the live area, plain on a machine stream.
func (r *serverInit) warn(s string) {
	if r.live != nil {
		r.live.Println(ui.Warn(s))
		return
	}
	fmt.Fprintln(r.cmd.ErrOrStderr(), "server init: warning: "+s)
}

func (r *serverInit) warningLine(s string) string {
	if r.live != nil {
		return ui.Warn(s)
	}
	return "warning: " + s
}

func (r *serverInit) note(s string) {
	if r.live != nil {
		r.live.Println(ui.Note(s))
		return
	}
	fmt.Fprintln(r.cmd.ErrOrStderr(), "server init: "+s)
}

func (r *serverInit) finish(s serverInitSummary) {
	if r.live == nil {
		return
	}
	rows := [][2]string{
		{"server", s.Server},
		{"node", s.Node},
		{"hostname", s.Hostname},
		{"platform", s.Platform},
		{"admin user", s.AdminUser},
		{"route", s.API},
		{"database", "external PostgreSQL"},
	}
	if s.NodeExporter != "" {
		rows = append(rows, [2]string{"node_exporter", s.NodeExporter})
	}
	if s.SecretsOut != "" {
		rows = append(rows, [2]string{"secrets", s.SecretsOut})
	}
	footer := make([]string, 0, len(r.warnings)+3)
	for _, warning := range r.warnings {
		footer = append(footer, ui.Warn(warning))
	}
	footer = append(footer,
		ui.Dim("next:"),
		"  "+ui.Code("eregion commands --json"),
		"  "+ui.Code("eregion ui"),
	)
	r.live.Println("")
	r.live.Println(ui.Card("Server "+s.Server+" is ready", rows, footer))
}

func dryRun(cmd *cobra.Command) bool {
	v, _ := cmd.Flags().GetBool("dry-run")
	return v
}

func flagString(cmd *cobra.Command, name string) string {
	if cmd == nil || (cmd.Flags().Lookup(name) == nil && cmd.InheritedFlags().Lookup(name) == nil) {
		return ""
	}
	value, _ := cmd.Flags().GetString(name)
	return strings.TrimSpace(value)
}

// withResumeHint turns a step failure into a resumable instruction without
// hiding the original fault.
func withResumeHint(err error, server string) error {
	var fault *contract.Fault
	if errors.As(err, &fault) {
		copy := *fault
		hint := "Fix the reported step, then continue with eregion server init --resume --context-name " + server + "."
		if copy.Hint != "" {
			hint = copy.Hint + " " + hint
		}
		copy.Hint = hint
		return &copy
	}
	return err
}
