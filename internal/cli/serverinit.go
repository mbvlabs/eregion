package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync"

	"charm.land/huh/v2"
	"github.com/mbvlabs/eregion/internal/access"
	"github.com/mbvlabs/eregion/internal/cli/ui"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/host"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

// serverSpecs is the operator entry point: eregion server init walks a human
// through first contact in a terminal (preflight, interview, plan, apply,
// handoff), and takes the same answers as flags for agents.
func (a *app) serverSpecs() []spec {
	parent := readMeta("Set up and manage eregion servers",
		[]string{"Set up a fresh Debian 13 or Ubuntu 26.04 server as an eregion control plane."},
		[]string{"eregion server init", "eregion server init --host root@203.0.113.10 --yes --accept-new-host-key --json"})
	parent.Platforms = []string{"linux"}
	meta := parent
	meta.Summary = "Set up a fresh server as the main node (control plane)"
	meta.WhenToUse = []string{"Run it bare in a terminal for the guided setup. Agents pass --host, --yes and a host key decision; every question has a flag."}
	meta.Mutating, meta.DryRun, meta.Prompts, meta.Remote = true, true, true, true
	meta.InputFlags = []string{"host", "identity", "node", "database-url-file", "context-name", "host-key-fingerprint", "accept-new-host-key", "yes"}
	meta.NeedsRestart = "never"
	meta.ErrorCodes = append(meta.ErrorCodes, contract.ValidationFailed, contract.PreconditionFailed, contract.ConfirmationRequired)
	return []spec{{
		use:  "server",
		meta: parent,
		run:  func(cmd *cobra.Command, _ []string) (result, error) { return a.help(cmd), nil },
		children: []spec{{
			use:  "init",
			meta: meta,
			flags: func(cmd *cobra.Command) {
				f := cmd.Flags()
				f.String("host", "", "Server to set up: user@host or user@host:port")
				f.String("identity", "", "SSH private key; default: ssh-agent and ~/.ssh keys")
				f.String("node", "", "Main node name (default main)")
				f.String("database-url-file", "", "External PostgreSQL URL file; otherwise EREGION_DATABASE_URL")
				f.String("context-name", "", "Local context name (default: the node name)")
				f.String("host-key-fingerprint", "", "Expected SHA256 host key fingerprint")
				f.Bool("accept-new-host-key", false, "Trust an unpinned host key without checking it")
				f.Bool("yes", false, "Apply without the confirmation prompt")
				f.Bool("dry-run", false, "Stop after showing the plan")
			},
			run: a.serverInit,
		}},
	}}
}

type serverInitAnswers struct {
	Host, User, Port string
	Identity         string
	Node             string
	DatabaseURL      string
	Context          string
}

type serverInitSummary struct {
	Server      string `json:"server"`
	Node        string `json:"node"`
	Platform    string `json:"platform"`
	Context     string `json:"context"`
	API         string `json:"api"`
	OperationID string `json:"operation_id"`
	DryRun      bool   `json:"dry_run,omitempty"`
}

type serverInit struct {
	a           *app
	cmd         *cobra.Command
	interactive bool
	live        *ui.Live
	answers     serverInitAnswers
	hostKey     string
	hostPub     ssh.PublicKey
}

func (a *app) serverInit(cmd *cobra.Command, _ []string) (res result, err error) {
	jsonMode, _ := cmd.Flags().GetBool("json")
	outputMode, _ := cmd.Flags().GetString("output")
	r := &serverInit{a: a, cmd: cmd}
	r.interactive = !jsonMode && outputMode != "json" && term.IsTerminal(int(os.Stdin.Fd())) && ui.Enabled(os.Stderr) //nolint:gosec // G115: file descriptors fit in int.
	if r.interactive {
		r.live = ui.Start(os.Stderr)
		defer func() {
			ui.Finish(err != nil)
			if errors.Is(err, ui.ErrAborted) {
				err = contract.NewFault(contract.Cancelled, "server init cancelled", "Nothing was changed after the last finished step; run eregion server init again to resume.")
			}
		}()
		r.live.Println("")
		r.live.Println(ui.Header("eregion server init", r.live.Width()))
	}
	summary, err := r.run(cmd.Context())
	if err != nil {
		return result{}, err
	}
	return result{data: summary, streamed: r.interactive}, nil
}

