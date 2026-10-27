//go:build unix

package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/mbvlabs/eregion/internal/buildinfo"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/host"
	"github.com/mbvlabs/eregion/internal/transport"
	"github.com/spf13/cobra"
)

// The tunnel daemon owns the only WireGuard device for a tunnel configuration
// and every eregion process dials through its unix socket
// (internal/transport/shared.go). The first command that needs the node spawns
// it; it exits after DefaultTunnelPersist with no command using it.

const (
	// tunnelDaemonCommand is how the daemon names itself in its singleton lock.
	tunnelDaemonCommand = "eregion tunnel serve"
	// DefaultTunnelPersist is how long an unused daemon stays up.
	DefaultTunnelPersist = 10 * time.Minute
	// envTunnel selects the tunnel: "inprocess" runs one in this process
	// instead of using the daemon.
	envTunnel        = "EREGION_TUNNEL"
	envTunnelPersist = "EREGION_TUNNEL_PERSIST"
	// envTunnelReadyFD tells the daemon its readiness pipe's descriptor.
	envTunnelReadyFD = "EREGION_TUNNEL_READY_FD"
	// The remaining variables carry the daemon's route without its private
	// key: the key stays in its owner-only file.
	envTunnelPrivateKeyFile = "EREGION_TUNNEL_PRIVATE_KEY_FILE"
	envTunnelAddress        = "EREGION_TUNNEL_ADDRESS"
	envTunnelNodeAddress    = "EREGION_TUNNEL_NODE_ADDRESS"
	envTunnelPeerPublicKey  = "EREGION_TUNNEL_PEER_PUBLIC_KEY"
	envTunnelEndpoint       = "EREGION_TUNNEL_ENDPOINT"
	envTunnelAllowedIPs     = "EREGION_TUNNEL_ALLOWED_IPS"
	// maxSocketPath leaves room under sun_path (104 bytes on macOS, 108 on
	// Linux).
	maxSocketPath = 100
)

// tunnelSpecs is the hidden daemon surface. Agents spawn the daemon; status
// and stop make a stuck tunnel inspectable and restartable.
func (a *app) tunnelSpecs() []spec {
	meta := func(summary, when, example string, mutating bool) CommandMeta {
		m := readMeta(summary, []string{when}, []string{example})
		m.Platforms = []string{"linux", "darwin"}
		if mutating {
			m.Mutating = true
			m.DryRunReason = "Restarting the local tunnel only re-opens the same node route; it changes nothing on the node."
		}
		m.ErrorCodes = append(m.ErrorCodes, contract.ValidationFailed, contract.PreconditionFailed, contract.RemoteUnreachable)
		return m
	}
	serveMeta := meta("Run the shared WireGuard tunnel daemon",
		"Every eregion command spawns this itself; it owns the tunnel's only WireGuard device and serves its unix socket.",
		"eregion tunnel serve --json", false)
	statusMeta := meta("Show the context's shared tunnel daemon",
		"See whether the tunnel daemon runs, which eregion commands use it, and when it exits idle.",
		"eregion tunnel status --json", false)
	stopMeta := meta("Stop the context's shared tunnel daemon",
		"Without --force the daemon takes no new commands and exits once the running ones finish; the next command starts a new one.",
		"eregion tunnel stop --force --json", true)
	return []spec{{
		use:    "tunnel",
		hidden: true,
		meta:   meta("Manage the shared WireGuard tunnel", "Inspect or restart the local daemon that every operator command dials through.", "eregion tunnel status --json", false),
		run:    func(cmd *cobra.Command, _ []string) (result, error) { return a.help(cmd), nil },
		children: []spec{
			{use: "serve", meta: serveMeta, flags: func(cmd *cobra.Command) {
				cmd.Flags().String("name", "", "Context name; otherwise the daemon reads its route from the environment")
			}, run: runTunnelServe},
			{use: "status", meta: statusMeta, flags: contextNameFlag, run: runTunnelStatus},
			{use: "stop", meta: stopMeta, flags: func(cmd *cobra.Command) {
				contextNameFlag(cmd)
				cmd.Flags().Bool("force", false, "Close the tunnel now, dropping the streams of running commands (they re-attach)")
			}, run: runTunnelStop},
		},
	}}
}

