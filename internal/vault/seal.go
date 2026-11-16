package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math"
)

const (
	gcmNonceBytes = 12
	gcmTagBytes   = 16
	headerFixed   = 8 + 1 + 1 // magic + envelope version + key-ID length
)

// Seal encrypts one plaintext for an exact namespace revision. The authenticated
// header carries the envelope version and key ID so a stored ciphertext is
// self-describing for master-key rotation. AAD binds the namespace and revision,
// so ciphertext cannot be moved to another secret or revision.
func (k *KeyRing) Seal(namespace Namespace, revision uint64, plaintext []byte) ([]byte, string, error) {
	if k == nil || k.current == "" {
		return nil, "", contractFault("secret encryption key is not loaded", "Configure the protected master-key file before accepting secret mutations.")
	}
	if err := namespace.Validate(); err != nil {
		return nil, "", err
	}
	if revision == 0 {
		return nil, "", contractFault("secret revision must be positive", "Create revision 1 through the vault store; revisions are never zero.")
	}
	return k.sealEnvelope("secret", revision, plaintext, func(keyID string, revision uint64) []byte {
		return sealAAD(keyID, namespace, revision)
	})
}

// sealEnvelope builds the authenticated envelope shared by application secrets
// and provider credentials. kind labels faults, and aad binds the caller's
// exact identity to the ciphertext.
func (k *KeyRing) sealEnvelope(kind string, revision uint64, plaintext []byte, aad func(keyID string, revision uint64) []byte) ([]byte, string, error) {
	key := k.keys[k.current]
	if key == nil {
		return nil, "", contractFault(kind+" encryption key is missing", "Repair the protected master-key file.")
	}
	block, err := aes.NewCipher(key.key[:])
	if err != nil {
		return nil, "", contractFault(kind+" encryption is unavailable", "Check the binary build and master-key format.")
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, "", contractFault(kind+" encryption is unavailable", "Check the binary build.")
	}
	nonce := make([]byte, gcmNonceBytes)
	if _, err := rand.Read(nonce); err != nil {
		return nil, "", contractFault("could not generate a "+kind+" nonce", "Check the operating system random source.")
	}
	keyID := key.id
	keyIDLen := len(keyID)
	if keyIDLen > math.MaxUint8 {
		return nil, "", contractFault(kind+" encryption key ID is too long", "Repair the protected master-key file.")
	}
	envelope := make([]byte, 0, headerFixed+keyIDLen+gcmNonceBytes+len(plaintext)+gcmTagBytes)
	envelope = append(envelope, envelopeMagic[:]...)
	envelope = append(envelope, EnvelopeVersion)
	envelope = append(envelope, byte(keyIDLen))
	envelope = append(envelope, keyID...)
	envelope = append(envelope, nonce...)
	envelope = aead.Seal(envelope, nonce, plaintext, aad(keyID, revision))
	return envelope, keyID, nil
}

// Open authenticates and decrypts one envelope. A wrong key, namespace,
// revision, header, or ciphertext all return ErrCiphertext without detail.
func (k *KeyRing) Open(namespace Namespace, revision uint64, envelope []byte) ([]byte, string, error) {
	if err := namespace.Validate(); err != nil {
		return nil, "", err
	}
	if revision == 0 {
		return nil, "", contractFault("secret revision must be positive", "Read an existing immutable revision.")
	}
	return k.openEnvelope("secret", revision, envelope, func(keyID string, revision uint64) []byte {
		return sealAAD(keyID, namespace, revision)
	})
}

// SealProvider encrypts one provider credential for an exact typed scope and
// revision. The AAD includes a provider-scope tag, so provider ciphertext can
// never be opened as an application secret (or vice versa).
func (k *KeyRing) SealProvider(scope ProviderScope, revision uint64, plaintext []byte) ([]byte, string, error) {
	if k == nil || k.current == "" {
		return nil, "", contractFault("provider credential encryption key is not loaded", "Configure the protected master-key file before accepting provider credentials.")
	}
	if err := scope.Validate(); err != nil {
		return nil, "", err
	}
	if revision == 0 {
		return nil, "", contractFault("provider credential revision must be positive", "Create revision 1 through the provider store; revisions are never zero.")
	}
	return k.sealEnvelope("provider credential", revision, plaintext, func(keyID string, revision uint64) []byte {
		return providerSealAAD(keyID, scope, revision)
	})
}

// OpenProvider authenticates and decrypts one provider credential envelope.
// A wrong key, scope, revision, header, or ciphertext all return
// ErrCiphertext without detail.
func (k *KeyRing) OpenProvider(scope ProviderScope, revision uint64, envelope []byte) ([]byte, string, error) {
	if err := scope.Validate(); err != nil {
		return nil, "", err
	}
	if revision == 0 {
		return nil, "", contractFault("provider credential revision must be positive", "Read an existing immutable revision.")
	}
	return k.openEnvelope("provider credential", revision, envelope, func(keyID string, revision uint64) []byte {
		return providerSealAAD(keyID, scope, revision)
	})
}

