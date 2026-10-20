// Package sshfixture starts a throwaway SSH node for the transport tests. The
// node is a container with a generated host key and one authorized key; it
// holds no credential of the control plane and is removed when the test ends.
//
// The fixture is optional: when Docker is missing, or the image cannot be
// built, Start reports that it is unavailable and the caller skips the test.
package sshfixture

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/engine"
	"golang.org/x/crypto/curve25519"
	"golang.org/x/crypto/ssh"
)

// ErrUnavailable reports that the fixture cannot run in this environment. A
// test skips on it; a broken fixture never hides a real failure.
var ErrUnavailable = errors.New("the SSH fixture is unavailable")

// DefaultImage is the fixture image name.
const DefaultImage = "eregion-ssh-fixture"

// Options configure the fixture.
type Options struct {
	// Image overrides the fixture image name.
	Image string
	// NodeName is the node's name. It defaults to "fixture-node".
	NodeName string
	// User is the node account. It defaults to "eregion".
	User string
	// TaskCommand replaces the command that starts a detached node task. The
	// node uses it verbatim, so a fixture can make a task that never starts.
	TaskCommand []string
	// WireGuard puts the node behind kernel WireGuard. Only the WireGuard UDP
	// port is published, so SSH is reachable only through the tunnel. The
	// container needs NET_ADMIN and the host's wireguard module; without them
	// Start reports ErrUnavailable.
	WireGuard bool
	// ClientKeyFile reuses an existing SSH identity (mode 0600, OpenSSH
	// private-key PEM) instead of generating one, so several nodes can trust
	// the one key a control plane holds. Create one with NewClientKey.
	ClientKeyFile string
	// Files are executable files installed in the node, by absolute path. A
	// test uses them to stand in for host tools (docker, pack) without
	// installing them.
	Files map[string]string
	// WireGuardApply is the node's privileged WireGuard apply helper command.
	WireGuardApply []string
	// NetAdmin gives the node NET_ADMIN and publishes both its SSH port and its
	// WireGuard UDP port, without configuring any interface. A control plane
	// reaches it directly first and then configures WireGuard through the
	// reconcile flow. Without the host's wireguard module Start reports
	// ErrUnavailable.
	NetAdmin bool
}

// NewClientKey writes a fresh SSH client identity into dir and returns its path.
func NewClientKey(dir string) (string, error) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	block, err := ssh.MarshalPrivateKey(key, "eregion-fixture-client")
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		return "", err
	}
	return path, nil
}

// WireGuardNode describes the fixture's WireGuard side.
type WireGuardNode struct {
	// Endpoint is the published UDP host:port of the node's wg0.
	Endpoint string
	// PublicKey is the node's WireGuard public key in wg base64 form.
	PublicKey string
	// PrivateKeyFile is the control plane's private key, mode 0600. The node
	// knows its public half as its only peer.
	PrivateKeyFile string
	// ControlAddress is the control plane's tunnel address.
	ControlAddress string
	// NodeAddress is the node's tunnel address.
	NodeAddress string
	// Subnet is the tunnel subnet.
	Subnet string
}

// The fixture's tunnel addressing.
const (
	wireGuardControlAddress = "10.77.0.1"
	wireGuardNodeAddress    = "10.77.0.2"
	wireGuardSubnet         = "10.77.0.0/24"
	wireGuardPort           = "51820"
)

// Fixture is a running throwaway SSH node.
type Fixture struct {
	// Name is the container name.
	Name string
	// Address is the published host:port of the node's SSH server.
	Address string
	// IdentityFile is the client's private key. It is mode 0600.
	IdentityFile string
	// HostKey is the node's host key. Pin exactly this key.
	HostKey ssh.PublicKey
	// KnownHostsFile is a known_hosts file with the pinned host key.
	KnownHostsFile string
	// WireGuard is set when the node is behind WireGuard. Address is then the
	// node's tunnel address.
	WireGuard *WireGuardNode
	// UDPEndpoint is the published WireGuard host:port of a NetAdmin node.
	UDPEndpoint string

	image string
}

