package host

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/transport"
	"golang.org/x/crypto/ssh"
)

// RemoteConfig describes one first-contact SSH target. The host key is pinned
// through the same known_hosts implementation the node transport uses; there
// is no trust-on-first-use. The identity must be an unencrypted, owner-only
// private key.
type RemoteConfig struct {
	Address      string
	User         string
	IdentityFile string
	KnownHosts   string
	Timeout      time.Duration
}

var remoteUserPattern = regexp.MustCompile(`^[a-z_][a-z0-9._-]{0,31}$`)

// RemoteFile is one file uploaded for a remote bootstrap. Mode is applied on
// the remote side, so a database URL or token input never becomes world-readable.
type RemoteFile struct {
	Name string
	Data []byte
	Mode os.FileMode
}

// Remote runs commands on a host that is about to become a control plane or
// node. It is deliberately small: bootstrap needs mkdir, tar, cat, rm, and the
// uploaded eregion binary, not an interactive shell.
type Remote struct {
	config RemoteConfig
	signer ssh.Signer
	keys   *transport.HostKeys
}

// NewRemote validates the target and loads the pinned host keys.
func NewRemote(config RemoteConfig) (*Remote, error) {
	if config.Address == "" {
		return nil, errors.New("the remote SSH address is required")
	}
	if _, _, err := net.SplitHostPort(config.Address); err != nil {
		return nil, fmt.Errorf("the remote SSH address %q must be host:port", config.Address)
	}
	if !remoteUserPattern.MatchString(config.User) {
		return nil, fmt.Errorf("the remote SSH user %q is not a plain account name", config.User)
	}
	info, err := os.Stat(config.IdentityFile)
	if err != nil {
		return nil, fmt.Errorf("read the remote identity: %w", err)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("the remote identity %s must not be readable by group or others", config.IdentityFile)
	}
	raw, err := os.ReadFile(config.IdentityFile) //nolint:gosec // G304: operator-provided identity file.
	if err != nil {
		return nil, fmt.Errorf("read the remote identity: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(raw)
	if err != nil {
		var missing *ssh.PassphraseMissingError
		if errors.As(err, &missing) {
			return nil, fmt.Errorf("the remote identity %s is encrypted; use an unencrypted dedicated key", config.IdentityFile)
		}
		return nil, fmt.Errorf("parse the remote identity: %w", err)
	}
	keys, err := transport.LoadHostKeys(config.KnownHosts)
	if err != nil {
		return nil, err
	}
	if !keys.Pinned(config.Address) {
		return nil, fmt.Errorf("no host key is pinned for %s; add it to %s", config.Address, config.KnownHosts)
	}
	if config.Timeout <= 0 {
		config.Timeout = 30 * time.Second
	}
	return &Remote{config: config, signer: signer, keys: keys}, nil
}

func (r *Remote) connect(ctx context.Context) (*ssh.Client, error) {
	dialer := &net.Dialer{Timeout: r.config.Timeout}
	raw, err := dialer.DialContext(ctx, "tcp", r.config.Address)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", r.config.Address, err)
	}
	deadline := time.Now().Add(r.config.Timeout)
	if ctxDeadline, ok := ctx.Deadline(); ok && ctxDeadline.Before(deadline) {
		deadline = ctxDeadline
	}
	_ = raw.SetDeadline(deadline)
	config := &ssh.ClientConfig{
		User:              r.config.User,
		Auth:              []ssh.AuthMethod{ssh.PublicKeys(r.signer)},
		HostKeyCallback:   r.keys.Callback(r.config.Address),
		HostKeyAlgorithms: r.keys.Algorithms(r.config.Address),
		Timeout:           r.config.Timeout,
	}
	clientConn, channels, requests, err := ssh.NewClientConn(raw, r.config.Address, config)
	if err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("the SSH handshake with %s failed: %w", r.config.Address, err)
	}
	_ = raw.SetDeadline(time.Time{})
	return ssh.NewClient(clientConn, channels, requests), nil
}

// Run executes one command over a fresh connection. Standard input is optional;
// standard output and error are streamed to the caller so remote apply progress
// is visible without waiting for the final envelope.
func (r *Remote) Run(ctx context.Context, command string, stdin io.Reader, stdout, stderr io.Writer) error {
	client, err := r.connect(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()
	session, err := client.NewSession()
	if err != nil {
		return err
	}
	defer func() { _ = session.Close() }()
	session.Stdin, session.Stdout, session.Stderr = stdin, stdout, stderr
	if err := session.Start(command); err != nil {
		return fmt.Errorf("start the remote command: %w", err)
	}
	done := make(chan error, 1)
	go func() { done <- session.Wait() }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		_ = client.Close()
		<-done
		return ctx.Err()
	}
}

// Output runs one command and returns its standard output. It is used for
// short, non-secret reads such as the handed-off operator token.
func (r *Remote) Output(ctx context.Context, command string) ([]byte, error) {
	var out, stderr bytes.Buffer
	if err := r.Run(ctx, command, nil, &out, &stderr); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return nil, fmt.Errorf("%w: %s", err, detail)
		}
		return nil, err
	}
	return out.Bytes(), nil
}

// Upload writes files under destDir using a tar stream over standard input.
// Every member is a regular file with an explicit mode; links and traversal are
// never accepted because this code creates the archive itself.
func (r *Remote) Upload(ctx context.Context, destDir string, files []RemoteFile) error {
	if !strings.HasPrefix(destDir, "/") || strings.ContainsAny(destDir, "\x00\r\n") {
		return errors.New("the remote upload directory must be an absolute path")
	}
	var archive bytes.Buffer
	gz := gzip.NewWriter(&archive)
	tw := tar.NewWriter(gz)
	for _, file := range files {
		name := path.Base(file.Name)
		if name == "." || name == ".." || strings.ContainsAny(name, "/\x00\r\n") {
			return fmt.Errorf("unsafe remote file name %q", file.Name)
		}
		if file.Mode == 0 {
			file.Mode = 0o600
		}
		header := &tar.Header{Name: name, Mode: int64(file.Mode.Perm()), Size: int64(len(file.Data)), Typeflag: tar.TypeReg}
		if err := tw.WriteHeader(header); err != nil {
			return err
		}
		if _, err := tw.Write(file.Data); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	if err := gz.Close(); err != nil {
		return err
	}
	command := "set -eu\nmkdir -p " + shellQuote(destDir) + "\ntar -xzf - -C " + shellQuote(destDir)
	var stderr strings.Builder
	if err := r.Run(ctx, command, bytes.NewReader(archive.Bytes()), io.Discard, &stderr); err != nil {
		detail := strings.TrimSpace(stderr.String())
		if detail != "" {
			return fmt.Errorf("upload the bootstrap bundle: %w: %s", err, detail)
		}
		return fmt.Errorf("upload the bootstrap bundle: %w", err)
	}
	return nil
}

// Remove deletes one remote path. It is best-effort cleanup for a temporary
// uploaded bundle.
func (r *Remote) Remove(ctx context.Context, remotePath string) error {
	if !strings.HasPrefix(remotePath, "/") || strings.ContainsAny(remotePath, "\x00\r\n") {
		return errors.New("the remote path must be absolute")
	}
	return r.Run(ctx, "rm -rf "+shellQuote(remotePath), nil, io.Discard, io.Discard)
}