func (r *serverInit) run(ctx context.Context) (serverInitSummary, error) {
	if err := r.collect(); err != nil {
		return serverInitSummary{}, err
	}
	ans := r.answers
	address := net.JoinHostPort(ans.Host, ans.Port)
	if store, err := host.LoadContextStore(""); err == nil {
		if existing, err := store.Get(ans.Context); err == nil && existing.SSH != nil && existing.SSH.WireGuard != nil {
			if !r.interactive {
				return serverInitSummary{}, contract.NewFault(contract.PreconditionFailed, "context "+ans.Context+" already points at a locked-down main node", "Pick another --context-name, or run in a terminal to replace it after a rebuild.")
			}
			ok, err := ui.YesNoDefault("Context "+ans.Context+" already points at a set-up main node. Replace it?", "Only after the server was rebuilt or reinstalled; the old context stops working.", false)
			if err != nil {
				return serverInitSummary{}, err
			}
			if !ok {
				return serverInitSummary{}, contract.NewFault(contract.Cancelled, "kept context "+ans.Context, "Pick another --context-name for a new server.")
			}
		}
	}
	summary := serverInitSummary{Server: ans.User + "@" + address, Node: ans.Node, Context: ans.Context}

	// Preflight: the binary that is uploaded is this one.
	task := r.start("preflight", "Local preflight")
	if runtime.GOOS != "linux" {
		task.end(ui.EndFail, "")
		return summary, contract.NewFault(contract.PreconditionFailed, "server init uploads this binary, which is built for "+runtime.GOOS, "Run eregion server init from a linux build of eregion.")
	}
	task.end(ui.EndOK, "eregion "+runtime.GOOS+"/"+runtime.GOARCH)

	// Host key: pinned in eregion's known_hosts, trusted on first contact.
	knownHosts, err := knownHostsPath()
	if err != nil {
		return summary, err
	}
	if err := r.trustHostKey(ctx, knownHosts, address); err != nil {
		return summary, err
	}
	auth, err := sshAuth(ans.Identity, r.interactive)
	if err != nil {
		return summary, err
	}
	remote, err := host.NewRemote(host.RemoteConfig{Address: address, User: ans.User, KnownHosts: knownHosts, Auth: auth})
	if err != nil {
		return summary, contract.NewFault(contract.ValidationFailed, "the SSH target is not usable", err.Error())
	}

	// First contact: root, a supported release, a matching architecture.
	task = r.start("contact", "Connecting to "+summary.Server)
	platform, err := firstContact(ctx, remote)
	if err != nil {
		task.end(ui.EndFail, "")
		return summary, err
	}
	summary.Platform = platform.String()
	task.end(ui.EndOK, platform.PrettyName+", "+platform.Arch)

	cfg := host.DefaultConfig(host.RoleMain, ans.Node)
	cfg.DatabaseURL = r.answers.DatabaseURL
	// The token is written on the server, read once over SSH, then removed.
	cfg.OperatorTokenOut = host.DefaultLayout().StateDir + "/operator.handoff"
	if err := cfg.Validate(); err != nil {
		return summary, err
	}

	task = r.start("upload", "Uploading eregion and planning")
	session, err := newRemoteSession(ctx, remote, cfg, ans.Host)
	if err != nil {
		task.end(ui.EndFail, "")
		return summary, err
	}
	plan, err := remotePlan(ctx, session)
	if err != nil {
		task.end(ui.EndFail, "")
		return summary, err
	}
	task.end(ui.EndOK, fmt.Sprintf("%d steps", len(plan.Steps)))
	r.showPlan(plan)
	if plan.ApplyBlocked != "" {
		return summary, contract.NewFault(contract.PreconditionFailed, "the server cannot be set up: "+plan.ApplyBlocked, "Fix the reported precondition and run eregion server init again.")
	}
	if dryRun(r.cmd) {
		summary.DryRun = true
		return summary, nil
	}
	if err := r.confirm(summary.Server); err != nil {
		return summary, err
	}

	report, err := r.apply(ctx, session, plan)
	if err != nil {
		return summary, err
	}
	summary.OperationID = report.OperationID

	// The operator token is read once over root SSH, before root login closes.
	task = r.start("token", "Reading the operator token")
	token, err := r.readToken(ctx, remote, cfg)
	if err != nil {
		task.end(ui.EndFail, "")
		return summary, err
	}
	task.end(ui.EndOK, "")
	if err := session.Remove(ctx); err != nil {
		r.note("uploaded bundle cleanup failed: " + err.Error())
	}

	// Access, the vaire way: admin user, WireGuard, public SSH closed.
	id, err := loadOrCreateIdentity(ans.Context)
	if err != nil {
		return summary, err
	}
	_, apiPortText, _ := net.SplitHostPort(cfg.APIListen)
	apiPort, _ := strconv.Atoi(apiPortText)
	route, err := r.secureAccess(ctx, remote, id, apiPort, knownHosts)
	if err != nil {
		return summary, err
	}

	task = r.start("handoff", "Saving context "+ans.Context)
	apiURL := "http://" + cfg.APIListen
	if err := r.saveContext(token, apiURL, route); err != nil {
		task.end(ui.EndFail, "")
		return summary, err
	}
	client, err := r.a.contextClient(r.cmd)
	if err == nil {
		err = client.Ready(ctx)
	}
	if err != nil {
		task.end(ui.EndFail, "")
		return summary, err
	}
	task.end(ui.EndOK, "API answers through SSH over WireGuard")
	summary.API = apiURL + " via " + access.AdminUser + "@" + access.NodeAddress + " over WireGuard"
	if r.interactive {
		h := &secretsHandoff{node: ans.Node, groups: []handoffGroup{
			{"Main node " + ans.Node, []handoffField{
				{"Public address", ans.Host},
				{"Platform", summary.Platform},
				{"SSH host key", r.hostKey},
				{"WireGuard endpoint", route.WireGuard.Endpoint},
				{"WireGuard address", route.WireGuard.NodeAddress},
				{"WireGuard public key", route.WireGuard.NodePublicKey},
			}},
			{"Operator", []handoffField{
				{"Admin user", access.AdminUser},
				{"Sudo password", id.sudoPassword},
				{"WireGuard address", route.WireGuard.Address},
				{"WireGuard private key", id.wg.Encode()},
				{"SSH private key", "\n" + strings.TrimSpace(id.sshKeyPEM)},
			}},
			{"Control plane API", []handoffField{{"URL", apiURL + " (on the server)"}, {"Operator token", token}, {"Context", ans.Context}}},
			{"Control-plane database", []handoffField{{"URL", r.answers.DatabaseURL}}},
			{"Break-glass", []handoffField{{"Public SSH closed", "From the provider console, as root: eregion node-agent access revert"}}},
		}}
		for _, g := range h.groups {
			for _, f := range g.fields {
				r.a.secrets.Add(f.value)
			}
		}
		if err := h.show(os.Stderr); err != nil {
			return summary, err
		}
	}
	r.finish(summary)
	return summary, nil
}

