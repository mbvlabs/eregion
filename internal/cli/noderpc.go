package cli

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/buildinfo"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/controlplane"
	"github.com/mbvlabs/eregion/internal/host"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/rpc"
	"github.com/mbvlabs/eregion/internal/secrets"
	"github.com/mbvlabs/eregion/internal/vault"
	"github.com/spf13/cobra"
)

// Backend is the operator-side application surface. The saved-context
// implementation runs every method through `eregion node rpc` over SSH inside
// WireGuard; the HTTP client remains only for CI/webhook/readyz.
type Backend interface {
	Test(context.Context, operations.TestInput, string, bool) (json.RawMessage, error)
	Get(context.Context, contract.OperationID) (json.RawMessage, error)
	Events(context.Context, contract.Cursor, int) (json.RawMessage, error)
	Cancel(context.Context, contract.OperationID) (json.RawMessage, error)
	Ready(context.Context) error
	SubmitAction(context.Context, string, string, json.RawMessage, string, bool) (json.RawMessage, error)
	PlatformInventory(context.Context, string, url.Values) (json.RawMessage, error)
	SetSecret(context.Context, vault.Namespace, []byte, uint64) (vault.Metadata, error)
	ArchiveSecret(context.Context, vault.Namespace, uint64) (vault.Metadata, error)
	GetSecretMetadata(context.Context, vault.Namespace) (vault.Metadata, error)
	ListSecrets(context.Context, string, string, int) ([]vault.Metadata, error)
	ExportSecret(context.Context, vault.Namespace) (vault.Metadata, []byte, error)
}

const (
	nodeBinaryPath = "/usr/local/bin/eregion"

	methodVersion        = "version"
	methodUpgradeInstall = "upgrade.install"
	methodTest           = "operations.test"
	methodSubmit         = "operations.submit"
	methodGet            = "operations.get"
	methodEvents         = "operations.events"
	methodCancel         = "operations.cancel"
	methodInventory      = "platform.inventory"
	methodSecretSet      = "vault.set"
	methodSecretArchive  = "vault.archive"
	methodSecretMetadata = "vault.metadata"
	methodSecretList     = "vault.list"
	methodSecretExport   = "vault.export"
	methodReady          = "ready"
)

// nodeRPCCommand is the fixed command the operator runs on the node.
func nodeRPCCommand(sshUser string) string {
	if sshUser == "root" {
		return nodeBinaryPath + " node rpc"
	}
	return "sudo -n " + nodeBinaryPath + " node rpc"
}

type rpcVersionResult struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
}

type rpcSubmitParams struct {
	Kind   string          `json:"kind"`
	Target string          `json:"target"`
	Input  json.RawMessage `json:"input,omitempty"`
}

type rpcOperationParams struct {
	OperationID string `json:"operation_id"`
}

type rpcEventsParams struct {
	OperationID string `json:"operation_id"`
	After       uint64 `json:"after"`
	Limit       int    `json:"limit"`
}

type rpcInventoryParams struct {
	Collection string              `json:"collection"`
	Filters    map[string][]string `json:"filters,omitempty"`
}

type rpcSecretParams struct {
	App              string `json:"app"`
	Environment      string `json:"environment"`
	Name             string `json:"name"`
	ExpectedRevision uint64 `json:"expected_revision"`
}

type rpcSecretListParams struct {
	App         string `json:"app"`
	Environment string `json:"environment"`
	Limit       int    `json:"limit"`
}

type rpcSecretExportResult struct {
	Secret      vault.Metadata `json:"secret"`
	ValueBase64 string         `json:"value_base64"`
}

type rpcInstallParams struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type rpcCallOptions struct {
	Secrets        map[string]string
	DryRun         bool
	IdempotencyKey string
}

type nodeClient struct {
	remote      *host.Remote
	route       *host.ContextSSH
	operator    string
	autoUpgrade bool
	stderr      io.Writer

	mu      sync.Mutex
	checked bool
}

