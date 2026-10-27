package transport

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/secrets"
	"golang.org/x/crypto/curve25519"
	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

// TunnelMTU is the netstack interface MTU. WireGuard over IPv6 adds 80 bytes,
// so 1380 fits a 1492-byte PPPoE path. Netstack derives the TCP MSS it
// advertises from it, so the node's replies fit without touching its wg0.
const TunnelMTU = 1380

// DefaultKeepalive is the persistent keepalive. It keeps NAT mappings open
// while a call waits for a detached node task.
const DefaultKeepalive = 25 * time.Second

// DefaultHandshakeTimeout bounds the first handshake of a tunnel. WireGuard
// retransmits a handshake every five seconds, so this is two attempts.
const DefaultHandshakeTimeout = 10 * time.Second

// DefaultProbeInterval is how often the liveness probe checks the tunnel.
const DefaultProbeInterval = 2 * time.Second

// DefaultLossAfter is how long a tunnel may receive nothing before it is
// declared lost. It matches WireGuard's own keepalive-plus-rekey timeout.
const DefaultLossAfter = 15 * time.Second

// ErrTunnelDown marks a tunnel that did not come up. No byte of a request
// passed it, so nothing was dispatched.
var ErrTunnelDown = errors.New("the WireGuard tunnel is down")

// ErrTunnelLost marks a connection whose tunnel was lost after it was open.
// The SSH transport reports it as a lost stream: the outcome is unknown, and a
// lost tunnel never means the node task stopped.
var ErrTunnelLost = errors.New("the WireGuard tunnel was lost")

// ErrDialerClosed reports a dial on a dialer that was shut down.
var ErrDialerClosed = errors.New("the WireGuard dialer is closed")

// ErrKeyInUse reports a second process on this host that wants the same
// WireGuard key. The node knows one endpoint per peer, so two processes with
// one key steal the peer from each other and both lose their streams.
var ErrKeyInUse = errors.New("another process on this host holds the WireGuard key")

// TunnelDownError reports a tunnel whose handshake did not complete. A wrong
// key looks exactly like a node that is down, because WireGuard is silent to
// an unauthenticated peer, so the error is retryable and names what to check.
type TunnelDownError struct {
	Endpoint      string
	PeerPublicKey string
	Timeout       time.Duration
	Cause         error
}

func (e *TunnelDownError) Error() string {
	message := fmt.Sprintf("the WireGuard tunnel to %s (peer %s) did not come up within %s; no request was sent", e.Endpoint, e.PeerPublicKey, e.Timeout)
	if e.Cause != nil {
		message += ": " + e.Cause.Error()
	}
	return message
}

// Unwrap exposes the down classification and the cause.
func (e *TunnelDownError) Unwrap() []error {
	if e.Cause == nil {
		return []error{ErrTunnelDown}
	}
	return []error{ErrTunnelDown, e.Cause}
}

// IsTunnelDown reports whether a dial failed because the tunnel did not come
// up. Such a failure never dispatched a request.
func IsTunnelDown(err error) bool { return errors.Is(err, ErrTunnelDown) }

// WireGuardKey is a WireGuard private key. Every printed form of it is
// redacted, so a configuration can be logged without leaking the key.
type WireGuardKey struct {
	raw [32]byte
	set bool
}

// GenerateWireGuardKey returns a new private key, clamped as wg genkey does.
func GenerateWireGuardKey() (WireGuardKey, error) {
	var key WireGuardKey
	if _, err := rand.Read(key.raw[:]); err != nil {
		return WireGuardKey{}, err
	}
	key.raw[0] &= 248
	key.raw[31] = (key.raw[31] & 127) | 64
	key.set = true
	return key, nil
}

// ParseWireGuardKey parses a private key in wg base64 form. The error never
// contains the input.
func ParseWireGuardKey(text string) (WireGuardKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(text))
	if err != nil || len(raw) != curve25519.ScalarSize {
		return WireGuardKey{}, errors.New("the WireGuard private key is not a base64 32-byte key")
	}
	var key WireGuardKey
	copy(key.raw[:], raw)
	key.set = true
	return key, nil
}

