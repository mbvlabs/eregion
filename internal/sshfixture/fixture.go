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
}

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
	identity, identityPath, authorized, err := generateClientKey()
	if err != nil {
		return nil, err
	}
	_ = identity

	container := "eregion-ssh-fixture-" + strconv.Itoa(os.Getpid()) + "-" + randomSuffix()
	if _, err := runDocker(ctx, "run", "-d", "--init", "--name", container, "-p", "127.0.0.1::2222", image, "sleep", "infinity"); err != nil {
		return nil, err
	}
	fixture := &Fixture{Name: container, image: image, HostKey: hostKey, IdentityFile: identityPath}
	cleanup := func() { _, _ = runDocker(context.Background(), "rm", "-f", container) }

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
	address, err := publishedAddress(ctx, container)
	if err != nil {
		cleanup()
		return nil, err
	}
	fixture.Address = address
	knownHosts := filepath.Join(filepath.Dir(identityPath), "known_hosts")
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(hostKey)))
	if err := os.WriteFile(knownHosts, []byte(knownHostsLine(address)+" "+line+"\n"), 0o600); err != nil {
		cleanup()
		return nil, err
	}
	fixture.KnownHostsFile = knownHosts
	if err := waitForPort(ctx, address, 30*time.Second); err != nil {
		cleanup()
		return nil, err
	}
	return fixture, nil
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
	sum := sha256.Sum256(binary)
	tag := image + ":" + hex.EncodeToString(sum[:])[:16]
	if _, err := runDocker(ctx, "image", "inspect", tag); err == nil {
		return tag, nil
	}
	context, err := os.MkdirTemp("", "eregion-ssh-fixture")
	if err != nil {
		return "", err
	}
	defer func() { _ = os.RemoveAll(context) }()
	dockerfile, err := os.ReadFile(filepath.Join(root, "testdata", "ssh", "Dockerfile"))
	if err != nil {
		return "", err
	}
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

// publishedAddress returns the published host:port of the node's SSH server.
func publishedAddress(ctx context.Context, container string) (string, error) {
	out, err := runDocker(ctx, "port", container, "2222/tcp")
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