func (n *nodeClient) Test(ctx context.Context, input operations.TestInput, key string, dryRun bool) (json.RawMessage, error) {
	raw, err := json.Marshal(input)
	if err != nil {
		return nil, err
	}
	return n.call(ctx, methodTest, rpcSubmitParams{Input: raw}, rpcCallOptions{DryRun: dryRun, IdempotencyKey: key})
}

func (n *nodeClient) Get(ctx context.Context, id contract.OperationID) (json.RawMessage, error) {
	return n.call(ctx, methodGet, rpcOperationParams{OperationID: string(id)}, rpcCallOptions{})
}

func (n *nodeClient) Events(ctx context.Context, cursor contract.Cursor, limit int) (json.RawMessage, error) {
	return n.call(ctx, methodEvents, rpcEventsParams{OperationID: string(cursor.OperationID), After: cursor.After, Limit: limit}, rpcCallOptions{})
}

func (n *nodeClient) Cancel(ctx context.Context, id contract.OperationID) (json.RawMessage, error) {
	return n.call(ctx, methodCancel, rpcOperationParams{OperationID: string(id)}, rpcCallOptions{})
}

func (n *nodeClient) Ready(ctx context.Context) error {
	_, err := n.call(ctx, methodReady, nil, rpcCallOptions{})
	return err
}

func (n *nodeClient) SubmitAction(ctx context.Context, kind, target string, input json.RawMessage, idempotencyKey string, dryRun bool) (json.RawMessage, error) {
	return n.call(ctx, methodSubmit, rpcSubmitParams{Kind: kind, Target: target, Input: input}, rpcCallOptions{DryRun: dryRun, IdempotencyKey: idempotencyKey})
}

func (n *nodeClient) PlatformInventory(ctx context.Context, collection string, filters url.Values) (json.RawMessage, error) {
	return n.call(ctx, methodInventory, rpcInventoryParams{Collection: collection, Filters: filters}, rpcCallOptions{})
}

func (n *nodeClient) SetSecret(ctx context.Context, namespace vault.Namespace, value []byte, expectedRevision uint64) (vault.Metadata, error) {
	raw, err := n.call(ctx, methodSecretSet, rpcSecretParams{
		App: namespace.App, Environment: namespace.Environment, Name: namespace.Name, ExpectedRevision: expectedRevision,
	}, rpcCallOptions{Secrets: map[string]string{"value_base64": base64Encode(value)}})
	if err != nil {
		return vault.Metadata{}, err
	}
	var metadata vault.Metadata
	if err := json.Unmarshal(raw, &metadata); err != nil || metadata.Validate() != nil {
		return vault.Metadata{}, invalidNodeData("secret metadata")
	}
	return metadata, nil
}

func (n *nodeClient) ArchiveSecret(ctx context.Context, namespace vault.Namespace, expectedRevision uint64) (vault.Metadata, error) {
	raw, err := n.call(ctx, methodSecretArchive, rpcSecretParams{
		App: namespace.App, Environment: namespace.Environment, Name: namespace.Name, ExpectedRevision: expectedRevision,
	}, rpcCallOptions{})
	if err != nil {
		return vault.Metadata{}, err
	}
	var metadata vault.Metadata
	if err := json.Unmarshal(raw, &metadata); err != nil || metadata.Validate() != nil {
		return vault.Metadata{}, invalidNodeData("secret metadata")
	}
	return metadata, nil
}

func (n *nodeClient) GetSecretMetadata(ctx context.Context, namespace vault.Namespace) (vault.Metadata, error) {
	raw, err := n.call(ctx, methodSecretMetadata, rpcSecretParams{
		App: namespace.App, Environment: namespace.Environment, Name: namespace.Name,
	}, rpcCallOptions{})
	if err != nil {
		return vault.Metadata{}, err
	}
	var metadata vault.Metadata
	if err := json.Unmarshal(raw, &metadata); err != nil || metadata.Validate() != nil {
		return vault.Metadata{}, invalidNodeData("secret metadata")
	}
	return metadata, nil
}