// Start builds the fixture image when needed and starts a node container.
func Start(ctx context.Context, options Options) (*Fixture, error) {
	if _, err := exec.LookPath("docker"); err != nil {
		return nil, fmt.Errorf("%w: docker is not installed", ErrUnavailable)
	}
	root, err := moduleRoot()
	if err != nil {
		return nil, err
	}
	image := options.Image
	if image == "" {
		image = DefaultImage
	}
	tag, err := ensureImage(ctx, root, image)
	if err != nil {
		return nil, err
	}
	image = tag

	nodeName := options.NodeName
	if nodeName == "" {
		nodeName = "fixture-node"
	}
	user := options.User
	if user == "" {
		user = "eregion"
	}
	hostKey, hostKeyPath, err := generateHostKey()
	if err != nil {
		return nil, err
	}
	var identityPath string
	var authorized []byte
	if options.ClientKeyFile != "" {
		raw, err := os.ReadFile(options.ClientKeyFile)
		if err != nil {
			return nil, err
		}
		signer, err := ssh.ParsePrivateKey(raw)
		if err != nil {
			return nil, err
		}
		identityPath, authorized = options.ClientKeyFile, ssh.MarshalAuthorizedKey(signer.PublicKey())
	} else {
		_, identityPath, authorized, err = generateClientKey()
		if err != nil {
			return nil, err
		}
	}

	container := "eregion-ssh-fixture-" + strconv.Itoa(os.Getpid()) + "-" + randomSuffix()
	run := []string{"run", "-d", "--init", "--name", container, "-p", "127.0.0.1::2222"}
	if options.WireGuard {
		run = []string{"run", "-d", "--init", "--name", container, "--cap-add", "NET_ADMIN", "-p", "127.0.0.1::" + wireGuardPort + "/udp"}
	}
	if options.NetAdmin {
		run = []string{"run", "-d", "--init", "--name", container, "--cap-add", "NET_ADMIN", "-p", "127.0.0.1::2222", "-p", "127.0.0.1::" + wireGuardPort + "/udp"}
	}
	if _, err := runDocker(ctx, append(run, image, "sleep", "infinity")...); err != nil {
		return nil, err
	}
	fixture := &Fixture{Name: container, image: image, HostKey: hostKey, IdentityFile: identityPath}
	cleanup := func() { _, _ = runDocker(context.Background(), "rm", "-f", container) }
	if options.WireGuard {
		node, err := setupWireGuard(ctx, container, filepath.Dir(identityPath))
		if err != nil {
			cleanup()
			return nil, err
		}
		fixture.WireGuard = node
	}

	if err := copyIntoContainer(ctx, container, hostKeyPath, "/etc/ssh/ssh_host_ed25519_key"); err != nil {
		cleanup()
		return nil, err
	}
	if _, err := runDocker(ctx, "exec", container, "sh", "-c", "chmod 600 /etc/ssh/ssh_host_ed25519_key && rm -f /etc/ssh/ssh_host_rsa_key /etc/ssh/ssh_host_ecdsa_key /etc/ssh/ssh_host_dsa_key"); err != nil {
		cleanup()
		return nil, err
	}
	home := "/home/" + user
	if _, err := runDocker(ctx, "exec", container, "sh", "-c", "mkdir -p "+home+"/.ssh && chown -R "+user+":"+user+" "+home); err != nil {
		cleanup()
		return nil, err
	}
	authorizedPath := filepath.Join(filepath.Dir(identityPath), "authorized_keys")
	if err := os.WriteFile(authorizedPath, authorized, 0o600); err != nil {
		cleanup()
		return nil, err
	}
	if err := copyIntoContainer(ctx, container, authorizedPath, home+"/.ssh/authorized_keys"); err != nil {
		cleanup()
		return nil, err
	}
	if _, err := runDocker(ctx, "exec", container, "sh", "-c", "chown "+user+":"+user+" "+home+"/.ssh/authorized_keys && chmod 600 "+home+"/.ssh/authorized_keys"); err != nil {
		cleanup()
		return nil, err
	}
	config := map[string]any{"node": nodeName, "state_dir": "/var/lib/eregion/node"}
	if len(options.WireGuardApply) > 0 {
		config["wireguard_apply"] = options.WireGuardApply
	}
	scratch, err := os.MkdirTemp("", "eregion-ssh-files")
	if err != nil {
		cleanup()
		return nil, err
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	for path, content := range options.Files {
		if err := installFile(ctx, container, scratch, path, content); err != nil {
			cleanup()
			return nil, err
		}
	}
	if len(options.TaskCommand) > 0 {
		config["task_command"] = options.TaskCommand
	}
	raw, err := json.Marshal(config)
	if err != nil {
		cleanup()
		return nil, err
	}
	configPath := filepath.Join(filepath.Dir(identityPath), "node.json")
	if err := os.WriteFile(configPath, raw, 0o600); err != nil {
		cleanup()
		return nil, err
	}
	if _, err := runDocker(ctx, "exec", container, "mkdir", "-p", "/etc/eregion"); err != nil {
		cleanup()
		return nil, err
	}
	if err := copyIntoContainer(ctx, container, configPath, "/etc/eregion/node.json"); err != nil {
		cleanup()
		return nil, err
	}
	if _, err := runDocker(ctx, "exec", container, "sh", "-c", "chown "+user+":"+user+" /etc/eregion/node.json && chmod 600 /etc/eregion/node.json && chown -R "+user+":"+user+" /var/lib/eregion"); err != nil {
		cleanup()
		return nil, err
	}
	if _, err := runDocker(ctx, "exec", "-d", container, "/usr/sbin/sshd", "-D", "-e", "-f", "/etc/ssh/sshd_config", "-p", "2222"); err != nil {
		cleanup()
		return nil, err
	}
	address := net.JoinHostPort(wireGuardNodeAddress, "2222")
	if !options.WireGuard {
		address, err = publishedAddress(ctx, container, "2222/tcp")
		if err != nil {
			cleanup()
			return nil, err
		}
	}
	fixture.Address = address
	if options.NetAdmin {
		if out, err := runDocker(ctx, "exec", container, "ip", "link", "add", "wgprobe", "type", "wireguard"); err != nil {
			cleanup()
			return nil, fmt.Errorf("%w: the node container cannot create a kernel WireGuard interface (it needs NET_ADMIN and the host's wireguard module): %s", ErrUnavailable, strings.TrimSpace(out))
		}
		_, _ = runDocker(ctx, "exec", container, "ip", "link", "del", "wgprobe")
		if fixture.UDPEndpoint, err = publishedAddress(ctx, container, wireGuardPort+"/udp"); err != nil {
			cleanup()
			return nil, err
		}
	}
	knownHosts := filepath.Join(filepath.Dir(identityPath), "known_hosts")
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(hostKey)))
	if err := os.WriteFile(knownHosts, []byte(knownHostsLine(address)+" "+line+"\n"), 0o600); err != nil {
		cleanup()
		return nil, err
	}
	fixture.KnownHostsFile = knownHosts
	if options.WireGuard {
		// The SSH port is not published: wait for it inside the node.
		if _, err := runDocker(ctx, "exec", container, "sh", "-c", "for i in $(seq 1 300); do ss -ltn | grep -q ':2222 ' && exit 0; sleep 0.1; done; exit 1"); err != nil {
			cleanup()
			return nil, fmt.Errorf("the node's SSH server did not start: %w", err)
		}
		return fixture, nil
	}
	if err := waitForPort(ctx, address, 30*time.Second); err != nil {
		cleanup()
		return nil, err
	}
	return fixture, nil
}

