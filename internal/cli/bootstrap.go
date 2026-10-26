package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/host"
	"github.com/spf13/cobra"
)

// bootstrapSpecs is attached by newApp. It is the local control-plane
// bootstrap surface plus a pinned-SSH first-contact command. No placeholder
// actions are registered: every child below runs real Check/Apply steps or a
// real remote command.
func (a *app) bootstrapSpecs() []spec {
	parentMeta := readMeta("Bootstrap a supported Debian/Ubuntu host",
		[]string{"Plan and converge one fresh host with a node-local journal that never depends on PostgreSQL. Access-changing apply returns a rollback window; confirm it from a fresh connection."},
		[]string{"eregion bootstrap plan --role main --node main --database-url-file /root/eregion-db.url --json", "eregion bootstrap remote --ssh-address root@203.0.113.10:22 --ssh-identity /root/eregion-bootstrap --ssh-known-hosts /root/known_hosts --role main --database-url-file /root/eregion-db.url --apply --json"})
	parentMeta.Platforms = []string{"linux"}
	parentMeta.Environment = []string{"EREGION_DATABASE_URL"}
	parentMeta.ErrorCodes = append(parentMeta.ErrorCodes, contract.ValidationFailed, contract.PreconditionFailed, contract.ConfirmationRequired, contract.Forbidden, contract.NotFound)
	parentMeta.Prompts = false

	children := []spec{
		a.bootstrapActionSpec("plan", false, parentMeta, "Preview every role-aware step without writing state or starting a service"),
		a.bootstrapActionSpec("diff", false, parentMeta, "Show the desired-vs-observed diff for the role"),
		a.bootstrapActionSpec("status", false, parentMeta, "Read the node-local bootstrap state and current drift"),
		a.bootstrapActionSpec("apply", true, parentMeta, "Converge the host after an exact plan-bound confirmation"),
		a.bootstrapConfirmSpec(parentMeta),
		a.bootstrapRemoteSpec(parentMeta),
	}
	return []spec{{
		use:      "bootstrap",
		meta:     parentMeta,
		run:      func(cmd *cobra.Command, _ []string) (result, error) { return a.help(cmd), nil },
		children: children,
	}}
}

func (a *app) bootstrapActionSpec(action string, mutating bool, parent CommandMeta, summary string) spec {
	meta := parent
	meta.Summary = "Bootstrap " + action + ": " + summary
	meta.Mutating = mutating
	if mutating {
		meta.DryRun, meta.Confirmation, meta.NeedsRestart = true, "plan-bound", "required"
		meta.Examples = []string{"eregion bootstrap apply --role main --node main --database-url-file /root/eregion-db.url --apply --confirm TOKEN --json"}
	}
	return spec{use: action, meta: meta, flags: bootstrapFlags, run: func(cmd *cobra.Command, _ []string) (result, error) {
		if action == "apply" {
			return a.bootstrapLocalApply(cmd)
		}
		cfg, err := a.bootstrapConfig(cmd)
		if err != nil {
			return result{}, err
		}
		if action == "status" {
			runner, err := host.NewStatusRunner(cfg, a.secrets)
			if err != nil {
				return result{}, err
			}
			report, err := runner.Status(cmd.Context())
			return hostResult(report), err
		}
		runner, err := host.NewRunner(cfg, a.secrets)
		if err != nil {
			return result{}, err
		}
		plan, err := runner.Plan(cmd.Context())
		return hostResult(plan), err
	}}
}