// ReadWireGuardKey reads a private key file. The file must be a regular file
// that neither the group nor others can access, the same rule the SSH
// identity follows. The path is not a secret; the content is.
func ReadWireGuardKey(path string) (WireGuardKey, error) {
	if path == "" {
		return WireGuardKey{}, errors.New("the WireGuard private key file is required")
	}
	info, err := os.Stat(path)
	if err != nil {
		return WireGuardKey{}, fmt.Errorf("read the WireGuard private key: %w", err)
	}
	if !info.Mode().IsRegular() {
		return WireGuardKey{}, fmt.Errorf("the WireGuard private key %s is not a regular file", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return WireGuardKey{}, fmt.Errorf("the WireGuard private key %s must not be accessible by group or others", path)
	}
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the operator's own key file.
	if err != nil {
		return WireGuardKey{}, fmt.Errorf("read the WireGuard private key: %w", err)
	}
	key, err := ParseWireGuardKey(string(raw))
	if err != nil {
		return WireGuardKey{}, fmt.Errorf("the WireGuard private key %s is not a base64 32-byte key", path)
	}
	return key, nil
}

// Encode returns the wg base64 form for writing the key to its protected file.
// It is the only way to persist a key; never log or return the result.
func (k WireGuardKey) Encode() string { return base64.StdEncoding.EncodeToString(k.raw[:]) }

// IsZero reports an unset key.
func (k WireGuardKey) IsZero() bool { return !k.set }

// PublicKey derives the public key.
func (k WireGuardKey) PublicKey() WireGuardPublicKey {
	public, err := curve25519.X25519(k.raw[:], curve25519.Basepoint)
	if err != nil {
		return WireGuardPublicKey{}
	}
	var key WireGuardPublicKey
	copy(key[:], public)
	return key
}

// Register adds every printed form of the key to a secret registry, so a log
// line, an error, or an output that contains it is redacted.
func (k WireGuardKey) Register(registry *secrets.Registry) {
	if registry == nil || !k.set {
		return
	}
	registry.Add(base64.StdEncoding.EncodeToString(k.raw[:]), hex.EncodeToString(k.raw[:]))
}

// String redacts the key.
func (k WireGuardKey) String() string { return secrets.Placeholder }

// GoString redacts the key in %#v.
func (k WireGuardKey) GoString() string { return secrets.Placeholder }

// MarshalJSON redacts the key.
func (k WireGuardKey) MarshalJSON() ([]byte, error) {
	return []byte(strconv.Quote(secrets.Placeholder)), nil
}

// LogValue redacts the key in structured logs.
func (k WireGuardKey) LogValue() slog.Value { return slog.StringValue(secrets.Placeholder) }

// uapiHex returns the form the in-process UAPI expects.
func (k WireGuardKey) uapiHex() string { return hex.EncodeToString(k.raw[:]) }

// WireGuardPublicKey is a WireGuard public key. It is not a secret.
type WireGuardPublicKey [32]byte

// ParseWireGuardPublicKey parses a public key in wg base64 form.
func ParseWireGuardPublicKey(text string) (WireGuardPublicKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(text))
	if err != nil || len(raw) != curve25519.PointSize {
		return WireGuardPublicKey{}, fmt.Errorf("the WireGuard public key %q is not a base64 32-byte key", text)
	}
	var key WireGuardPublicKey
	copy(key[:], raw)
	return key, nil
}

// String returns the wg base64 form.
func (k WireGuardPublicKey) String() string { return base64.StdEncoding.EncodeToString(k[:]) }

// IsZero reports an unset key.
func (k WireGuardPublicKey) IsZero() bool { return k == WireGuardPublicKey{} }

