package cli

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mbvlabs/eregion/internal/access"
	"github.com/mbvlabs/eregion/internal/cli/ui"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/host"
	"github.com/mbvlabs/eregion/internal/transport"
	"golang.org/x/crypto/ssh"
)

// operatorIdentity is what server init generates for the operator, like
// vaire: an SSH key for the admin user, a WireGuard key for the operator
// peer, and the admin sudo password. It lives owner-only next to the context
// file, so a re-run reuses it.
type operatorIdentity struct {
	dir          string
	sshKeyPath   string
	sshKeyPEM    string
	signer       ssh.Signer
	wgKeyPath    string
	wg           transport.WireGuardKey
	sudoPassword string
}

func identityDir(name string) (string, error) {
	contexts, err := host.DefaultContextPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(contexts), "servers", name), nil
}

func loadOrCreateIdentity(name string) (*operatorIdentity, error) {
	dir, err := identityDir(name)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	id := &operatorIdentity{dir: dir, sshKeyPath: filepath.Join(dir, "ssh_ed25519"), wgKeyPath: filepath.Join(dir, "wireguard.key")}
	if _, err := os.Stat(id.sshKeyPath); errors.Is(err, os.ErrNotExist) {
		_, priv, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			return nil, err
		}
		block, err := ssh.MarshalPrivateKey(priv, "eregion operator "+name)
		if err != nil {
			return nil, err
		}
		if err := writePrivate(id.sshKeyPath, pem.EncodeToMemory(block)); err != nil {
			return nil, err
		}
	}
	raw, err := os.ReadFile(id.sshKeyPath) //nolint:gosec // G304: eregion's own key.
	if err != nil {
		return nil, err
	}
	id.sshKeyPEM = string(raw)
	if id.signer, err = ssh.ParsePrivateKey(raw); err != nil {
		return nil, err
	}
	if _, err := os.Stat(id.wgKeyPath); errors.Is(err, os.ErrNotExist) {
		key, err := transport.GenerateWireGuardKey()
		if err != nil {
			return nil, err
		}
		if err := writePrivate(id.wgKeyPath, []byte(key.Encode()+"\n")); err != nil {
			return nil, err
		}
	}
	if id.wg, err = transport.ReadWireGuardKey(id.wgKeyPath); err != nil {
		return nil, err
	}
	passPath := filepath.Join(dir, "sudo-password")
	if _, err := os.Stat(passPath); errors.Is(err, os.ErrNotExist) {
		secret := make([]byte, 16)
		if _, err := rand.Read(secret); err != nil {
			return nil, err
		}
		if err := writePrivate(passPath, []byte(hex.EncodeToString(secret)+"\n")); err != nil {
			return nil, err
		}
	}
	pass, err := os.ReadFile(passPath) //nolint:gosec // G304: eregion's own file.
	if err != nil {
		return nil, err
	}
	id.sudoPassword = strings.TrimSpace(string(pass))
	return id, nil
}

func writePrivate(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600) //nolint:gosec // G304: eregion's own file.
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

func (id *operatorIdentity) authorizedKey() string {
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(id.signer.PublicKey())))
}

// wireGuardRoute is the context route to a locked-down node: SSH as the admin
// user inside the embedded WireGuard tunnel.
func (id *operatorIdentity) wireGuardRoute(publicHost, nodePublicKey, knownHosts, adminUser string) *host.ContextSSH {
	return &host.ContextSSH{
		Address:    net.JoinHostPort(access.NodeAddress, "22"),
		User:       adminUser,
		Identity:   id.sshKeyPath,
		KnownHosts: knownHosts,
		WireGuard: &host.ContextWireGuard{
			PrivateKeyFile: id.wgKeyPath,
			Address:        access.OperatorAddress,
			NodeAddress:    access.NodeAddress,
			NodePublicKey:  nodePublicKey,
			Endpoint:       net.JoinHostPort(publicHost, strconv.Itoa(access.ListenPort)),
		},
	}
}

// wireGuardDialers holds one tunnel per operator key for the whole process.
// The node has one peer per key: a second tunnel with the same key steals the
// peer, and the first one's keepalives steal it back, which drops the SSH
// connections running over either.
var (
	wireGuardMu      sync.Mutex
	wireGuardDialers = map[string]*transport.WireGuardDialer{}
)

