package telemetry

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/telemetry/remote"
)

// Status values reported by the production StatusProvider. They describe what
// was actually observed from the placement table and, optionally, a TCP probe of
// the private read endpoints. They never claim service health.
const (
	StatusAbsent      = "absent"
	StatusUnknown     = "unknown"
	StatusReachable   = "reachable"
	StatusUnreachable = "unreachable"
)

// NodeQueryer is the read-only database subset the production adapters need.
// *pgxpool.Pool satisfies it; tests use a small fake.
type NodeQueryer interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// ConfigureProduction builds the database- and transport-backed adapters and
// registers the durable telemetry actions on reg. It does not add migrations and
// never installs anything locally: every mutation is dispatched to the target
// node through transport.
func ConfigureProduction(reg *platform.Registry, pool *pgxpool.Pool, transport node.Transport) error {
	if pool == nil {
		return contract.NewFault(contract.ValidationFailed, "telemetry production runtime needs a database pool", "Open the external PostgreSQL pool before registering telemetry actions.")
	}
	if transport == nil {
		return contract.NewFault(contract.ValidationFailed, "telemetry production runtime needs a node transport", "Configure the pinned node transport before registering telemetry actions; there is no local fallback.")
	}
	if reg == nil {
		reg = platform.Default()
	}
	dispatch := &transportDispatch{transport: transport}
	return Configure(reg, RuntimeConfig{
		Placement: &tablePlacement{pool: pool},
		Dispatch:  dispatch,
		Status:    newTableStatus(pool, tcpReachable),
	})
}

// tablePlacement reads authoritative placement from eregion_platform_nodes.
// The table stores the node address but no host-key column, so the dispatch
// identity binds the recorded address. SSH host-key pinning remains the
// transport's responsibility (EREGION_NODE_SSH_KNOWN_HOSTS).
type tablePlacement struct {
	pool NodeQueryer
}

func (p *tablePlacement) ResolveNode(ctx context.Context, name string) (Placement, error) {
	if p == nil || p.pool == nil {
		return Placement{}, contract.NewFault(contract.Unavailable, "telemetry placement is not configured", "Configure the production telemetry runtime before submitting an install.")
	}
	var roles []string
	var address string
	err := p.pool.QueryRow(ctx, `SELECT roles, address FROM eregion_platform_nodes WHERE name=$1`, name).Scan(&roles, &address)
	if errors.Is(err, pgx.ErrNoRows) {
		return Placement{}, contract.NewFault(contract.NotFound, "node not found", "Register the node's placement facts before installing telemetry.")
	}
	if err != nil {
		return Placement{}, contract.NewFault(contract.Unavailable, "could not read node placement", "Check the PostgreSQL connection; do not assume the node is absent.")
	}
	controlPlane := hasRole(roles, platform.RoleControlPlane)
	if controlPlane && hasRole(roles, RoleTelemetry) {
		return Placement{}, contract.NewFault(contract.PreconditionFailed, "telemetry cannot run on a control-plane node", "Remove the telemetry role from the control plane and enroll a dedicated telemetry node.")
	}
	host, _, err := net.SplitHostPort(strings.TrimSpace(address))
	if err != nil {
		return Placement{}, contract.NewFault(contract.PreconditionFailed, "node has no usable private address", "Record the node address as a literal private IP and port, for example 10.77.0.10:22.")
	}
	ip := net.ParseIP(host)
	if ip == nil || !privateAddress(ip) {
		return Placement{}, contract.NewFault(contract.PreconditionFailed, "node address is not private", "Record an RFC 1918 or IPv6 ULA address for the node.")
	}
	return Placement{
		Name: name, PrivateAddress: ip.String(), HostID: hostIdentity(address),
		Roles: roles, IsMain: controlPlane, IsControlPlane: controlPlane,
	}, nil
}