func (a *app) bootstrapLocalApply(cmd *cobra.Command) (result, error) {
	cfg, err := a.bootstrapConfig(cmd)
	if err != nil {
		return result{}, err
	}
	runner, err := host.NewRunner(cfg, a.secrets)
	if err != nil {
		return result{}, err
	}
	dry, _ := cmd.Flags().GetBool("dry-run")
	if dry {
		plan, err := runner.Plan(cmd.Context())
		return hostResult(plan), err
	}
	apply, _ := cmd.Flags().GetBool("apply")
	if !apply {
		return result{}, contract.NewFault(contract.ConfirmationRequired, "host mutation requires --apply", "Run bootstrap plan (or apply --dry-run), review the diff, then supply --apply and --confirm with its plan token.")
	}
	confirm, _ := cmd.Flags().GetString("confirm")
	if confirm == "" {
		return result{}, contract.NewFault(contract.ConfirmationRequired, "--confirm is required for bootstrap apply", "Run bootstrap plan, review the plan and diff, then pass its confirmation token.")
	}
	runner.Reporter = bootstrapReporter(a, cmd)
	report, err := runner.Apply(cmd.Context(), confirm, false)
	return hostResult(report), err
}

func (a *app) bootstrapConfirmSpec(parent CommandMeta) spec {
	meta := parent
	meta.Summary = "Confirm an access-changing bootstrap before its rollback deadline"
	meta.Mutating = true
	meta.DryRunReason = "Confirmation is the commit of an already applied change; inspect bootstrap status before confirming."
	meta.Examples = []string{"eregion bootstrap confirm op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa --json", "eregion bootstrap confirm op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa --ssh-address root@host:22 --ssh-identity /root/key --ssh-known-hosts /root/known_hosts --json"}
	return spec{
		use:  "confirm <operation_id>",
		meta: meta,
		args: oneID,
		flags: func(cmd *cobra.Command) {
			bootstrapFlags(cmd)
			remoteFlags(cmd)
		},
		run: func(cmd *cobra.Command, args []string) (result, error) {
			ctx := cmd.Context()
			if address, _ := cmd.Flags().GetString("ssh-address"); address != "" {
				return a.bootstrapRemoteConfirm(cmd, args[0])
			}
			cfg, err := a.bootstrapConfig(cmd)
			if err != nil {
				return result{}, err
			}
			runner, err := host.NewConfirmRunner(cfg, a.secrets)
			if err != nil {
				return result{}, err
			}
			report, err := runner.Confirm(ctx, args[0])
			return hostResult(report), err
		},
	}
}

func (a *app) bootstrapRemoteSpec(parent CommandMeta) spec {
	meta := parent
	meta.Summary = "Upload a verified binary and run resumable bootstrap over pinned SSH"
	meta.Mutating = true
	meta.DryRun = true
	meta.NeedsRestart = "required"
	meta.Examples = []string{
		"eregion bootstrap remote --ssh-address root@203.0.113.10:22 --ssh-identity /root/eregion-bootstrap --ssh-known-hosts /root/known_hosts --role main --node main --database-url-file /root/eregion-db.url --dry-run --json",
		"eregion bootstrap remote --ssh-address root@203.0.113.10:22 --ssh-identity /root/eregion-bootstrap --ssh-known-hosts /root/known_hosts --role main --node main --database-url-file /root/eregion-db.url --apply --json",
	}
	return spec{
		use:   "remote",
		meta:  meta,
		flags: func(cmd *cobra.Command) { bootstrapFlags(cmd); remoteFlags(cmd) },
		run:   a.bootstrapRemote,
	}
}

func remoteFlags(cmd *cobra.Command) {
	f := cmd.Flags()
	f.String("ssh-address", "", "Pinned SSH target host:port; empty means operate locally")
	f.String("ssh-user", "root", "SSH account used for first contact")
	f.String("ssh-identity", "", "Owner-only unencrypted SSH private key")
	f.String("ssh-known-hosts", "", "known_hosts file containing the pinned host key")
}

func bootstrapFlags(cmd *cobra.Command) {
	bootstrapInputFlags(cmd)
	f := cmd.Flags()
	f.Bool("dry-run", false, "Read-only plan/diff; never applies")
	f.Bool("apply", false, "Explicitly authorize host mutation (apply and remote commands)")
	f.String("confirm", "", "Exact unexpired plan-bound confirmation token")
}