// collect reads flags, then asks for what is missing in a terminal.
func (r *serverInit) collect() error {
	cmd := r.cmd
	get := func(name string) string { v, _ := cmd.Flags().GetString(name); return strings.TrimSpace(v) }
	ans := serverInitAnswers{Identity: get("identity"), Node: get("node"), Context: get("context-name")}
	target := get("host")
	if target == "" {
		if !r.interactive {
			return contract.NewFault(contract.InputRequired, "--host is required without a terminal", "Pass --host root@203.0.113.10.")
		}
		v, err := ui.Input("Server to set up", "user@host or user@host:port, as root", "")
		if err != nil {
			return err
		}
		target = v
	}
	user, hostPart, port, err := parseTarget(target)
	if err != nil {
		return err
	}
	ans.User, ans.Host, ans.Port = user, hostPart, port

	if path := get("database-url-file"); path != "" {
		raw, err := host.ReadPrivateInput(path)
		if err != nil {
			return err
		}
		ans.DatabaseURL = strings.TrimSpace(string(raw))
	} else if env := os.Getenv("EREGION_DATABASE_URL"); env != "" {
		ans.DatabaseURL = env
	}

	if r.interactive {
		if err := r.interview(&ans); err != nil {
			return err
		}
	}
	if ans.Node == "" {
		ans.Node = "main"
	}
	if ans.Context == "" {
		ans.Context = ans.Node
	}
	if ans.DatabaseURL == "" {
		return contract.NewFault(contract.InputRequired, "the control plane needs an external PostgreSQL URL", "Pass --database-url-file or set EREGION_DATABASE_URL.")
	}
	r.a.secrets.Add(ans.DatabaseURL)
	r.answers = ans
	return nil
}

