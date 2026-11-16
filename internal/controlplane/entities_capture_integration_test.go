package controlplane

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"math"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/platform"
)

func operationLastSequence(t *testing.T, pool *pgxpool.Pool, id contract.OperationID) int64 {
	t.Helper()
	var sequence int64
	if err := pool.QueryRow(context.Background(), `SELECT last_sequence FROM eregion_operations WHERE id=$1`, id).Scan(&sequence); err != nil {
		t.Fatal(err)
	}
	return sequence
}

// WithinTx owns begin, commit and rollback. A callback error, a panic, a
// canceled context and a stale fence must all leave no partial capture state and
// release the borrowed pool for the next transaction.
func TestIntegrationEntitiesWithinTxOwnsRollback(t *testing.T) {
	f := newCandidateCaptureFixture(t, "entities-tx-rollback", nil)
	store := f.service.entities
	ctx := context.Background()
	fence := entities.CandidateCaptureFence{
		OperationID:   f.op.ID,
		Attempt:       1,
		EnvironmentID: f.env.ID,
		DeploymentID:  f.dep.ID,
	}
	if rejected, err := entities.NewStore(nil); err == nil || rejected != nil {
		t.Fatalf("nil pool store = %v, %v; want a rejection", rejected, err)
	}
	before := operationLastSequence(t, f.pool, f.op.ID)

	// A callback error rolls back a claim that was already written inside the
	// transaction, and the callback error is preserved rather than replaced.
	callbackErr := errors.New("entities callback failure")
	err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		claimed, err := tx.CandidateCapture(fence).Claim("entities-error-v1")
		if err != nil || !claimed {
			t.Fatalf("claim before callback error = %v, %v", claimed, err)
		}
		return callbackErr
	})
	if !errors.Is(err, callbackErr) {
		t.Fatalf("callback error = %v, want %v", err, callbackErr)
	}
	if events := readCandidateCaptureEvents(t, f.pool, f.op.ID); events.claims != 0 {
		t.Fatalf("callback error retained %d claims", events.claims)
	}
	if got := operationLastSequence(t, f.pool, f.op.ID); got != before {
		t.Fatalf("callback error sequence = %d, want %d", got, before)
	}

	// A panic unwinds the transaction before it escapes to the caller.
	func() {
		defer func() {
			if recovered := recover(); recovered != "entities transaction panic" {
				t.Fatalf("transaction panic = %v, want the caller-visible panic", recovered)
			}
		}()
		_ = store.WithinTx(ctx, func(tx *entities.StoreTx) error {
			claimed, err := tx.CandidateCapture(fence).Claim("entities-panic-v1")
			if err != nil || !claimed {
				t.Fatalf("claim before panic = %v, %v", claimed, err)
			}
			panic("entities transaction panic")
		})
	}()
	if events := readCandidateCaptureEvents(t, f.pool, f.op.ID); events.claims != 0 {
		t.Fatalf("panic retained %d claims", events.claims)
	}
	if got := operationLastSequence(t, f.pool, f.op.ID); got != before {
		t.Fatalf("panic sequence = %d, want %d", got, before)
	}

	// A canceled caller context is rejected by begin, and does not poison the
	// pool or the next transaction.
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := store.WithinTx(canceled, func(*entities.StoreTx) error {
		t.Fatal("canceled transaction body ran")
		return nil
	}); err == nil {
		t.Fatal("canceled context opened a transaction")
	}

	// Completion without the durable claim is refused before any event write.
	unclaimedStep := "entities-unclaimed-v1"
	err = store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		return tx.CandidateCapture(fence).Complete(unclaimedStep, []contract.Event{{
			Kind:    "warning",
			Stage:   platform.CandidateDiagnosticStage,
			Step:    unclaimedStep,
			Message: "must not persist without a claim",
		}})
	})
	if !errors.Is(err, entities.ErrCandidateCaptureNotClaimed) {
		t.Fatalf("unclaimed completion = %v, want ErrCandidateCaptureNotClaimed", err)
	}
	if events := readCandidateCaptureEvents(t, f.pool, f.op.ID); events.claims != 0 || events.summary != "" {
		t.Fatalf("unclaimed completion wrote events: %+v", events)
	}

	// The same pool still commits normal claims, including a second distinct
	// step in the same attempt.
	for index, step := range []string{"entities-ok-v1", "entities-ok-v2"} {
		err = store.WithinTx(ctx, func(tx *entities.StoreTx) error {
			claimed, err := tx.CandidateCapture(fence).Claim(step)
			if err != nil {
				return err
			}
			if !claimed {
				return errors.New("claim was not inserted")
			}
			return nil
		})
		if err != nil {
			t.Fatalf("claim %s: %v", step, err)
		}
		if events := readCandidateCaptureEvents(t, f.pool, f.op.ID); events.claims != index+1 {
			t.Fatalf("claims after %s = %d, want %d", step, events.claims, index+1)
		}
	}
	if got := operationLastSequence(t, f.pool, f.op.ID); got != before+2 {
		t.Fatalf("committed sequence = %d, want %d", got, before+2)
	}
}

