package transport

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"sort"
	"strings"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// HostKeyError is a presented host key that is not the pinned one.
type HostKeyError struct {
	// Host is the known_hosts name of the address.
	Host string
	// Got is the fingerprint of the key the node presented.
	Got string
	// Want lists the fingerprints of the pinned keys.
	Want []string
	// Unknown reports that no key is pinned for the host at all.
	Unknown bool
}

func (e *HostKeyError) Error() string {
	if e.Unknown {
		return "no host key is pinned for " + e.Host
	}
	return "the host key of " + e.Host + " (" + e.Got + ") does not match the pinned key"
}

// HostKeys holds the pinned host keys of the nodes the control plane talks
// to. A presented key must equal one of the host's pinned keys, so a host
// pinned with several key types never trips a false mismatch. There is no
// trust-on-first-use here: an unpinned host is a hard failure.
type HostKeys struct {
	hosts map[string][]ssh.PublicKey
}

// NewHostKeys returns an empty pin set.
func NewHostKeys() *HostKeys {
	return &HostKeys{hosts: map[string][]ssh.PublicKey{}}
}

// HostID is the known_hosts name of an address: the host alone for port 22,
// [host]:port otherwise.
func HostID(address string) string { return knownhosts.Normalize(address) }

// LoadHostKeys reads a known_hosts file. A missing file is empty, which
// means every node is unpinned and therefore refused.
func LoadHostKeys(path string) (*HostKeys, error) {
	keys := NewHostKeys()
	raw, err := os.ReadFile(path) //nolint:gosec // G304: the operator's known_hosts file.
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return keys, nil
		}
		return nil, err
	}
	rest := raw
	for len(bytes.TrimSpace(rest)) > 0 {
		_, hosts, key, _, next, err := ssh.ParseKnownHosts(rest)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		for _, host := range hosts {
			keys.add(host, key)
		}
		rest = next
	}
	return keys, nil
}

// Pin pins a host key for an address.
func (h *HostKeys) Pin(address string, key ssh.PublicKey) {
	h.add(HostID(address), key)
}

func (h *HostKeys) add(host string, key ssh.PublicKey) {
	for _, existing := range h.hosts[host] {
		if bytes.Equal(existing.Marshal(), key.Marshal()) {
			return
		}
	}
	h.hosts[host] = append(h.hosts[host], key)
}

// Keys returns the keys pinned for an address.
func (h *HostKeys) Keys(address string) []ssh.PublicKey { return h.hosts[HostID(address)] }

// Pinned reports whether at least one key is pinned for an address.
func (h *HostKeys) Pinned(address string) bool { return len(h.hosts[HostID(address)]) > 0 }

// Callback accepts only a key that is pinned for the address.
func (h *HostKeys) Callback(address string) ssh.HostKeyCallback {
	host := HostID(address)
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		pinned := h.hosts[host]
		for _, want := range pinned {
			if bytes.Equal(want.Marshal(), key.Marshal()) {
				return nil
			}
		}
		failure := &HostKeyError{Host: host, Got: ssh.FingerprintSHA256(key), Unknown: len(pinned) == 0}
		for _, want := range pinned {
			failure.Want = append(failure.Want, ssh.FingerprintSHA256(want))
		}
		return failure
	}
}

// Algorithms returns the host key algorithms to offer for an address, so the
// node presents a key type that is pinned.
func (h *HostKeys) Algorithms(address string) []string {
	var out []string
	seen := map[string]bool{}
	add := func(algorithms ...string) {
		for _, algorithm := range algorithms {
			if !seen[algorithm] {
				seen[algorithm] = true
				out = append(out, algorithm)
			}
		}
	}
	for _, key := range h.hosts[HostID(address)] {
		switch key.Type() {
		case ssh.KeyAlgoRSA:
			add(ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256)
		default:
			add(key.Type())
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i] == ssh.KeyAlgoED25519 && out[j] != ssh.KeyAlgoED25519 })
	return out
}

// ParseHostKey parses one authorized_keys-style host key line.
func ParseHostKey(line string) (ssh.PublicKey, error) {
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(strings.TrimSpace(line)))
	if err != nil {
		return nil, err
	}
	return key, nil
}
