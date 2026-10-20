package transport

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
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
	// Revoked reports that the known_hosts file revokes the presented key.
	Revoked bool
}

func (e *HostKeyError) Error() string {
	if e.Revoked {
		return "the host key of " + e.Host + " (" + e.Got + ") is revoked by the known_hosts file"
	}
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
	// hosts maps a plain known_hosts name to the keys pinned for it.
	hosts map[string][]ssh.PublicKey
	// revoked maps a plain known_hosts name to the keys the known_hosts file
	// revokes for it. A revoked key is never a valid pin.
	revoked map[string][]ssh.PublicKey
	// hashed holds the entries of hashed known_hosts names, pinned and
	// revoked alike.
	hashed []hashedEntry
}

// hashedEntry is one entry of a hashed known_hosts name.
type hashedEntry struct {
	// name is the hashed name from the file: "|1|salt|hash".
	name string
	// salt is the decoded salt of the name.
	salt []byte
	// key is the key of the entry.
	key ssh.PublicKey
	// revoked reports that the file revokes the key.
	revoked bool
}

// matches reports whether the entry applies to a plain known_hosts name. A
// hashed entry matches when the name hashes to the entry's name with the
// entry's salt.
func (e hashedEntry) matches(host string) bool {
	return hashHostName(host, e.salt) == e.name
}

// NewHostKeys returns an empty pin set.
func NewHostKeys() *HostKeys {
	return &HostKeys{hosts: map[string][]ssh.PublicKey{}, revoked: map[string][]ssh.PublicKey{}}
}

// HostID is the known_hosts name of an address: the host alone for port 22,
// [host]:port otherwise.
func HostID(address string) string { return knownhosts.Normalize(address) }

// LoadHostKeys reads a known_hosts file. A missing file is empty, which
// means every node is unpinned and therefore refused.
//
// The markers of a known_hosts file are honoured:
//
//   - "@revoked" never pins a key. A revoked key is refused even when
//     another line pins it, and the refusal is reported as a revocation
//     rather than as a mismatch.
//   - "@cert-authority" is skipped. A CA host certificate is out of scope
//     for a pinned host key, and such a line must never become a plain pin.
//   - A hashed host name ("|1|salt|hash") is matched against the hashed
//     address, so a hashed known_hosts file pins its node like a plain one.
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
		marker, hosts, key, _, next, err := ssh.ParseKnownHosts(rest)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		switch marker {
		case "":
			for _, host := range hosts {
				if err := keys.addHost(host, key); err != nil {
					return nil, fmt.Errorf("parse %s: %w", path, err)
				}
			}
		case "revoked":
			for _, host := range hosts {
				if err := keys.revokeHost(host, key); err != nil {
					return nil, fmt.Errorf("parse %s: %w", path, err)
				}
			}
		case "cert-authority":
			// A CA host certificate is out of scope for a pinned host key.
			// The line is skipped and never becomes a plain pin.
		default:
			return nil, fmt.Errorf("parse %s: unsupported known_hosts marker %q", path, marker)
		}
		rest = next
	}
	return keys, nil
}

// addHost pins a key for a known_hosts name. A hashed name is matched
// against the hashed address instead of being compared as a plain name.
func (h *HostKeys) addHost(host string, key ssh.PublicKey) error {
	if strings.HasPrefix(host, "|") {
		salt, err := hashedSalt(host)
		if err != nil {
			return err
		}
		h.hashed = append(h.hashed, hashedEntry{name: host, salt: salt, key: key})
		return nil
	}
	h.add(host, key)
	return nil
}

// revokeHost records a revoked key. A revoked key is never a pin, and it is
// refused even when another line pins the same key.
func (h *HostKeys) revokeHost(host string, key ssh.PublicKey) error {
	if strings.HasPrefix(host, "|") {
		salt, err := hashedSalt(host)
		if err != nil {
			return err
		}
		h.hashed = append(h.hashed, hashedEntry{name: host, salt: salt, key: key, revoked: true})
		return nil
	}
	for _, existing := range h.revoked[host] {
		if bytes.Equal(existing.Marshal(), key.Marshal()) {
			return nil
		}
	}
	h.revoked[host] = append(h.revoked[host], key)
	return nil
}

// hashedSalt returns the salt of a hashed known_hosts name.
func hashedSalt(name string) ([]byte, error) {
	parts := strings.Split(name, "|")
	if len(parts) != 4 || parts[1] != "1" {
		return nil, fmt.Errorf("unsupported hashed known_hosts entry %q", name)
	}
	salt, err := base64.StdEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("unsupported hashed known_hosts entry %q: %w", name, err)
	}
	return salt, nil
}

// hashHostName computes the hashed known_hosts name of a host with the salt
// of an entry. The format is the one OpenSSH writes: "|1|salt|hash" with an
// HMAC-SHA1 of the host name.
func hashHostName(host string, salt []byte) string {
	mac := hmac.New(sha1.New, salt)
	mac.Write([]byte(host))
	return strings.Join([]string{
		"",
		"1",
		base64.StdEncoding.EncodeToString(salt),
		base64.StdEncoding.EncodeToString(mac.Sum(nil)),
	}, "|")
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
func (h *HostKeys) Keys(address string) []ssh.PublicKey {
	host := HostID(address)
	out := make([]ssh.PublicKey, 0, len(h.hosts[host])+len(h.hashed))
	out = append(out, h.hosts[host]...)
	for _, entry := range h.hashed {
		if !entry.revoked && entry.matches(host) {
			out = append(out, entry.key)
		}
	}
	return out
}

// Pinned reports whether at least one key is pinned for an address.
func (h *HostKeys) Pinned(address string) bool { return len(h.Keys(address)) > 0 }

// Revoked reports whether the known_hosts file revokes a key for an address.
func (h *HostKeys) Revoked(address string, key ssh.PublicKey) bool {
	host := HostID(address)
	for _, want := range h.revoked[host] {
		if bytes.Equal(want.Marshal(), key.Marshal()) {
			return true
		}
	}
	for _, entry := range h.hashed {
		if entry.revoked && entry.matches(host) && bytes.Equal(entry.key.Marshal(), key.Marshal()) {
			return true
		}
	}
	return false
}

// Callback accepts only a key that is pinned for the address. A key the
// known_hosts file revokes is refused, even when another line pins it.
func (h *HostKeys) Callback(address string) ssh.HostKeyCallback {
	host := HostID(address)
	return func(_ string, _ net.Addr, key ssh.PublicKey) error {
		if h.Revoked(host, key) {
			return &HostKeyError{Host: host, Got: ssh.FingerprintSHA256(key), Revoked: true}
		}
		pinned := h.Keys(host)
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
	for _, key := range h.Keys(address) {
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