func (n *nodeClient) ListSecrets(ctx context.Context, app, environment string, limit int) ([]vault.Metadata, error) {
	if limit == 0 {
		limit = 100
	}
	raw, err := n.call(ctx, methodSecretList, rpcSecretListParams{App: app, Environment: environment, Limit: limit}, rpcCallOptions{})
	if err != nil {
		return nil, err
	}
	var page struct {
		Secrets []vault.Metadata `json:"secrets"`
	}
	if json.Unmarshal(raw, &page) != nil || page.Secrets == nil || len(page.Secrets) > limit {
		return nil, invalidNodeData("secret metadata list")
	}
	for _, metadata := range page.Secrets {
		if metadata.Validate() != nil || metadata.App != app || metadata.Environment != environment {
			return nil, invalidNodeData("secret metadata list")
		}
	}
	return page.Secrets, nil
}

func (n *nodeClient) ExportSecret(ctx context.Context, namespace vault.Namespace) (vault.Metadata, []byte, error) {
	raw, err := n.call(ctx, methodSecretExport, rpcSecretParams{
		App: namespace.App, Environment: namespace.Environment, Name: namespace.Name,
	}, rpcCallOptions{})
	if err != nil {
		return vault.Metadata{}, nil, err
	}
	var response rpcSecretExportResult
	if json.Unmarshal(raw, &response) != nil || response.Secret.Validate() != nil || response.Secret.Namespace() != namespace {
		return vault.Metadata{}, nil, invalidNodeData("secret export")
	}
	value, err := base64Decode(response.ValueBase64)
	if err != nil || len(value) == 0 || len(value) > vault.MaxValueBytes {
		clearBytes(value)
		return vault.Metadata{}, nil, invalidNodeData("secret export")
	}
	return response.Secret, value, nil
}

func invalidNodeData(what string) *contract.Fault {
	return contract.NewFault(contract.Unavailable, "the node returned invalid "+what, "Check that the operator and node run the same build; do not assume the action was rejected.")
}

func (n *nodeClient) call(ctx context.Context, method string, params any, opts rpcCallOptions) (json.RawMessage, error) {
	if method != methodVersion && method != methodUpgradeInstall {
		if err := n.ensureVersion(ctx); err != nil {
			return nil, err
		}
	}
	return n.callRaw(ctx, method, params, opts)
}

func (n *nodeClient) ensureVersion(ctx context.Context) error {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.checked {
		return nil
	}
	got, err := n.version(ctx)
	if err != nil {
		return err
	}
	want := rpc.Build{Version: buildinfo.Version, Commit: buildinfo.Commit}
	gotBuild := rpc.Build{Version: got.Version, Commit: got.Commit}
	if rpc.SameBuild(gotBuild, want) {
		n.checked = true
		return nil
	}
	if !n.autoUpgrade {
		return nodeVersionMismatch(gotBuild, want)
	}
	if runtime.GOOS != "linux" {
		return contract.NewFault(contract.PreconditionFailed, "this CLI cannot push a node binary", "Run the upgrade from a linux build of eregion, or install the matching build on the node.")
	}
	if got.Arch != "" && runtime.GOARCH != got.Arch {
		return contract.NewFault(contract.PreconditionFailed, "this CLI is built for "+runtime.GOARCH+" but the node is "+got.Arch, "Run an eregion build for the node's architecture.")
	}
	if err := n.installOwnBuild(ctx); err != nil {
		return err
	}
	got, err = n.version(ctx)
	if err != nil {
		return err
	}
	if !rpc.SameBuild(gotBuild, want) {
		return nodeVersionMismatch(gotBuild, want)
	}
	n.checked = true
	return nil
}

func (n *nodeClient) version(ctx context.Context) (rpcVersionResult, error) {
	raw, err := n.callRaw(ctx, methodVersion, nil, rpcCallOptions{})
	if err != nil {
		return rpcVersionResult{}, err
	}
	var got rpcVersionResult
	if err := json.Unmarshal(raw, &got); err != nil || got.Version == "" {
		return rpcVersionResult{}, contract.NewFault(contract.Unavailable, "the node returned no version", "Check that /usr/local/bin/eregion is installed and runs on the node.")
	}
	return got, nil
}

