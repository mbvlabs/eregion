package node

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// IdentityContext domain-separates a node identity attestation from every other
// signature.
const IdentityContext = "eregion-node-identity-v1"

// ErrIdentityMismatch means a node presented a key other than the pinned one,
// or a signature that does not verify. It is permanent: a retry cannot make an
// impostor genuine.
var ErrIdentityMismatch = errors.New("node identity does not match the pinned identity")

// Attestation is a node's proof of identity. The node generated the key itself
// and the private half never leaves it; the control plane pins the public half
// the first time it verifies a challenge, or checks it against a fingerprint
// the operator supplied out of band. A node name or hostname is never part of
// the proof of who the node is, only of what it was asked to sign.
type Attestation struct {
	// PublicKey is the raw ed25519 public key in standard base64.
	PublicKey   string `json:"public_key"`
	Fingerprint string `json:"fingerprint"`
	// Signature signs IdentityMessage(node, nonce).
	Signature string `json:"signature"`
}

// IdentityMessage is the byte string a node signs.
func IdentityMessage(nodeName, nonce string) []byte {
	return []byte(IdentityContext + "\n" + nodeName + "\n" + nonce + "\n")
}

// Fingerprint returns the SHA256:<base64> fingerprint of an identity key.
func Fingerprint(public ed25519.PublicKey) string {
	sum := sha256.Sum256(public)
	return "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
}

// IdentityNonce derives the deterministic challenge for one operation. It is
// unique per operation and node, so a retried attempt reuses the recorded
// attestation instead of signing something new.
func IdentityNonce(operationID, nodeName string) string {
	sum := sha256.Sum256([]byte("eregion-identity-nonce\n" + operationID + "\n" + nodeName))
	return base64.RawStdEncoding.EncodeToString(sum[:])
}

// VerifyAttestation checks the signature over the challenge and, when
// pinnedKey or pinnedFingerprint is set, that the key is the pinned one. Pass
// both empty to verify a first-use attestation; the caller then pins the
// returned fingerprint.
func VerifyAttestation(a Attestation, nodeName, nonce, pinnedKey, pinnedFingerprint string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(a.PublicKey)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return "", fmt.Errorf("%w: the attestation key is malformed", ErrIdentityMismatch)
	}
	public := ed25519.PublicKey(raw)
	fingerprint := Fingerprint(public)
	if a.Fingerprint != "" && a.Fingerprint != fingerprint {
		return "", fmt.Errorf("%w: the attestation fingerprint does not describe its key", ErrIdentityMismatch)
	}
	signature, err := base64.StdEncoding.DecodeString(a.Signature)
	if err != nil || !ed25519.Verify(public, IdentityMessage(nodeName, nonce), signature) {
		return "", fmt.Errorf("%w: the signature over the challenge does not verify", ErrIdentityMismatch)
	}
	if pinnedKey != "" && pinnedKey != a.PublicKey {
		return "", fmt.Errorf("%w: the node presents %s, not the pinned key %s", ErrIdentityMismatch, fingerprint, fingerprintOfKeyText(pinnedKey))
	}
	if pinnedFingerprint != "" && pinnedFingerprint != fingerprint {
		return "", fmt.Errorf("%w: the node presents %s, expected %s", ErrIdentityMismatch, fingerprint, pinnedFingerprint)
	}
	return fingerprint, nil
}

func fingerprintOfKeyText(text string) string {
	raw, err := base64.StdEncoding.DecodeString(text)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return "(unreadable)"
	}
	return Fingerprint(ed25519.PublicKey(raw))
}

// identityPath is where a node keeps its identity key, beneath the state dir.
func identityPath(root string) string { return filepath.Join(root, "identity", "node.ed25519") }

// LoadOrCreateIdentity returns the node's identity key, creating it once. The
// private key is a 0600 file in the node's private state directory and is
// never returned over the protocol.
func LoadOrCreateIdentity(root string) (ed25519.PrivateKey, error) {
	path := identityPath(root)
	if key, err := readIdentity(path); err == nil {
		return key, nil
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	encoded := base64.StdEncoding.EncodeToString(key.Seed()) + "\n"
	// Create-once: the first writer wins and every later caller reads that key.
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600) //nolint:gosec // G304: fixed path beneath the node state directory.
	if err != nil {
		if os.IsExist(err) {
			return readIdentity(path)
		}
		return nil, err
	}
	if _, err := file.WriteString(encoded); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return nil, err
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return nil, err
	}
	return key, nil
}

func readIdentity(path string) (ed25519.PrivateKey, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: fixed path beneath the node state directory.
	if err != nil {
		return nil, err
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, errors.New("the node identity key is corrupt")
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

// Attest signs a challenge with the node's identity key.
func Attest(root, nodeName, nonce string) (Attestation, error) {
	key, err := LoadOrCreateIdentity(root)
	if err != nil {
		return Attestation{}, err
	}
	public := key.Public().(ed25519.PublicKey)
	return Attestation{
		PublicKey:   base64.StdEncoding.EncodeToString(public),
		Fingerprint: Fingerprint(public),
		Signature:   base64.StdEncoding.EncodeToString(ed25519.Sign(key, IdentityMessage(nodeName, nonce))),
	}, nil
}
