package cli

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/mbvlabs/eregion/internal/cli/ui"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/doctor"
	"github.com/mbvlabs/eregion/internal/host"
	"github.com/spf13/cobra"
	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

// hostSpecs is `plan`, `apply`, `doctor` and `ssh`: the operator's convergence
// and inspection surface. Every command runs on the node through the saved
// context; no plaintext host secret travels in the request.
func (a *app) hostSpecs() []spec {
	planMeta := operationMeta("Show the host steps an apply would change", "eregion plan --json")
	planMeta.Next = []string{"eregion apply", "eregion doctor"}
	applyMeta := operationMeta("Converge the node to its recorded host intent", "eregion apply --json")
	applyMeta.Mutating, applyMeta.DryRun = true, true
	applyMeta.NeedsRestart = "possible"
	applyMeta.Next = []string{"eregion doctor", "eregion server status"}
	doctorMeta := operationMeta("Check host drift, security, certificates, disk, versions and services", "eregion doctor --json")
	doctorMeta.Next = []string{"eregion plan", "eregion apply"}
	sshMeta := operationMeta("Open a shell or run one command on the node over WireGuard", "eregion ssh --command 'uname -m'")
	sshMeta.WhenToUse = []string{"Use an interactive shell at a terminal, or --command for one remote command over the pinned SSH route; the remote exit status becomes the command's status."}
	sshMeta.Examples = []string{"eregion ssh", "eregion ssh --command 'uname -m'", "eregion ssh --command 'sudo systemctl status eregion' --json"}
	sshMeta.Platforms = []string{"linux", "darwin"}
	sshMeta.Prompts = false
	sshMeta.Next = []string{"eregion server status"}
	return []spec{
		{use: "plan", meta: planMeta, run: a.hostPlan},
		{use: "apply", meta: applyMeta, flags: func(cmd *cobra.Command) {
			cmd.Flags().Bool("dry-run", false, "Show the plan without changing the host")
		}, run: a.hostApply},
		{use: "doctor", meta: doctorMeta, run: a.hostDoctor},
		{use: "ssh [node]", meta: sshMeta, args: cobra.MaximumNArgs(1), flags: func(cmd *cobra.Command) {
			cmd.Flags().String("command", "", "Run one command on the node and exit with its status")
		}, run: a.ssh},
	}
}

func (a *app) hostPlan(cmd *cobra.Command, _ []string) (result, error) {
	client, err := a.nodeClient(cmd)
	if err != nil {
		return result{}, err
	}
	plan, err := client.HostPlan(cmd.Context())
	if err != nil {
		return result{}, err
	}
	return result{data: plan, plain: planText(plan)}, nil
}

func (a *app) hostApply(cmd *cobra.Command, _ []string) (result, error) {
	client, err := a.nodeClient(cmd)
	if err != nil {
		return result{}, err
	}
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	report, plan, err := client.HostApply(cmd.Context(), dryRun, "")
	if err != nil {
		return result{}, err
	}
	if dryRun {
		return result{data: plan, plain: planText(plan)}, nil
	}
	return result{data: report, plain: applyText(report)}, nil
}

func (a *app) hostDoctor(cmd *cobra.Command, _ []string) (result, error) {
	client, err := a.nodeClient(cmd)
	if err != nil {
		return result{}, err
	}
	report, err := client.HostDoctor(cmd.Context(), false)
	if err != nil {
		return result{}, err
	}
	message := doctorText(report)
	failed := doctor.Failed(report.Results)
	if len(failed) == 0 {
		return result{data: report, plain: message}, nil
	}
	if !structuredOutput(cmd) {
		// A terminal shows the whole report before the exit status; JSON keeps
		// it in the failure envelope's details.
		_, _ = fmt.Fprintln(cmd.ErrOrStderr(), message)
	}
	names := make([]string, 0, len(failed))
	for _, finding := range failed {
		name := finding.ID
		if finding.Subject != "" {
			name += " " + finding.Subject
		}
		names = append(names, name)
	}
	fault := contract.NewFault(contract.PreconditionFailed,
		fmt.Sprintf("%d doctor check(s) fail: %s", len(failed), strings.Join(names, ", ")),
		"Fix each reported finding, then run eregion doctor again.")
	fault.Details = map[string]any{"report": report, "summary": report.Summary}
	return result{}, fault
}

