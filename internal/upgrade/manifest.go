package upgrade

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path"
	"runtime"
	"strings"
	"time"
	"unicode"

	"github.com/mbvlabs/eregion/internal/contract"
)

// ManifestSchema identifies the signed release-manifest format. The schema is
// independent of the binary version and of the agent result contract.
const ManifestSchema = "eregion/release/v1"

// Target selects which binary an artifact replaces.
type Target string

const (
	TargetOperator Target = "operator"
	TargetNode     Target = "node"
)

// ParseTarget validates a target name.
func ParseTarget(text string) (Target, error) {
	target := Target(strings.TrimSpace(text))
	if target != TargetOperator && target != TargetNode {
		return "", contract.NewFault(contract.ValidationFailed, "upgrade target must be operator or node", "Use --target operator or --target node.")
	}
	return target, nil
}

// Supports reports whether the target may run on an operating system. The node
// binary is Linux-only; the operator CLI is portable.
func (t Target) Supports(goos string) bool {
	switch t {
	case TargetOperator:
		return goos == "linux" || goos == "darwin" || goos == "windows"
	case TargetNode:
		return goos == "linux"
	default:
		return false
	}
}

// Artifact is one downloadable, signed release archive.
type Artifact struct {
	Name   string `json:"name"`
	Target Target `json:"target"`
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	Entry  string `json:"entry"`
	URL    string `json:"url"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// Manifest is the Ed25519-signed release description operators verify before
// downloading anything. Signature covers the canonical JSON with the signature
// field empty. The schema is forward-only: unknown fields are rejected.
type Manifest struct {
	Schema        string     `json:"schema"`
	Version       string     `json:"version"`
	Channel       string     `json:"channel"`
	PublishedAt   string     `json:"published_at"`
	Contract      string     `json:"contract"`
	NodeProtocol  int        `json:"node_protocol"`
	SchemaVersion int        `json:"schema_version"`
	UpgradeFrom   []string   `json:"upgrade_from,omitempty"`
	Artifacts     []Artifact `json:"artifacts"`
	Signature     string     `json:"signature"`
}

// ParseManifest decodes one signed manifest strictly. Unknown fields are
// rejected so a future schema cannot be silently interpreted with today's
// fields; a forward-incompatible format must publish a new schema name and a
// binary that understands it. The signature is not checked here.
func ParseManifest(raw []byte) (Manifest, error) {
	var manifest Manifest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return manifest, contract.NewFault(contract.ValidationFailed, "signed manifest is not valid strict JSON", "Publish exactly the documented fields for "+ManifestSchema+"; unknown fields are refused.")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return manifest, contract.NewFault(contract.ValidationFailed, "signed manifest has trailing JSON", "Publish exactly one manifest object.")
	}
	return manifest, nil
}

// Validate checks the unsigned structure. Signature verification is separate so
// a corrupt manifest fails with a clear structural error first.
func (m Manifest) Validate() error {
	if m.Schema != ManifestSchema {
		return contract.NewFault(contract.ValidationFailed, "unsupported release manifest schema", "Use a manifest published for "+ManifestSchema+".")
	}
	if _, err := ParseVersion(m.Version); err != nil {
		return err
	}
	if strings.TrimSpace(m.Channel) == "" || len(m.Channel) > 64 {
		return contract.NewFault(contract.ValidationFailed, "release manifest channel is missing or too long", "Publish a short stable channel name.")
	}
	if _, err := time.Parse(time.RFC3339, m.PublishedAt); err != nil {
		return contract.NewFault(contract.ValidationFailed, "release manifest published_at is not RFC3339", "Publish an RFC3339 UTC timestamp.")
	}
	if !validContract(m.Contract) {
		return contract.NewFault(contract.ValidationFailed, "release manifest contract version is invalid", "Use the eregion/vN envelope contract this release implements.")
	}
	if m.NodeProtocol < 1 {
		return contract.NewFault(contract.ValidationFailed, "release manifest node protocol must be positive", "Publish the node wire protocol version the release speaks.")
	}
	if m.SchemaVersion < 1 {
		return contract.NewFault(contract.ValidationFailed, "release manifest schema_version must be positive", "Publish the forward-only database schema version the release requires.")
	}
	seenVersion := map[string]bool{}
	for _, value := range m.UpgradeFrom {
		version, err := ParseVersion(value)
		if err != nil {
			return err
		}
		if seenVersion[version.String()] {
			return contract.NewFault(contract.ValidationFailed, "release manifest repeats an upgrade_from version", "List each supported source version once.")
		}
		seenVersion[version.String()] = true
	}
	if len(m.Artifacts) == 0 {
		return contract.NewFault(contract.ValidationFailed, "release manifest has no artifacts", "Publish at least one signed artifact.")
	}
	type key struct {
		target Target
		os     string
		arch   string
	}
	seen := map[key]bool{}
	seenName := map[string]bool{}
	for _, artifact := range m.Artifacts {
		if err := artifact.validate(); err != nil {
			return err
		}
		k := key{artifact.Target, artifact.OS, artifact.Arch}
		if seen[k] {
			return contract.NewFault(contract.ValidationFailed, "release manifest has duplicate target/os/arch artifacts", "Publish exactly one artifact per platform.")
		}
		if seenName[artifact.Name] {
			return contract.NewFault(contract.ValidationFailed, "release manifest repeats an artifact name", "Give every artifact a unique file name.")
		}
		seen[k], seenName[artifact.Name] = true, true
	}
	return nil
}

func (a Artifact) validate() error {
	if a.Target != TargetOperator && a.Target != TargetNode {
		return contract.NewFault(contract.ValidationFailed, "release artifact has an unknown target", "Publish only operator and node artifacts.")
	}
	if !a.Target.Supports(a.OS) {
		return contract.NewFault(contract.ValidationFailed, "release artifact target does not support its operating system", "Node artifacts are Linux-only; operator artifacts are Linux, macOS, or Windows.")
	}
	if !validArch(a.Arch) {
		return contract.NewFault(contract.ValidationFailed, "release artifact has an unsupported architecture", "Publish amd64 or arm64 artifacts.")
	}
	if err := validName(a.Name); err != nil {
		return err
	}
	if err := validEntry(a.Entry); err != nil {
		return err
	}
	if a.Size <= 0 || a.Size > MaxArtifactBytes {
		return contract.NewFault(contract.ValidationFailed, "release artifact size is outside the allowed range", "Publish a nonempty archive smaller than the download bound.")
	}
	if len(a.SHA256) != sha256.Size*2 || strings.ToLower(a.SHA256) != a.SHA256 {
		return contract.NewFault(contract.ValidationFailed, "release artifact SHA256 is not 64 lowercase hex characters", "Checksum the exact archive bytes with SHA256.")
	}
	if _, err := hex.DecodeString(a.SHA256); err != nil {
		return contract.NewFault(contract.ValidationFailed, "release artifact SHA256 is not hexadecimal", "Checksum the exact archive bytes with SHA256.")
	}
	return ValidateArtifactURL(a.URL)
}

func validContract(value string) bool {
	if !strings.HasPrefix(value, "eregion/v") || len(value) <= len("eregion/v") {
		return false
	}
	for _, r := range value[len("eregion/v"):] {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func validArch(value string) bool {
	switch value {
	case "amd64", "arm64":
		return true
	default:
		return false
	}
}

func validName(value string) error {
	if value == "" || len(value) > 200 || value == "." || value == ".." {
		return contract.NewFault(contract.ValidationFailed, "release artifact name is missing or unsafe", "Use a plain file name without path separators.")
	}
	if path.Base(value) != value || strings.ContainsAny(value, `/\\`) {
		return contract.NewFault(contract.ValidationFailed, "release artifact name must not contain path separators", "Use a plain file name.")
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			return contract.NewFault(contract.ValidationFailed, "release artifact name contains whitespace or control characters", "Use a plain file name.")
		}
	}
	return nil
}

func validEntry(value string) error {
	if value == "" || strings.HasPrefix(value, "/") || strings.HasPrefix(value, "\\") {
		return contract.NewFault(contract.ValidationFailed, "release artifact entry is missing or absolute", "Name the binary path inside the archive without a leading slash.")
	}
	cleaned := path.Clean(value)
	if cleaned != value || cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return contract.NewFault(contract.ValidationFailed, "release artifact entry contains path traversal", "Use a plain relative path inside the archive.")
	}
	return nil
}

// canonical returns the bytes the signature covers. JSON struct field order is
// fixed and no maps are encoded, so independent signers and verifiers agree.
func (m Manifest) canonical() ([]byte, error) {
	copy := m
	copy.Signature = ""
	return json.Marshal(copy)
}

// Sign signs the canonical manifest with key and stores a standard-base64
// detached signature. It never retains or prints the private key.
func (m *Manifest) Sign(key ed25519.PrivateKey) error {
	if len(key) != ed25519.PrivateKeySize {
		return contract.NewFault(contract.ValidationFailed, "release signing key is not an Ed25519 private key", "Configure a 32-byte Ed25519 seed or 64-byte private key in the release secret.")
	}
	if err := m.Validate(); err != nil {
		return err
	}
	payload, err := m.canonical()
	if err != nil {
		return err
	}
	m.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(key, payload))
	return nil
}

// Verify checks the detached Ed25519 signature and then the structure. A
// tampered manifest, artifact list, or signature fails closed.
func (m Manifest) Verify(public ed25519.PublicKey) error {
	if len(public) != ed25519.PublicKeySize {
		return contract.NewFault(contract.ValidationFailed, "trusted release public key is not an Ed25519 public key", "Configure the published Ed25519 public key.")
	}
	if strings.TrimSpace(m.Signature) == "" {
		return contract.NewFault(contract.ValidationFailed, "release manifest is unsigned", "Refuse unsigned releases; configure the trusted public key and publish a signed manifest.")
	}
	signature, err := base64.StdEncoding.DecodeString(m.Signature)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return contract.NewFault(contract.ValidationFailed, "release manifest signature is malformed", "Republish the manifest with a valid Ed25519 signature.")
	}
	payload, err := m.canonical()
	if err != nil {
		return contract.NewFault(contract.ValidationFailed, "release manifest could not be canonicalized", "Republish a structurally valid manifest.")
	}
	if !ed25519.Verify(public, payload, signature) {
		return contract.NewFault(contract.ValidationFailed, "release manifest signature is not valid for the trusted key", "Verify the configured public key and refuse the release.")
	}
	return m.Validate()
}

// ParsePublicKey accepts a base64 (standard or raw) or hex Ed25519 public key.
// Public keys are not secrets, but callers still never log their contents.
func ParsePublicKey(text string) (ed25519.PublicKey, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, contract.NewFault(contract.InputRequired, "trusted release public key is not configured", "Set EREGION_UPGRADE_PUBLIC_KEY_FILE (preferred) or EREGION_UPGRADE_PUBLIC_KEY.")
	}
	var raw []byte
	var err error
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if raw, err = encoding.DecodeString(text); err == nil && len(raw) == ed25519.PublicKeySize {
			return ed25519.PublicKey(raw), nil
		}
	}
	if raw, err = hex.DecodeString(text); err == nil && len(raw) == ed25519.PublicKeySize {
		return ed25519.PublicKey(raw), nil
	}
	return nil, contract.NewFault(contract.ValidationFailed, "trusted release public key is malformed", "Use a base64 or hex Ed25519 public key.")
}

// ParsePrivateKey accepts a base64 Ed25519 seed (32 bytes) or private key
// (64 bytes). Only the release tooling uses it.
func ParsePrivateKey(text string) (ed25519.PrivateKey, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, contract.NewFault(contract.InputRequired, "release signing key is not configured", "Set the release signing secret before publishing.")
	}
	raw, err := base64.StdEncoding.DecodeString(text)
	if err != nil {
		raw, err = base64.RawStdEncoding.DecodeString(text)
	}
	if err != nil {
		return nil, contract.NewFault(contract.ValidationFailed, "release signing key is not valid base64", "Configure a base64 Ed25519 seed in the release secret.")
	}
	switch len(raw) {
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(raw), nil
	case ed25519.PrivateKeySize:
		return ed25519.PrivateKey(raw), nil
	default:
		return nil, contract.NewFault(contract.ValidationFailed, "release signing key has the wrong length", "Configure a 32-byte seed or 64-byte Ed25519 private key.")
	}
}

// LoadPublicKey reads a trusted public key from a regular, non-symlink file and
// refuses group- or world-writable permissions.
func LoadPublicKey(path string) (ed25519.PublicKey, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, contract.NewFault(contract.ValidationFailed, "trusted release public key file could not be read", "Check EREGION_UPGRADE_PUBLIC_KEY_FILE.")
	}
	if !info.Mode().IsRegular() {
		return nil, contract.NewFault(contract.ValidationFailed, "trusted release public key must be a regular file", "Point EREGION_UPGRADE_PUBLIC_KEY_FILE at the published key, not a symlink or device.")
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o022 != 0 {
		return nil, contract.NewFault(contract.ValidationFailed, "trusted release public key file is group- or world-writable", "chmod 0644 or stricter; the trust anchor must not be mutable by other users.")
	}
	if info.Size() > 4096 {
		return nil, contract.NewFault(contract.ValidationFailed, "trusted release public key file is too large", "Use a 32-byte Ed25519 public key.")
	}
	raw, err := os.ReadFile(path) //nolint:gosec // G304: configured trust anchor path.
	if err != nil {
		return nil, contract.NewFault(contract.ValidationFailed, "trusted release public key file could not be read", "Check EREGION_UPGRADE_PUBLIC_KEY_FILE.")
	}
	return ParsePublicKey(string(raw))
}