// wireGuardDial is the dial function for a context's WireGuard route. Every
// route with the same tunnel configuration shares one daemon, so concurrent
// eregion commands never bring up competing devices with one operator key.
func wireGuardDial(wg *host.ContextWireGuard) (func(context.Context, string, string) (net.Conn, error), error) {
	if os.Getenv(envTunnel) == "inprocess" {
		return inProcessWireGuardDial(wg)
	}
	cfg, err := wireGuardConfig(wg)
	if err != nil {
		return nil, err
	}
	fingerprint := transport.TunnelFingerprint(cfg)
	key := cfg.PrivateKey.PublicKey().String()
	wireGuardMu.Lock()
	if entry, ok := wireGuardDialers[fingerprint]; ok {
		dialer := entry.dialer
		wireGuardMu.Unlock()
		return dialer.DialContext, nil
	}
	// A different route for the same key (a rebuilt node) replaces the old
	// tunnel so the node's single peer follows this process.
	for id, entry := range wireGuardDialers {
		if id != fingerprint && entry.key == key {
			entry.dialer.Close()
			delete(wireGuardDialers, id)
		}
	}
	wireGuardMu.Unlock()

	socket, err := tunnelSocketPath(tunnelSocketID(cfg))
	if err != nil {
		return nil, err
	}
	dialer := transport.NewSharedDialer(transport.SharedDialerConfig{
		Socket:      socket,
		SpawnLock:   socket + ".spawn.lock",
		Fingerprint: fingerprint,
		Build:       tunnelBuild(),
		PID:         os.Getpid(),
		Command:     "eregion",
		Spawn:       func(ctx context.Context) error { return spawnTunnelDaemon(ctx, wg.PrivateKeyFile, cfg) },
	})
	wireGuardMu.Lock()
	if entry, ok := wireGuardDialers[fingerprint]; ok {
		wireGuardMu.Unlock()
		dialer.Close()
		return entry.dialer.DialContext, nil
	}
	wireGuardDialers[fingerprint] = &sharedRoute{dialer: dialer, key: key}
	wireGuardMu.Unlock()
	return dialer.DialContext, nil
}

type sharedRoute struct {
	dialer *transport.SharedDialer
	key    string
}

var (
	wireGuardMu      sync.Mutex
	wireGuardDialers = map[string]*sharedRoute{}
)

// closeWireGuardRoute stops the daemon for a removed context and drops this
// process's cache entry, so nothing keeps the node peer alive after its key
// files are gone.
func closeWireGuardRoute(route *host.ContextSSH) {
	if route == nil || route.WireGuard == nil {
		return
	}
	cfg, err := wireGuardConfig(route.WireGuard)
	if err != nil {
		return
	}
	fingerprint := transport.TunnelFingerprint(cfg)
	key := cfg.PrivateKey.PublicKey().String()
	wireGuardMu.Lock()
	for id, entry := range wireGuardDialers {
		if id == fingerprint || entry.key == key {
			entry.dialer.Close()
			delete(wireGuardDialers, id)
		}
	}
	wireGuardMu.Unlock()
	if socket, err := tunnelSocketPath(tunnelSocketID(cfg)); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, _, _ = stopTunnelDaemonSocket(ctx, socket, true)
	}
	closeInProcessWireGuardRoute(route)
}

// wireGuardConfig builds the userspace tunnel configuration for a saved route.
func wireGuardConfig(wg *host.ContextWireGuard) (transport.WireGuardConfig, error) {
	key, err := transport.ReadWireGuardKey(wg.PrivateKeyFile)
	if err != nil {
		return transport.WireGuardConfig{}, err
	}
	peer, err := transport.ParseWireGuardPublicKey(wg.NodePublicKey)
	if err != nil {
		return transport.WireGuardConfig{}, err
	}
	self, err := netip.ParseAddr(wg.Address)
	if err != nil {
		return transport.WireGuardConfig{}, err
	}
	nodeAddr, err := netip.ParseAddr(wg.NodeAddress)
	if err != nil {
		return transport.WireGuardConfig{}, err
	}
	return normalizeTunnelConfig(transport.WireGuardConfig{
		PrivateKey: key, Address: self, NodeAddress: nodeAddr, PeerPublicKey: peer, Endpoint: wg.Endpoint,
	}), nil
}