// doctorText renders the same findings as doctor.Message with the section
// header and meaning colours the rest of the human output uses.
func doctorText(report doctor.Report) string {
	var out strings.Builder
	out.WriteString(ui.Header("Doctor", 72))
	for _, line := range strings.Split(doctor.Message(report.Results), "\n") {
		if line == "" {
			continue
		}
		status, rest, ok := strings.Cut(line, " ")
		switch strings.ToLower(status) {
		case "pass", "warn", "fail", "skip":
			out.WriteString("\n" + ui.State(strings.ToUpper(status)))
			if ok {
				out.WriteString(" " + rest)
			}
		default:
			out.WriteString("\n" + line)
		}
	}
	return out.String()
}

func planText(plan host.Plan) string {
	var out strings.Builder
	fmt.Fprintf(&out, "%d of %d host steps would change on %s\n", plan.WouldChange, plan.Total, plan.Node)
	for _, entry := range plan.Steps {
		if entry.Complete && entry.Error == "" {
			continue
		}
		status := "change"
		if entry.Error != "" {
			status = "error"
		}
		fmt.Fprintf(&out, "  %-28s %-4s %s\n", entry.ID, entry.Version, status)
		if entry.Error != "" {
			fmt.Fprintf(&out, "    %s\n", entry.Error)
		} else if entry.Detail != "" {
			fmt.Fprintf(&out, "    %s\n", entry.Detail)
		}
		for _, line := range entry.Diff {
			fmt.Fprintf(&out, "    %s\n", line)
		}
	}
	return strings.TrimRight(out.String(), "\n")
}

func applyText(report host.ApplyReport) string {
	var out strings.Builder
	fmt.Fprintf(&out, "applied %d, skipped %d of %d host steps on %s", report.Applied, report.Skipped, report.Total, report.Node)
	if !report.CompletedAt.IsZero() {
		fmt.Fprintf(&out, " (%s)", report.CompletedAt.UTC().Format("2006-01-02T15:04:05Z"))
	}
	for _, entry := range report.Steps {
		if !entry.Executed {
			continue
		}
		fmt.Fprintf(&out, "\n  applied %s", entry.ID)
	}
	return out.String()
}

func structuredOutput(cmd *cobra.Command) bool {
	jsonMode, _ := cmd.Flags().GetBool("json")
	outputMode, _ := cmd.Flags().GetString("output")
	if mode, err := outputModeFromFlags(jsonMode, outputMode); err == nil && mode == "json" {
		return true
	}
	return jsonMode
}

func outputModeFromFlags(jsonMode bool, outputMode string) (string, error) {
	if jsonMode && outputMode != "" && outputMode != "json" {
		return "", contract.NewFault(contract.InvalidOutputMode, "conflicting output modes", "Use --json or --output json; do not combine --json with --output plain.")
	}
	if jsonMode || outputMode == "json" {
		return "json", nil
	}
	return "plain", nil
}