// installFile copies an executable file into the node.
func installFile(ctx context.Context, container, scratch, path, content string) error {
	source := filepath.Join(scratch, hex.EncodeToString([]byte(path)))
	if err := os.WriteFile(source, []byte(content), 0o700); err != nil {
		return err
	}
	if _, err := runDocker(ctx, "exec", container, "mkdir", "-p", filepath.Dir(path)); err != nil {
		return err
	}
	if err := copyIntoContainer(ctx, container, source, path); err != nil {
		return err
	}
	_, err := runDocker(ctx, "exec", container, "chmod", "0755", path)
	return err
}

// InstallFile installs an executable file in a running node.
func (f *Fixture) InstallFile(ctx context.Context, path, content string) error {
	scratch, err := os.MkdirTemp("", "eregion-ssh-files")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(scratch) }()
	return installFile(ctx, f.Name, scratch, path, content)
}

// Exec runs a shell command in the node as root and returns its output.
func (f *Fixture) Exec(ctx context.Context, command string) (string, error) {
	return runDocker(ctx, "exec", f.Name, "sh", "-c", command)
}

// ExecDetached starts a long-running root shell command in the node.
func (f *Fixture) ExecDetached(ctx context.Context, command string) error {
	_, err := runDocker(ctx, "exec", "-d", f.Name, "sh", "-c", command)
	return err
}

