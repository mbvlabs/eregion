package entities

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities/internal/query"
	"github.com/mbvlabs/eregion/internal/platform"
)

// ErrCandidateCaptureNotOwned reports that the operation attempt or the exact
// deployment owner no longer matches the capture fence. A stale capture must
// write nothing rather than complete against newer work.
var ErrCandidateCaptureNotOwned = errors.New("candidate capture is fenced to another deployment attempt")

// ErrCandidateCaptureNotClaimed reports that completion was attempted without
// the durable claim that makes a capture retry-safe.
var ErrCandidateCaptureNotClaimed = errors.New("candidate capture has no durable claim for this step")

// CandidateCaptureFence is the exact operation, attempt, environment and
// deployment identity a candidate capture is allowed to persist. The operation
// attempt is fenced independently of the deployment owner attempt.
type CandidateCaptureFence struct {
	OperationID   contract.OperationID
	Attempt       uint32
	EnvironmentID platform.EnvironmentID
	DeploymentID  platform.DeploymentID
}

// CandidateCaptureTx is the typed candidate-capture boundary inside one
// StoreTx. Claim and Complete each lock the operation row first, then the exact
// deployment owner, and never inspect or change terminal operation state:
// terminal-owner recovery may be the first opportunity to capture.
type CandidateCaptureTx struct {
	owner *StoreTx
	fence CandidateCaptureFence
}

// CandidateCapture returns the candidate-capture view of this transaction.
func (tx *StoreTx) CandidateCapture(fence CandidateCaptureFence) *CandidateCaptureTx {
	return &CandidateCaptureTx{owner: tx, fence: fence}
}

// Claim durably records the unconfirmed capture claim for one step. It returns
// true only when this call inserted the claim and the exact owner fence still
// holds; a replay, a missing owner or a stale attempt returns false without a
// write and without an error.
func (c *CandidateCaptureTx) Claim(step string) (bool, error) {
	base, owned, err := c.lock()
	if err != nil || !owned {
		return false, err
	}
	claimed, err := c.claimExists(step)
	if err != nil || claimed {
		return false, err
	}
	if base < 0 || base == math.MaxInt64 {
		return false, errors.New("candidate capture sequence is outside the bigint range")
	}
	at := time.Now().UTC()
	event := contract.Event{
		OperationID: c.fence.OperationID,
		Sequence:    uint64(base) + 1,
		At:          at,
		Kind:        "warning",
		Attempt:     c.fence.Attempt,
		Stage:       platform.CandidateDiagnosticStage,
		Step:        step,
		Message:     "Candidate diagnostic capture started; completion is unconfirmed until a summary follows.",
		CandidateDiagnostic: &contract.CandidateDiagnostic{
			DeploymentID: string(c.fence.DeploymentID),
			Status:       "unconfirmed",
		},
	}
	if err := event.Validate(); err != nil {
		return false, err
	}
	raw, err := json.Marshal(event)
	if err != nil {
		return false, err
	}
	affected, err := c.owner.q.AppendCandidateCaptureClaim(c.owner.ctx, query.AppendCandidateCaptureClaimParams{
		EventJson:    raw,
		BaseSequence: base,
		UpdatedAt:    pgtype.Timestamptz{Time: at, Valid: true},
		OperationID:  string(c.fence.OperationID),
	})
	if err != nil {
		return false, err
	}
	if affected != 1 {
		return false, fmt.Errorf("candidate capture claim affected %d rows, want 1", affected)
	}
	return true, nil
}

// Complete appends the claimed step's observations, lines and summary as one
// atomic set. Every event is validated and assigned a contiguous sequence
// before the single CTE statement; a rejected event, a short insert, a stale
// base sequence or a bigint overflow affects no rows and rolls the whole
// completion back, leaving the durable claim honestly unconfirmed.
func (c *CandidateCaptureTx) Complete(step string, events []contract.Event) error {
	base, owned, err := c.lock()
	if err != nil {
		return err
	}
	if !owned {
		return ErrCandidateCaptureNotOwned
	}
	claimed, err := c.claimExists(step)
	if err != nil {
		return err
	}
	if !claimed {
		return ErrCandidateCaptureNotClaimed
	}
	if len(events) == 0 {
		return nil
	}
	rows, at, err := c.captureRows(base, step, events)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	affected, err := c.owner.q.AppendCaptureEvents(c.owner.ctx, query.AppendCaptureEventsParams{
		ID:        string(c.fence.OperationID),
		Column2:   base,
		Column3:   int64(len(events)),
		UpdatedAt: pgtype.Timestamptz{Time: at, Valid: true},
		Column5:   encoded,
	})
	if err != nil {
		return err
	}
	if affected != int64(len(events)) {
		return fmt.Errorf("candidate capture batch affected %d of %d events", affected, len(events))
	}
	return nil
}

type captureEventRow struct {
	Sequence int64           `json:"sequence"`
	Event    json.RawMessage `json:"event"`
}

// captureRows validates and marshals the bounded capture set. The loop is the
// count bound: base+len(events) may not exceed the bigint column, and no
// conversion or increment happens before the overflow guard.
func (c *CandidateCaptureTx) captureRows(base int64, step string, events []contract.Event) ([]captureEventRow, time.Time, error) {
	if base < 0 {
		return nil, time.Time{}, errors.New("candidate capture sequence is outside the bigint range")
	}
	rows := make([]captureEventRow, 0, len(events))
	sequence := base
	at := time.Now().UTC()
	for index := range events {
		event := &events[index]
		if event.Kind == "state" || event.Result != nil {
			return nil, time.Time{}, errors.New("candidate capture accepts only observation, log and summary events")
		}
		if event.Step != step {
			return nil, time.Time{}, errors.New("candidate capture event does not match the claimed step")
		}
		if sequence == math.MaxInt64 {
			return nil, time.Time{}, errors.New("candidate capture sequence exceeds the bigint range")
		}
		sequence++
		event.OperationID, event.Sequence, event.Attempt = c.fence.OperationID, uint64(sequence), c.fence.Attempt
		event.At = at
		if err := event.Validate(); err != nil {
			return nil, time.Time{}, err
		}
		raw, err := json.Marshal(*event)
		if err != nil {
			return nil, time.Time{}, err
		}
		rows = append(rows, captureEventRow{Sequence: sequence, Event: raw})
	}
	return rows, at, nil
}

// lock takes the operation row lock first, then the exact environment,
// operation, deployment and attempt owner row. Missing rows are a fence
// rejection, not a database failure, so Claim can treat them as "not claimed".
func (c *CandidateCaptureTx) lock() (int64, bool, error) {
	operation, err := c.owner.q.LockCandidateCaptureOperation(c.owner.ctx, string(c.fence.OperationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	if operation.Attempt != int64(c.fence.Attempt) {
		return 0, false, nil
	}
	_, err = c.owner.q.LockCandidateCaptureOwner(c.owner.ctx, query.LockCandidateCaptureOwnerParams{
		EnvironmentID: string(c.fence.EnvironmentID),
		OperationID:   string(c.fence.OperationID),
		DeploymentID:  string(c.fence.DeploymentID),
		OwnerAttempt:  int64(c.fence.Attempt),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return operation.LastSequence, true, nil
}

func (c *CandidateCaptureTx) claimExists(step string) (bool, error) {
	return c.owner.q.CandidateCaptureClaimExists(c.owner.ctx, query.CandidateCaptureClaimExistsParams{
		OperationID: string(c.fence.OperationID),
		Stage:       platform.CandidateDiagnosticStage,
		Attempt:     strconv.FormatUint(uint64(c.fence.Attempt), 10),
		Step:        step,
	})
}
