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
	"net/url"
	"os"
	"runtime"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/controlplane"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/output"
	"github.com/mbvlabs/eregion/internal/secrets"
	"github.com/spf13/cobra"
)

func operationMeta(summary, example string) CommandMeta {
	meta := readMeta(summary, []string{summary + ". Requires a scoped API credential; operations belong to the authenticated actor."}, []string{example})
	meta.Remote = true
	meta.Environment = []string{"EREGION_API_URL", "EREGION_API_TOKEN"}
	meta.ErrorCodes = []contract.Code{contract.Usage, contract.InvalidOutputMode, contract.InputRequired, contract.ValidationFailed, contract.NotFound, contract.Unauthenticated, contract.Forbidden, contract.Conflict, contract.PreconditionFailed, contract.Unavailable, contract.Internal, contract.Cancelled}
	return meta
}

func serverMeta(summary, example, reason string) CommandMeta {
	meta := operationMeta(summary, example)
	meta.Platforms, meta.Remote, meta.Mutating = []string{"linux"}, false, true
	meta.DryRunReason = reason
	meta.Environment = []string{"EREGION_DATABASE_URL"}
	meta.WhenToUse = []string{summary + ". Runs locally on a Linux control plane using an external PostgreSQL database."}
	return meta
}

func (a *app) runtimeSpecs() []spec {
	submitMeta := operationMeta("Submit a harmless durable diagnostic operation", "eregion operations test --idempotency-key ci:diagnostic-1 --json")
	submitMeta.Mutating, submitMeta.DryRun = true, true
	cancelMeta := operationMeta("Request cancellation at a diagnostic safe boundary", "eregion operations cancel op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa --json")
	cancelMeta.Mutating = true
	cancelMeta.DryRunReason = "Cancellation is an idempotent request, not a deployment plan; inspect state with operations get."
	migrateMeta := serverMeta("Apply explicit PostgreSQL and River migrations", "eregion system migrate --dry-run --json", "")
	migrateMeta.DryRun = true
	serveMeta := serverMeta("Run the authenticated control-plane API", "eregion serve --listen 127.0.0.1:8080", "A running API/worker service has no static mutation preview; use system migrate --dry-run to inspect schema changes.")
	serveMeta.Environment = append(serveMeta.Environment, "EREGION_AUTH_FILE")
	return []spec{
		{use: "serve", meta: serveMeta, flags: func(cmd *cobra.Command) {
			cmd.Flags().String("listen", "127.0.0.1:8080", "API listen address; non-loopback requires TLS")
			cmd.Flags().String("tls-cert", "", "TLS certificate file")
			cmd.Flags().String("tls-key", "", "TLS private-key file")
			cmd.Flags().Bool("workers", true, "Run River workers alongside the API")
			cmd.Flags().Int("concurrency", 2, "Diagnostic workers in this process (1–16)")
		}, run: a.serve},
		{use: "worker", meta: serverMeta("Run River workers without an API listener", "eregion worker --concurrency 2", "Background execution cannot be previewed; this command starts workers for accepted operations."), flags: func(cmd *cobra.Command) {
			cmd.Flags().Int("concurrency", 2, "Diagnostic workers in this process (1–16)")
		}, run: a.worker},
		{use: "system", meta: readMeta("Inspect local control-plane maintenance commands", []string{"Discover explicit database maintenance commands."}, []string{"eregion help system --json"}), run: func(cmd *cobra.Command, _ []string) (result, error) { return a.help(cmd), nil }, children: []spec{
			{use: "migrate", meta: migrateMeta, flags: func(cmd *cobra.Command) {
				cmd.Flags().Bool("dry-run", false, "Inspect pending schema versions without applying DDL")
			}, run: a.migrate},
		}},
		{use: "operations", meta: readMeta("Inspect durable diagnostic operations", []string{"Discover authenticated operation submission and observation commands."}, []string{"eregion help operations --json"}), flags: func(cmd *cobra.Command) {
			cmd.PersistentFlags().String("api-url", "", "API origin URL (defaults to EREGION_API_URL or loopback)")
		}, run: func(cmd *cobra.Command, _ []string) (result, error) { return a.help(cmd), nil }, children: []spec{
			{use: "test", meta: submitMeta, flags: func(cmd *cobra.Command) {
				cmd.Flags().String("idempotency-key", "", "Stable submission key; required unless --dry-run")
				cmd.Flags().Int("steps", 3, "Diagnostic steps (1–20); writes only node-local markers")
				cmd.Flags().Int("step-delay-ms", 100, "Delay per step (0–1000 ms)")
				cmd.Flags().Int("fail-until-attempt", 0, "Intentionally fail attempts up to this number (0–3)")
				cmd.Flags().Bool("dry-run", false, "Validate with the API without inserting an operation or job")
			}, run: a.submitTest},
			{use: "get <operation_id>", meta: operationMeta("Read the authoritative operation snapshot", "eregion operations get op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa --json"), args: oneID, run: func(cmd *cobra.Command, args []string) (result, error) {
				client, err := a.client(cmd)
				if err != nil {
					return result{}, err
				}
				raw, err := client.Get(cmd.Context(), contract.OperationID(args[0]))
				return apiResult(raw), err
			}},
			{use: "events <operation_id>", meta: operationMeta("Read a bounded page of durable operation events", "eregion operations events op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa --after 0 --json"), args: oneID, flags: func(cmd *cobra.Command) {
				cmd.Flags().Uint64("after", 0, "Exclusive event sequence cursor")
				cmd.Flags().Int("limit", 100, "Maximum events (1–200)")
			}, run: func(cmd *cobra.Command, args []string) (result, error) {
				client, err := a.client(cmd)
				if err != nil {
					return result{}, err
				}
				after, _ := cmd.Flags().GetUint64("after")
				limit, _ := cmd.Flags().GetInt("limit")
				raw, err := client.Events(cmd.Context(), contract.Cursor{OperationID: contract.OperationID(args[0]), After: after}, limit)
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

func (a *app) client(cmd *cobra.Command) (*api.Client, error) {
	endpoint, _ := cmd.Flags().GetString("api-url")
	if endpoint == "" {
		endpoint = os.Getenv("EREGION_API_URL")
	}
	if endpoint == "" {
		endpoint = "http://127.0.0.1:8080"
	}
	token := os.Getenv("EREGION_API_TOKEN")
	a.secrets.Add(token)
	return api.NewClient(endpoint, token)
}

func (a *app) submitTest(cmd *cobra.Command, _ []string) (result, error) {
	steps, _ := cmd.Flags().GetInt("steps")
	delay, _ := cmd.Flags().GetInt("step-delay-ms")
	fail, _ := cmd.Flags().GetInt("fail-until-attempt")
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	key, _ := cmd.Flags().GetString("idempotency-key")
	if !dryRun && key == "" {
		return result{}, contract.NewFault(contract.InputRequired, "--idempotency-key is required", "Choose a stable key and reuse it if submission times out.")
	}
	input := operations.TestInput{Steps: steps, StepDelayMS: delay, FailUntilAttempt: fail}
	raw, _ := json.Marshal(input)
	if _, err := operations.NormalizeTestInput(raw); err != nil {
		return result{}, err
	}
	if !dryRun {
		if err := contract.ValidateIdempotencyKey(key); err != nil {
			return result{}, err
		}
	}
	client, err := a.client(cmd)
	if err != nil {
		return result{}, err
	}
	response, err := client.Test(cmd.Context(), input, key, dryRun)
	return apiResult(response), err
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
	registry.Add(os.Getenv("EREGION_API_TOKEN"), os.Getenv("PGPASSWORD"), dsn)
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

func (a *app) startRuntime(cmd *cobra.Command, concurrency int) (*pgxpool.Pool, *controlplane.Runtime, error) {
	ctx, cancel := context.WithTimeout(cmd.Context(), 10*time.Second)
	defer cancel()
	pool, err := a.database(ctx)
	if err != nil {
		return nil, nil, err
	}
	if err := controlplane.Ready(ctx, pool); err != nil {
		pool.Close()
		return nil, nil, err
	}
	run, err := controlplane.New(pool, a.logger(cmd), controlplane.WorkerConfig{Concurrency: concurrency})
	if err != nil {
		pool.Close()
		return nil, nil, err
	}
	return pool, run, nil
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
	pool, run, err := a.startRuntime(cmd, concurrency)
	if err != nil {
		return result{}, err
	}
	defer pool.Close()
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
	pool, run, err := a.startRuntime(cmd, concurrency)
	if err != nil {
		return result{}, err
	}
	defer pool.Close()
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
	server := &http.Server{Handler: api.NewHandler(auth, run.Service, func(ctx context.Context) error { return controlplane.Ready(ctx, pool) }), TLSConfig: tlsConfig, ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelWarn), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: time.Minute, MaxHeaderBytes: 8192}
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
