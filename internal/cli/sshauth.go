package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/cli/ui"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/host"
	"github.com/mbvlabs/eregion/internal/transport"
	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
	"golang.org/x/crypto/ssh/knownhosts"
)

// sshAuth picks the SSH credential the way OpenSSH does: an explicit
// identity, otherwise ssh-agent keys plus the default ~/.ssh keys in one
// publickey method (x/crypto/ssh tries only the first method of each kind).
// An encrypted key is unlocked with a passphrase prompt when prompt is set.
func sshAuth(identity string, prompt bool) ([]ssh.AuthMethod, error) {
	if identity != "" {
		signer, err := loadSigner(expandHome(identity), prompt)
		if err != nil {
			return nil, contract.NewFault(contract.ValidationFailed, "the SSH identity "+identity+" is not usable", err.Error())
		}
		return []ssh.AuthMethod{ssh.PublicKeys(signer)}, nil
	}
	var signers []ssh.Signer
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if conn, err := net.Dial("unix", sock); err == nil {
			if agentSigners, err := agent.NewClient(conn).Signers(); err == nil {
				signers = append(signers, agentSigners...)
			}
		}
	}
	var skipped []string
	if home, err := os.UserHomeDir(); err == nil {
		for _, name := range []string{"id_ed25519", "id_ecdsa", "id_rsa"} {
			path := filepath.Join(home, ".ssh", name)
			if _, err := os.Stat(path); err != nil {
				continue
			}
			signer, err := loadSigner(path, prompt)
			if err != nil {
				skipped = append(skipped, path)
				continue
			}
			signers = append(signers, signer)
		}
	}
	if len(signers) == 0 {
		hint := "Pass --identity <key>, or load a key into ssh-agent."
		if len(skipped) > 0 {
			hint = "These keys could not be used: " + strings.Join(skipped, ", ") + ". Load one with ssh-add, or pass --identity in a terminal."
		}
		return nil, contract.NewFault(contract.InputRequired, "no SSH credential is available", hint)
	}
	return []ssh.AuthMethod{ssh.PublicKeys(signers...)}, nil
}

func loadSigner(path string, prompt bool) (ssh.Signer, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the operator's own SSH key.
	if err != nil {
		return nil, err
	}
	signer, err := ssh.ParsePrivateKey(raw)
	var missing *ssh.PassphraseMissingError
	if !errors.As(err, &missing) {
		return signer, err
	}
	if !prompt {
		return nil, errors.New("the key is encrypted; load it into ssh-agent")
	}
	pass, err := ui.Secret("Passphrase for " + path)
	if err != nil {
		return nil, err
	}
	return ssh.ParsePrivateKeyWithPassphrase(raw, []byte(pass))
}

func expandHome(path string) string {
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, rest)
		}
	}
	return path
}

// knownHostsPath is eregion's own known_hosts, next to the context file.
func knownHostsPath() (string, error) {
	contexts, err := host.DefaultContextPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(contexts), "known_hosts"), nil
}

// probeHostKey reads the server's host key without authenticating.
func probeHostKey(ctx context.Context, address string) (ssh.PublicKey, error) {
	var key ssh.PublicKey
	errProbed := errors.New("probed")
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	conn, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	_, _, _, err = ssh.NewClientConn(conn, address, &ssh.ClientConfig{
		User: "probe",
		HostKeyCallback: func(_ string, _ net.Addr, k ssh.PublicKey) error {
			key = k
			return errProbed
		},
		HostKeyAlgorithms: []string{ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256},
	})
	if key != nil {
		return key, nil
	}
	return nil, fmt.Errorf("read the host key of %s: %w", address, err)
}

// pinHostKey appends the key for address to the known_hosts file.
func pinHostKey(path, address string, key ssh.PublicKey) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600) //nolint:gosec // G304: eregion's own known_hosts.
	if err != nil {
		return err
	}
	defer file.Close()
	_, err = fmt.Fprintln(file, knownhosts.Line([]string{knownhosts.Normalize(address)}, key))
	return err
}

// contextTunnel connects to a context's host over SSH; the API client then
// dials the loopback API through it.
func contextTunnel(ctx context.Context, route *host.ContextSSH) (*ssh.Client, error) {
	auth, err := sshAuth(route.Identity, false)
	if err != nil {
		return nil, err
	}
	remote, err := host.NewRemote(host.RemoteConfig{Address: route.Address, User: route.User, KnownHosts: route.KnownHosts, Auth: auth, Timeout: 15 * time.Second})
	if err != nil {
		return nil, contract.NewFault(contract.ValidationFailed, "the context's SSH route is not usable", err.Error())
	}
	client, err := remote.Dial(ctx)
	if err != nil {
		return nil, contract.NewFault(contract.Unavailable, "could not reach the server over SSH", err.Error())
	}
	return client, nil
}

func hostKeyPinned(path, address string) bool {
	if _, err := os.Stat(path); err != nil {
		return false
	}
	keys, err := transport.LoadHostKeys(path)
	return err == nil && keys.Pinned(address)
}