// normalizeTunnelConfig fills the default route before the fingerprint is
// computed, so the client and daemon always agree on the configuration.
func normalizeTunnelConfig(cfg transport.WireGuardConfig) transport.WireGuardConfig {
	if len(cfg.AllowedIPs) == 0 && cfg.NodeAddress.IsValid() {
		cfg.AllowedIPs = []netip.Prefix{netip.PrefixFrom(cfg.NodeAddress, cfg.NodeAddress.BitLen())}
	}
	return cfg
}

// tunnelBuild identifies this binary, so a rebuilt or upgraded binary
// replaces an idle daemon of the old one.
func tunnelBuild() string {
	build := buildinfo.Version + "/" + buildinfo.Commit
	if exe, err := os.Executable(); err == nil {
		if info, err := os.Stat(exe); err == nil {
			build += fmt.Sprintf("/%d/%d", info.Size(), info.ModTime().UnixNano())
		}
	}
	return build
}

// tunnelSocketID names the daemon's socket and lock. It uses the operator
// public key, not the full configuration, so a changed node endpoint reaches
// the same daemon and can replace an idle one instead of leaving an old
// device holding the node's single peer.
func tunnelSocketID(cfg transport.WireGuardConfig) string {
	return cfg.PrivateKey.PublicKey().String()
}

// tunnelSocketPath names the daemon's socket after its operator key. The
// socket, lock and log live in a private directory under the operator's
// eregion config directory.
func tunnelSocketPath(identity string) (string, error) {
	sum := sha256.Sum256([]byte(identity))
	name := "t-" + hex.EncodeToString(sum[:16])
	dir, err := tunnelStateDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, name+".sock")
	if len(path) <= maxSocketPath {
		return path, nil
	}
	fallback := filepath.Join(os.TempDir(), "eregion-"+strconv.Itoa(os.Getuid()))
	if xdg := os.Getenv("XDG_RUNTIME_DIR"); runtime.GOOS == "linux" && xdg != "" {
		fallback = filepath.Join(xdg, "eregion")
	}
	if err := privateDir(fallback); err != nil {
		return "", contract.NewFault(contract.PreconditionFailed, err.Error(),
			"The eregion config directory's path is too long for a unix socket, and the fallback directory is unsafe; use a shorter XDG_CONFIG_HOME.")
	}
	path = filepath.Join(fallback, name+".sock")
	if len(path) > maxSocketPath {
		return "", contract.NewFault(contract.PreconditionFailed, "no unix socket path short enough for the tunnel daemon: "+path,
			"Use a shorter XDG_CONFIG_HOME or TMPDIR.")
	}
	return path, nil
}

// tunnelStateDir is the operator's private tunnel directory.
func tunnelStateDir() (string, error) {
	contexts, err := host.DefaultContextPath()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(filepath.Dir(contexts), "tunnels")
	if err := privateDir(dir); err != nil {
		return "", err
	}
	return dir, nil
}

// privateDir creates dir 0700, or checks that an existing one is a real
// directory owned by this user with no access for others.
func privateDir(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil { //nolint:gosec // G703: the user's own config or runtime directory.
		return err
	}
	info, err := os.Lstat(dir) //nolint:gosec // G703: checked right here.
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", dir)
	}
	if st, ok := info.Sys().(*syscall.Stat_t); ok && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s belongs to uid %d", dir, st.Uid)
	}
	if info.Mode().Perm() != 0o700 {
		return fmt.Errorf("%s has mode %04o, want 0700", dir, info.Mode().Perm())
	}
	return nil
}