// WireGuardConfig is one control-plane-to-node tunnel.
type WireGuardConfig struct {
	// PrivateKey is the control plane's key. Read it with ReadWireGuardKey.
	PrivateKey WireGuardKey
	// Address is the control plane's own tunnel address.
	Address netip.Addr
	// NodeAddress is the node's tunnel address. The SSH address must be on
	// it, and the liveness probe pings it.
	NodeAddress netip.Addr
	// PeerPublicKey is the node's WireGuard public key.
	PeerPublicKey WireGuardPublicKey
	// Endpoint is the node's UDP endpoint, host:port. A host name is resolved
	// each time the tunnel comes up.
	Endpoint string
	// AllowedIPs are routed to the node. They default to the node address
	// alone and are limited to the node subnet.
	AllowedIPs []netip.Prefix
	// Keepalive is the persistent keepalive.
	Keepalive time.Duration
	// HandshakeTimeout bounds the first handshake of a tunnel.
	HandshakeTimeout time.Duration
	// ProbeInterval is how often the liveness probe checks the tunnel.
	ProbeInterval time.Duration
	// LossAfter is how long the tunnel may receive nothing before it is lost.
	LossAfter time.Duration
	// LockDir holds the per-key lock. When it is set, the dialer takes an
	// exclusive lock for its key before it is used and keeps it until Close,
	// so a second process on this host with the same key fails at startup
	// instead of stealing the node's peer. Empty skips the lock.
	LockDir string
	// Logf receives WireGuard errors. Nil discards them. The key never
	// reaches it.
	Logf func(format string, args ...any) `json:"-"`
}

// WireGuardDialer opens TCP connections to the node inside a userspace
// WireGuard tunnel. It needs no root, no TUN device, and no system
// configuration. One tunnel is shared by every call; it comes up on the first
// dial and is rebuilt on the next dial after it was lost.
type WireGuardDialer struct {
	config WireGuardConfig

	release engine.Release

	mu      sync.Mutex
	closed  bool
	current *tunnel
	pending *bringUp
}

var _ Dialer = (*WireGuardDialer)(nil)

// bringUp is a bring-up in progress. Concurrent dials wait for it.
type bringUp struct {
	done chan struct{}
	err  error
}

// NewWireGuardDialer validates the configuration. It does not touch the
// network: the tunnel comes up on the first dial.
func NewWireGuardDialer(config WireGuardConfig) (*WireGuardDialer, error) {
	if config.PrivateKey.IsZero() {
		return nil, errors.New("the WireGuard private key is required")
	}
	if config.PeerPublicKey.IsZero() {
		return nil, errors.New("the node's WireGuard public key is required")
	}
	if config.PeerPublicKey == config.PrivateKey.PublicKey() {
		return nil, errors.New("the node's WireGuard public key is the control plane's own key")
	}
	if !config.Address.IsValid() {
		return nil, errors.New("the control plane's WireGuard address is required")
	}
	if !config.NodeAddress.IsValid() {
		return nil, errors.New("the node's WireGuard address is required")
	}
	config.Address, config.NodeAddress = config.Address.Unmap(), config.NodeAddress.Unmap()
	if config.Address == config.NodeAddress {
		return nil, errors.New("the control plane and the node cannot share a WireGuard address")
	}
	if _, port, err := net.SplitHostPort(config.Endpoint); err != nil || port == "" {
		return nil, fmt.Errorf("the WireGuard endpoint %q must be host:port", config.Endpoint)
	} else if value, err := strconv.Atoi(port); err != nil || value < 1 || value > 65535 {
		return nil, fmt.Errorf("the WireGuard endpoint %q has an invalid port", config.Endpoint)
	}
	if len(config.AllowedIPs) == 0 {
		config.AllowedIPs = []netip.Prefix{netip.PrefixFrom(config.NodeAddress, config.NodeAddress.BitLen())}
	}
	if err := validateAllowedIPs(config.AllowedIPs, config.NodeAddress); err != nil {
		return nil, err
	}
	if config.Keepalive <= 0 {
		config.Keepalive = DefaultKeepalive
	}
	if config.Keepalive < time.Second || config.Keepalive > time.Hour {
		return nil, fmt.Errorf("the WireGuard keepalive %s must be between 1s and 1h", config.Keepalive)
	}
	if config.HandshakeTimeout <= 0 {
		config.HandshakeTimeout = DefaultHandshakeTimeout
	}
	if config.ProbeInterval <= 0 {
		config.ProbeInterval = DefaultProbeInterval
	}
	if config.LossAfter <= 0 {
		config.LossAfter = DefaultLossAfter
	}
	if config.LossAfter < 2*config.ProbeInterval {
		return nil, fmt.Errorf("the WireGuard loss threshold %s must be at least two probe intervals (%s)", config.LossAfter, 2*config.ProbeInterval)
	}
	dialer := &WireGuardDialer{config: config}
	if config.LockDir != "" {
		release, err := lockKey(config.LockDir, config.PrivateKey.PublicKey())
		if err != nil {
			return nil, err
		}
		dialer.release = release
	}
	return dialer, nil
}

