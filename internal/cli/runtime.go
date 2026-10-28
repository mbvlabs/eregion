package cli

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/controlplane"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/output"
	"github.com/mbvlabs/eregion/internal/secrets"
	"github.com/mbvlabs/eregion/internal/transport"
	"github.com/spf13/cobra"
)

func operationMeta(summary, example string) CommandMeta {
	meta := readMeta(summary, []string{summary + ". Runs on the node through the active context over SSH inside WireGuard; operations belong to the named operator."}, []string{example})
	meta.Remote = true
	meta.Environment = []string{"EREGION_CONTEXT_FILE", "EREGION_CONTEXT"}
	meta.ErrorCodes = []contract.Code{contract.Usage, contract.InvalidOutputMode, contract.InputRequired, contract.ValidationFailed, contract.NotFound, contract.Unauthenticated, contract.Forbidden, contract.Conflict, contract.PreconditionFailed, contract.NodeVersionMismatch, contract.Unavailable, contract.Internal, contract.Cancelled}
	return meta
}

func serverMeta(summary, example, reason string) CommandMeta {
	meta := operationMeta(summary, example)
	meta.Audience = []string{"node"}
	meta.Platforms, meta.Remote, meta.Mutating = []string{"linux"}, false, true
	meta.DryRunReason = reason
	meta.Environment = []string{"EREGION_DATABASE_URL"}
	meta.WhenToUse = []string{summary + ". Runs locally on a Linux control plane using an external PostgreSQL database."}
	return meta
}

// nodeTransportEnvironment lists the environment variables that select and
// configure the node transport. The local transport is the default.
var nodeTransportEnvironment = []string{
	"EREGION_NODE_TRANSPORT",
	"EREGION_NODE_NAME",
	"EREGION_NODE_SSH_ADDRESS",
	"EREGION_NODE_SSH_USER",
	"EREGION_NODE_SSH_IDENTITY",
	"EREGION_NODE_SSH_KNOWN_HOSTS",
	"EREGION_NODE_SSH_COMMAND",
	"EREGION_NODE_SSH_NODE",
	"EREGION_NODE_WG_PRIVATE_KEY_FILE",
	"EREGION_NODE_WG_ADDRESS",
	"EREGION_NODE_WG_PEER_PUBLIC_KEY",
	"EREGION_NODE_WG_ENDPOINT",
	"EREGION_NODE_WG_ALLOWED_IPS",
	"EREGION_NODE_WG_KEEPALIVE",
	"EREGION_NODE_WG_KEY_DIR",
	"EREGION_NODE_WG_SUBNET",
}

// nodeSpecs is the hidden node-mode group. Its commands run on the server, not
// on the operator machine, and never appear in `commands --json`'s operator
// listing. Operator commands reach `node rpc` over SSH inside WireGuard; the
// service, migration and bootstrap entry points are invoked by systemd units or
// by the operator's first-contact path.
func (a *app) nodeSpecs() []spec {
	nodeMeta := readMeta("Run node-mode control-plane and host commands on the server",
		[]string{"Run the server half of eregion. Operator commands invoke these over SSH inside WireGuard; systemd invokes the service and migration entry points."},
		[]string{"sudo eregion node migrate --json", "eregion node bootstrap plan --role main --node main --json"})
	nodeMeta.Audience = []string{"node"}
	nodeMeta.Platforms = []string{"linux"}
	nodeMeta.Remote = false
	nodeMeta.Environment = []string{"EREGION_DATABASE_URL", "EREGION_CONTEXT_FILE"}
	nodeMeta.ErrorCodes = append(nodeMeta.ErrorCodes, contract.ValidationFailed, contract.PreconditionFailed, contract.ConfirmationRequired, contract.Forbidden, contract.NotFound)
	children := append(a.nodeServiceSpecs(), a.bootstrapSpecs()...)
	return []spec{{
		use:      "node",
		hidden:   true,
		meta:     nodeMeta,
		run:      func(cmd *cobra.Command, _ []string) (result, error) { return a.help(cmd), nil },
		children: children,
	}}
}