// setupWireGuard brings up kernel WireGuard in the node container. It
// reports ErrUnavailable when the container cannot create a WireGuard
// interface, so a host without the capability skips instead of failing.
func setupWireGuard(ctx context.Context, container, directory string) (*WireGuardNode, error) {
	if out, err := runDocker(ctx, "exec", container, "ip", "link", "add", "wg0", "type", "wireguard"); err != nil {
		return nil, fmt.Errorf("%w: the node container cannot create a kernel WireGuard interface (it needs NET_ADMIN and the host's wireguard module): %s", ErrUnavailable, strings.TrimSpace(out))
	}
	nodePrivate, nodePublic, err := generateWireGuardKey()
	if err != nil {
		return nil, err
	}
	controlPrivate, controlPublic, err := generateWireGuardKey()
	if err != nil {
		return nil, err
	}
	nodeKeyPath := filepath.Join(directory, "wg-node.key")
	if err := os.WriteFile(nodeKeyPath, []byte(nodePrivate+"\n"), 0o600); err != nil {
		return nil, err
	}
	controlKeyPath := filepath.Join(directory, "wg-control.key")
	if err := os.WriteFile(controlKeyPath, []byte(controlPrivate+"\n"), 0o600); err != nil {
		return nil, err
	}
	if _, err := runDocker(ctx, "exec", container, "mkdir", "-p", "-m", "0700", "/etc/wireguard"); err != nil {
		return nil, err
	}
	if err := copyIntoContainer(ctx, container, nodeKeyPath, "/etc/wireguard/node.key"); err != nil {
		return nil, err
	}
	script := "chmod 600 /etc/wireguard/node.key" +
		" && wg set wg0 listen-port " + wireGuardPort + " private-key /etc/wireguard/node.key peer " + controlPublic + " allowed-ips " + wireGuardControlAddress + "/32" +
		" && ip address add " + wireGuardNodeAddress + "/24 dev wg0" +
		" && ip link set wg0 up"
	if _, err := runDocker(ctx, "exec", container, "sh", "-c", script); err != nil {
		return nil, err
	}
	endpoint, err := publishedAddress(ctx, container, wireGuardPort+"/udp")
	if err != nil {
		return nil, err
	}
	return &WireGuardNode{
		Endpoint:       endpoint,
		PublicKey:      nodePublic,
		PrivateKeyFile: controlKeyPath,
		ControlAddress: wireGuardControlAddress,
		NodeAddress:    wireGuardNodeAddress,
		Subnet:         wireGuardSubnet,
	}, nil
}

// SetWireGuardPeer replaces the control-plane key the node accepts, so a
// test can make a node that does not know the control plane.
func (f *Fixture) SetWireGuardPeer(ctx context.Context, publicKey string) error {
	if f.WireGuard == nil {
		return errors.New("the fixture has no WireGuard node")
	}
	out, err := runDocker(ctx, "exec", f.Name, "wg", "show", "wg0", "peers")
	if err != nil {
		return err
	}
	script := ""
	for _, peer := range strings.Fields(out) {
		script += "wg set wg0 peer " + peer + " remove && "
	}
	script += "wg set wg0 peer " + publicKey + " allowed-ips " + wireGuardControlAddress + "/32"
	_, err = runDocker(ctx, "exec", f.Name, "sh", "-c", script)
	return err
}

// generateWireGuardKey returns a private key, clamped as wg genkey does, and
// its public key, both in wg base64 form.
func generateWireGuardKey() (string, string, error) {
	var private [curve25519.ScalarSize]byte
	if _, err := rand.Read(private[:]); err != nil {
		return "", "", err
	}
	private[0] &= 248
	private[31] = (private[31] & 127) | 64
	public, err := curve25519.X25519(private[:], curve25519.Basepoint)
	if err != nil {
		return "", "", err
	}
	return base64.StdEncoding.EncodeToString(private[:]), base64.StdEncoding.EncodeToString(public), nil
}

// Close removes the node container.
func (f *Fixture) Close(ctx context.Context) error {
	if f == nil || f.Name == "" {
		return nil
	}
	_, err := runDocker(ctx, "rm", "-f", f.Name)
	return err
}