// tunnelPersist is how long an unused daemon stays up.
func tunnelPersist() (time.Duration, error) {
	text := strings.TrimSpace(os.Getenv(envTunnelPersist))
	if text == "" {
		return DefaultTunnelPersist, nil
	}
	d, err := time.ParseDuration(text)
	if err != nil || d < 0 {
		return DefaultTunnelPersist, fmt.Errorf("tunnel persist %q is not a duration like 10m", text)
	}
	return d, nil
}

// daemonReady is the line the daemon writes on its readiness pipe.
type daemonReady struct {
	OK      bool          `json:"ok"`
	Code    contract.Code `json:"code,omitempty"`
	Message string        `json:"message,omitempty"`
	Hint    string        `json:"hint,omitempty"`
}

// spawnTunnelDaemon starts eregion tunnel serve for the route, detached in its
// own session, and waits for it to report on the readiness pipe.
func spawnTunnelDaemon(ctx context.Context, privateKeyFile string, cfg transport.WireGuardConfig) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	socket, err := tunnelSocketPath(tunnelSocketID(cfg))
	if err != nil {
		return err
	}
	logPath := socket + ".log"
	logFile, err := os.OpenFile(logPath, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600) //nolint:gosec // G304: the operator's private log.
	if err != nil {
		return err
	}
	defer func() { _ = logFile.Close() }()
	devNull, err := os.Open(os.DevNull)
	if err != nil {
		return err
	}
	defer func() { _ = devNull.Close() }()
	r, w, err := os.Pipe()
	if err != nil {
		return err
	}
	defer func() { _ = r.Close() }()
	cmd := exec.Command(exe, "tunnel", "serve") //nolint:gosec // G204: re-runs this binary.
	cmd.Dir = "/"
	cmd.Env = append(os.Environ(),
		envTunnelReadyFD+"=3",
		envTunnelPrivateKeyFile+"="+privateKeyFile,
		envTunnelAddress+"="+cfg.Address.String(),
		envTunnelNodeAddress+"="+cfg.NodeAddress.String(),
		envTunnelPeerPublicKey+"="+cfg.PeerPublicKey.String(),
		envTunnelEndpoint+"="+cfg.Endpoint,
		envTunnelAllowedIPs+"="+prefixList(cfg.AllowedIPs),
	)
	cmd.Stdin = devNull
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.ExtraFiles = []*os.File{w}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		_ = w.Close()
		return err
	}
	_ = w.Close()
	// Reap the daemon if it exits while this process still runs.
	go func() { _ = cmd.Wait() }()

	type result struct {
		line []byte
		err  error
	}
	got := make(chan result, 1)
	go func() {
		line, err := io.ReadAll(io.LimitReader(r, 64<<10))
		got <- result{line, err}
	}()
	var res result
	select {
	case res = <-got:
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(60 * time.Second):
		return contract.NewFault(contract.RemoteUnreachable, "the tunnel daemon did not start within 60s", "See "+logPath+".")
	}
	var ready daemonReady
	if len(strings.TrimSpace(string(res.line))) == 0 || json.Unmarshal(res.line, &ready) != nil {
		fault := contract.NewFault(contract.RemoteUnreachable, "the tunnel daemon exited during startup", "See "+logPath+".")
		fault.Details = map[string]any{"log": logPath}
		return fault
	}
	if ready.OK {
		return nil
	}
	if ready.Code == "already_running" {
		return transport.ErrDaemonRunning
	}
	code := ready.Code
	if !contract.KnownCode(code) {
		code = contract.RemoteUnreachable
	}
	return contract.NewFault(code, ready.Message, ready.Hint)
}

func prefixList(prefixes []netip.Prefix) string {
	parts := make([]string, 0, len(prefixes))
	for _, prefix := range prefixes {
		parts = append(parts, prefix.String())
	}
	return strings.Join(parts, ",")
}