func (a *app) nodeServiceSpecs() []spec {
	migrateMeta := serverMeta("Apply explicit PostgreSQL and River migrations", "eregion node migrate --dry-run --json", "")
	migrateMeta.DryRun = true
	serveMeta := serverMeta("Run the authenticated control-plane API", "eregion node serve --listen 127.0.0.1:8080", "A running API/worker service has no static mutation preview; use node migrate --dry-run to inspect schema changes.")
	serveMeta.Environment = append(serveMeta.Environment, "EREGION_AUTH_FILE")
	serveMeta.Environment = append(serveMeta.Environment, nodeTransportEnvironment...)
	serveMeta.Environment = append(serveMeta.Environment, extensionEnvironment...)
	serveMeta.Environment = append(serveMeta.Environment, serveExtensionEnvironment...)
	workerMeta := serverMeta("Run River workers without an API listener", "eregion node worker --concurrency 2", "Background execution cannot be previewed; this command starts workers for accepted operations.")
	workerMeta.Environment = append(workerMeta.Environment, nodeTransportEnvironment...)
	workerMeta.Environment = append(workerMeta.Environment, extensionEnvironment...)
	return []spec{
		{use: "serve", meta: serveMeta, flags: func(cmd *cobra.Command) {
			cmd.Flags().String("listen", "127.0.0.1:8080", "API listen address; non-loopback requires TLS")
			cmd.Flags().String("tls-cert", "", "TLS certificate file")
			cmd.Flags().String("tls-key", "", "TLS private-key file")
			cmd.Flags().Bool("workers", true, "Run River workers alongside the API")
			cmd.Flags().Int("concurrency", 2, "Diagnostic workers in this process (1–16)")
			cmd.Flags().String("private-upstream", "", "Private upstream URL enforced by access grants under /private/; defaults to EREGION_PRIVATE_UPSTREAM")
		}, run: a.serve},
		{use: "worker", meta: workerMeta, flags: func(cmd *cobra.Command) {
			cmd.Flags().Int("concurrency", 2, "Diagnostic workers in this process (1–16)")
		}, run: a.worker},
		{use: "migrate", meta: migrateMeta, flags: func(cmd *cobra.Command) {
			cmd.Flags().Bool("dry-run", false, "Inspect pending schema versions without applying DDL")
		}, run: a.migrate},
	}
}

func (a *app) runtimeSpecs() []spec {
	followMeta := operationMeta("Follow an operation's durable state and events", "eregion operations follow op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa --output ndjson")
	followMeta.Output = "events"
	cancelMeta := operationMeta("Request cancellation at a safe boundary", "eregion operations cancel op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa --json")
	cancelMeta.Mutating = true
	cancelMeta.DryRunReason = "Cancellation is an idempotent request, not a deployment plan; inspect state with operations get."
	return []spec{
		{use: "operations", meta: readMeta("Inspect durable operations", []string{"Discover authenticated operation observation and cancellation commands."}, []string{"eregion help operations --json"}), flags: func(cmd *cobra.Command) {
		}, run: func(cmd *cobra.Command, _ []string) (result, error) { return a.help(cmd), nil }, children: []spec{
			a.operationsListSpec(),
			{use: "follow <operation_id>", meta: followMeta, args: oneID, flags: followFlags, run: a.follow},
			{use: "get <operation_id>", meta: operationMeta("Read the authoritative operation snapshot", "eregion operations get op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa --json"), args: oneID, run: func(cmd *cobra.Command, args []string) (result, error) {
				client, err := a.client(cmd)
				if err != nil {
					return result{}, err
				}
				raw, err := client.Get(cmd.Context(), contract.OperationID(args[0]))
				return apiResult(raw), err
			}},
			{use: "cancel <operation_id>", meta: cancelMeta, args: oneID, run: func(cmd *cobra.Command, args []string) (result, error) {
				client, err := a.client(cmd)
				if err != nil {
					return result{}, err
				}
				raw, err := client.Cancel(cmd.Context(), contract.OperationID(args[0]))
				return apiResult(raw), err
			}},
		}},
	}
}

