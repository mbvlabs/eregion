package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/transport"
)

// NodeTransports resolves the authenticated transport that reaches one node.
// It is the single place a worker, and any extension that dispatches node work
// (telemetry, resources), learns how to reach a node:
//
//	tr, err := nodes.TransportFor(ctx, "builder-1")
//	response, err := tr.Call(ctx, request, onEvent)
//
// The contract:
//
//   - The node is looked up in PostgreSQL by name. A node that is pending or
//     failed enrollment is refused with a placement_violation (reason
//     not_enrolled): a record is never a credential.
//   - An enrolled node is reached over pinned SSH (optionally inside the
//     userspace WireGuard tunnel once its peers were reconciled). The host key
//     is the one the operator pinned at enrollment, and the handshake must
//     report the enrolled node name; any other host is refused before a byte
//     of a request is sent.
//   - A node that was only registered is served by the worker's own configured
//     transport (local dispatch or the env-configured SSH transport), and only
//     when that transport authenticates as exactly that node.
//   - The returned transport is shared and safe for concurrent use. The caller
//     never closes it; the resolver owns its tunnel.
//   - Placement (roles, capabilities) is a separate check: use the Service's
//     placement helpers before dispatching work that needs a role.
type NodeTransports interface {
	TransportFor(ctx context.Context, nodeName string) (node.Transport, error)
}

// ResolverConfig configures how enrolled nodes are reached.
type ResolverConfig struct {
	// SSHIdentityFile is the control plane's SSH private key, authorized on every
	// enrolled node. Without it only the fallback transport is available.
	SSHIdentityFile string
	// WireGuardKeyDir holds the control plane's per-node WireGuard private keys
	// (mode 0600, one file per node). They never enter the database.
	WireGuardKeyDir string
	// WireGuardControlAddress is the control plane's tunnel address (default
	// 10.77.0.1).
	WireGuardControlAddress string
	// WireGuardSubnet is the private network node addresses are allocated from
	// (default 10.77.0.0/24).
	WireGuardSubnet string
	// LockDir holds the per-key WireGuard locks (default XDG_RUNTIME_DIR).
	LockDir string
	// Timeout and PollInterval tune the SSH transport; zero uses its defaults.
	Timeout      time.Duration
	PollInterval time.Duration
	// Logf receives tunnel diagnostics. The keys never reach it.
	Logf func(format string, args ...any)
}

// Defaults for the private network.
const (
	DefaultWireGuardControlAddress = "10.77.0.1"
	DefaultWireGuardSubnet         = "10.77.0.0/24"
	wireGuardListenPort            = 51820
)

func (c ResolverConfig) controlAddress() string {
	if c.WireGuardControlAddress != "" {
		return c.WireGuardControlAddress
	}
	return DefaultWireGuardControlAddress
}

func (c ResolverConfig) subnet() string {
	if c.WireGuardSubnet != "" {
		return c.WireGuardSubnet
	}
	return DefaultWireGuardSubnet
}

type cachedTransport struct {
	key       string
	transport node.Transport
	closer    func()
}

// NodeResolver is the PostgreSQL-backed NodeTransports.
type NodeResolver struct {
	pool     *pgxpool.Pool
	config   ResolverConfig
	fallback func() node.Transport

	mu    sync.Mutex
	cache map[string]*cachedTransport
}

var _ NodeTransports = (*NodeResolver)(nil)

// NewNodeResolver builds a resolver. fallback returns the worker's own
// configured transport (nil when the process has none); it is read lazily so
// a transport installed after construction is honoured.
func NewNodeResolver(pool *pgxpool.Pool, config ResolverConfig, fallback func() node.Transport) *NodeResolver {
	return &NodeResolver{pool: pool, config: config, fallback: fallback, cache: map[string]*cachedTransport{}}
}

// Close releases every cached tunnel.
func (r *NodeResolver) Close() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for name, entry := range r.cache {
		entry.closer()
		delete(r.cache, name)
	}
}

// TransportFor implements NodeTransports.
func (r *NodeResolver) TransportFor(ctx context.Context, nodeName string) (node.Transport, error) {
	rec, err := loadNodeConn(ctx, r.pool, nodeName)
	if err != nil {
		return nil, err
	}
	switch rec.Enrollment {
	case platform.EnrollmentPending, platform.EnrollmentFailed:
		return nil, placementViolation(ReasonNotEnrolled, rec.Name, fmt.Sprintf("node %q is not enrolled (%s)", rec.Name, rec.Enrollment),
			"Finish enrollment with platform nodes enroll or platform nodes retry.", nil)
	case platform.EnrollmentEnrolled:
		if rec.Transport == "ssh" {
			return r.cached(rec, false)
		}
	}
	return r.fallbackFor(rec.Name)
}