func nodeVersionMismatch(got, want rpc.Build) *contract.Fault {
	fault := contract.NewFault(contract.NodeVersionMismatch,
		fmt.Sprintf("the node runs eregion %s (%s); this CLI is %s (%s)", got.Version, got.Commit, want.Version, want.Commit),
		"Run the same build on both sides, re-run with --auto-upgrade-nodes, or set auto_upgrade_nodes on the context.")
	fault.Details = map[string]any{"node": got, "client": want}
	return fault
}

func (n *nodeClient) installOwnBuild(ctx context.Context) error {
	executable, err := os.Executable()
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(executable) //nolint:gosec // G304: the running operator binary.
	if err != nil {
		return err
	}
	sum := sha256.Sum256(raw)
	dir := "/tmp/eregion-upgrade-" + hex.EncodeToString(sum[:6])
	if err := n.remote.Upload(ctx, dir, []host.RemoteFile{{Name: "eregion", Data: raw, Mode: 0o755}}); err != nil {
		return contract.NewFault(contract.Unavailable, "could not upload the node binary", err.Error())
	}
	defer func() { _ = n.remote.Remove(context.Background(), dir) }()
	if _, err := n.callRaw(ctx, methodUpgradeInstall, rpcInstallParams{Path: dir + "/eregion", SHA256: hex.EncodeToString(sum[:])}, rpcCallOptions{}); err != nil {
		return err
	}
	return nil
}

func (n *nodeClient) callRaw(ctx context.Context, method string, params any, opts rpcCallOptions) (json.RawMessage, error) {
	var rawParams json.RawMessage
	if params != nil {
		encoded, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		rawParams = encoded
	}
	request := rpc.Request{
		Schema: rpc.Schema, RequestID: rpc.NewRequestID(), Method: method, Params: rawParams,
		Secrets: opts.Secrets, DryRun: opts.DryRun, IdempotencyKey: opts.IdempotencyKey,
		Operator: n.operator, ClientVersion: buildinfo.Version, ClientCommit: buildinfo.Commit,
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return nil, err
	}
	payload = append(payload, '\n')
	pr, pw := io.Pipe()
	stderr := &rpcTail{max: 8 * 1024}
	var errOut io.Writer = stderr
	if n.stderr != nil {
		errOut = io.MultiWriter(stderr, n.stderr)
	}
	runErr := make(chan error, 1)
	go func() {
		err := n.remote.Run(ctx, nodeRPCCommand(n.operator), bytes.NewReader(payload), pw, errOut)
		_ = pw.CloseWithError(io.EOF)
		runErr <- err
	}()
	result, got, parseErr := readRPCResult(pr)
	err = <-runErr
	if got {
		if parseErr != nil {
			return nil, parseErr
		}
		if !result.OK {
			if result.Error != nil {
				return nil, result.Error
			}
			return nil, contract.NewFault(contract.Internal, "the node reported a failure without an error", "Report the command and binary version.")
		}
		return result.Data, nil
	}
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	detail := strings.TrimSpace(stderr.String())
	hint := "Re-run with --verbose to see the node's stderr. sudo may be refusing eregion node rpc."
	if detail != "" {
		hint = detail
	}
	return nil, contract.NewFault(contract.Unavailable, "eregion node rpc "+method+" on "+n.remoteTarget()+" ended without a result", hint)
}

func (n *nodeClient) remoteTarget() string {
	if n.route == nil {
		return "the node"
	}
	return n.route.User + "@" + n.route.Address
}

func readRPCResult(r io.Reader) (rpc.Result, bool, error) {
	reader := bufio.NewReaderSize(r, 256*1024)
	for {
		line, err := reader.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var envelope struct {
				rpc.Result
				Event json.RawMessage `json:"event"`
			}
			if jerr := json.Unmarshal(line, &envelope); jerr == nil {
				if envelope.Type == "result" {
					_, _ = io.Copy(io.Discard, reader)
					return envelope.Result, true, nil
				}
			}
		}
		if err != nil {
			return rpc.Result{}, false, nil
		}
	}
}