// wireGuardDial is the dial function for a context's WireGuard route. Every
// route with the same key and node shares one tunnel.
func wireGuardDial(wg *host.ContextWireGuard) (func(context.Context, string, string) (net.Conn, error), error) {
	key, err := transport.ReadWireGuardKey(wg.PrivateKeyFile)
	if err != nil {
		return nil, err
	}
	id := key.PublicKey().String() + "|" + wg.NodePublicKey + "|" + wg.Endpoint + "|" + wg.Address + "|" + wg.NodeAddress
	wireGuardMu.Lock()
	defer wireGuardMu.Unlock()
	if dialer, ok := wireGuardDialers[id]; ok {
		return dialer.DialContext, nil
	}
	peer, err := transport.ParseWireGuardPublicKey(wg.NodePublicKey)
	if err != nil {
		return nil, err
	}
	self, err := netip.ParseAddr(wg.Address)
	if err != nil {
		return nil, err
	}
	nodeAddr, err := netip.ParseAddr(wg.NodeAddress)
	if err != nil {
		return nil, err
	}
	dialer, err := transport.NewWireGuardDialer(transport.WireGuardConfig{PrivateKey: key, Address: self, NodeAddress: nodeAddr, PeerPublicKey: peer, Endpoint: wg.Endpoint})
	if err != nil {
		return nil, err
	}
	// A different tunnel for the same key (a rebuilt node) replaces the old one.
	for other, d := range wireGuardDialers {
		if strings.HasPrefix(other, key.PublicKey().String()+"|") {
			_ = d.Close()
			delete(wireGuardDialers, other)
		}
	}
	wireGuardDialers[id] = dialer
	return dialer.DialContext, nil
}

// closeWireGuardRoute closes the process's tunnel for a removed context so
// nothing keeps the node peer alive after its key files are gone.
func closeWireGuardRoute(route *host.ContextSSH) {
	if route == nil || route.WireGuard == nil {
		return
	}
	key, err := transport.ReadWireGuardKey(route.WireGuard.PrivateKeyFile)
	if err != nil {
		return
	}
	prefix := key.PublicKey().String() + "|"
	wireGuardMu.Lock()
	defer wireGuardMu.Unlock()
	for id, dialer := range wireGuardDialers {
		if strings.HasPrefix(id, prefix) {
			_ = dialer.Close()
			delete(wireGuardDialers, id)
		}
	}
}

// routeRemote opens a host.Remote for a context route.
func routeRemote(route *host.ContextSSH, prompt bool) (*host.Remote, error) {
	auth, err := sshAuth(route.Identity, prompt)
	if err != nil {
		return nil, err
	}
	cfg := host.RemoteConfig{Address: route.Address, User: route.User, KnownHosts: route.KnownHosts, Auth: auth, Timeout: 20 * time.Second}
	if route.WireGuard != nil {
		if cfg.Dial, err = wireGuardDial(route.WireGuard); err != nil {
			return nil, contract.NewFault(contract.ValidationFailed, "the context's WireGuard route is not usable", err.Error())
		}
	}
	return host.NewRemote(cfg)
}