// Direct returns an uncached transport over the node's direct SSH address,
// ignoring any tunnel. Enrollment and tunnel repair use it, so a broken tunnel
// cannot lock the control plane out of the node. The caller closes it.
func (r *NodeResolver) Direct(rec nodeConn) (node.Transport, func(), error) {
	return r.build(rec, false)
}

// fallbackFor returns the worker's own transport when it authenticates as the
// requested node.
func (r *NodeResolver) fallbackFor(name string) (node.Transport, error) {
	var tr node.Transport
	if r.fallback != nil {
		tr = r.fallback()
	}
	if tr == nil {
		return nil, contract.NewFault(contract.Unavailable, "node transport is not configured", "Run this work in a worker configured with a node transport, or enroll the node.")
	}
	provider, ok := tr.(interface{ NodeIdentity() string })
	identity := ""
	if ok {
		identity = strings.TrimSpace(provider.NodeIdentity())
	}
	if identity == "" {
		return nil, placementViolation(ReasonTransportUnpinned, name, "node transport identity is not pinned", "Configure the transport's expected node name so placement is verified, not inferred from a hostname.", nil)
	}
	if identity != name {
		return nil, placementViolation(ReasonIdentityMismatch, name, fmt.Sprintf("this worker's authenticated transport identifies %q, not %q", identity, name),
			"Enroll the node so the control plane reaches it over its own pinned SSH transport. Hostnames are not authorization.", map[string]any{"transport_identity": identity})
	}
	return tr, nil
}

// cached returns the shared transport of an enrolled node, rebuilding it when
// the node record's connection facts changed.
func (r *NodeResolver) cached(rec nodeConn, direct bool) (node.Transport, error) {
	key := connectionKey(rec, r.usesTunnel(rec, direct))
	r.mu.Lock()
	defer r.mu.Unlock()
	if entry, ok := r.cache[rec.Name]; ok {
		if entry.key == key {
			return entry.transport, nil
		}
		entry.closer()
		delete(r.cache, rec.Name)
	}
	built, closer, err := r.build(rec, direct)
	if err != nil {
		return nil, err
	}
	r.cache[rec.Name] = &cachedTransport{key: key, transport: built, closer: closer}
	return built, nil
}

func connectionKey(rec nodeConn, tunnel bool) string {
	sum := sha256.Sum256([]byte(strings.Join([]string{rec.SSHAddress, rec.SSHUser, rec.HostKey, rec.WGAddress, rec.WGEndpoint, rec.WGPublicKey, rec.ControlWGPublicKey, fmt.Sprint(tunnel)}, "\x00")))
	return hex.EncodeToString(sum[:])
}

// usesTunnel reports whether SSH should run inside the node's WireGuard
// tunnel: only once the node's peers were applied and a key directory exists.
func (r *NodeResolver) usesTunnel(rec nodeConn, direct bool) bool {
	return !direct && rec.WGEnabled && rec.WGState == platform.WireGuardApplied &&
		r.config.WireGuardKeyDir != "" && rec.WGAddress != "" && rec.WGPublicKey != "" && rec.WGEndpoint != ""
}

func (r *NodeResolver) build(rec nodeConn, direct bool) (node.Transport, func(), error) {
	noop := func() {}
	if r.config.SSHIdentityFile == "" {
		return nil, noop, contract.NewFault(contract.PreconditionFailed, "the control plane has no SSH identity for enrolled nodes", "Set EREGION_NODE_SSH_IDENTITY to the control plane's SSH key, which is authorized on every node.")
	}
	if rec.SSHAddress == "" || rec.SSHUser == "" || rec.HostKey == "" {
		return nil, noop, contract.NewFault(contract.PreconditionFailed, "node connection facts are incomplete", "Re-run the enrollment with an address, user, and pinned host key.")
	}
	key, err := transport.ParseHostKey(rec.HostKey)
	if err != nil {
		return nil, noop, contract.NewFault(contract.PreconditionFailed, "the pinned host key of the node is unreadable", "Re-run the enrollment with the node's host key.")
	}
	address := rec.SSHAddress
	config := transport.SSHConfig{
		User:         rec.SSHUser,
		IdentityFile: r.config.SSHIdentityFile,
		ExpectedNode: rec.Name,
		Timeout:      r.config.Timeout,
		PollInterval: r.config.PollInterval,
	}
	closer := noop
	if r.usesTunnel(rec, direct) {
		_, port, err := net.SplitHostPort(rec.SSHAddress)
		if err != nil {
			return nil, noop, contract.NewFault(contract.PreconditionFailed, "node SSH address is invalid", "Re-run the enrollment.")
		}
		address = net.JoinHostPort(rec.WGAddress, port)
		dialer, err := r.tunnel(rec)
		if err != nil {
			return nil, noop, err
		}
		config.Dialer = dialer
		closer = func() { _ = dialer.Close() }
	}
	keys := transport.NewHostKeys()
	keys.Pin(address, key)
	config.Address, config.HostKeys = address, keys
	built, err := transport.NewSSHTransport(config)
	if err != nil {
		closer()
		return nil, noop, contract.NewFault(contract.PreconditionFailed, "the SSH transport for the node is not usable", err.Error())
	}
	return built, closer, nil
}

