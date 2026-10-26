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
func (id *operatorIdentity) wireGuardRoute(publicHost, nodePublicKey, knownHosts string) *host.ContextSSH {
	return &host.ContextSSH{
		Address:    net.JoinHostPort(access.NodeAddress, "22"),
		User:       access.AdminUser,
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

// wireGuardDial is the dial function for a context's WireGuard route.
func wireGuardDial(wg *host.ContextWireGuard) (func(context.Context, string, string) (net.Conn, error), error) {
	key, err := transport.ReadWireGuardKey(wg.PrivateKeyFile)
	if err != nil {
		return nil, err
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
	return dialer.DialContext, nil
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
func (r *serverInit) secureAccess(ctx context.Context, root *host.Remote, id *operatorIdentity, apiPort int, knownHosts string) (*host.ContextSSH, error) {
	port, _ := strconv.Atoi(r.answers.Port)
	task := r.start("harden", "Hardening the server and bringing up WireGuard")
	setup := access.Setup{AdminUser: access.AdminUser, OperatorSSHKey: id.authorizedKey(), SudoPassword: id.sudoPassword, OperatorPublicKey: id.wg.PublicKey().String(), SSHPort: port, APIPort: apiPort}
	var out, stderr strings.Builder
	if err := root.Run(ctx, "bash -s", strings.NewReader(access.Program("setup", setup.Env())), &out, &stderr); err != nil {
		task.end(ui.EndFail, "")
		return nil, contract.NewFault(contract.Unavailable, "hardening the server failed", lastLine(stderr.String(), err))
	}
	nodeKey, err := access.PublicKey(out.String())
	if err != nil {
		task.end(ui.EndFail, "")
		return nil, contract.NewFault(contract.Unavailable, err.Error(), "")
	}
	task.end(ui.EndOK, "admin user "+access.AdminUser+", wg0 "+access.NodeAddress+", ufw, sshd")

	route := id.wireGuardRoute(r.answers.Host, nodeKey, knownHosts)
	if !hostKeyPinned(knownHosts, route.Address) && r.hostPub != nil {
		if err := pinHostKey(knownHosts, route.Address, r.hostPub); err != nil {
			return nil, err
		}
	}

	task = r.start("wireguard", "SSH as "+access.AdminUser+" over WireGuard")
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
		return nil, contract.NewFault(contract.Unavailable, "the lockdown could not be armed", err.Error())
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