type rpcTail struct {
	buf bytes.Buffer
	max int
}

func (t *rpcTail) Write(p []byte) (int, error) {
	t.buf.Write(p)
	if over := t.buf.Len() - t.max; over > 0 {
		t.buf.Next(over)
	}
	return len(p), nil
}

func (t *rpcTail) String() string { return t.buf.String() }

func boolFlag(cmd *cobra.Command, name string) bool {
	if cmd == nil {
		return false
	}
	if flag := cmd.Flags().Lookup(name); flag != nil {
		value, _ := cmd.Flags().GetBool(name)
		return value
	}
	if flag := cmd.InheritedFlags().Lookup(name); flag != nil {
		value, _ := cmd.InheritedFlags().GetBool(name)
		return value
	}
	return false
}

func base64Encode(value []byte) string {
	return base64.StdEncoding.EncodeToString(value)
}

func base64Decode(value string) ([]byte, error) {
	return base64.StdEncoding.Strict().DecodeString(value)
}

// serveNodeRPC is `eregion node rpc`: one request on stdin, NDJSON on stdout,
// ending in exactly one result. It is hidden from the operator command tree.
func serveNodeRPC(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) (code int) {
	registry := &secrets.Registry{}
	defer func() {
		if recovered := recover(); recovered != nil {
			code = writeRPCResult(stdout, nil, contract.NewFault(contract.Internal, "node rpc failed internally", "Report the command and binary version."))
		}
	}()
	if runtime.GOOS != "linux" {
		return writeRPCResult(stdout, nil, contract.NewFault(contract.PreconditionFailed, "eregion node rpc runs on Linux nodes", "Run it on the node over SSH."))
	}
	request, err := rpc.DecodeRequest(stdin)
	if err != nil {
		return writeRPCResult(stdout, nil, err)
	}
	for _, value := range request.Secrets {
		registry.Add(value)
	}
	if os.Geteuid() != 0 {
		return writeRPCResult(stdout, nil, contract.NewFault(contract.PreconditionFailed, "eregion node rpc must run as root", "Run it through sudo -n /usr/local/bin/eregion node rpc."))
	}
	if request.Method == methodVersion {
		return writeRPCResult(stdout, rpcVersionResult{
			Version: buildinfo.Version, Commit: buildinfo.Commit, OS: runtime.GOOS, Arch: runtime.GOARCH,
		}, nil)
	}
	if request.Method == methodUpgradeInstall {
		var params rpcInstallParams
		if err := decodeRPCParams(request.Params, &params); err != nil {
			return writeRPCResult(stdout, nil, err)
		}
		if err := installNodeBinary(ctx, params); err != nil {
			return writeRPCResult(stdout, nil, err)
		}
		return writeRPCResult(stdout, map[string]bool{"installed": true}, nil)
	}
	if err := checkNodeClientVersion(request); err != nil {
		return writeRPCResult(stdout, nil, err)
	}

	logger := slog.New(slog.NewTextHandler(safeLogWriter{out: stderr, registry: registry}, &slog.HandlerOptions{Level: slog.LevelInfo}))
	app, closeRuntime, err := openNodeApp(ctx, logger, registry)
	if err != nil {
		return writeRPCResult(stdout, nil, err)
	}
	defer closeRuntime()
	data, err := dispatchNodeMethod(ctx, app, request)
	if request.Method == methodSecretExport {
		if exported, ok := data.(rpcSecretExportResult); ok {
			registry.Add(exported.ValueBase64)
		}
	}
	if err != nil {
		return writeRPCResult(stdout, nil, err)
	}
	return writeRPCResult(stdout, data, nil)
}