// tunnel builds the userspace WireGuard dialer for one node with the control
// plane's per-node key.
func (r *NodeResolver) tunnel(rec nodeConn) (*transport.WireGuardDialer, error) {
	key, err := transport.ReadWireGuardKey(r.controlKeyPath(rec.Name))
	if err != nil {
		return nil, contract.NewFault(contract.PreconditionFailed, "the control plane WireGuard key for the node is unreadable", err.Error())
	}
	local, err := netip.ParseAddr(r.config.controlAddress())
	if err != nil {
		return nil, contract.NewFault(contract.ValidationFailed, "the control plane WireGuard address is invalid", "Check EREGION_NODE_WG_ADDRESS.")
	}
	nodeAddress, err := netip.ParseAddr(rec.WGAddress)
	if err != nil {
		return nil, contract.NewFault(contract.PreconditionFailed, "the node tunnel address is invalid", "Re-run the enrollment.")
	}
	peer, err := transport.ParseWireGuardPublicKey(rec.WGPublicKey)
	if err != nil {
		return nil, contract.NewFault(contract.PreconditionFailed, "the node WireGuard public key is invalid", "Reconcile the node's WireGuard keys.")
	}
	lockDir := r.config.LockDir
	if lockDir == "" {
		lockDir = os.Getenv("XDG_RUNTIME_DIR")
	}
	if lockDir == "" {
		lockDir = os.TempDir()
	}
	dialer, err := transport.NewWireGuardDialer(transport.WireGuardConfig{
		PrivateKey: key, Address: local, NodeAddress: nodeAddress, PeerPublicKey: peer,
		Endpoint: rec.WGEndpoint, LockDir: lockDir, Logf: r.config.Logf,
	})
	if err != nil {
		if errors.Is(err, transport.ErrKeyInUse) {
			return nil, contract.NewFault(contract.Unavailable, "another control-plane process on this host holds the node's WireGuard key", "Run API and workers in one process, or retry shortly.")
		}
		return nil, contract.NewFault(contract.PreconditionFailed, "the WireGuard tunnel to the node is not usable", err.Error())
	}
	return dialer, nil
}

func (r *NodeResolver) controlKeyPath(nodeName string) string {
	return filepath.Join(r.config.WireGuardKeyDir, nodeName+".key")
}

// ControlPublicKey returns the control plane's WireGuard public key for one
// node, creating the key (a 0600 file in the key directory) the first time.
// The private key never leaves the file.
func (r *NodeResolver) ControlPublicKey(nodeName string) (string, error) {
	if r.config.WireGuardKeyDir == "" {
		return "", contract.NewFault(contract.PreconditionFailed, "the WireGuard key directory is not configured", "Set EREGION_NODE_WG_KEY_DIR to a private directory on the control host.")
	}
	if err := os.MkdirAll(r.config.WireGuardKeyDir, 0o700); err != nil {
		return "", contract.NewFault(contract.Unavailable, "the WireGuard key directory is not writable", "Check EREGION_NODE_WG_KEY_DIR permissions.")
	}
	path := r.controlKeyPath(nodeName)
	if key, err := transport.ReadWireGuardKey(path); err == nil {
		return key.PublicKey().String(), nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", contract.NewFault(contract.PreconditionFailed, "the control plane WireGuard key for the node is unreadable", err.Error())
	}
	key, err := transport.GenerateWireGuardKey()
	if err != nil {
		return "", err
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: a node-named file in the operator's private key directory.
	if err != nil {
		if os.IsExist(err) {
			return r.ControlPublicKey(nodeName)
		}
		return "", contract.NewFault(contract.Unavailable, "the WireGuard key could not be created", "Check EREGION_NODE_WG_KEY_DIR permissions.")
	}
	if _, err := file.WriteString(key.Encode() + "\n"); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return key.PublicKey().String(), nil
}