// hostIdentity derives the dispatch identity from the recorded node address.
// It is a change detector and placement binding, not a substitute for pinned
// SSH host keys.
func hostIdentity(address string) string {
	sum := sha256.Sum256([]byte("eregion/telemetry/node-address/v1\x00" + strings.TrimSpace(address)))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// transportDispatch applies telemetry steps through the existing node protocol.
// The protocol has no arbitrary command or file request, so the install plan is
// expressed as the narrow telemetry-* step family in internal/telemetry/remote.
type transportDispatch struct {
	transport node.Transport
}

var _ Dispatch = (*transportDispatch)(nil)
var _ OperationDispatch = (*transportDispatch)(nil)

func (d *transportDispatch) Open(context.Context, TargetIdentity) (NodeExecutor, error) {
	return nil, contract.NewFault(contract.Unavailable, "telemetry dispatch needs a durable operation identity", "Execute the action through the platform worker so node effects carry an operation ID and attempt.")
}

func (d *transportDispatch) OpenOperation(_ context.Context, identity TargetIdentity, operationID contract.OperationID, attempt uint32, root string) (NodeExecutor, error) {
	if d == nil || d.transport == nil {
		return nil, contract.NewFault(contract.Unavailable, "telemetry node transport is not configured", "Configure the pinned node transport before submitting telemetry work.")
	}
	if err := identity.Validate(); err != nil {
		return nil, err
	}
	if err := operationID.Validate(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(root) || filepath.Clean(root) == string(filepath.Separator) {
		return nil, contract.NewFault(contract.ValidationFailed, "telemetry install root must be an absolute non-root directory", "Use a root-owned absolute directory beneath /var/lib/eregion.")
	}
	if err := nodeTransportIdentity(d.transport, identity.Node); err != nil {
		return nil, err
	}
	return &transportExecutor{dispatch: d, identity: identity, operationID: operationID, attempt: attempt, root: filepath.Clean(root)}, nil
}

// nodeTransportIdentity uses the optional identity method implemented by the
// local and pinned SSH transports. A transport that does not report a name is
// accepted only because its host key/connection configuration is still the
// operator's pinned choice; the limitation is documented in docs/telemetry.md.
func nodeTransportIdentity(transport node.Transport, expected string) error {
	type named interface{ NodeIdentity() string }
	name, ok := transport.(named)
	if !ok {
		return nil
	}
	if got := name.NodeIdentity(); got != "" && got != expected {
		return contract.NewFault(contract.PreconditionFailed, "node transport identity does not match telemetry placement", fmt.Sprintf("The transport identifies %q, not %q. Hostnames are not authorization.", got, expected))
	}
	return nil
}

type transportExecutor struct {
	dispatch    *transportDispatch
	identity    TargetIdentity
	operationID contract.OperationID
	attempt     uint32
	root        string

	mu         sync.Mutex
	mkdirSteps int
	fileSteps  int
	argvSteps  int
}

func (e *transportExecutor) Identity() TargetIdentity { return e.identity }

func (e *transportExecutor) MkdirAll(ctx context.Context, path string, mode os.FileMode) error {
	e.mu.Lock()
	e.mkdirSteps++
	index := e.mkdirSteps
	e.mu.Unlock()
	payload := remoteMkdirPayload(e.root, path, uint32(mode), e.stepID("mkdir", index))
	return e.call(ctx, e.stepID("mkdir", index), payload)
}

func (e *transportExecutor) WriteFile(ctx context.Context, path string, data []byte, mode os.FileMode) error {
	e.mu.Lock()
	e.fileSteps++
	index := e.fileSteps
	e.mu.Unlock()
	step := e.stepID("file", index)
	payload := remoteFilePayload(e.root, path, data, uint32(mode), step)
	return e.call(ctx, step, payload)
}

func (e *transportExecutor) Run(ctx context.Context, name string, args ...string) error {
	e.mu.Lock()
	e.argvSteps++
	index := e.argvSteps
	e.mu.Unlock()
	step := e.stepID("argv", index)
	payload := remoteArgvPayload(e.root, append([]string{name}, args...), step)
	return e.call(ctx, step, payload)
}

func (e *transportExecutor) stepID(kind string, index int) string {
	return fmt.Sprintf("telemetry-%s-%02d-v1", kind, index)
}

func (e *transportExecutor) call(ctx context.Context, step string, payload any) error {
	raw, err := marshalStep(payload)
	if err != nil {
		return contract.NewFault(contract.Internal, "could not encode telemetry node step", "Report the command and binary version.")
	}
	exec := engineExecution(e.operationID, e.attempt, step)
	response, err := e.dispatch.transport.Call(ctx, node.Request{
		Method: node.MethodRun, RequestID: exec.RequestID, Execution: exec, Payload: raw,
	}, nil)
	if err != nil {
		if node.IsPermanentTransport(err) {
			return contract.NewFault(contract.PreconditionFailed, "the node transport refused the telemetry step", "Check the pinned node identity, host key, and node protocol version.")
		}
		return contract.NewFault(contract.Unavailable, "telemetry node step did not complete", "Retry the operation; the node asks for status before running the same effect again.")
	}
	switch response.Status {
	case engine.StatusCompleted:
		return nil
	case engine.StatusFailed:
		code := contract.Code(response.ErrorCode)
		if code == "" {
			code = contract.Internal
		}
		detail := response.Detail
		if detail == "" {
			detail = "telemetry node step failed"
		}
		return contract.NewFault(code, "telemetry node step failed: "+detail, "Inspect operation events and node state before resubmitting.")
	default:
		return contract.NewFault(contract.Unavailable, "telemetry node task is still running", "Retry the operation; the existing effect is attached rather than started twice.")
	}
}

// tableStatus reports only facts it can observe. Placement comes from the
// database; reachability comes from an optional TCP probe of the private read
// endpoints. There is no synthetic "healthy" value.
type tableStatus struct {
	pool  NodeQueryer
	now   func() time.Time
	probe func(context.Context, string) bool
}

func newTableStatus(pool NodeQueryer, probe func(context.Context, string) bool) *tableStatus {
	return &tableStatus{pool: pool, now: time.Now, probe: probe}
}

func (s *tableStatus) TelemetryStatus(ctx context.Context) (RuntimeStatus, error) {
	if s == nil || s.pool == nil {
		return RuntimeStatus{Configured: false, State: StatusAbsent, Retention: DefaultRetention(), Message: "No telemetry node is configured. Operation history remains available."}, nil
	}
	var name, address string
	var roles []string
	err := s.pool.QueryRow(ctx, `SELECT name, address, roles FROM eregion_platform_nodes
        WHERE 'telemetry' = ANY(roles) ORDER BY name LIMIT 1`).Scan(&name, &address, &roles)
	if errors.Is(err, pgx.ErrNoRows) {
		return RuntimeStatus{Configured: false, State: StatusAbsent, Retention: DefaultRetention(), Message: "No telemetry node is configured. Operation history remains available."}, nil
	}
	if err != nil {
		return RuntimeStatus{Configured: false, State: StatusUnknown, Retention: DefaultRetention(), Message: "Telemetry placement could not be read."},
			contract.NewFault(contract.Unavailable, "could not read telemetry placement", "Check the PostgreSQL connection; do not render this as absent or healthy.")
	}
	result := RuntimeStatus{
		Configured: true, Node: name, State: StatusUnknown, Retention: DefaultRetention(),
		Traces: false, Observed: false,
		Message: "Telemetry placement is configured but service health has not been observed.",
	}
	host, _, splitErr := net.SplitHostPort(strings.TrimSpace(address))
	if splitErr == nil {
		if ip := net.ParseIP(host); ip != nil && privateAddress(ip) {
			result.PrivateAddress = ip.String()
			result.IngestEndpoint = net.JoinHostPort(ip.String(), "4317")
			result.HTTPEndpoint = net.JoinHostPort(ip.String(), "4318")
			result.Endpoints = map[string]string{
				"metrics": net.JoinHostPort(ip.String(), "8427"),
				"logs":    net.JoinHostPort(ip.String(), "9427"),
				"grafana": net.JoinHostPort(ip.String(), "3000"),
			}
		}
	}
	if result.PrivateAddress == "" {
		result.Message = "Telemetry node is configured but has no private address to observe."
		return result, nil
	}
	if s.probe == nil {
		return result, nil
	}
	now := time.Now
	if s.now != nil {
		now = s.now
	}
	checked := now().UTC()
	result.CheckedAt = &checked
	result.Observed = true
	reachable := false
	for _, endpoint := range []string{result.Endpoints["metrics"], result.Endpoints["logs"]} {
		if s.probe(ctx, endpoint) {
			reachable = true
			break
		}
	}
	if reachable {
		result.State = StatusReachable
		result.Message = "At least one telemetry read endpoint accepted a TCP connection. This is reachability, not service health."
	} else {
		result.State = StatusUnreachable
		result.Message = "Telemetry read endpoints did not accept a TCP connection."
	}
	return result, nil
}

func tcpReachable(ctx context.Context, endpoint string) bool {
	dialer := net.Dialer{Timeout: time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", endpoint)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func engineExecution(id contract.OperationID, attempt uint32, step string) engine.Execution {
	requestID := make([]byte, 16)
	if _, err := rand.Read(requestID); err != nil {
		requestID = []byte("telemetry-request")
	}
	return engine.Execution{OperationID: id, Attempt: attempt, Step: step, RequestID: hex.EncodeToString(requestID)}
}

func marshalStep(value any) (json.RawMessage, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(raw), nil
}

func remoteMkdirPayload(root, path string, mode uint32, step string) remote.MkdirPayload {
	return remote.MkdirPayload{Step: step, Root: root, Path: path, Mode: mode}
}

func remoteFilePayload(root, path string, data []byte, mode uint32, step string) remote.FilePayload {
	sum := sha256.Sum256(data)
	return remote.FilePayload{
		Step: step, Root: root, Path: path, Mode: mode,
		ContentBase64: base64.StdEncoding.EncodeToString(data),
		SHA256:        hex.EncodeToString(sum[:]),
	}
}

func remoteArgvPayload(root string, argv []string, step string) remote.ArgvPayload {
	return remote.ArgvPayload{Step: step, Root: root, Argv: argv, TimeoutSeconds: 600}
}