// bootstrapInputFlags is the noninteractive/wizard parity surface. It is kept
// separate from the mutation switches so the guided flow cannot accidentally
// inherit a --dry-run/--confirm default.
func bootstrapInputFlags(cmd *cobra.Command) {
	f := cmd.Flags()
	f.String("role", "main", "Placement: main, application, builder, or telemetry")
	f.String("node", "local", "Node name; local and main are accepted names for the in-process local identity")
	f.String("database-url-file", "", "Owner-only external PostgreSQL URL file; otherwise EREGION_DATABASE_URL")
	f.String("binary", "", "eregion binary to install on the host; defaults to this executable")
	f.String("api-listen", "127.0.0.1:8080", "API listener; non-loopback apply is access-changing")
	f.String("api-tls-cert", "", "TLS certificate source")
	f.String("api-tls-key", "", "TLS private key source")
	f.String("hostname", "", "Public hostname of the main node; Caddy serves it with an ACME certificate")
	f.String("node-authorized-keys-file", "", "SSH public keys source; apply is access-changing")
	f.Int("worker-concurrency", 2, "Worker concurrency (1–16)")
	f.Int("commit-confirm-seconds", int(host.DefaultCommitConfirmWindow/time.Second), "Rollback window for access-changing apply (30–900)")
}

func (a *app) bootstrapConfig(cmd *cobra.Command) (host.Config, error) {
	f := cmd.Flags()
	get := func(name string) string { v, _ := f.GetString(name); return v }
	cfg := host.DefaultConfig(host.Role(get("role")), get("node"))
	cfg.DatabaseURL = os.Getenv("EREGION_DATABASE_URL")
	if path := get("database-url-file"); path != "" {
		raw, err := host.ReadPrivateInput(path)
		if err != nil {
			return cfg, err
		}
		cfg.DatabaseURL = strings.TrimSpace(string(raw))
	}
	a.secrets.Add(cfg.DatabaseURL)
	cfg.BinaryPath = get("binary")
	cfg.APIListen, cfg.APITLSCert, cfg.APITLSKey = get("api-listen"), get("api-tls-cert"), get("api-tls-key")
	cfg.NodeAuthorizedKeysFile = get("node-authorized-keys-file")
	cfg.Hostname = strings.TrimSpace(get("hostname"))
	cfg.WorkerConcurrency, _ = f.GetInt("worker-concurrency")
	cfg.CommitConfirmSeconds, _ = f.GetInt("commit-confirm-seconds")
	return cfg, nil
}

func bootstrapReporter(a *app, cmd *cobra.Command) engine.Reporter {
	return func(event engine.Event) {
		message := a.secrets.Redact(event.Message)
		if strings.TrimSpace(message) == "" {
			message = event.Step
		}
		fmt.Fprintf(cmd.ErrOrStderr(), "bootstrap %s: %s\n", event.Step, message)
	}
}

// bootstrapRemote is the resumable first-contact path. It uploads the exact
// local binary, runs the node-local plan and apply over separate SSH
// connections, and streams step progress from the remote stderr.
func (a *app) bootstrapRemote(cmd *cobra.Command, _ []string) (result, error) {
	cfg, err := a.bootstrapConfig(cmd)
	if err != nil {
		return result{}, err
	}
	remote, address, err := a.remoteClient(cmd)
	if err != nil {
		return result{}, err
	}
	dry, _ := cmd.Flags().GetBool("dry-run")
	apply, _ := cmd.Flags().GetBool("apply")
	if !dry && !apply {
		return result{}, contract.NewFault(contract.ConfirmationRequired, "remote bootstrap requires --dry-run or --apply", "Run with --dry-run to review the remote plan, then with --apply to converge and receive the operator credential.")
	}
	session, err := newRemoteSession(cmd.Context(), remote, cfg, address)
	if err != nil {
		return result{}, err
	}
	defer session.Close()
	plan, err := remotePlan(cmd.Context(), session)
	if err != nil {
		return result{}, err
	}
	if dry {
		return hostResult(plan), nil
	}
	report, remoteErr := remoteApply(cmd.Context(), session, plan.Confirmation.Token, cmd.ErrOrStderr())
	if remoteErr != nil {
		var fault *contract.Fault
		if errors.As(remoteErr, &fault) && fault.Code == contract.ConfirmationRequired {
			if fault.Hint != "" {
				fault.Hint += " "
			}
			fault.Hint += "Add the same --ssh-address, --ssh-identity, and --ssh-known-hosts flags to confirm from a fresh connection."
		}
		return result{}, remoteErr
	}
	if err := session.Remove(cmd.Context()); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "bootstrap remote: uploaded bundle cleanup failed: %v\n", err)
	}
	return hostResult(report), nil
}