func (a *app) ssh(cmd *cobra.Command, args []string) (result, error) {
	command, _ := cmd.Flags().GetString("command")
	structured := structuredOutput(cmd)
	if command == "" && (structured || !stdoutIsTerminal() || !stdinIsTerminal()) {
		return result{}, contract.NewFault(contract.Usage,
			"eregion ssh needs a terminal or --command",
			"Pass --command to run one command, or use a TTY for a shell.")
	}
	name := ""
	if len(args) == 1 {
		name = args[0]
	}
	client, err := a.nodeClientFor(cmd, name)
	if err != nil {
		return result{}, err
	}
	if err := client.ensureVersion(cmd.Context()); err != nil {
		return result{}, err
	}
	sshClient, err := client.remote.Dial(cmd.Context())
	if err != nil {
		return result{}, contract.NewFault(contract.Unavailable, "could not open SSH to the node", err.Error())
	}
	defer func() { _ = sshClient.Close() }()
	session, err := sshClient.NewSession()
	if err != nil {
		return result{}, contract.NewFault(contract.Unavailable, "the node refused an SSH session", err.Error())
	}
	defer func() { _ = session.Close() }()

	// Like ssh -t, a command from a terminal gets a PTY so sudo can prompt.
	// Piped or structured output runs without one: a PTY merges stderr into
	// stdout and turns newlines into \r\n, which breaks `ssh --command | jq`.
	if !sshUsesPTY(command, structured, stdinIsTerminal(), stdoutIsTerminal()) {
		stderr := &sudoPasswordWatch{w: os.Stderr}
		session.Stdin, session.Stdout, session.Stderr = cmd.InOrStdin(), os.Stdout, stderr
		err := session.Run(command)
		var exitErr *ssh.ExitError
		if errors.As(err, &exitErr) && stderr.seen {
			fault := sshSudoFault(exitErr.ExitStatus())
			return result{streamed: true, streamFault: fault}, fault
		}
		return sshExitResult(err)
	}

	width, height := 80, 24
	if w, h, err := term.GetSize(int(os.Stdout.Fd())); err == nil { //nolint:gosec // G115: file descriptors fit in int.
		width, height = w, h
	}
	modes := ssh.TerminalModes{ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 14400, ssh.TTY_OP_OSPEED: 14400}
	if err := session.RequestPty("xterm-256color", height, width, modes); err != nil {
		return result{}, contract.NewFault(contract.PreconditionFailed, "the node refused a PTY", err.Error())
	}
	fd := int(os.Stdin.Fd()) //nolint:gosec // G115: file descriptors fit in int.
	oldState, err := term.MakeRaw(fd)
	if err != nil {
		return result{}, contract.NewFault(contract.PreconditionFailed, "could not put the terminal in raw mode", "Run eregion ssh from a terminal.")
	}
	defer func() { _ = term.Restore(fd, oldState) }()
	session.Stdin, session.Stdout, session.Stderr = os.Stdin, os.Stdout, os.Stderr
	if command != "" {
		return sshExitResult(session.Run(command))
	}
	if err := session.Shell(); err != nil {
		return result{}, contract.NewFault(contract.Unavailable, "could not start the remote shell", err.Error())
	}
	return sshExitResult(session.Wait())
}

// sshUsesPTY mirrors ssh -t: a command runs on a PTY only when the caller is a
// terminal and structured output was not requested. A shell always needs one,
// which the caller enforces before reaching this decision.
func sshUsesPTY(command string, structured, stdinTTY, stdoutTTY bool) bool {
	return command == "" || (!structured && stdinTTY && stdoutTTY)
}

// sshSudoFault is the diagnostic for a remote sudo that needed a password but
// had no terminal. The runner prints it even though the remote stderr was
// already streamed, so the fault code and fix do not disappear.
func sshSudoFault(exitStatus int) error {
	fault := contract.NewFault(contract.InputRequired,
		"sudo on the node needs a password, and without a terminal there is none to ask for",
		"Run the command from a terminal with stdout not piped so sudo can prompt, or write the output to a file with sudo and read it back with eregion ssh --command 'cat <file>'.")
	fault.Details = map[string]any{"exit_status": exitStatus}
	return fault
}

// sshExitResult turns the remote status into the operator's exit status. The
// remote streams are already written, so the runner must not add an envelope.
func sshExitResult(err error) (result, error) {
	if err == nil {
		return result{streamed: true}, nil
	}
	var exitErr *ssh.ExitError
	if errors.As(err, &exitErr) {
		return result{streamed: true}, &exitStatusError{code: exitErr.ExitStatus()}
	}
	return result{streamed: true}, contract.NewFault(contract.Unavailable, "the remote command ended unexpectedly", err.Error())
}

// sudoPasswordWatch passes the remote stderr through and notes whether sudo
// refused for want of a terminal to read the password from.
type sudoPasswordWatch struct {
	w    io.Writer
	tail []byte
	seen bool
}

// sudoNoTTY are the lines sudo prints when it needs a password but has no
// terminal.
var sudoNoTTY = [][]byte{[]byte("sudo: a terminal is required"), []byte("sudo: a password is required")}

func (s *sudoPasswordWatch) Write(b []byte) (int, error) {
	if !s.seen {
		// Keep a short tail so a message split across writes still matches.
		s.tail = append(s.tail, b...)
		for _, m := range sudoNoTTY {
			if bytes.Contains(s.tail, m) {
				s.seen = true
			}
		}
		if keep := 64; len(s.tail) > keep {
			s.tail = append(s.tail[:0], s.tail[len(s.tail)-keep:]...)
		}
	}
	return s.w.Write(b)
}

func stdinIsTerminal() bool  { return term.IsTerminal(int(os.Stdin.Fd())) }  //nolint:gosec // G115: file descriptors fit in int.
func stdoutIsTerminal() bool { return term.IsTerminal(int(os.Stdout.Fd())) } //nolint:gosec // G115: file descriptors fit in int.