// Terminal-owner recovery may be the first opportunity to capture. The typed
// boundary fences the operation attempt but deliberately does not reject a
// terminal operation state; completion still follows the durable claim.
func TestIntegrationCandidateCaptureTerminalOwnerRecovery(t *testing.T) {
	f := newCandidateCaptureFixture(t, "capture-terminal-owner", nil)
	ctx := context.Background()
	if _, err := f.pool.Exec(
		ctx,
		`UPDATE eregion_operations SET state='failed',error_code='unavailable' WHERE id=$1`,
		f.op.ID,
	); err != nil {
		t.Fatal(err)
	}
	transport := &candidateCaptureTransport{
		call: func(context.Context, platform.CandidateDiagnosticPayload) (platform.CandidateDiagnosticResult, error) {
			return candidateUsefulResult(f.plan), nil
		},
	}
	f.useTransport(t, transport)
	f.service.captureCandidateDiagnostics(ctx, f.env, f.dep, 1)
	events := readCandidateCaptureEvents(t, f.pool, f.op.ID)
	if events.claims != 1 || events.summary != "complete" || events.observed != 1 || events.captured != 1 {
		t.Fatalf("terminal owner capture: %+v", events)
	}
	var state contract.State
	if err := f.pool.QueryRow(ctx, `SELECT state FROM eregion_operations WHERE id=$1`, f.op.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != contract.Failed {
		t.Fatalf("capture changed terminal state to %q", state)
	}
	var owners int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM eregion_platform_deployment_owners WHERE operation_id=$1`, f.op.ID).Scan(&owners); err != nil {
		t.Fatal(err)
	}
	if owners != 1 {
		t.Fatalf("capture changed the exact owner row: %d", owners)
	}
}

// A BEFORE INSERT trigger that returns NULL drops one captured row without an
// SQL error. The CTE then affects fewer rows than the completion supplied; the
// row-count check must roll back the insert and the sequence advance together.
func TestIntegrationCandidateCaptureCompletionShortInsertRollsBack(t *testing.T) {
	f := newCandidateCaptureFixture(t, "capture-short-insert", nil)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `CREATE FUNCTION eregion_test_drop_captured_event() RETURNS trigger
		LANGUAGE plpgsql AS $body$
		BEGIN
			IF NEW.event->'candidate_diagnostic'->>'status' = 'captured' THEN
				RETURN NULL;
			END IF;
			RETURN NEW;
		END
		$body$`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `CREATE TRIGGER eregion_test_drop_captured_event
		BEFORE INSERT ON eregion_operation_events
		FOR EACH ROW EXECUTE FUNCTION eregion_test_drop_captured_event()`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.Background(), `DROP TRIGGER IF EXISTS eregion_test_drop_captured_event ON eregion_operation_events`)
		_, _ = f.pool.Exec(context.Background(), `DROP FUNCTION IF EXISTS eregion_test_drop_captured_event()`)
	})
	var logBuffer bytes.Buffer
	f.service.logger = slog.New(slog.NewTextHandler(&logBuffer, nil))
	transport, release, done := runGatedCandidateCapture(t, f, candidateUsefulResult(f.plan))
	before := operationLastSequence(t, f.pool, f.op.ID)
	close(release)
	waitCaptureDone(t, done)
	events := readCandidateCaptureEvents(t, f.pool, f.op.ID)
	if events.claims != 1 || events.summary != "" || events.observed != 0 || events.captured != 0 {
		t.Fatalf("short insert did not roll back the completion: %+v", events)
	}
	if got := operationLastSequence(t, f.pool, f.op.ID); got != before {
		t.Fatalf("short insert advanced sequence %d -> %d", before, got)
	}
	if !strings.Contains(logBuffer.String(), "candidate_capture_completion_unconfirmed") ||
		!strings.Contains(logBuffer.String(), "reason=persistence") {
		t.Fatalf("short insert was not logged as unconfirmed: %q", logBuffer.String())
	}
	if transport.ReadCount() != 1 {
		t.Fatalf("node reads = %d, want one", transport.ReadCount())
	}
}

// A completion whose first event would exceed the bigint sequence column is
// rejected before marshaling or SQL. The claim remains the only record and the
// operation sequence is unchanged.
func TestIntegrationCandidateCaptureCompletionSequenceOverflow(t *testing.T) {
	f := newCandidateCaptureFixture(t, "capture-sequence-overflow", nil)
	ctx := context.Background()
	var logBuffer bytes.Buffer
	f.service.logger = slog.New(slog.NewTextHandler(&logBuffer, nil))
	transport, release, done := runGatedCandidateCapture(t, f, candidateUsefulResult(f.plan))
	if _, err := f.pool.Exec(
		ctx,
		`UPDATE eregion_operations SET last_sequence=$2 WHERE id=$1`,
		f.op.ID,
		int64(math.MaxInt64),
	); err != nil {
		t.Fatal(err)
	}
	close(release)
	waitCaptureDone(t, done)
	events := readCandidateCaptureEvents(t, f.pool, f.op.ID)
	if events.claims != 1 || events.summary != "" || events.observed != 0 || events.captured != 0 {
		t.Fatalf("overflow wrote completion events: %+v", events)
	}
	if got := operationLastSequence(t, f.pool, f.op.ID); got != int64(math.MaxInt64) {
		t.Fatalf("overflow changed sequence to %d", got)
	}
	if !strings.Contains(logBuffer.String(), "candidate_capture_completion_unconfirmed") ||
		!strings.Contains(logBuffer.String(), "reason=persistence") {
		t.Fatalf("overflow was not logged as unconfirmed: %q", logBuffer.String())
	}
	if transport.ReadCount() != 1 {
		t.Fatalf("node reads = %d, want one", transport.ReadCount())
	}
}