// lockKey takes the host-local lock for one WireGuard key. The lock file is
// named after a hash of the public key, never the private one.
func lockKey(directory string, public WireGuardPublicKey) (engine.Release, error) {
	sum := sha256.Sum256(public[:])
	path := filepath.Join(directory, "eregion-wg-"+hex.EncodeToString(sum[:8])+".lock")
	release, err := engine.Acquire(context.Background(), path, engine.LockExclusive, 0)
	if errors.Is(err, engine.ErrLockHeld) {
		return nil, fmt.Errorf("%w (%s, lock %s); give each control-plane process its own WireGuard key and tunnel address", ErrKeyInUse, public, path)
	}
	if err != nil {
		return nil, fmt.Errorf("lock the WireGuard key: %w", err)
	}
	return release, nil
}

// validateAllowedIPs keeps the routed prefixes to the node subnet: no default
// route, nothing broader than a /16 or a /48, and the node address inside.
func validateAllowedIPs(prefixes []netip.Prefix, nodeAddress netip.Addr) error {
	contains := false
	for _, prefix := range prefixes {
		if !prefix.IsValid() {
			return fmt.Errorf("the WireGuard allowed IP %s is not a prefix", prefix)
		}
		minimum := 16
		if prefix.Addr().Is6() {
			minimum = 48
		}
		if prefix.Bits() < minimum {
			return fmt.Errorf("the WireGuard allowed IP %s is broader than the node subnet; use a /%d or narrower", prefix, minimum)
		}
		if prefix.Contains(nodeAddress) {
			contains = true
		}
	}
	if !contains {
		return fmt.Errorf("the WireGuard allowed IPs do not contain the node address %s", nodeAddress)
	}
	return nil
}

// DialContext opens a TCP connection to address inside the tunnel. A tunnel
// that cannot come up is a *TunnelDownError, returned before any byte of a
// request was sent.
func (d *WireGuardDialer) DialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if network != "tcp" && network != "tcp4" && network != "tcp6" {
		return nil, fmt.Errorf("the WireGuard dialer only dials TCP, not %q", network)
	}
	target, err := netip.ParseAddrPort(address)
	if err != nil {
		return nil, fmt.Errorf("the node address %q must be a literal tunnel address and port", address)
	}
	if !d.routes(target.Addr()) {
		return nil, fmt.Errorf("the node address %s is outside the WireGuard allowed IPs", address)
	}
	current, err := d.up(ctx)
	if err != nil {
		return nil, err
	}
	raw, err := current.net.DialContextTCPAddrPort(ctx, netip.AddrPortFrom(target.Addr().Unmap(), target.Port()))
	if err != nil {
		if lost := current.lostErr(); lost != nil {
			return nil, lost
		}
		return nil, err
	}
	wrapped, err := current.track(raw)
	if err != nil {
		_ = raw.Close()
		return nil, err
	}
	return wrapped, nil
}

// routes reports whether an address is inside the allowed IPs.
func (d *WireGuardDialer) routes(address netip.Addr) bool {
	address = address.Unmap()
	for _, prefix := range d.config.AllowedIPs {
		if prefix.Contains(address) {
			return true
		}
	}
	return false
}