func openNodeApp(ctx context.Context, logger *slog.Logger, registry *secrets.Registry) (api.AppService, func(), error) {
	if err := host.LoadServiceEnv(host.ServiceEnvPath); err != nil {
		return api.AppService{}, nil, contract.NewFault(contract.Unavailable, "the node service configuration is not readable", err.Error())
	}
	dsn := os.Getenv("EREGION_DATABASE_URL")
	if strings.TrimSpace(dsn) == "" {
		return api.AppService{}, nil, contract.NewFault(contract.InputRequired, "the node has no control database URL", "Check "+host.ServiceEnvPath+" on the node.")
	}
	registry.Add(dsn)
	if parsed, err := url.Parse(dsn); err == nil && parsed.User != nil {
		password, _ := parsed.User.Password()
		registry.Add(password)
	}
	if parsed, err := controlplane.ParseDatabaseConfig(dsn); err == nil {
		registry.Add(parsed.ConnConfig.Password)
	}
	pool, err := controlplane.Open(ctx, dsn)
	if err != nil {
		return api.AppService{}, nil, err
	}
	if err := controlplane.Ready(ctx, pool); err != nil {
		pool.Close()
		return api.AppService{}, nil, err
	}
	a := &app{secrets: &secrets.Registry{}}
	parts, resolver, err := a.startExtensions(pool)
	if err != nil {
		pool.Close()
		return api.AppService{}, nil, err
	}
	run, err := controlplane.New(pool, logger, controlplane.WorkerConfig{
		Concurrency: 0, SecretResolver: resolver, LocalNodeName: os.Getenv("EREGION_NODE_NAME"),
	})
	if err != nil {
		pool.Close()
		return api.AppService{}, nil, err
	}
	appService := api.NewAppService(run.Service, parts.Vault, func(ctx context.Context) error { return controlplane.Ready(ctx, pool) })
	return appService, func() { run.Close(); pool.Close() }, nil
}

func checkNodeClientVersion(request rpc.Request) error {
	nodeBuild := rpc.Build{Version: buildinfo.Version, Commit: buildinfo.Commit}
	clientBuild := rpc.Build{Version: request.ClientVersion, Commit: request.ClientCommit}
	if rpc.SameBuild(nodeBuild, clientBuild) {
		return nil
	}
	fault := contract.NewFault(contract.NodeVersionMismatch,
		fmt.Sprintf("operator build %s (%s) does not match node build %s (%s)", request.ClientVersion, request.ClientCommit, buildinfo.Version, buildinfo.Commit),
		"Install the operator's build on the node, or run a matching operator.")
	fault.Details = map[string]any{"client": clientBuild, "node": nodeBuild}
	return fault
}