// openEnvelope authenticates and decrypts one envelope. kind labels faults, and
// aad must exactly match the authenticated header used when it was sealed.
func (k *KeyRing) openEnvelope(kind string, revision uint64, envelope []byte, aad func(keyID string, revision uint64) []byte) ([]byte, string, error) {
	keyID, nonce, ciphertext, err := parseEnvelope(envelope)
	if err != nil {
		return nil, "", err
	}
	if k == nil {
		return nil, "", contractFault(kind+" encryption key is not loaded", "Configure the protected master-key file.")
	}
	key := k.keys[keyID]
	if key == nil {
		return nil, "", contractFault(kind+" ciphertext references an unavailable key", "Restore the key ID from the protected key file; never delete an old key before re-encrypting its revisions.")
	}
	block, err := aes.NewCipher(key.key[:])
	if err != nil {
		return nil, "", ErrCiphertext
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, "", ErrCiphertext
	}
	plaintext, err := aead.Open(nil, nonce, ciphertext, aad(keyID, revision))
	if err != nil {
		return nil, "", ErrCiphertext
	}
	return plaintext, keyID, nil
}

func providerSealAAD(keyID string, scope ProviderScope, revision uint64) []byte {
	aad := make([]byte, 0, 64+len(keyID)+len(scope.Provider)+len(scope.ZoneID))
	aad = append(aad, envelopeMagic[:]...)
	aad = append(aad, EnvelopeVersion)
	//nolint:gosec // G115: key IDs are bounded to 64 bytes by keyIDPattern and parseEnvelope.
	aad = binary.BigEndian.AppendUint16(aad, uint16(len(keyID)))
	aad = append(aad, keyID...)
	aad = append(aad, []byte("provider-scope")...)
	aad = appendField(aad, scope.Provider)
	aad = appendField(aad, scope.ZoneID)
	aad = binary.BigEndian.AppendUint64(aad, revision)
	return aad
}

func parseEnvelope(envelope []byte) (string, []byte, []byte, error) {
	if len(envelope) < headerFixed+gcmNonceBytes+gcmTagBytes {
		return "", nil, nil, ErrCiphertext
	}
	for i := range envelopeMagic {
		if envelope[i] != envelopeMagic[i] {
			return "", nil, nil, ErrCiphertext
		}
	}
	if envelope[8] != EnvelopeVersion {
		return "", nil, nil, contractFault("secret ciphertext uses an unsupported envelope version", "Use a compatible binary; never downgrade ciphertext format.")
	}
	keyLen := int(envelope[9])
	if keyLen < 1 || keyLen > 64 || len(envelope) < headerFixed+keyLen+gcmNonceBytes+gcmTagBytes {
		return "", nil, nil, ErrCiphertext
	}
	keyID := string(envelope[headerFixed : headerFixed+keyLen])
	if !keyIDPattern.MatchString(keyID) {
		return "", nil, nil, ErrCiphertext
	}
	offset := headerFixed + keyLen
	nonce := envelope[offset : offset+gcmNonceBytes]
	ciphertext := envelope[offset+gcmNonceBytes:]
	return keyID, nonce, ciphertext, nil
}

func sealAAD(keyID string, namespace Namespace, revision uint64) []byte {
	aad := make([]byte, 0, 8+1+2+len(keyID)+len(namespace.App)+len(namespace.Environment)+len(namespace.Name)+8)
	aad = append(aad, envelopeMagic[:]...)
	aad = append(aad, EnvelopeVersion)
	//nolint:gosec // G115: key IDs are bounded to 64 bytes by keyIDPattern and parseEnvelope.
	aad = binary.BigEndian.AppendUint16(aad, uint16(len(keyID)))
	aad = append(aad, keyID...)
	aad = appendField(aad, namespace.App)
	aad = appendField(aad, namespace.Environment)
	aad = appendField(aad, namespace.Name)
	aad = binary.BigEndian.AppendUint64(aad, revision)
	return aad
}

func appendField(dst []byte, value string) []byte {
	//nolint:gosec // G115: validated namespace and provider fields are bounded well below 65536 bytes.
	dst = binary.BigEndian.AppendUint16(dst, uint16(len(value)))
	return append(dst, value...)
}

// RevisionHash binds a ciphertext to its exact namespace revision without using
// plaintext. It is deterministic across processes and safe to persist or log.
func RevisionHash(namespace Namespace, revision uint64, keyID string, ciphertext []byte) string {
	hash := sha256.New()
	hash.Write([]byte("eregion-secret-revision/v1"))
	writeHashField(hash, namespace.App)
	writeHashField(hash, namespace.Environment)
	writeHashField(hash, namespace.Name)
	var number [8]byte
	binary.BigEndian.PutUint64(number[:], revision)
	hash.Write(number[:])
	writeHashField(hash, keyID)
	writeHashField(hash, string(ciphertext))
	return hex.EncodeToString(hash.Sum(nil))
}

type hashWriter interface {
	Write([]byte) (int, error)
}

func writeHashField(writer hashWriter, value string) {
	var length [4]byte
	//nolint:gosec // G115: secret and metadata fields are far below the 4 GiB uint32 limit.
	binary.BigEndian.PutUint32(length[:], uint32(len(value)))
	_, _ = writer.Write(length[:])
	_, _ = writer.Write([]byte(value))
}

// CiphertextSHA256 is a non-secret integrity aid stored beside the ciphertext.
func CiphertextSHA256(ciphertext []byte) string {
	sum := sha256.Sum256(ciphertext)
	return hex.EncodeToString(sum[:])
}

// IsCiphertextError reports an authentication failure without exposing detail.
func IsCiphertextError(err error) bool { return errors.Is(err, ErrCiphertext) }