// runTunnelServe is the daemon: it takes the tunnel singleton lock, brings up
// the tunnel, serves the socket until idle or stopped, then tears down.
func runTunnelServe(cmd *cobra.Command, _ []string) (result, error) {
	ready := func(daemonReady) {}
	if fd, err := strconv.Atoi(os.Getenv(envTunnelReadyFD)); err == nil && fd > 2 {
		pipe := os.NewFile(uintptr(fd), "ready")
		_ = os.Unsetenv(envTunnelReadyFD)
		ready = func(r daemonReady) {
			raw, _ := json.Marshal(r)
			_, _ = pipe.Write(append(raw, '\n'))
			_ = pipe.Close()
			ready = func(daemonReady) {}
		}
	}
	fail := func(err error) (result, error) {
		r := daemonReady{Code: contract.RemoteUnreachable, Message: err.Error()}
		var fault *contract.Fault
		if errors.As(err, &fault) {
			r.Code, r.Hint = fault.Code, fault.Hint
		}
		ready(r)
		tunnelLogf("startup failed: %s", r.Message)
		return result{}, err
	}
	cfg, err := tunnelServeConfig(cmd)
	if err != nil {
		return fail(err)
	}
	fingerprint := transport.TunnelFingerprint(cfg)
	socket, err := tunnelSocketPath(tunnelSocketID(cfg))
	if err != nil {
		return fail(err)
	}
	lockPath := socket + ".lock"
	release, held, err := tryTunnelLock(lockPath, tunnelDaemonCommand)
	if err != nil {
		return fail(err)
	}
	if !held {
		ready(daemonReady{Code: "already_running", Message: "a tunnel daemon is already running for this context"})
		return result{}, nil
	}
	defer release()
	logPath := socket + ".log"
	_ = os.Truncate(logPath, 0)
	tunnelLogf("tunnel daemon starting (pid %d, build %s)", os.Getpid(), tunnelBuild())

	persist, perr := tunnelPersist()
	if perr != nil {
		tunnelLogf("%v; using %s", perr, persist)
	}
	// Only the lock holder may remove the socket: one left by a daemon that
	// died is stale.
	_ = os.Remove(socket)
	old := syscall.Umask(0o077)
	ln, err := net.Listen("unix", socket)
	syscall.Umask(old)
	if err != nil {
		return fail(contract.NewFault(contract.RemoteUnreachable, fmt.Sprintf("listen on %s: %v", socket, err), ""))
	}
	defer func() { _ = os.Remove(socket) }()
	_ = os.Chmod(socket, 0o600)

	cfg.Logf = tunnelLogf
	dialer, err := transport.NewWireGuardDialer(cfg)
	if err != nil {
		_ = ln.Close()
		return fail(err)
	}
	defer func() { _ = dialer.Close() }()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	signal.Ignore(syscall.SIGHUP, syscall.SIGPIPE)
	srv := transport.NewSharedServer(transport.SharedServerConfig{
		Listener:      ln,
		Dialer:        dialer,
		Allowed:       cfg.AllowedIPs,
		Fingerprint:   fingerprint,
		Build:         tunnelBuild(),
		Persist:       persist,
		DialTimeout:   transport.SharedDialTimeout,
		LastHandshake: dialer.LastHandshake,
		Logf:          tunnelLogf,
	})
	tunnelLogf("serving %s; exits after %s with no client", socket, persist)
	ready(daemonReady{OK: true})
	reason := srv.Serve(ctx)
	tunnelLogf("exiting (%s)", reason)
	return result{plain: "tunnel daemon exited (" + reason + ")"}, nil
}

// tunnelServeConfig reads the route from the spawn environment, or from the
// named saved context when the daemon is run by hand.
func tunnelServeConfig(cmd *cobra.Command) (transport.WireGuardConfig, error) {
	if os.Getenv(envTunnelPrivateKeyFile) != "" {
		return tunnelEnvConfig()
	}
	store, err := host.LoadContextStore("")
	if err != nil {
		return transport.WireGuardConfig{}, err
	}
	current, err := store.Get(flagString(cmd, "name"))
	if err != nil {
		return transport.WireGuardConfig{}, err
	}
	if current.SSH == nil || current.SSH.WireGuard == nil {
		return transport.WireGuardConfig{}, contract.NewFault(contract.PreconditionFailed, "context "+current.Name+" has no WireGuard route",
			"Import a server init handoff with eregion login, or re-run server init.")
	}
	return wireGuardConfig(current.SSH.WireGuard)
}