// interview asks the setup questions the flags did not answer, each with
// its default filled in.
func (r *serverInit) interview(ans *serverInitAnswers) error {
	if ans.Node == "" {
		ans.Node = "main"
	}
	var fields []huh.Field
	if !r.cmd.Flags().Changed("node") {
		fields = append(fields, huh.NewInput().Title("Main node name").Description("This server becomes the main node: control plane, API, workers and builds. Extra nodes are added later with eregion platform nodes enroll.").Value(&ans.Node).Validate(nonEmpty))
	}
	if !r.cmd.Flags().Changed("context-name") {
		ans.Context = ""
	}
	groups := []*huh.Group{}
	if len(fields) > 0 {
		groups = append(groups, huh.NewGroup(fields...))
	}
	askURL := ans.DatabaseURL == ""
	if askURL {
		groups = append(groups, huh.NewGroup(
			huh.NewInput().Title("External PostgreSQL URL").
				Description("postgres://user:password@host:5432/db").
				EchoMode(huh.EchoModePassword).Value(&ans.DatabaseURL).Validate(nonEmpty),
		))
	}
	if len(groups) > 0 {
		if err := ui.RunForm(groups...); err != nil {
			return err
		}
	}
	// huh clears a finished form; keep each answer in the scrollback.
	if !r.cmd.Flags().Changed("node") {
		r.live.Println(ui.Answered("Main node name", ans.Node))
	}
	if askURL {
		r.live.Println(ui.Answered("External PostgreSQL URL", ui.SecretAnswer))
	}
	if ans.Context == "" {
		ans.Context = ans.Node
	}
	r.live.Println(ui.Note("main node " + ans.Node + " · external PostgreSQL · context " + ans.Context))
	return nil
}

func nonEmpty(v string) error {
	if strings.TrimSpace(v) == "" {
		return errors.New("required")
	}
	return nil
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
	if _, convErr := strconv.Atoi(port); convErr != nil {
		return "", "", "", contract.NewFault(contract.ValidationFailed, "the SSH port must be a number", "")
	}
	return user, hostPart, port, nil
}

// trustHostKey pins the server's key on first contact: a matching
// --host-key-fingerprint, --accept-new-host-key, or a yes in a terminal.
func (r *serverInit) trustHostKey(ctx context.Context, knownHosts, address string) error {
	approved := false
	if hostKeyPinned(knownHosts, address) {
		key, err := probeHostKey(ctx, address)
		if err != nil {
			return contract.NewFault(contract.Unavailable, "the server does not answer on SSH", err.Error())
		}
		if pinnedKeyMatches(knownHosts, address, key) {
			r.hostKey = key.Type() + " " + ssh.FingerprintSHA256(key)
			r.hostPub = key
			return nil
		}
		// A rebuilt server has new host keys. Replacing the pin is a decision.
		fp := ssh.FingerprintSHA256(key)
		want, _ := r.cmd.Flags().GetString("host-key-fingerprint")
		accept, _ := r.cmd.Flags().GetBool("accept-new-host-key")
		switch {
		case want != "" && (want == fp || "SHA256:"+want == fp):
		case accept:
		case r.interactive:
			r.note(ui.Warn("the host key of " + address + " changed to " + key.Type() + " " + fp))
			ok, err := ui.YesNoDefault("Replace the pinned host key?", "Expected after a rebuild or reinstall. Otherwise someone may be intercepting the connection.", false)
			if err != nil {
				return err
			}
			if !ok {
				return contract.NewFault(contract.Cancelled, "the changed host key was not trusted", "Check it on the provider console with ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub.")
			}
		default:
			return contract.NewFault(contract.PreconditionFailed, "the host key of "+address+" changed to "+fp, "After a rebuild pass --host-key-fingerprint "+fp+" or --accept-new-host-key.")
		}
		for _, pinned := range []string{address, net.JoinHostPort(access.NodeAddress, "22")} {
			if err := unpinHostKey(knownHosts, pinned); err != nil {
				return err
			}
		}
		approved = true
	}
	task := r.start("hostkey", "Reading the host key of "+address)
	key, err := probeHostKey(ctx, address)
	if err != nil {
		task.end(ui.EndFail, "")
		return contract.NewFault(contract.Unavailable, "the server does not answer on SSH", err.Error())
	}
	fp := ssh.FingerprintSHA256(key)
	r.hostKey = key.Type() + " " + fp
	r.hostPub = key
	task.end(ui.EndOK, key.Type()+" "+fp)
	want, _ := r.cmd.Flags().GetString("host-key-fingerprint")
	accept, _ := r.cmd.Flags().GetBool("accept-new-host-key")
	switch {
	case approved:
	case want != "":
		if want != fp && "SHA256:"+want != fp {
			return contract.NewFault(contract.PreconditionFailed, "the host key "+fp+" does not match --host-key-fingerprint", "Check it on the provider console with ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub.")
		}
	case accept:
	case r.interactive:
		ok, err := ui.YesNoDefault("Trust host key "+fp+"?", "Compare with ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub on the provider console.", true)
		if err != nil {
			return err
		}
		if !ok {
			return contract.NewFault(contract.Cancelled, "the host key was not trusted", "Pass --host-key-fingerprint "+fp+" once it is checked.")
		}
	default:
		return contract.NewFault(contract.InputRequired, "first contact needs the host key "+fp+" to be confirmed", "Pass --host-key-fingerprint "+fp+" or --accept-new-host-key.")
	}
	return pinHostKey(knownHosts, address, key)
}