func oneID(_ *cobra.Command, args []string) error {
	if len(args) == 0 {
		return contract.NewFault(contract.InputRequired, "operation_id argument is required", "Use the operation ID returned by submission.")
	}
	if len(args) != 1 {
		return contract.NewFault(contract.Usage, "exactly one operation_id is required", "Inspect the command with --help --json.")
	}
	return contract.OperationID(args[0]).Validate()
}

func apiResult(raw json.RawMessage) result {
	var pretty bytes.Buffer
	_ = json.Indent(&pretty, raw, "", "  ")
	return result{data: raw, plain: pretty.String()}
}

func (a *app) client(cmd *cobra.Command) (Backend, error) {
	return a.contextClient(cmd)
}

func (a *app) database(ctx context.Context) (*pgxpool.Pool, error) {
	if runtime.GOOS != "linux" {
		return nil, contract.NewFault(contract.ValidationFailed, "control-plane commands require Linux", "Use the portable operations client to reach a Linux control plane.")
	}
	dsn := os.Getenv("EREGION_DATABASE_URL")
	if dsn == "" {
		return nil, contract.NewFault(contract.InputRequired, "EREGION_DATABASE_URL is required", "Supply the external PostgreSQL URL through the environment, not command arguments.")
	}
	a.secrets.Add(dsn)
	if parsed, err := url.Parse(dsn); err == nil && parsed.User != nil {
		password, _ := parsed.User.Password()
		a.secrets.Add(password)
	}
	if parsed, err := controlplane.ParseDatabaseConfig(dsn); err == nil {
		a.secrets.Add(parsed.ConnConfig.Password)
	}
	return controlplane.Open(ctx, dsn)
}

func (a *app) migrate(cmd *cobra.Command, _ []string) (result, error) {
	ctx, cancel := context.WithTimeout(cmd.Context(), time.Minute)
	defer cancel()
	pool, err := a.database(ctx)
	if err != nil {
		return result{}, err
	}
	defer pool.Close()
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	var plan controlplane.MigrationPlan
	if dryRun {
		plan, err = controlplane.PlanMigrations(ctx, pool)
	} else {
		plan, err = controlplane.Migrate(ctx, pool)
	}
	raw, _ := json.Marshal(plan)
	return apiResult(raw), err
}

type safeLogWriter struct {
	out      io.Writer
	registry *secrets.Registry
}

func registerEnvironmentSecrets(registry *secrets.Registry) {
	dsn := os.Getenv("EREGION_DATABASE_URL")
	registry.Add(os.Getenv("PGPASSWORD"), dsn)
	if parsed, err := url.Parse(dsn); err == nil {
		if parsed.User != nil {
			password, _ := parsed.User.Password()
			registry.Add(password)
		}
		registry.Add(parsed.Query().Get("password"))
	}
}

func (w safeLogWriter) Write(raw []byte) (int, error) {
	text := output.SafeText(w.registry.Redact(string(raw)))
	n, err := io.WriteString(w.out, text)
	if err == nil && n != len(text) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return 0, err
	}
	return len(raw), nil
}

func (a *app) logger(cmd *cobra.Command) *slog.Logger {
	return slog.New(slog.NewTextHandler(safeLogWriter{out: cmd.ErrOrStderr(), registry: a.secrets}, &slog.HandlerOptions{Level: slog.LevelInfo}))
}