func (a *app) remoteClient(cmd *cobra.Command) (*host.Remote, string, error) {
	address, _ := cmd.Flags().GetString("ssh-address")
	user, _ := cmd.Flags().GetString("ssh-user")
	identity, _ := cmd.Flags().GetString("ssh-identity")
	knownHosts, _ := cmd.Flags().GetString("ssh-known-hosts")
	full := address
	if !strings.Contains(address, "@") {
		full = user + "@" + address
	}
	hostPart, port, err := net.SplitHostPort(strings.SplitN(full, "@", 2)[1])
	if err != nil || hostPart == "" || port == "" {
		return nil, "", contract.NewFault(contract.ValidationFailed, "the SSH address must be user@host:port or host:port", "Use the form root@203.0.113.10:22.")
	}
	if identity == "" || knownHosts == "" {
		return nil, "", contract.NewFault(contract.InputRequired, "the SSH identity and known_hosts files are required", "Pass --ssh-identity and --ssh-known-hosts. A host without a pinned key is refused.")
	}
	remote, err := host.NewRemote(host.RemoteConfig{
		Address: strings.SplitN(full, "@", 2)[1], User: user,
		IdentityFile: identity, KnownHosts: knownHosts,
	})
	if err != nil {
		return nil, "", contract.NewFault(contract.ValidationFailed, "the remote SSH target is not usable", err.Error())
	}
	return remote, hostPart, nil
}

// remoteBundleDir derives a stable per-host/per-intent directory. A random or
// timestamped directory would change the uploaded --binary path on every
// invocation, and the remote ConfigHash includes that path: the plan token and
// the journal generation would never match, so a re-run could not resume.
func remoteBundleDir(cfg host.Config, address string) (string, error) {
	hash := sha256.New()
	hash.Write([]byte("eregion/remote-bundle/v1\x00" + address + "\x00" + cfg.Node + "\x00" + string(cfg.Role) + "\x00"))
	paths := []string{cfg.BinaryPath}
	if cfg.BinaryPath == "" {
		if effective, err := cfg.EffectiveBinaryPath(); err == nil {
			paths = append(paths, effective)
		}
	}
	for _, path := range paths {
		if path == "" {
			continue
		}
		digest, err := fileDigest(path)
		if err != nil {
			return "", err
		}
		hash.Write([]byte(digest))
		hash.Write([]byte{0})
	}
	name := "bundle-" + hex.EncodeToString(hash.Sum(nil))[:24]
	return filepath.Join(host.DefaultLayout().StateDir, "remote", name), nil
}