func dispatchNodeMethod(ctx context.Context, app api.AppService, request rpc.Request) (any, error) {
	actor, err := nodeOperator(request)
	if err != nil {
		return nil, err
	}
	switch request.Method {
	case methodTest:
		var params rpcSubmitParams
		if err := decodeRPCParams(request.Params, &params); err != nil {
			return nil, err
		}
		return app.SubmitDiagnostic(ctx, actor, params.Input, request.IdempotencyKey, request.DryRun)
	case methodSubmit:
		var params rpcSubmitParams
		if err := decodeRPCParams(request.Params, &params); err != nil {
			return nil, err
		}
		return app.PreviewOrSubmit(ctx, actor, params.Kind, params.Target, params.Input, request.IdempotencyKey, request.DryRun)
	case methodGet:
		var params rpcOperationParams
		if err := decodeRPCParams(request.Params, &params); err != nil {
			return nil, err
		}
		id := contract.OperationID(params.OperationID)
		if err := id.Validate(); err != nil {
			return nil, err
		}
		return app.GetOperation(ctx, actor, id)
	case methodEvents:
		var params rpcEventsParams
		if err := decodeRPCParams(request.Params, &params); err != nil {
			return nil, err
		}
		id := contract.OperationID(params.OperationID)
		if err := id.Validate(); err != nil {
			return nil, err
		}
		if params.Limit < 1 || params.Limit > 200 {
			return nil, contract.NewFault(contract.ValidationFailed, "event limit out of range", "Use an event limit from 1 to 200.")
		}
		return app.OperationEvents(ctx, actor, contract.Cursor{OperationID: id, After: params.After}, params.Limit)
	case methodCancel:
		var params rpcOperationParams
		if err := decodeRPCParams(request.Params, &params); err != nil {
			return nil, err
		}
		id := contract.OperationID(params.OperationID)
		if err := id.Validate(); err != nil {
			return nil, err
		}
		return app.CancelOperation(ctx, actor, id)
	case methodInventory:
		var params rpcInventoryParams
		if err := decodeRPCParams(request.Params, &params); err != nil {
			return nil, err
		}
		query, err := api.ParseInventoryQueryValues(params.Filters)
		if err != nil {
			return nil, err
		}
		if params.Collection != "" {
			query.Collection = params.Collection
		}
		if query.Collection == "" {
			return nil, contract.NewFault(contract.InputRequired, "inventory collection is required", "Use summary, nodes, apps, environments, releases, deployments, or changes.")
		}
		return app.Inventory(ctx, actor, query)
	case methodSecretSet:
		var params rpcSecretParams
		if err := decodeRPCParams(request.Params, &params); err != nil {
			return nil, err
		}
		value, err := base64Decode(request.Secrets["value_base64"])
		if err != nil {
			return nil, contract.NewFault(contract.ValidationFailed, "invalid secret value encoding", "Send value_base64 as standard base64.")
		}
		defer clearBytes(value)
		namespace, err := vault.NewNamespace(params.App, params.Environment, params.Name)
		if err != nil {
			return nil, err
		}
		return api.SetSecretValue(ctx, app.Vault, actor, namespace, value, params.ExpectedRevision)
	case methodSecretArchive:
		var params rpcSecretParams
		if err := decodeRPCParams(request.Params, &params); err != nil {
			return nil, err
		}
		namespace, err := vault.NewNamespace(params.App, params.Environment, params.Name)
		if err != nil {
			return nil, err
		}
		return api.ArchiveSecretValue(ctx, app.Vault, actor, namespace, params.ExpectedRevision)
	case methodSecretMetadata:
		var params rpcSecretParams
		if err := decodeRPCParams(request.Params, &params); err != nil {
			return nil, err
		}
		namespace, err := vault.NewNamespace(params.App, params.Environment, params.Name)
		if err != nil {
			return nil, err
		}
		return api.GetSecretMetadata(ctx, app.Vault, namespace)
	case methodSecretList:
		var params rpcSecretListParams
		if err := decodeRPCParams(request.Params, &params); err != nil {
			return nil, err
		}
		metadata, err := api.ListSecretMetadata(ctx, app.Vault, params.App, params.Environment, params.Limit)
		if err != nil {
			return nil, err
		}
		return map[string]any{"secrets": metadata}, nil
	case methodSecretExport:
		var params rpcSecretParams
		if err := decodeRPCParams(request.Params, &params); err != nil {
			return nil, err
		}
		namespace, err := vault.NewNamespace(params.App, params.Environment, params.Name)
		if err != nil {
			return nil, err
		}
		metadata, value, err := api.ExportSecretValue(ctx, app.Vault, namespace)
		if err != nil {
			return nil, err
		}
		encoded := base64Encode(value)
		clearBytes(value)
		return rpcSecretExportResult{Secret: metadata, ValueBase64: encoded}, nil
	case methodReady:
		if err := app.Ready(ctx); err != nil {
			return nil, err
		}
		return map[string]bool{"ready": true}, nil
	default:
		return nil, contract.NewFault(contract.Usage, "unknown node RPC method "+request.Method, "Use a method supported by this node build.")
	}
}

func nodeOperator(request rpc.Request) (contract.Actor, error) {
	subject := strings.TrimSpace(request.Operator)
	if subject == "" {
		return contract.Actor{}, contract.NewFault(contract.InputRequired, "the request names no operator", "Set operator to the SSH-authenticated admin user.")
	}
	actor := contract.Actor{Kind: "operator", Subject: subject}
	if err := actor.Validate(); err != nil {
		return contract.Actor{}, err
	}
	return actor, nil
}

func decodeRPCParams(raw json.RawMessage, target any) error {
	if len(raw) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return contract.NewFault(contract.InputRequired, "method params are required", "Send one JSON object of params.")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return contract.NewFault(contract.ValidationFailed, "invalid method params", err.Error())
	}
	return nil
}