// startRuntime opens the database and builds the runtime. The returned
// closer releases the node transport (a WireGuard tunnel and its key lock);
// call it after the workers stopped.
func (a *app) startRuntime(cmd *cobra.Command, concurrency int) (*pgxpool.Pool, *controlplane.Runtime, extensionRuntime, func(), error) {
	ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
	defer cancel()
	pool, err := a.database(ctx)
	if err != nil {
		return nil, nil, extensionRuntime{}, nil, err
	}
	if err := controlplane.Ready(ctx, pool); err != nil {
		pool.Close()
		return nil, nil, extensionRuntime{}, nil, err
	}
	// A process without workers never dispatches node work, so it builds no
	// node transport and holds no WireGuard key.
	var nodeTransport node.Transport
	closeTransport := func() {}
	if concurrency > 0 {
		nodeTransport, closeTransport, err = a.nodeTransport(a.logger(cmd))
		if err != nil {
			pool.Close()
			return nil, nil, extensionRuntime{}, nil, err
		}
		// The local transport is the default. Build it here so the shared
		// per-node resolver sees the same authenticated transport the worker
		// uses; controlplane.New is given this instance and does not replace it.
		if nodeTransport == nil {
			localName := strings.TrimSpace(os.Getenv("EREGION_NODE_NAME"))
			if localName == "" {
				pool.Close()
				return nil, nil, extensionRuntime{}, nil, contract.NewFault(contract.ValidationFailed, "the worker needs EREGION_NODE_NAME", "Set EREGION_NODE_NAME to this node's registered name before starting the worker.")
			}
			local, localErr := node.NewLocal(node.DefaultStateDir(), localName, &secrets.Registry{})
			if localErr != nil {
				pool.Close()
				return nil, nil, extensionRuntime{}, nil, contract.NewFault(contract.ValidationFailed, "could not initialise the local node transport", "Check EREGION_NODE_STATE_DIR permissions and that every worker on this host uses the same directory.")
			}
			nodeTransport = local
		}
	}
	logger := a.logger(cmd)
	// One resolver serves extension dispatch and the worker. Sharing it keeps
	// one transport cache and one WireGuard key lock per node.
	nodes := controlplane.NewNodeResolver(pool, a.fleetResolverConfig(logger), func() node.Transport { return nodeTransport })
	// Extensions register before New so the registry is complete before any
	// action can be normalized, submitted, or executed.
	parts, secretResolver, err := a.startExtensions(pool)
	if err != nil {
		closeTransport()
		pool.Close()
		return nil, nil, extensionRuntime{}, nil, err
	}
	run, err := controlplane.New(pool, logger, controlplane.WorkerConfig{
		Concurrency: concurrency, Transport: nodeTransport, SecretResolver: secretResolver,
		LocalNodeName: os.Getenv("EREGION_NODE_NAME"),
		Nodes:         a.fleetResolverConfig(logger), Resolver: nodes, Redaction: a.secrets,
	})
	if err != nil {
		closeTransport()
		pool.Close()
		return nil, nil, extensionRuntime{}, nil, err
	}
	// A fresh installation must list its local node before the first platform
	// submission. This is idempotent and a no-op for remote-only workers.
	if err := run.EnsureLocalNode(cmd.Context()); err != nil {
		run.Close()
		closeTransport()
		pool.Close()
		return nil, nil, extensionRuntime{}, nil, err
	}
	// The per-node tunnels are released with the transport, after the workers.
	return pool, run, parts, func() { run.Close(); closeTransport() }, nil
}

// nodeTransport builds the transport a worker dispatches node work through.
// The local transport is the default; EREGION_NODE_TRANSPORT=ssh selects the
// pinned SSH transport. The choice never changes the worker, the operation
// contract, or the database model.
func (a *app) nodeTransport(logger *slog.Logger) (node.Transport, func(), error) {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("EREGION_NODE_TRANSPORT"))) {
	case "", "local":
		return nil, func() {}, nil
	case "ssh":
		return a.sshTransport(logger)
	default:
		return nil, nil, contract.NewFault(contract.ValidationFailed, "unknown node transport", "Set EREGION_NODE_TRANSPORT to local or ssh.")
	}
}

