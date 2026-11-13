package controlplane

import (
	"context"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
)

func TestDefinitionFinalStateRequiresSettledChildren(t *testing.T) {
	for _, tc := range []struct {
		cancel, active bool
		failure        contract.Code
		want           contract.State
	}{
		{false, true, "", ""},
		{true, true, "", ""},
		{true, true, contract.Internal, ""},
		{false, false, "", contract.Succeeded},
		{true, false, "", contract.Canceled},
		{true, false, contract.Unavailable, contract.Failed},
	} {
		if got := definitionFinalState(tc.cancel, tc.active, tc.failure); got != tc.want {
			t.Fatalf("definitionFinalState(%t,%t,%q) = %q, want %q", tc.cancel, tc.active, tc.failure, got, tc.want)
		}
	}
}

// This integration test requires an explicitly isolated test database. It is
// not part of the offline unit-test selection.
func TestIntegrationDefinitionDrainReservesUntilChildSettles(t *testing.T) {
	pool, _ := testPool(t, true)
	ctx := context.Background()
	parent := contract.OperationID("op_11111111111111111111111111111111")
	child := contract.OperationID("op_22222222222222222222222222222222")
	for _, row := range []struct {
		id               contract.OperationID
		kind, key, state string
	}{
		{parent, "platform.definition.apply", "parent", "cancelling"},
		{child, "platform.environment.update", "child", "running"},
	} {
		_, err := pool.Exec(ctx, `INSERT INTO eregion_operations(id,kind,target,actor_kind,actor_subject,idempotency_key,input_hash,input,state,attempt,completed_steps,last_sequence,created_at,updated_at,error_code) VALUES ($1,$2,'env/app/prod','operator','test',$3,'hash','{}'::jsonb,$4,1,0,0,clock_timestamp(),clock_timestamp(),'')`, row.id, row.kind, row.key, row.state)
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_definition_runs(operation_id,plan_nonce,environment_id,approved,next_step,failure_code,failure_message) VALUES ($1,'nonce','env-id','{}',0,'','')`, parent); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_definition_steps(parent_operation_id,ordinal,kind,target,input,child_operation_id,dns_progress) VALUES ($1,0,'platform.environment.update','env/app/prod','{}',$2,'[]')`, parent, child); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer rollback(tx)
	op, err := scanOperation(tx.QueryRow(ctx, "SELECT "+columns+" FROM eregion_operations WHERE id=$1 FOR UPDATE", parent))
	if err != nil {
		t.Fatal(err)
	}
	active, err := definitionDrainTx(ctx, tx, &op)
	if err != nil || !active {
		t.Fatalf("drain active=%t err=%v", active, err)
	}
	if err := definitionFinishTx(ctx, tx, &op, contract.Canceled, "", "cancelled"); err == nil {
		t.Fatal("finished parent while child was active")
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	var state contract.State
	if err := pool.QueryRow(ctx, `SELECT state FROM eregion_operations WHERE id=$1`, child).Scan(&state); err != nil || state != contract.Cancelling {
		t.Fatalf("child state=%q err=%v", state, err)
	}
	var unfinished bool
	if err := pool.QueryRow(ctx, `SELECT finished_at IS NULL FROM eregion_definition_runs WHERE operation_id=$1`, parent).Scan(&unfinished); err != nil || !unfinished {
		t.Fatalf("reservation retained=%t err=%v", unfinished, err)
	}
	// Drive the cancelling child through the ordinary worker and then let the
	// parent drain finish; the synthetic journal above tests only this lifecycle.
	s := &Service{pool: pool}
	if err := runPlatformAttempt(ctx, s, child, 1); err != nil {
		t.Fatalf("settle cancelling child: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT state FROM eregion_operations WHERE id=$1`, child).Scan(&state); err != nil || state != contract.Canceled {
		t.Fatalf("settled child state=%q err=%v", state, err)
	}
	if err := (&platformWorker{service: s}).runDefinitionApply(ctx, op, 1); err != nil {
		t.Fatalf("finish parent drain: %v", err)
	}
	var code contract.Code
	if err := pool.QueryRow(ctx, `SELECT o.state,o.error_code,r.finished_at IS NULL FROM eregion_operations o JOIN eregion_definition_runs r ON r.operation_id=o.id WHERE o.id=$1`, parent).Scan(&state, &code, &unfinished); err != nil || state != contract.Canceled || code != "" || unfinished {
		t.Fatalf("parent state=%q error_code=%q reservation retained=%t err=%v", state, code, unfinished, err)
	}
}

func TestDefinitionFailureDrainKeepsPublicErrorCodeEmpty(t *testing.T) {
	// The private run journal holds failure intent during cancellation. Public
	// snapshots may carry an error code only after the terminal transition.
	now := time.Now()
	op := contract.Operation{ID: "op_0123456789abcdef0123456789abcdef", Kind: "platform.definition.apply", Target: "env/app/prod", Actor: contract.Actor{Kind: "operator", Subject: "test"}, State: contract.Cancelling, Attempt: 1, LastSequence: 1, CreatedAt: now, UpdatedAt: now}
	if err := op.Validate(); err != nil {
		t.Fatalf("valid drain snapshot rejected: %v", err)
	}
	op.ErrorCode = contract.Internal
	if err := op.Validate(); err == nil {
		t.Fatal("cancelling snapshot carrying error code unexpectedly accepted")
	}
	op.State = contract.Failed
	if err := op.Validate(); err != nil {
		t.Fatalf("terminal failure snapshot rejected: %v", err)
	}
}