func tunnelEnvConfig() (transport.WireGuardConfig, error) {
	key, err := transport.ReadWireGuardKey(os.Getenv(envTunnelPrivateKeyFile))
	if err != nil {
		return transport.WireGuardConfig{}, err
	}
	peer, err := transport.ParseWireGuardPublicKey(os.Getenv(envTunnelPeerPublicKey))
	if err != nil {
		return transport.WireGuardConfig{}, err
	}
	self, err := netip.ParseAddr(os.Getenv(envTunnelAddress))
	if err != nil {
		return transport.WireGuardConfig{}, errors.New("the tunnel daemon's WireGuard address is not an IP address")
	}
	nodeAddr, err := netip.ParseAddr(os.Getenv(envTunnelNodeAddress))
	if err != nil {
		return transport.WireGuardConfig{}, errors.New("the tunnel daemon's node address is not an IP address")
	}
	var allowed []netip.Prefix
	for _, field := range strings.Split(os.Getenv(envTunnelAllowedIPs), ",") {
		if field = strings.TrimSpace(field); field == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(field)
		if err != nil {
			return transport.WireGuardConfig{}, errors.New("the tunnel daemon's allowed IPs are not prefixes")
		}
		allowed = append(allowed, prefix)
	}
	return normalizeTunnelConfig(transport.WireGuardConfig{
		PrivateKey: key, Address: self, NodeAddress: nodeAddr, PeerPublicKey: peer,
		Endpoint: os.Getenv(envTunnelEndpoint), AllowedIPs: allowed,
	}), nil
}

// tunnelStatusData is tunnel status's result.
type tunnelStatusData struct {
	Socket string `json:"socket"`
	Log    string `json:"log"`
	*transport.SharedStatus
}

func runTunnelStatus(cmd *cobra.Command, _ []string) (result, error) {
	cfg, err := tunnelContextConfig(cmd)
	if err != nil {
		return result{}, err
	}
	socket, err := tunnelSocketPath(tunnelSocketID(cfg))
	if err != nil {
		return result{}, err
	}
	data := tunnelStatusData{Socket: socket, Log: socket + ".log"}
	ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
	defer cancel()
	st, err := transport.SharedHello(ctx, socket)
	if err != nil {
		if transport.IsNoDaemon(err) {
			return result{data: data, plain: "no tunnel daemon runs; the next command starts one"}, nil
		}
		return result{}, contract.NewFault(contract.RemoteUnreachable, "ask the tunnel daemon: "+err.Error(), "See "+data.Log+".")
	}
	data.SharedStatus = &st
	return result{data: data, plain: tunnelStatusMessage(data)}, nil
}

func tunnelStatusMessage(d tunnelStatusData) string {
	st := d.SharedStatus
	lines := []string{fmt.Sprintf("tunnel daemon: pid %d, up since %s", st.PID, st.StartedAt)}
	if st.LastHandshake != "" {
		lines = append(lines, "last WireGuard handshake: "+st.LastHandshake)
	}
	lines = append(lines, fmt.Sprintf("%d attached, %d open streams", st.Attached, st.Streams))
	for _, c := range st.Clients {
		lines = append(lines, fmt.Sprintf("  pid %-7d %s (since %s)", c.PID, c.Command, c.Since))
	}
	switch {
	case st.Draining:
		lines = append(lines, "stopping: exits once its clients finish")
	case st.IdleDeadline != "":
		lines = append(lines, "idle; exits at "+st.IdleDeadline+" unless a command uses it")
	}
	lines = append(lines, "build "+st.Build, "socket "+d.Socket, "log "+d.Log)
	return strings.Join(lines, "\n")
}

