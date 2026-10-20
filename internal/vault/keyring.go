package vault

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
)

const (
	// KeyRingFormat versions the protected master-key file.
	KeyRingFormat = "eregion/vault-keyring/v1"
	// EnvelopeVersion versions the authenticated ciphertext header.
	EnvelopeVersion byte = 1

	maxKeyFileBytes = 64 * 1024
	maxKeyRingKeys  = 16
)

var (
	keyIDPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{2,63}$`)
	envelopeMagic = [8]byte{'E', 'R', 'G', 'S', 'E', 'C', '0', '1'}
)

// ErrCiphertext means authenticated decryption failed: the ciphertext, header,
// namespace, revision, or key is wrong. Callers must not reveal which.
var ErrCiphertext = errors.New("secret ciphertext failed authentication")

type masterKey struct {
	id        string
	key       [32]byte
	createdAt time.Time
}

// KeyRing holds one current encryption key and zero or more older decryption
// keys. Old keys remain usable for existing ciphertext so a key rotation never
// makes stored revisions unreadable.
type KeyRing struct {
	current string
	keys    map[string]*masterKey
}

// LoadKeyRing reads a protected 0600 (or stricter) regular file. Symlinks are
// rejected, permissions are checked on the opened descriptor, and the content is
// bounded before JSON decoding.
func LoadKeyRing(path string) (*KeyRing, error) {
	raw, err := readProtectedFile(path, maxKeyFileBytes)
	if err != nil {
		return nil, err
	}
	defer clearBytes(raw)
	var file keyFile
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&file); err != nil || decoder.Decode(new(any)) != io.EOF {
		return nil, contract.NewFault(contract.ValidationFailed, "invalid master-key file", "Use the documented eregion/vault-keyring/v1 JSON file with no unknown fields.")
	}
	if file.Format != KeyRingFormat || !keyIDPattern.MatchString(file.Current) || len(file.Keys) == 0 || len(file.Keys) > maxKeyRingKeys {
		return nil, contract.NewFault(contract.ValidationFailed, "invalid master-key file identity", "Provide 1–16 keys, a valid current key ID, and the supported format.")
	}
	ring := &KeyRing{current: file.Current, keys: make(map[string]*masterKey, len(file.Keys))}
	for _, entry := range file.Keys {
		if !keyIDPattern.MatchString(entry.ID) {
			return nil, keyFileFault("master-key file contains an invalid key ID")
		}
		if _, exists := ring.keys[entry.ID]; exists {
			return nil, keyFileFault("master-key file contains a duplicate key ID")
		}
		material, err := base64.StdEncoding.Strict().DecodeString(entry.Key)
		if err != nil || len(material) != 32 {
			clearBytes(material)
			return nil, keyFileFault("master-key file contains a key that is not 32 base64-encoded bytes")
		}
		key := &masterKey{id: entry.ID, createdAt: entry.CreatedAt.UTC()}
		copy(key.key[:], material)
		clearBytes(material)
		ring.keys[entry.ID] = key
	}
	if _, ok := ring.keys[ring.current]; !ok {
		return nil, keyFileFault("master-key file current key is missing")
	}
	return ring, nil
}

func keyFileFault(message string) *contract.Fault {
	fault := contract.NewFault(contract.ValidationFailed, message, "Regenerate the master-key file with 32-byte keys and unique IDs.")
	return fault
}

type keyFile struct {
	Format  string         `json:"format"`
	Current string         `json:"current"`
	Keys    []keyFileEntry `json:"keys"`
}

type keyFileEntry struct {
	ID        string    `json:"id"`
	Key       string    `json:"key"`
	CreatedAt time.Time `json:"created_at"`
}

// GenerateKeyRing creates a new ring with one random current key.
func GenerateKeyRing() (*KeyRing, error) {
	key := &masterKey{createdAt: time.Now().UTC()}
	if _, err := rand.Read(key.key[:]); err != nil {
		return nil, contract.NewFault(contract.Internal, "could not generate a master key", "Check the operating system random source.")
	}
	var idBytes [6]byte
	if _, err := rand.Read(idBytes[:]); err != nil {
		return nil, contract.NewFault(contract.Internal, "could not generate a key ID", "Check the operating system random source.")
	}
	key.id = "k_" + hex.EncodeToString(idBytes[:])
	return &KeyRing{current: key.id, keys: map[string]*masterKey{key.id: key}}, nil
}

// WriteKeyRing creates a new protected key file. It never overwrites an
// existing path; rotate by writing a new file and switching the configured path.
func WriteKeyRing(path string, ring *KeyRing) error {
	if ring == nil || len(ring.keys) == 0 || ring.keys[ring.current] == nil {
		return keyFileFault("key ring is incomplete")
	}
	if !filepath.IsAbs(path) || strings.ContainsAny(path, "\x00\r\n") {
		return contract.NewFault(contract.ValidationFailed, "master-key destination must be an absolute path", "Write the key file to a protected local directory.")
	}
	entries := make([]keyFileEntry, 0, len(ring.keys))
	ids := ring.KeyIDs()
	for _, id := range ids {
		key := ring.keys[id]
		entries = append(entries, keyFileEntry{ID: id, Key: base64.StdEncoding.EncodeToString(key.key[:]), CreatedAt: key.createdAt})
	}
	raw, err := json.MarshalIndent(keyFile{Format: KeyRingFormat, Current: ring.current, Keys: entries}, "", "  ")
	if err != nil {
		return contract.NewFault(contract.Internal, "could not encode the master-key file", "Report the binary version.")
	}
	raw = append(raw, '\n')
	defer clearBytes(raw)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return contract.NewFault(contract.PreconditionFailed, "could not create the master-key file", "Choose a new absolute path in a protected directory; existing files are never overwritten.")
	}
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(path)
		}
	}()
	if _, err := file.Write(raw); err != nil {
		_ = file.Close()
		return contract.NewFault(contract.Unavailable, "could not write the master-key file", "Check directory permissions and free space.")
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return contract.NewFault(contract.Unavailable, "could not sync the master-key file", "Check the filesystem.")
	}
	if err := file.Close(); err != nil {
		return contract.NewFault(contract.Unavailable, "could not close the master-key file", "Check the filesystem.")
	}
	cleanup = false
	return nil
}

// CurrentKeyID returns the key used for new ciphertext.
func (k *KeyRing) CurrentKeyID() string {
	if k == nil {
		return ""
	}
	return k.current
}

// KeyIDs returns sorted key IDs without key material.
func (k *KeyRing) KeyIDs() []string {
	if k == nil {
		return nil
	}
	ids := make([]string, 0, len(k.keys))
	for id := range k.keys {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

// CheckRotation reports whether the current key is older than maxAge. It is a
// policy signal for operators; old keys stay available for decryption.
func (k *KeyRing) CheckRotation(now time.Time, maxAge time.Duration) error {
	if k == nil || maxAge <= 0 {
		return nil
	}
	key := k.keys[k.current]
	if key == nil || key.createdAt.IsZero() || now.Sub(key.createdAt) <= maxAge {
		return nil
	}
	return contract.NewFault(contract.PreconditionFailed, "current master key is due for rotation", "Add a new key to the protected key file, make it current, and restart the control plane. Existing revisions remain readable under the old key.")
}

func readProtectedFile(path string, max int64) ([]byte, error) {
	if !filepath.IsAbs(path) || strings.ContainsAny(path, "\x00\r\n") {
		return nil, contract.NewFault(contract.ValidationFailed, "protected file path must be absolute", "Use an absolute path to a regular file readable only by its owner.")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, contract.NewFault(contract.PreconditionFailed, "protected file is unavailable or not a regular file", "Use a real 0600 file; symlinks, devices, and directories are rejected.")
	}
	if info.Mode().Perm()&0o077 != 0 || info.Mode().Perm()&0o400 == 0 {
		return nil, contract.NewFault(contract.PreconditionFailed, "protected file permissions are unsafe", "Set mode 0600 (or stricter) so no group or other user can read it.")
	}
	if info.Size() <= 0 || info.Size() > max {
		return nil, contract.NewFault(contract.PreconditionFailed, "protected file size is outside the supported bound", "Use a nonempty file no larger than the documented limit.")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, contract.NewFault(contract.PreconditionFailed, "protected file could not be opened", "Check ownership and permissions.")
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) || !opened.Mode().IsRegular() || opened.Mode().Perm()&0o077 != 0 {
		return nil, contract.NewFault(contract.PreconditionFailed, "protected file changed while it was opened", "Replace the file atomically and retry.")
	}
	raw, err := io.ReadAll(io.LimitReader(file, max+1))
	if err != nil || int64(len(raw)) > max {
		clearBytes(raw)
		return nil, contract.NewFault(contract.PreconditionFailed, "protected file could not be read within its bound", "Check the file size and permissions.")
	}
	return raw, nil
}

func clearBytes(raw []byte) {
	for i := range raw {
		raw[i] = 0
	}
}

func isHexDigest(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// String never exposes key material.
func (k *KeyRing) String() string {
	if k == nil {
		return "vault key ring (unloaded)"
	}
	return fmt.Sprintf("vault key ring current=%s keys=%d", k.current, len(k.keys))
}
