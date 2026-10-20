package host

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
)

// ConfirmationTTL is how long a plan-bound confirmation remains valid.
const ConfirmationTTL = 15 * time.Minute

// Confirmation is a plan-bound, expiring acknowledgement. It is not an
// authorization token: it proves the operator saw the exact plan that apply is
// about to run.
type Confirmation struct {
	Token     string    `json:"token"`
	ExpiresAt time.Time `json:"expires_at"`
}

func planInputs(configHash string, generation uint32, risks []string) []byte {
	copied := append([]string(nil), risks...)
	sortStrings(copied)
	raw := fmt.Sprintf("eregion/host/plan/v1\x00%s\x00%d\x00%s", configHash, generation, strings.Join(copied, "\n"))
	sum := sha256.Sum256([]byte(raw))
	return []byte(hex.EncodeToString(sum[:]))
}

// inputHash produces the normalized, lowercase SHA-256 digest the plan binding
// requires. Callers pass normalized inputs, not a pre-computed digest.
func inputHash(inputs []byte) string {
	sum := sha256.Sum256(inputs)
	return hex.EncodeToString(sum[:])
}

func issueConfirmation(action, target, baseRevision string, inputs []byte, now time.Time) (Confirmation, error) {
	expires := now.Add(ConfirmationTTL).UTC().Truncate(time.Second)
	digest, err := (contract.PlanBinding{
		Actor:        contract.Actor{Kind: "system", Subject: "host-bootstrap"},
		Action:       action,
		Target:       target,
		BaseRevision: baseRevision,
		InputsHash:   inputHash(inputs),
		ExpiresAt:    expires,
	}).Digest()
	if err != nil {
		return Confirmation{}, err
	}
	return Confirmation{Token: digest + ":" + strconv.FormatInt(expires.Unix(), 10), ExpiresAt: expires}, nil
}

func verifyConfirmation(token, action, target, baseRevision string, inputs []byte, now time.Time) error {
	digest, expiryText, ok := strings.Cut(strings.TrimSpace(token), ":")
	if !ok || len(digest) != sha256.Size*2 {
		return confirmationRequired("the confirmation token is malformed")
	}
	expiryUnix, err := strconv.ParseInt(expiryText, 10, 64)
	if err != nil {
		return confirmationRequired("the confirmation token has no valid expiry")
	}
	expires := time.Unix(expiryUnix, 0).UTC()
	if now.After(expires) {
		return contract.NewFault(contract.ConfirmationRequired, "the bootstrap plan has expired", "Run host plan again and review the current diff before applying.")
	}
	want, err := (contract.PlanBinding{
		Actor:        contract.Actor{Kind: "system", Subject: "host-bootstrap"},
		Action:       action,
		Target:       target,
		BaseRevision: baseRevision,
		InputsHash:   inputHash(inputs),
		ExpiresAt:    expires,
	}).Digest()
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare([]byte(digest), []byte(want)) != 1 {
		return confirmationRequired("the confirmation does not match this host intent")
	}
	return nil
}

func confirmationRequired(message string) error {
	return contract.NewFault(contract.ConfirmationRequired, message, "Run host plan, review the plan and diff, then pass its confirmation token to host apply.")
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