func runTunnelStop(cmd *cobra.Command, _ []string) (result, error) {
	cfg, err := tunnelContextConfig(cmd)
	if err != nil {
		return result{}, err
	}
	socket, err := tunnelSocketPath(tunnelSocketID(cfg))
	if err != nil {
		return result{}, err
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 20*time.Second)
	defer cancel()
	pid, draining, err := stopTunnelDaemonSocket(ctx, socket, boolFlag(cmd, "force"))
	if err != nil {
		return result{}, contract.NewFault(contract.RemoteUnreachable, "stop the tunnel daemon: "+err.Error(), "See "+socket+".log.")
	}
	data := map[string]any{"pid": pid, "stopped": pid != 0 && !draining, "draining": draining, "socket": socket}
	switch {
	case pid == 0:
		return result{data: data, plain: "no tunnel daemon runs"}, nil
	case draining:
		return result{data: data, plain: fmt.Sprintf("the tunnel daemon (pid %d) takes no new commands and exits once the running ones finish", pid)}, nil
	}
	return result{data: data, plain: fmt.Sprintf("stopped the tunnel daemon (pid %d)", pid)}, nil
}

func tunnelContextConfig(cmd *cobra.Command) (transport.WireGuardConfig, error) {
	store, err := host.LoadContextStore("")
	if err != nil {
		return transport.WireGuardConfig{}, err
	}
	current, err := store.Get(flagString(cmd, "name"))
	if err != nil {
		return transport.WireGuardConfig{}, err
	}
	if current.SSH == nil || current.SSH.WireGuard == nil {
		return transport.WireGuardConfig{}, contract.NewFault(contract.PreconditionFailed, "context "+current.Name+" has no WireGuard route",
			"Import a server init handoff with eregion login, or re-run server init.")
	}
	return wireGuardConfig(current.SSH.WireGuard)
}

// stopTunnelDaemonSocket asks the daemon on socket to exit and, unless it
// drains, waits for it to go. pid is 0 when none ran.
func stopTunnelDaemonSocket(ctx context.Context, socket string, force bool) (pid int, draining bool, err error) {
	st, err := transport.SharedHello(ctx, socket)
	if err != nil {
		if transport.IsNoDaemon(err) {
			return 0, false, nil
		}
		return 0, false, err
	}
	draining, err = transport.SharedStop(ctx, socket, force)
	if err != nil {
		if transport.IsNoDaemon(err) {
			return st.PID, false, nil
		}
		return st.PID, false, err
	}
	if !draining && !transport.SharedWaitGone(ctx, socket, 10*time.Second) {
		return st.PID, false, fmt.Errorf("the tunnel daemon (pid %d) did not exit", st.PID)
	}
	return st.PID, draining, nil
}

// tunnelLockHolder is written into the singleton lock by the daemon.
type tunnelLockHolder struct {
	PID       int    `json:"pid"`
	Command   string `json:"command"`
	StartedAt string `json:"started_at"`
}

// tryTunnelLock takes the tunnel singleton lock without waiting. held is
// false when another process has it. The returned func releases it.
func tryTunnelLock(path, command string) (release func(), held bool, err error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600) //nolint:gosec // G304: the operator's own lock file.
	if err != nil {
		return nil, false, err
	}
	fd := int(f.Fd()) //nolint:gosec // G115: file descriptors fit in int.
	if err := syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, false, nil
		}
		return nil, false, err
	}
	raw, _ := json.Marshal(tunnelLockHolder{PID: os.Getpid(), Command: command, StartedAt: time.Now().UTC().Format(time.RFC3339)})
	if err := f.Truncate(0); err == nil {
		_, _ = f.WriteAt(append(raw, '\n'), 0)
	}
	return func() {
		_ = f.Truncate(0)
		_ = syscall.Flock(fd, syscall.LOCK_UN)
		_ = f.Close()
	}, true, nil
}

// tunnelLogf writes one timestamped line to the daemon's log.
func tunnelLogf(format string, args ...any) {
	line := fmt.Sprintf(format, args...)
	_, _ = fmt.Fprintf(os.Stderr, "%s %s\n", time.Now().UTC().Format(time.RFC3339), line)
}