func fileDigest(path string) (string, error) {
	file, err := os.Open(path) //nolint:gosec // G304: operator-provided bootstrap input.
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

// remoteSession keeps one uploaded bundle for a plan/apply pair. The remote
// configuration hash includes the uploaded binary path, so plan, apply, and a
// later resume must use the same directory.
type remoteSession struct {
	remote *host.Remote
	dir    string
	args   []string
}

func newRemoteSession(ctx context.Context, remote *host.Remote, cfg host.Config, address string) (*remoteSession, error) {
	dir, err := remoteBundleDir(cfg, address)
	if err != nil {
		return nil, contract.NewFault(contract.ValidationFailed, "cannot derive the remote bootstrap bundle", err.Error())
	}
	files, args, err := remoteBundle(cfg, dir)
	if err != nil {
		return nil, err
	}
	if err := remote.Upload(ctx, dir, files); err != nil {
		return nil, contract.NewFault(contract.Unavailable, "could not upload the bootstrap bundle", err.Error())
	}
	return &remoteSession{remote: remote, dir: dir, args: args}, nil
}

// Close is intentionally a no-op. A failed or interrupted apply must keep the
// stable bundle so the next invocation resumes the same generation, and a
// pending commit-confirm still needs it. Success removes it explicitly with
// Remove.
func (s *remoteSession) Close() {}

// Remove is the explicit cleanup after a fully successful remote action.
func (s *remoteSession) Remove(ctx context.Context) error {
	return s.remote.Remove(ctx, s.dir)
}

func (s *remoteSession) run(ctx context.Context, action string, extra []string, stderr io.Writer) ([]byte, error) {
	command := remoteCommand(s.dir, action, s.args, extra)
	var stdout bytes.Buffer
	err := s.remote.Run(ctx, command, nil, &stdout, stderr)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// A nonzero remote exit (for example confirmation_required) still writes
		// a structured envelope. Only a transport failure with no output is
		// classified here; the envelope keeps its code and details.
		if stdout.Len() == 0 {
			return nil, contract.NewFault(contract.Unavailable, "the remote bootstrap command failed", err.Error())
		}
	}
	return stdout.Bytes(), nil
}

func remotePlan(ctx context.Context, session *remoteSession) (host.Plan, error) {
	raw, err := session.run(ctx, "plan", nil, io.Discard)
	if err != nil {
		return host.Plan{}, err
	}
	return decodeRemoteEnvelope[host.Plan](raw, "plan")
}

func remoteApply(ctx context.Context, session *remoteSession, token string, stderr io.Writer) (host.ApplyReport, error) {
	raw, err := session.run(ctx, "apply", []string{"--apply", "--confirm", token}, stderr)
	if err != nil {
		return host.ApplyReport{}, err
	}
	return decodeRemoteEnvelope[host.ApplyReport](raw, "apply")
}

func remoteBundle(cfg host.Config, dir string) ([]host.RemoteFile, []string, error) {
	var files []host.RemoteFile
	binary, err := cfg.EffectiveBinaryPath()
	if err != nil {
		return nil, nil, contract.NewFault(contract.ValidationFailed, "cannot locate the eregion binary", "Supply --binary explicitly.")
	}
	raw, err := os.ReadFile(binary) //nolint:gosec // G304: the running/operator-selected binary.
	if err != nil {
		return nil, nil, err
	}
	files = append(files, host.RemoteFile{Name: "eregion", Data: raw, Mode: 0o755})
	args := []string{"--role", string(cfg.Role), "--node", cfg.Node}
	if cfg.BinaryPath == "" {
		cfg.BinaryPath = filepath.Join(dir, "eregion")
		args = append(args, "--binary", cfg.BinaryPath)
	} else {
		rawBinary, err := os.ReadFile(cfg.BinaryPath) //nolint:gosec // G304: operator-provided binary.
		if err != nil {
			return nil, nil, err
		}
		if contentEqual(rawBinary, raw) {
			args = append(args, "--binary", filepath.Join(dir, "eregion"))
		} else {
			files = append(files, host.RemoteFile{Name: "eregion-source", Data: rawBinary, Mode: 0o755})
			args = append(args, "--binary", filepath.Join(dir, "eregion-source"))
		}
	}
	if cfg.DatabaseURL != "" {
		files = append(files, host.RemoteFile{Name: "database-url", Data: []byte(cfg.DatabaseURL + "\n"), Mode: 0o600})
		args = append(args, "--database-url-file", filepath.Join(dir, "database-url"))
	}
	uploadPair := func(name, source string) error {
		raw, err := os.ReadFile(source) //nolint:gosec // G304: operator-provided bootstrap input.
		if err != nil {
			return err
		}
		files = append(files, host.RemoteFile{Name: name, Data: raw, Mode: 0o600})
		args = append(args, "--"+name, filepath.Join(dir, name))
		return nil
	}
	if cfg.APITLSCert != "" {
		if err := uploadPair("api-tls-cert", cfg.APITLSCert); err != nil {
			return nil, nil, err
		}
		if err := uploadPair("api-tls-key", cfg.APITLSKey); err != nil {
			return nil, nil, err
		}
	}
	if cfg.NodeAuthorizedKeysFile != "" {
		if err := uploadPair("node-authorized-keys-file", cfg.NodeAuthorizedKeysFile); err != nil {
			return nil, nil, err
		}
	}
	args = append(args, "--api-listen", cfg.APIListen, "--worker-concurrency", fmt.Sprint(cfg.WorkerConcurrency))
	args = append(args, "--commit-confirm-seconds", fmt.Sprint(cfg.CommitConfirmSeconds))
	if cfg.Hostname != "" {
		args = append(args, "--hostname", cfg.Hostname)
	}
	return files, args, nil
}