// Close closes every connection and the tunnel. Later dials fail.
func (d *WireGuardDialer) Close() error {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		return nil
	}
	d.closed = true
	current := d.current
	d.current = nil
	release := d.release
	d.release = nil
	d.mu.Unlock()
	if current != nil {
		current.shutdown(ErrDialerClosed)
	}
	if release != nil {
		release()
	}
	return nil
}

// Up reports whether the tunnel is established.
func (d *WireGuardDialer) Up() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.current != nil
}

// LastHandshake reports the tunnel's most recent completed handshake, or the
// zero time when none completed or the tunnel is down. It reads the same UAPI
// state the liveness watch uses; the private key never leaves that value.
func (d *WireGuardDialer) LastHandshake() time.Time {
	d.mu.Lock()
	current := d.current
	d.mu.Unlock()
	if current == nil {
		return time.Time{}
	}
	sec := current.uapiValue("last_handshake_time_sec")
	nsec := current.uapiValue("last_handshake_time_nsec")
	if sec == 0 && nsec == 0 {
		return time.Time{}
	}
	return time.Unix(int64(sec), int64(nsec)) //nolint:gosec // G115: UAPI counters are small.
}

// up returns the established tunnel, bringing it up when it is down.
func (d *WireGuardDialer) up(ctx context.Context) (*tunnel, error) {
	for {
		d.mu.Lock()
		if d.closed {
			d.mu.Unlock()
			return nil, ErrDialerClosed
		}
		if d.current != nil {
			current := d.current
			d.mu.Unlock()
			return current, nil
		}
		if pending := d.pending; pending != nil {
			d.mu.Unlock()
			select {
			case <-pending.done:
				if pending.err != nil {
					return nil, pending.err
				}
				continue
			case <-ctx.Done():
				return nil, d.downError(ctx.Err())
			}
		}
		pending := &bringUp{done: make(chan struct{})}
		d.pending = pending
		d.mu.Unlock()

		current, err := d.bringUp(ctx)
		d.mu.Lock()
		d.pending = nil
		if err == nil && d.closed {
			current.shutdown(ErrDialerClosed)
			current, err = nil, ErrDialerClosed
		}
		if err == nil {
			d.current = current
			go d.watch(current)
		}
		pending.err = err
		close(pending.done)
		d.mu.Unlock()
		if err != nil {
			return nil, err
		}
		return current, nil
	}
}

func (d *WireGuardDialer) downError(cause error) error {
	return &TunnelDownError{Endpoint: d.config.Endpoint, PeerPublicKey: d.config.PeerPublicKey.String(), Timeout: d.config.HandshakeTimeout, Cause: cause}
}