// fleetResolverConfig configures how enrolled nodes are reached. Enrolled
// nodes carry their own pinned host key and address in PostgreSQL; the
// environment supplies only the control plane's own credentials: the SSH
// identity authorized on every node (EREGION_NODE_SSH_IDENTITY), the directory
// of per-node WireGuard keys, and the private network addressing.
func (a *app) fleetResolverConfig(logger *slog.Logger) controlplane.ResolverConfig {
	return controlplane.ResolverConfig{
		SSHIdentityFile:         os.Getenv("EREGION_NODE_SSH_IDENTITY"),
		WireGuardKeyDir:         os.Getenv("EREGION_NODE_WG_KEY_DIR"),
		WireGuardControlAddress: os.Getenv("EREGION_NODE_WG_ADDRESS"),
		WireGuardSubnet:         os.Getenv("EREGION_NODE_WG_SUBNET"),
		Logf: func(format string, args ...any) {
			logger.Warn("WireGuard: " + fmt.Sprintf(format, args...))
		},
	}
}

// sshTransport builds the pinned SSH transport from the environment. Every
// value comes from the environment, never from a command argument, so a
// credential never reaches a process list. When
// EREGION_NODE_WG_PRIVATE_KEY_FILE is set, the SSH connection runs inside a
// userspace WireGuard tunnel and EREGION_NODE_SSH_ADDRESS is the node's
// tunnel address.
func (a *app) sshTransport(logger *slog.Logger) (node.Transport, func(), error) {
	address := os.Getenv("EREGION_NODE_SSH_ADDRESS")
	user := os.Getenv("EREGION_NODE_SSH_USER")
	identity := os.Getenv("EREGION_NODE_SSH_IDENTITY")
	knownHosts := os.Getenv("EREGION_NODE_SSH_KNOWN_HOSTS")
	if address == "" || user == "" || identity == "" || knownHosts == "" {
		return nil, nil, contract.NewFault(contract.InputRequired, "the SSH node transport is not configured", "Set EREGION_NODE_SSH_ADDRESS, EREGION_NODE_SSH_USER, EREGION_NODE_SSH_IDENTITY, and EREGION_NODE_SSH_KNOWN_HOSTS.")
	}
	keys, err := transport.LoadHostKeys(knownHosts)
	if err != nil {
		return nil, nil, contract.NewFault(contract.ValidationFailed, "the pinned node host keys could not be read", "Check EREGION_NODE_SSH_KNOWN_HOSTS; it must contain the pinned host key of the node.")
	}
	config := transport.SSHConfig{
		Address:      address,
		User:         user,
		IdentityFile: identity,
		HostKeys:     keys,
		Command:      os.Getenv("EREGION_NODE_SSH_COMMAND"),
		ExpectedNode: os.Getenv("EREGION_NODE_SSH_NODE"),
	}
	closer := func() {}
	if os.Getenv("EREGION_NODE_WG_PRIVATE_KEY_FILE") != "" {
		dialer, err := a.wireGuardDialer(address, logger)
		if err != nil {
			return nil, nil, err
		}
		config.Dialer = dialer
		closer = func() { _ = dialer.Close() }
	}
	client, err := transport.NewSSHTransport(config)
	if err != nil {
		closer()
		return nil, nil, contract.NewFault(contract.ValidationFailed, "the SSH node transport is not usable", err.Error())
	}
	return client, closer, nil
}