func remoteCommand(dir, action string, args []string, extra []string) string {
	parts := []string{shellPath(filepath.Join(dir, "eregion")), "bootstrap", action}
	for _, value := range args {
		parts = append(parts, shellPath(value))
	}
	parts = append(parts, "--json")
	parts = append(parts, extra...)
	return strings.Join(parts, " ")
}

func shellPath(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

func contentEqual(left, right []byte) bool {
	return bytes.Equal(left, right)
}

func decodeRemoteEnvelope[T any](raw []byte, action string) (T, error) {
	var zero T
	var envelope struct {
		OK    bool            `json:"ok"`
		Data  json.RawMessage `json:"data"`
		Error *contract.Fault `json:"error"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return zero, contract.NewFault(contract.Unavailable, "the remote bootstrap returned unreadable output", "Check that the uploaded binary is the same version; the remote connection is not a second implementation.")
	}
	if !envelope.OK {
		if envelope.Error == nil {
			return zero, contract.NewFault(contract.Unavailable, "the remote bootstrap failed without a structured error", "Inspect the remote journal and run bootstrap status again.")
		}
		fault := *envelope.Error
		if fault.Hint == "" {
			fault.Hint = "Inspect the remote bootstrap journal; completed steps are resumable."
		}
		return zero, &fault
	}
	var value T
	if err := json.Unmarshal(envelope.Data, &value); err != nil {
		return zero, contract.NewFault(contract.Unavailable, "the remote bootstrap returned unexpected data", "Check binary compatibility; do not assume the host was unchanged.")
	}
	return value, nil
}

func (a *app) bootstrapRemoteConfirm(cmd *cobra.Command, id string) (result, error) {
	remote, address, err := a.remoteClient(cmd)
	if err != nil {
		return result{}, err
	}
	cfg, err := a.bootstrapConfig(cmd)
	if err != nil {
		return result{}, err
	}
	session, err := newRemoteSession(cmd.Context(), remote, cfg, address)
	if err != nil {
		return result{}, err
	}
	defer session.Close()
	raw, err := session.run(cmd.Context(), "confirm", []string{id}, cmd.ErrOrStderr())
	if err != nil {
		return result{}, err
	}
	report, err := decodeRemoteEnvelope[host.ConfirmReport](raw, "confirm")
	if err != nil {
		return result{}, err
	}
	if err := session.Remove(cmd.Context()); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "bootstrap confirm: uploaded bundle cleanup failed: %v\n", err)
	}
	return hostResult(report), nil
}