// Effects lists the effect files the node wrote for one operation.
func (f *Fixture) Effects(ctx context.Context, operationID string) ([]string, error) {
	out, err := runDocker(ctx, "exec", f.Name, "sh", "-c", "ls -1 /var/lib/eregion/node/effects/"+operationID+" 2>/dev/null || true")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(line) != "" {
			names = append(names, strings.TrimSpace(line))
		}
	}
	return names, nil
}

// EffectCount counts the effect files the node wrote for one operation.
func (f *Fixture) EffectCount(ctx context.Context, operationID string) (int, error) {
	names, err := f.Effects(ctx, operationID)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, name := range names {
		if strings.HasSuffix(name, ".applied") {
			count++
		}
	}
	return count, nil
}

// Read returns the content of a file in the node's state directory.
func (f *Fixture) Read(ctx context.Context, path string) (string, error) {
	out, err := runDocker(ctx, "exec", f.Name, "sh", "-c", "cat "+path+" 2>/dev/null || true")
	if err != nil {
		return "", err
	}
	return out, nil
}

// TaskOwnerPID returns the PID of the node task that owns the effect of one
// operation, or 0 when no task is running for it.
func (f *Fixture) TaskOwnerPID(ctx context.Context, operationID string) (int, error) {
	raw, err := f.Read(ctx, "/var/lib/eregion/node/journal.json")
	if err != nil {
		return 0, err
	}
	if strings.TrimSpace(raw) == "" {
		return 0, nil
	}
	var journal engine.Journal
	if err := json.Unmarshal([]byte(raw), &journal); err != nil {
		return 0, fmt.Errorf("read the node journal: %w", err)
	}
	for _, task := range journal.Tasks {
		if task.OperationID == operationID && task.Status == engine.TaskRunning {
			return task.Owner.PID, nil
		}
	}
	return 0, nil
}

// KillTask kills the node task that owns the effect of one operation. The
// kernel releases the effect lock it held, so the node observes a task that
// died mid-step.
func (f *Fixture) KillTask(ctx context.Context, operationID string) error {
	pid, err := f.TaskOwnerPID(ctx, operationID)
	if err != nil {
		return err
	}
	if pid == 0 {
		return fmt.Errorf("no node task is running for %s", operationID)
	}
	_, err = runDocker(ctx, "exec", f.Name, "sh", "-c", "kill -9 "+strconv.Itoa(pid))
	return err
}

// List returns the file names in a directory of the node's state directory.
func (f *Fixture) List(ctx context.Context, path string) ([]string, error) {
	out, err := runDocker(ctx, "exec", f.Name, "sh", "-c", "ls -1 "+path+" 2>/dev/null || true")
	if err != nil {
		return nil, err
	}
	var names []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(line) != "" {
			names = append(names, strings.TrimSpace(line))
		}
	}
	return names, nil
}

// ensureImage builds the fixture image when it is missing or out of date.
func ensureImage(ctx context.Context, root, image string) (string, error) {
	binary, err := buildNodeBinary(ctx, root)
	if err != nil {
		return "", err
	}
	dockerfile, err := os.ReadFile(filepath.Join(root, "testdata", "ssh", "Dockerfile"))
	if err != nil {
		return "", err
	}
	// The tag covers the Dockerfile too, so a changed image recipe is never
	// served from an image built by the old one.
	hash := sha256.New()
	hash.Write(binary)
	hash.Write(dockerfile)
	tag := image + ":" + hex.EncodeToString(hash.Sum(nil))[:16]
	if _, err := runDocker(ctx, "image", "inspect", tag); err == nil {
		return tag, nil
	}
	context, err := os.MkdirTemp("", "eregion-ssh-fixture")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(context) }()
	if err := os.WriteFile(filepath.Join(context, "Dockerfile"), dockerfile, 0o600); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(context, "eregion-node"), binary, 0o700); err != nil {
		return "", err
	}
	build := exec.CommandContext(ctx, "docker", "build", "-t", tag, context)
	if output, err := build.CombinedOutput(); err != nil {
		return "", fmt.Errorf("%w: the fixture image could not be built: %v: %s", ErrUnavailable, err, output)
	}
	pruneOldImages(ctx, image, tag)
	return tag, nil
}