func firstContact(ctx context.Context, remote *host.Remote) (host.Platform, error) {
	out, err := remote.Output(ctx, `. /etc/os-release; echo "$ID"; echo "$VERSION_ID"; echo "$PRETTY_NAME"; uname -m; id -u`)
	if err != nil {
		return host.Platform{}, contract.NewFault(contract.Unavailable, "could not log in over SSH", err.Error())
	}
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if len(lines) < 5 {
		return host.Platform{}, contract.NewFault(contract.Unavailable, "the server returned unexpected platform output", "")
	}
	arch := map[string]string{"x86_64": "amd64", "aarch64": "arm64"}[strings.TrimSpace(lines[3])]
	platform := host.Platform{ID: lines[0], VersionID: lines[1], PrettyName: lines[2], Arch: arch}
	if strings.TrimSpace(lines[4]) != "0" {
		return platform, contract.NewFault(contract.PreconditionFailed, "server init needs root on the server", "Use root@host; the setup installs packages and system services.")
	}
	if !platform.Supported() {
		return platform, contract.NewFault(contract.PreconditionFailed, "the server runs "+platform.PrettyName+", which eregion does not support", "Use Debian 13 or Ubuntu 26.04.")
	}
	if arch != runtime.GOARCH {
		return platform, contract.NewFault(contract.PreconditionFailed, "the server is "+lines[3]+" but this eregion is built for "+runtime.GOARCH, "Run a "+lines[3]+" build of eregion.")
	}
	return platform, nil
}

func (r *serverInit) showPlan(plan host.Plan) {
	var b strings.Builder
	for _, step := range plan.Steps {
		mark := ui.Dim("○")
		if step.Complete {
			mark = ui.Dim(ui.SymOK)
		}
		fmt.Fprintf(&b, "%s %s\n", mark, step.Description)
	}
	for _, risk := range plan.AccessRisks {
		b.WriteString(ui.Warn(risk) + "\n")
	}
	body := strings.TrimRight(b.String(), "\n")
	if r.live == nil {
		fmt.Fprintln(r.cmd.ErrOrStderr(), "plan:\n"+body)
		return
	}
	r.live.Println(ui.PanelFit("Plan for main node "+plan.Node+" on "+plan.Platform.String(), body, r.live.Width()))
}

func (r *serverInit) confirm(server string) error {
	if yes, _ := r.cmd.Flags().GetBool("yes"); yes {
		return nil
	}
	if !r.interactive {
		return contract.NewFault(contract.ConfirmationRequired, "server init needs confirmation", "Review the plan with --dry-run, then pass --yes.")
	}
	ok, err := ui.YesNoDefault("Set up "+server+" as the main node now?", "Installs packages, services and the eregion control plane.", true)
	if err != nil {
		return err
	}
	if !ok {
		return contract.NewFault(contract.Cancelled, "server init was not confirmed", "Nothing was applied.")
	}
	return nil
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

func (r *serverInit) readToken(ctx context.Context, remote *host.Remote, cfg host.Config) (string, error) {
	token, err := consumeRemoteToken(ctx, remote, cfg.OperatorTokenOut)
	if err != nil {
		return "", err
	}
	r.a.secrets.Add(token)
	return token, nil
}

func (r *serverInit) saveContext(token, apiURL string, route *host.ContextSSH) error {
	store, err := host.LoadContextStore("")
	if err != nil {
		return err
	}
	if _, err := store.Login(r.answers.Context, apiURL, token, host.KnownScopes()); err != nil {
		return err
	}
	return store.SetSSH(r.answers.Context, route)
}

func (r *serverInit) finish(s serverInitSummary) {
	if r.live == nil {
		return
	}
	r.live.Println("")
	r.live.Println(ui.Card("Main node "+s.Node+" is ready", [][2]string{
		{"server", s.Server},
		{"platform", s.Platform},
		{"database", "external PostgreSQL"},
		{"API", s.API},
		{"context", s.Context + " (current)"},
	}, []string{
		ui.Dim("next:"),
		"  " + ui.Code("eregion platform fleet"),
		"  " + ui.Code("eregion ui"),
	}))
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

func (r *serverInit) note(s string) {
	if r.live != nil {
		r.live.Println(ui.Note(s))
		return
	}
	fmt.Fprintln(r.cmd.ErrOrStderr(), "server init: "+s)
}

func dryRun(cmd *cobra.Command) bool {
	v, _ := cmd.Flags().GetBool("dry-run")
	return v
}