// bringUp creates the device and waits for its first handshake.
func (d *WireGuardDialer) bringUp(ctx context.Context) (*tunnel, error) {
	ctx, cancel := context.WithTimeout(ctx, d.config.HandshakeTimeout)
	defer cancel()
	endpoint, err := resolveEndpoint(ctx, d.config.Endpoint)
	if err != nil {
		return nil, d.downError(err)
	}
	tunDevice, tunNet, err := netstack.CreateNetTUN([]netip.Addr{d.config.Address}, nil, TunnelMTU)
	if err != nil {
		return nil, fmt.Errorf("create the userspace WireGuard interface: %w", err)
	}
	logf := device.DiscardLogf
	if d.config.Logf != nil {
		logf = d.config.Logf
	}
	dev := device.NewDevice(tunDevice, conn.NewDefaultBind(), &device.Logger{Verbosef: device.DiscardLogf, Errorf: logf})
	var uapi strings.Builder
	fmt.Fprintf(&uapi, "private_key=%s\n", d.config.PrivateKey.uapiHex())
	fmt.Fprintf(&uapi, "replace_peers=true\n")
	fmt.Fprintf(&uapi, "public_key=%s\n", hex.EncodeToString(d.config.PeerPublicKey[:]))
	fmt.Fprintf(&uapi, "endpoint=%s\n", endpoint)
	fmt.Fprintf(&uapi, "persistent_keepalive_interval=%d\n", int(d.config.Keepalive/time.Second))
	fmt.Fprintf(&uapi, "replace_allowed_ips=true\n")
	for _, prefix := range d.config.AllowedIPs {
		fmt.Fprintf(&uapi, "allowed_ip=%s\n", prefix.Masked())
	}
	if err := dev.IpcSet(uapi.String()); err != nil {
		dev.Close()
		// The UAPI error could echo a line of the configuration; it is never
		// passed on.
		return nil, errors.New("configure the userspace WireGuard device: the configuration was refused")
	}
	if err := dev.Up(); err != nil {
		dev.Close()
		return nil, fmt.Errorf("start the userspace WireGuard device: %w", err)
	}
	ping, err := tunNet.DialPingAddr(d.config.Address, d.config.NodeAddress)
	if err != nil {
		dev.Close()
		return nil, fmt.Errorf("open the WireGuard liveness probe: %w", err)
	}
	current := &tunnel{dev: dev, net: tunNet, ping: ping, node: d.config.NodeAddress, endpoint: d.config.Endpoint, conns: map[*tunnelConn]struct{}{}, stopped: make(chan struct{})}
	go current.drain()
	// Any packet starts the handshake; the probe is the one the tunnel needs
	// anyway.
	current.probe()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if current.handshaked() {
			return current, nil
		}
		select {
		case <-ctx.Done():
			current.shutdown(ErrTunnelDown)
			return nil, d.downError(errors.New("no handshake response"))
		case <-ticker.C:
		}
	}
}

// watch declares the tunnel lost when it received nothing for the loss
// threshold. A healthy node answers the probe, so a quiet tunnel is a dead
// one.
func (d *WireGuardDialer) watch(current *tunnel) {
	ticker := time.NewTicker(d.config.ProbeInterval)
	defer ticker.Stop()
	lastRx := current.rxBytes()
	lastRxAt := time.Now()
	for {
		select {
		case <-current.stopped:
			return
		case <-ticker.C:
		}
		rx := current.rxBytes()
		if rx != lastRx {
			lastRx, lastRxAt = rx, time.Now()
			continue
		}
		if quiet := time.Since(lastRxAt); quiet >= d.config.LossAfter {
			d.lose(current, fmt.Errorf("%w: the tunnel to %s received nothing for %s", ErrTunnelLost, d.config.Endpoint, quiet.Round(time.Millisecond)))
			return
		}
		current.probe()
	}
}

// lose drops a lost tunnel. The next dial rebuilds it.
func (d *WireGuardDialer) lose(current *tunnel, cause error) {
	d.mu.Lock()
	if d.current == current {
		d.current = nil
	}
	d.mu.Unlock()
	current.shutdown(cause)
}

// resolveEndpoint resolves the endpoint to the ip:port the device needs.
func resolveEndpoint(ctx context.Context, endpoint string) (netip.AddrPort, error) {
	host, portText, err := net.SplitHostPort(endpoint)
	if err != nil {
		return netip.AddrPort{}, err
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		return netip.AddrPort{}, err
	}
	if address, err := netip.ParseAddr(host); err == nil {
		return netip.AddrPortFrom(address.Unmap(), uint16(port)), nil
	}
	addresses, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
	if err != nil {
		return netip.AddrPort{}, fmt.Errorf("resolve the WireGuard endpoint %s: %w", host, err)
	}
	if len(addresses) == 0 {
		return netip.AddrPort{}, fmt.Errorf("the WireGuard endpoint %s has no address", host)
	}
	return netip.AddrPortFrom(addresses[0].Unmap(), uint16(port)), nil
}