// pruneOldImages removes the fixture images of an earlier node binary. It
// ignores failures: an image that is still in use must stay.
func pruneOldImages(ctx context.Context, repository, keep string) {
	output, err := runDocker(ctx, "images", "--format", "{{.Repository}}:{{.Tag}}", repository)
	if err != nil {
		return
	}
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		name := strings.TrimSpace(line)
		if name == "" || name == keep || !strings.HasPrefix(name, repository+":") {
			continue
		}
		_, _ = runDocker(ctx, "rmi", name)
	}
}

// buildNodeBinary builds the node binary for the fixture image.
func buildNodeBinary(ctx context.Context, root string) ([]byte, error) {
	directory, err := os.MkdirTemp("", "eregion-node-build")
	if err != nil {
		return nil, err
	}
	defer func() { _ = os.RemoveAll(directory) }()
	binary := filepath.Join(directory, "eregion-node")
	build := exec.CommandContext(ctx, "go", "build", "-o", binary, "./cmd/eregion-node")
	build.Dir = root
	if output, err := build.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("build the node binary: %v: %s", err, output)
	}
	return os.ReadFile(binary)
}

// generateHostKey returns a host key and the path of its private key.
func generateHostKey() (ssh.PublicKey, string, error) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", err
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		return nil, "", err
	}
	block, err := ssh.MarshalPrivateKey(key, "eregion-fixture-host")
	if err != nil {
		return nil, "", err
	}
	directory, err := os.MkdirTemp("", "eregion-ssh-host")
	if err != nil {
		return nil, "", err
	}
	path := filepath.Join(directory, "ssh_host_ed25519_key")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		return nil, "", err
	}
	return signer.PublicKey(), path, nil
}

// generateClientKey returns the client's identity file and its public half.
func generateClientKey() (ssh.Signer, string, []byte, error) {
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, "", nil, err
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		return nil, "", nil, err
	}
	block, err := ssh.MarshalPrivateKey(key, "eregion-fixture-client")
	if err != nil {
		return nil, "", nil, err
	}
	_ = block
	directory, err := os.MkdirTemp("", "eregion-ssh-client")
	if err != nil {
		return nil, "", nil, err
	}
	path := filepath.Join(directory, "id_ed25519")
	if err := os.WriteFile(path, pem.EncodeToMemory(block), 0o600); err != nil {
		return nil, "", nil, err
	}
	return signer, path, ssh.MarshalAuthorizedKey(signer.PublicKey()), nil
}

// copyIntoContainer copies a file into the node container.
func copyIntoContainer(ctx context.Context, container, source, destination string) error {
	_, err := runDocker(ctx, "cp", source, container+":"+destination)
	return err
}

// publishedAddress returns the published host:port of one container port.
func publishedAddress(ctx context.Context, container, port string) (string, error) {
	out, err := runDocker(ctx, "port", container, port)
	if err != nil {
		return "", err
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) == 0 {
		return "", errors.New("the node container publishes no port")
	}
	fields := strings.Fields(lines[len(lines)-1])
	if len(fields) == 0 {
		return "", errors.New("the node container publishes no port")
	}
	return fields[0], nil
}

// waitForPort waits until the node accepts connections.
func waitForPort(ctx context.Context, address string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		conn, err := netDialTimeout(address, time.Second)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("the node did not accept connections on %s: %w", address, err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// runDocker runs a docker command and returns its standard output.
func runDocker(ctx context.Context, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "docker", args...)
	output, err := command.CombinedOutput()
	if err != nil {
		return string(output), fmt.Errorf("docker %s: %v: %s", strings.Join(args, " "), err, output)
	}
	return string(output), nil
}

// moduleRoot finds the module root from the working directory.
func moduleRoot() (string, error) {
	directory, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(directory, "go.mod")); err == nil {
			return directory, nil
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			return "", errors.New("the module root was not found")
		}
		directory = parent
	}
}

// knownHostsLine returns the known_hosts name of an address: the host alone
// for port 22, [host]:port otherwise.
func knownHostsLine(address string) string {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return address
	}
	if port == "22" {
		return host
	}
	return "[" + host + "]:" + port
}

// randomSuffix returns a short random suffix for a container name.
func randomSuffix() string {
	value := make([]byte, 4)
	if _, err := rand.Read(value); err != nil {
		return "0000"
	}
	return hex.EncodeToString(value)
}