// wireGuardDialer builds the userspace WireGuard dialer from the environment.
// The private key comes only from a strict-mode file and is registered for
// redaction before anything else can print it.
func (a *app) wireGuardDialer(sshAddress string, logger *slog.Logger) (*transport.WireGuardDialer, error) {
	key, err := transport.ReadWireGuardKey(os.Getenv("EREGION_NODE_WG_PRIVATE_KEY_FILE"))
	if err != nil {
		return nil, contract.NewFault(contract.ValidationFailed, "the WireGuard private key could not be read", err.Error())
	}
	key.Register(a.secrets)
	usage := "Set EREGION_NODE_WG_ADDRESS, EREGION_NODE_WG_PEER_PUBLIC_KEY, and EREGION_NODE_WG_ENDPOINT; EREGION_NODE_SSH_ADDRESS must be the node's tunnel address."
	localText := os.Getenv("EREGION_NODE_WG_ADDRESS")
	peerText := os.Getenv("EREGION_NODE_WG_PEER_PUBLIC_KEY")
	endpoint := os.Getenv("EREGION_NODE_WG_ENDPOINT")
	if localText == "" || peerText == "" || endpoint == "" {
		return nil, contract.NewFault(contract.InputRequired, "the WireGuard tunnel is not configured", usage)
	}
	local, err := netip.ParseAddr(localText)
	if err != nil {
		return nil, contract.NewFault(contract.ValidationFailed, "EREGION_NODE_WG_ADDRESS is not an IP address", usage)
	}
	host, _, err := net.SplitHostPort(sshAddress)
	if err != nil {
		return nil, contract.NewFault(contract.ValidationFailed, "EREGION_NODE_SSH_ADDRESS must be host:port", usage)
	}
	nodeAddress, err := netip.ParseAddr(host)
	if err != nil {
		return nil, contract.NewFault(contract.ValidationFailed, "EREGION_NODE_SSH_ADDRESS must be the node's tunnel IP address when WireGuard is enabled", usage)
	}
	peer, err := transport.ParseWireGuardPublicKey(peerText)
	if err != nil {
		return nil, contract.NewFault(contract.ValidationFailed, "EREGION_NODE_WG_PEER_PUBLIC_KEY is not a WireGuard public key", usage)
	}
	var allowed []netip.Prefix
	for _, field := range strings.Split(os.Getenv("EREGION_NODE_WG_ALLOWED_IPS"), ",") {
		if field = strings.TrimSpace(field); field == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(field)
		if err != nil {
			return nil, contract.NewFault(contract.ValidationFailed, "EREGION_NODE_WG_ALLOWED_IPS must be comma-separated prefixes", "Use the node subnet, for example 10.77.0.0/24, or leave it unset to route only the node address.")
		}
		allowed = append(allowed, prefix)
	}
	var keepalive time.Duration
	if text := os.Getenv("EREGION_NODE_WG_KEEPALIVE"); text != "" {
		seconds, err := strconv.Atoi(text)
		if err != nil || seconds < 1 || seconds > 3600 {
			return nil, contract.NewFault(contract.ValidationFailed, "EREGION_NODE_WG_KEEPALIVE must be 1–3600 seconds", "Leave it unset for the 25-second default.")
		}
		keepalive = time.Duration(seconds) * time.Second
	}
	lockDir := os.Getenv("XDG_RUNTIME_DIR")
	if lockDir == "" {
		lockDir = os.TempDir()
	}
	dialer, err := transport.NewWireGuardDialer(transport.WireGuardConfig{
		PrivateKey:    key,
		Address:       local,
		NodeAddress:   nodeAddress,
		PeerPublicKey: peer,
		Endpoint:      endpoint,
		AllowedIPs:    allowed,
		Keepalive:     keepalive,
		LockDir:       lockDir,
		Logf: func(format string, args ...any) {
			logger.Warn("WireGuard: " + fmt.Sprintf(format, args...))
		},
	})
	if err != nil {
		return nil, contract.NewFault(contract.ValidationFailed, "the WireGuard tunnel is not usable", err.Error())
	}
	return dialer, nil
}

func stopWorkers(run *controlplane.Runtime) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	return run.Jobs.Stop(ctx)
}

func (a *app) worker(cmd *cobra.Command, _ []string) (result, error) {
	concurrency, _ := cmd.Flags().GetInt("concurrency")
	if concurrency < 1 {
		return result{}, contract.NewFault(contract.ValidationFailed, "worker concurrency must be positive", "Use --concurrency 1–16.")
	}
	pool, run, _, closeTransport, err := a.startRuntime(cmd, concurrency)
	if err != nil {
		return result{}, err
	}
	defer pool.Close()
	defer closeTransport()
	if err := run.Jobs.Start(cmd.Context()); err != nil {
		return result{}, err
	}
	a.logger(cmd).Info("River worker started", "concurrency", concurrency)
	<-cmd.Context().Done()
	err = stopWorkers(run)
	return result{data: map[string]bool{"stopped": true}, plain: "Worker stopped."}, err
}

