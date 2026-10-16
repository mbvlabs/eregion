package contract

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"
)

// PlanBinding identifies exactly what a server-side confirmation authorizes.
// A digest is NOT an authorization token. Signing, expiry enforcement, and checking
// the current revision belong to the future authenticated service.
type PlanBinding struct {
	Actor        Actor     `json:"actor"`
	Action       string    `json:"action"`
	Target       string    `json:"target"`
	BaseRevision string    `json:"base_revision"`
	InputsHash   string    `json:"inputs_hash"`
	ExpiresAt    time.Time `json:"expires_at"`
}

func (b PlanBinding) Digest() (string, error) {
	if err := b.Actor.Validate(); err != nil {
		return "", err
	}
	if strings.TrimSpace(b.Action) == "" || strings.TrimSpace(b.Target) == "" || strings.TrimSpace(b.BaseRevision) == "" || b.ExpiresAt.IsZero() {
		return "", NewFault(ValidationFailed, "incomplete confirmation binding", "Bind actor, action, target, revision, normalized input hash, and expiry.")
	}
	decoded, err := hex.DecodeString(b.InputsHash)
	if err != nil || len(decoded) != sha256.Size || b.InputsHash != strings.ToLower(b.InputsHash) {
		return "", NewFault(ValidationFailed, "invalid confirmation input hash", "Use a lowercase SHA-256 digest of normalized, validated inputs.")
	}
	b.ExpiresAt = b.ExpiresAt.UTC()
	encoded, err := json.Marshal(b)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:]), nil
}
