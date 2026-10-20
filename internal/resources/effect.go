package resources

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const effectSchema = "eregion/effect/v1"

type effectStatus string

const (
	effectStarted   effectStatus = "started"
	effectCompleted effectStatus = "completed"
)

// effectRecord is the protected, durable idempotency record for one mutating
// action. It stores only secret-free request evidence and the final Result.
// There is no queue: a retry reads this file before reapplying an effect.
type effectRecord struct {
	Schema      string       `json:"schema"`
	EffectID    string       `json:"effect_id"`
	OperationID string       `json:"operation_id,omitempty"`
	Fingerprint string       `json:"fingerprint"`
	Action      Action       `json:"action"`
	Resource    string       `json:"resource"`
	Host        string       `json:"host"`
	Status      effectStatus `json:"status"`
	Phase       string       `json:"phase,omitempty"`
	Result      *Result      `json:"result,omitempty"`
	UpdatedAt   time.Time    `json:"updated_at"`
	CompletedAt time.Time    `json:"completed_at,omitempty"`
}

func (s *Service) effectsDir() string {
	return filepath.Join(s.config.StateDir, "effects")
}

func (s *Service) effectPath(effectID string) string {
	sum := sha256.Sum256([]byte(effectID))
	return filepath.Join(s.effectsDir(), hex.EncodeToString(sum[:])+".json")
}

func shortEffectID(effectID string) string {
	sum := sha256.Sum256([]byte(effectID))
	return hex.EncodeToString(sum[:])[:24]
}

// requestEffectIdentity returns the stable effect key and a payload fingerprint.
// The fingerprint deliberately excludes operation/effect identifiers so a
// retry with the same effect key and same action payload is accepted, while a
// reused key with different data is a conflict.
func requestEffectIdentity(req Request, spec Spec) (string, string, error) {
	effectID := strings.TrimSpace(req.EffectID)
	if effectID == "" {
		effectID = strings.TrimSpace(req.OperationID)
	}
	if effectID == "" {
		return "", "", invalidResource(
			"mutating resource action requires an operation or effect id",
			"Supply the orchestrator operation id or an explicit idempotency effect id so a lost acknowledgement is not replayed.",
		)
	}
	fingerprintRequest := req
	fingerprintRequest.OperationID = ""
	fingerprintRequest.EffectID = ""
	fingerprintRequest.Spec = spec
	encoded, err := json.Marshal(fingerprintRequest)
	if err != nil {
		return "", "", err
	}
	sum := sha256.Sum256(encoded)
	return effectID, hex.EncodeToString(sum[:]), nil
}

func (s *Service) readEffect(effectID string) (effectRecord, bool, error) {
	raw, err := readProtectedFile(s.effectPath(effectID), 256*1024)
	if err != nil {
		if os.IsNotExist(err) {
			return effectRecord{}, false, nil
		}
		return effectRecord{}, false, unavailable(
			"effect record cannot be read safely",
			"Inspect the protected service state directory; this action will not reapply an unverifiable effect.",
		)
	}
	var record effectRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return effectRecord{}, false, precondition(
			"effect record is corrupt",
			"Inspect the effect record and do not reapply the action until its status is known.",
		)
	}
	if record.Schema != effectSchema || record.EffectID != effectID ||
		(record.Status != effectStarted && record.Status != effectCompleted) ||
		record.Action == "" || record.Resource == "" || record.Host == "" || record.Fingerprint == "" {
		return effectRecord{}, false, precondition(
			"effect record is invalid",
			"Inspect the protected effect evidence; an unknown effect status is not safe to replay.",
		)
	}
	if record.Status == effectCompleted && record.Result == nil {
		return effectRecord{}, false, precondition(
			"completed effect has no durable result",
			"Inspect the effect record; do not reapply an effect whose acknowledged result is missing.",
		)
	}
	return record, true, nil
}

func (s *Service) writeEffect(record effectRecord) error {
	if err := ensurePrivateDir(s.effectsDir()); err != nil {
		return unavailable("could not prepare the effect evidence directory", "Check permissions on the protected service state directory.")
	}
	record.Schema = effectSchema
	record.UpdatedAt = s.now()
	encoded, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	if err := writeAtomic(s.effectPath(record.EffectID), append(encoded, '\n'), 0o600); err != nil {
		return unavailable("could not write the effect record", "Check permissions and free space on the protected service state directory; the action is not acknowledged without durable evidence.")
	}
	return nil
}

// beginEffect enforces mutation idempotency. A completed effect is returned for
// replay. A started effect is returned so the action can check observed state
// and converge rather than blindly duplicating an external effect.
func (s *Service) beginEffect(req Request, spec Spec) (effectRecord, bool, error) {
	if !req.Action.Mutating() {
		return effectRecord{}, false, nil
	}
	effectID, fingerprint, err := requestEffectIdentity(req, spec)
	if err != nil {
		return effectRecord{}, false, err
	}
	record, exists, err := s.readEffect(effectID)
	if err != nil {
		return effectRecord{}, false, err
	}
	if exists {
		if record.Fingerprint != fingerprint || record.Action != req.Action ||
			record.Resource != spec.Name || record.Host != spec.Host {
			return effectRecord{}, false, conflict(
				"effect id was reused for a different resource action",
				"Use a fresh operation/effect id for a different action or payload; an accepted effect id is immutable.",
			)
		}
		return record, record.Status == effectCompleted, nil
	}
	record = effectRecord{
		EffectID: effectID, OperationID: req.OperationID, Fingerprint: fingerprint,
		Action: req.Action, Resource: spec.Name, Host: spec.Host,
		Status: effectStarted, Phase: "started",
	}
	if err := s.writeEffect(record); err != nil {
		return effectRecord{}, false, err
	}
	return record, false, nil
}

func replayedResult(record effectRecord, operationID string) (Result, error) {
	if record.Result == nil {
		return Result{}, precondition("completed effect has no durable result", "Do not reapply the effect; inspect the protected effect record.")
	}
	result := *record.Result
	result.OperationID = operationID
	result.EffectID = record.EffectID
	result.Replayed = true
	return result, nil
}

func (s *Service) updateEffectPhase(effectID, phase string, result *Result) error {
	record, exists, err := s.readEffect(effectID)
	if err != nil {
		return err
	}
	if !exists {
		return precondition("effect evidence disappeared before completion", "Do not reapply the action; inspect the protected service state directory.")
	}
	record.Phase = phase
	if result != nil {
		copy := *result
		record.Result = &copy
	}
	return s.writeEffect(record)
}

func (s *Service) completeEffect(effectID string, result Result) error {
	record, exists, err := s.readEffect(effectID)
	if err != nil {
		return err
	}
	if !exists {
		return precondition("effect evidence disappeared before completion", "Do not reapply the action; inspect the protected service state directory.")
	}
	copy := result
	record.Status = effectCompleted
	record.CompletedAt = s.now()
	record.Result = &copy
	return s.writeEffect(record)
}