// secureAccess hardens the server and moves SSH onto WireGuard (vaire §5.2):
// setup over public SSH as root, a login as the admin user over WireGuard,
// then the commit-confirm lockdown confirmed from a fresh connection.
func (r *serverInit) secureAccess(ctx context.Context, id *operatorIdentity, publicHost, knownHosts string) (*host.ContextSSH, error) {
	nodeKey := r.pending.NodePublicKey
	if !r.pending.Done(phaseAccessSetup) {
		root := r.remote
		if root == nil {
			return nil, contract.NewFault(contract.InputRequired, "the access setup needs the root first-contact connection", "Run server init --resume with the first-contact credential; root SSH is still open before the access setup runs.")
		}
		port, _ := strconv.Atoi(r.answers.Port)
		cfg := host.DefaultConfig(host.RoleMain, r.answers.Node)
		_, apiPortText, _ := net.SplitHostPort(cfg.APIListen)
		apiPort, _ := strconv.Atoi(apiPortText)
		task := r.start("harden", "Hardening the server and bringing up WireGuard")
		setup := access.Setup{AdminUser: r.answers.AdminUser, OperatorSSHKey: id.authorizedKey(), SudoPassword: id.sudoPassword, OperatorPublicKey: id.wg.PublicKey().String(), SSHPort: port, APIPort: apiPort}
		var out, stderr strings.Builder
		if err := root.Run(ctx, "bash -s", strings.NewReader(access.Program("setup", setup.Env())), &out, &stderr); err != nil {
			task.end(ui.EndFail, "")
			return nil, contract.NewFault(contract.Unavailable, "hardening the server failed", lastLine(stderr.String(), err))
		}
		got, keyErr := access.PublicKey(out.String())
		if keyErr == nil {
			nodeKey = got
			r.pending.NodePublicKey = nodeKey
		}
		// setup.sh installs the sshd drop-in last, so root SSH is already gone
		// when it returns. Record the phase before any remaining step can fail;
		// a resume then reconnects as the admin user over WireGuard.
		r.pending.Mark(phaseAccessSetup)
		if err := r.pending.Save(); err != nil {
			task.end(ui.EndFail, "")
			return nil, err
		}
		if keyErr != nil {
			task.end(ui.EndFail, "")
			return nil, contract.NewFault(contract.Unavailable, keyErr.Error(), "")
		}
		task.end(ui.EndOK, "admin user "+r.answers.AdminUser+", wg0 "+access.NodeAddress+", ufw, sshd")
	}
	if nodeKey == "" {
		return nil, contract.NewFault(contract.PreconditionFailed, "the pending init has no node WireGuard key", "Run server init --discard and start again; the access setup did not finish.")
	}

	route := id.wireGuardRoute(publicHost, nodeKey, knownHosts, r.answers.AdminUser)
	if !hostKeyPinned(knownHosts, route.Address) {
		// The node presents the same host keys on every address. Pin the key
		// learned over public SSH for the WireGuard address too; on a resume
		// that skipped first contact it comes from the pending known_hosts.
		public := net.JoinHostPort(publicHost, r.answers.Port)
		if keys, err := transport.LoadHostKeys(knownHosts); err == nil {
			for _, key := range keys.Keys(public) {
				if err := pinHostKey(knownHosts, route.Address, key); err != nil {
					return nil, err
				}
			}
		}
	}

	task := r.start("wireguard", "SSH as "+r.answers.AdminUser+" over WireGuard")
	if err := runWithRetry(ctx, route, "true", 45*time.Second); err != nil {
		task.end(ui.EndFail, "")
		return nil, contract.NewFault(contract.Unavailable, "SSH over WireGuard does not answer", err.Error()+" — check that UDP "+strconv.Itoa(access.ListenPort)+" reaches the server (provider firewall). Public SSH is still open.")
	}
	task.end(ui.EndOK, access.NodeAddress+":22")

	task = r.start("lockdown", "Closing public SSH")
	task.detail("the server reopens it unless confirmed within " + strconv.Itoa(access.LockdownSeconds) + "s")
	armed := time.Now()
	if err := runWithRetry(ctx, route, "sudo -n /usr/local/bin/eregion node-agent access lockdown", 30*time.Second); err != nil {
		task.end(ui.EndFail, "")
		return nil, contract.NewFault(contract.Unavailable, "the lockdown could not be armed", err.Error()+" — if it was armed, the server reopens public SSH within "+strconv.Itoa(access.LockdownSeconds)+"s")
	}
	// A fresh connection proves the new path works before it is kept.
	if err := runWithRetry(ctx, route, "sudo -n /usr/local/bin/eregion node-agent access confirm", 60*time.Second); err != nil {
		task.end(ui.EndFail, "")
		wait := time.Until(armed.Add(time.Duration(access.LockdownSeconds+10) * time.Second))
		return nil, contract.NewFault(contract.Unavailable, "SSH over WireGuard did not confirm the lockdown; the server reopens public SSH in "+wait.Round(time.Second).String(), err.Error())
	}
	task.end(ui.EndOK, "SSH answers only on WireGuard")
	return route, nil
}

// planRows adds the access changes to the host steps. The admin user, wg0,
// firewall and SSH lockdown switchover are applied by setup.sh, not by a host
// bootstrap step, so the plan names them explicitly. node_exporter is already
// a host step; add it only when a plan does not carry it.
func (r *serverInit) planRows(plan host.Plan) []host.PlanEntry {
	rows := append([]host.PlanEntry(nil), plan.Steps...)
	rows = append(rows,
		host.PlanEntry{ID: "access-admin", Description: "Create the admin user " + r.answers.AdminUser + " with the operator SSH key, sudo password and passwordless node RPC"},
		host.PlanEntry{ID: "access-wireguard", Description: "Bring up wg0 at " + access.NodeAddress + " with the operator peer"},
		host.PlanEntry{ID: "access-firewall", Description: "Enable ufw: deny incoming, allow WireGuard, HTTP, HTTPS and SSH on wg0"},
		host.PlanEntry{ID: "access-lockdown", Description: "Switch SSH to " + r.answers.AdminUser + " over WireGuard and close public SSH (reverted unless confirmed)"},
	)
	for _, row := range rows {
		if strings.Contains(strings.ToLower(row.Description), "node_exporter") {
			return rows
		}
	}
	return append(rows, host.PlanEntry{ID: "access-node-exporter", Description: "Install node_exporter on the WireGuard address"})
}

func runWithRetry(ctx context.Context, route *host.ContextSSH, command string, within time.Duration) error {
	deadline := time.Now().Add(within)
	var last error
	for time.Now().Before(deadline) {
		remote, err := routeRemote(route, false)
		if err != nil {
			return err
		}
		var stderr strings.Builder
		if last = remote.Run(ctx, command, nil, io.Discard, &stderr); last == nil {
			return nil
		}
		if s := strings.TrimSpace(stderr.String()); s != "" {
			last = errors.New(s)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	return last
}

func lastLine(s string, err error) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if l := strings.TrimSpace(lines[len(lines)-1]); l != "" {
		return l
	}
	return err.Error()
}