func writeRPCResult(out io.Writer, data any, err error) int {
	result := rpc.Result{OK: err == nil}
	if err == nil {
		raw, marshalErr := json.Marshal(data)
		if marshalErr != nil {
			result.OK, result.Error = false, contract.Classify(marshalErr)
		} else {
			result.Data = raw
		}
	} else {
		result.Error = contract.Classify(err)
	}
	line, marshalErr := rpc.MarshalResult(result)
	if marshalErr != nil {
		line, _ = rpc.MarshalResult(rpc.Result{OK: false, Error: contract.NewFault(contract.Internal, "could not encode the node result", "Report the command and binary version.")})
	}
	_, _ = out.Write(line)
	return contract.ExitCode(err)
}

func installNodeBinary(ctx context.Context, params rpcInstallParams) error {
	if !filepath.IsAbs(params.Path) || !strings.HasPrefix(params.Path, "/tmp/eregion-") || strings.ContainsAny(params.Path, "\x00\r\n") {
		return contract.NewFault(contract.ValidationFailed, "the uploaded binary path is not allowed", "Push the binary through eregion --auto-upgrade-nodes.")
	}
	if len(params.SHA256) != 64 {
		return contract.NewFault(contract.ValidationFailed, "the binary checksum is missing", "Push the binary through eregion --auto-upgrade-nodes.")
	}
	if _, err := hex.DecodeString(params.SHA256); err != nil {
		return contract.NewFault(contract.ValidationFailed, "the binary checksum is malformed", "Push the binary through eregion --auto-upgrade-nodes.")
	}
	source, err := os.Open(params.Path) //nolint:gosec // G304: bounded to /tmp/eregion-* above.
	if err != nil {
		return contract.NewFault(contract.PreconditionFailed, "the uploaded binary is not readable", "Push the binary again.")
	}
	defer source.Close()
	info, err := source.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return contract.NewFault(contract.PreconditionFailed, "the uploaded binary is not a regular file", "Push the binary again.")
	}
	stage, err := os.CreateTemp(nodeBinaryPathDir(), ".eregion.new-")
	if err != nil {
		return contract.NewFault(contract.PreconditionFailed, "could not stage the node binary", "Check /usr/local/bin permissions.")
	}
	stagePath := stage.Name()
	defer func() { _ = os.Remove(stagePath) }()
	hash := sha256.New()
	if _, err := io.Copy(io.MultiWriter(stage, hash), source); err != nil {
		_ = stage.Close()
		return contract.NewFault(contract.Unavailable, "could not write the staged node binary", "Check free space.")
	}
	if err := stage.Close(); err != nil {
		return contract.NewFault(contract.Unavailable, "could not close the staged node binary", "Check the filesystem.")
	}
	if got := hex.EncodeToString(hash.Sum(nil)); got != params.SHA256 {
		return contract.NewFault(contract.PreconditionFailed, "the uploaded binary does not match its sha256", "Do not retry. Push the binary again from a verified build.")
	}
	if err := os.Chmod(stagePath, 0o755); err != nil {
		return contract.NewFault(contract.PreconditionFailed, "could not make the node binary executable", "Check /usr/local/bin permissions.")
	}
	if os.Geteuid() == 0 {
		_ = os.Chown(stagePath, 0, 0)
	}
	if err := os.Rename(stagePath, nodeBinaryPath); err != nil {
		return contract.NewFault(contract.PreconditionFailed, "could not install the node binary", "Check /usr/local/bin permissions.")
	}
	command := exec.CommandContext(ctx, "systemctl", "restart", "eregion-api.service", "eregion-worker.service")
	if output, err := command.CombinedOutput(); err != nil {
		return contract.NewFault(contract.Unavailable, "the node binary was installed but the services did not restart", strings.TrimSpace(string(output)))
	}
	return nil
}

func nodeBinaryPathDir() string {
	return filepath.Dir(nodeBinaryPath)
}

func clearBytes(value []byte) {
	for i := range value {
		value[i] = 0
	}
}