func (a *app) serve(cmd *cobra.Command, _ []string) (res result, serveErr error) {
	address, _ := cmd.Flags().GetString("listen")
	cert, _ := cmd.Flags().GetString("tls-cert")
	key, _ := cmd.Flags().GetString("tls-key")
	if err := validateListener(address, cert, key); err != nil {
		return result{}, err
	}
	var tlsConfig *tls.Config
	if cert != "" {
		pair, err := tls.LoadX509KeyPair(cert, key)
		if err != nil {
			return result{}, contract.NewFault(contract.ValidationFailed, "could not load API TLS certificate/key", "Check --tls-cert and --tls-key; private material is not printed.")
		}
		tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{pair}}
	}
	authFile := os.Getenv("EREGION_AUTH_FILE")
	if authFile == "" {
		return result{}, contract.NewFault(contract.InputRequired, "EREGION_AUTH_FILE is required", "Provide a private JSON file of scoped API token hashes.")
	}
	auth, err := api.LoadAuth(authFile)
	if err != nil {
		return result{}, err
	}
	concurrency, _ := cmd.Flags().GetInt("concurrency")
	withWorkers, _ := cmd.Flags().GetBool("workers")
	if concurrency < 1 || concurrency > 16 {
		return result{}, contract.NewFault(contract.ValidationFailed, "worker concurrency out of range", "Use --concurrency 1–16.")
	}
	if !withWorkers {
		concurrency = 0
	}
	pool, run, parts, closeTransport, err := a.startRuntime(cmd, concurrency)
	if err != nil {
		return result{}, err
	}
	defer pool.Close()
	// Deferred before the worker stop below, so it runs after it.
	defer closeTransport()
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return result{}, contract.NewFault(contract.Unavailable, "could not bind API listener", "Check --listen and whether the port is available.")
	}
	defer listener.Close()
	if withWorkers {
		if err := run.Jobs.Start(cmd.Context()); err != nil {
			return result{}, err
		}
		defer func() {
			if err := stopWorkers(run); serveErr == nil {
				serveErr = err
			}
		}()
	}
	logger := a.logger(cmd)
	base := api.NewHandler(auth, run.Service, func(ctx context.Context) error { return controlplane.Ready(ctx, pool) })
	handler, err := a.buildMux(cmd, logger, auth, base, run.Service, parts)
	if err != nil {
		return result{}, err
	}
	server := &http.Server{Handler: handler, TLSConfig: tlsConfig, ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelWarn), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 8192}
	logger.Info("API listener bound", "address", listener.Addr().String(), "workers", withWorkers)
	done := make(chan error, 1)
	go func() {
		if cert != "" {
			done <- server.ServeTLS(listener, "", "")
		} else {
			done <- server.Serve(listener)
		}
	}()
	select {
	case err := <-done:
		if err != http.ErrServerClosed {
			return result{}, contract.NewFault(contract.Unavailable, "API listener stopped unexpectedly", "Check listener/TLS configuration and restart the service.")
		}
	case <-cmd.Context().Done():
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			_ = server.Close()
			return result{}, err
		}
	}
	return result{data: map[string]bool{"stopped": true}, plain: fmt.Sprintf("API stopped (%s).", listener.Addr())}, nil
}

func validateListener(address, cert, key string) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return contract.NewFault(contract.ValidationFailed, "invalid listen address", "Use a literal host:port such as 127.0.0.1:8080.")
	}
	ip := net.ParseIP(host)
	if (cert == "") != (key == "") || ((ip == nil || !ip.IsLoopback()) && cert == "") {
		return contract.NewFault(contract.ValidationFailed, "non-loopback API listeners require TLS", "Supply both --tls-cert and --tls-key, or use a literal loopback listener for development.")
	}
	return nil
}