// tunnel is one generation of the device. A lost tunnel is never reused.
type tunnel struct {
	dev      *device.Device
	net      *netstack.Net
	ping     *netstack.PingConn
	node     netip.Addr
	endpoint string

	mu      sync.Mutex
	conns   map[*tunnelConn]struct{}
	lost    error
	stopped chan struct{}
	once    sync.Once
	seq     uint16
}

// handshaked reports whether the device completed a handshake.
func (t *tunnel) handshaked() bool {
	sec, nsec := t.uapiValue("last_handshake_time_sec"), t.uapiValue("last_handshake_time_nsec")
	return sec != 0 || nsec != 0
}

// rxBytes is what the tunnel received so far.
func (t *tunnel) rxBytes() uint64 { return t.uapiValue("rx_bytes") }

// uapiValue reads one numeric peer value from the device. The UAPI dump also
// holds the private key; it is parsed here and never leaves this function.
func (t *tunnel) uapiValue(name string) uint64 {
	state, err := t.dev.IpcGet()
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(state, "\n") {
		if value, ok := strings.CutPrefix(line, name+"="); ok {
			parsed, err := strconv.ParseUint(value, 10, 64)
			if err == nil {
				return parsed
			}
		}
	}
	return 0
}

// probe sends one ICMP echo to the node. The netstack fills in the identifier
// and the checksum.
func (t *tunnel) probe() {
	t.mu.Lock()
	t.seq++
	seq := t.seq
	t.mu.Unlock()
	kind := byte(8) // ICMPv4 echo request
	if t.node.Is6() {
		kind = 128 // ICMPv6 echo request
	}
	message := []byte{kind, 0, 0, 0, 0, 0, byte(seq >> 8), byte(seq), 'e', 'r', 'e', 'g', 'i', 'o', 'n', 0}
	_ = t.ping.SetWriteDeadline(time.Now().Add(time.Second))
	_, _ = t.ping.Write(message)
}

// drain reads the probe replies, so they never fill the socket buffer. The
// replies count as received bytes; their content does not matter.
func (t *tunnel) drain() {
	buffer := make([]byte, 1500)
	for {
		if _, err := t.ping.Read(buffer); err != nil {
			if t.lostErr() != nil {
				return
			}
			if errors.Is(err, net.ErrClosed) {
				return
			}
		}
	}
}

// track wraps a connection so a lost tunnel closes it with ErrTunnelLost.
func (t *tunnel) track(raw net.Conn) (net.Conn, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.lost != nil {
		return nil, t.lost
	}
	wrapped := &tunnelConn{Conn: raw, tunnel: t}
	t.conns[wrapped] = struct{}{}
	return wrapped, nil
}

func (t *tunnel) forget(c *tunnelConn) {
	t.mu.Lock()
	delete(t.conns, c)
	t.mu.Unlock()
}

// lostErr reports why the tunnel was shut down, or nil while it is up.
func (t *tunnel) lostErr() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.lost
}

// shutdown closes every connection and the device, once.
func (t *tunnel) shutdown(cause error) {
	t.once.Do(func() {
		t.mu.Lock()
		t.lost = cause
		conns := make([]*tunnelConn, 0, len(t.conns))
		for c := range t.conns {
			conns = append(conns, c)
		}
		t.conns = map[*tunnelConn]struct{}{}
		close(t.stopped)
		t.mu.Unlock()
		for _, c := range conns {
			_ = c.Conn.Close()
		}
		_ = t.ping.Close()
		t.dev.Close()
	})
}

// tunnelConn is a TCP connection inside the tunnel. When the tunnel is lost,
// its reads and writes fail with the loss, not with a bare closed connection.
type tunnelConn struct {
	net.Conn
	tunnel *tunnel
}

func (c *tunnelConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	if err != nil {
		if lost := c.tunnel.lostErr(); lost != nil {
			return n, lost
		}
	}
	return n, err
}

func (c *tunnelConn) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if err != nil {
		if lost := c.tunnel.lostErr(); lost != nil {
			return n, lost
		}
	}
	return n, err
}

func (c *tunnelConn) Close() error {
	c.tunnel.forget(c)
	return c.Conn.Close()
}
