package controlplane

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/integrations"
	"github.com/mbvlabs/eregion/internal/platform"
)

func normalizeGrantAction(t *testing.T, registry *platform.Registry, kind string, input any) platform.Normalized {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	normalized, err := registry.Normalize(kind, "platform", raw)
	if err != nil {
		t.Fatal(err)
	}
	return normalized
}

func insertGrantOperation(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id contract.OperationID, kind string, normalized platform.Normalized) {
	t.Helper()
	_, err := pool.Exec(ctx, `INSERT INTO eregion_operations
		(id,kind,target,actor_kind,actor_subject,idempotency_key,input_hash,input,state,attempt,completed_steps,last_sequence,error_code,created_at,updated_at)
		VALUES($1,$2,'platform',$3,$4,$5,'fixture',$6,'running',1,0,0,'',clock_timestamp(),clock_timestamp())`,
		id, kind, testActor.Kind, testActor.Subject, string(id), normalized.Canonical)
	if err != nil {
		t.Fatal(err)
	}
}

// runGrantWorker drives the real registered extension executor through the
// control-plane worker settlement path: claim, execute, finish or fail.
func runGrantWorker(t *testing.T, ctx context.Context, run *Runtime, id contract.OperationID, attempt uint32, normalized platform.Normalized) contract.Operation {
	t.Helper()
	if _, owned, err := run.Service.mutateOwned(ctx, id, attempt, func(tx *entities.StoreTx, op *contract.Operation) error {
		return appendEvent(ctx, tx, op, contract.Event{Kind: "step", Stage: "integration", Message: "grant extension attempt started."})
	}); err != nil || !owned {
		t.Fatalf("grant operation claim = %v, %v", owned, err)
	}
	if err := (&platformWorker{service: run.Service}).runExtension(ctx, id, attempt, normalized, normalized.Executor()); err != nil {
		t.Fatalf("grant extension execution: %v", err)
	}
	op, err := run.Service.Get(ctx, testActor, id)
	if err != nil {
		t.Fatal(err)
	}
	return op
}

func grantResultData(t *testing.T, op contract.Operation) map[string]any {
	t.Helper()
	if op.LatestResult == nil || len(op.LatestResult.Data) == 0 {
		return nil
	}
	var data map[string]any
	if err := json.Unmarshal(op.LatestResult.Data, &data); err != nil {
		t.Fatal(err)
	}
	return data
}

// executeGrantAction calls the registered executor exactly as a retried worker
// attempt does, without replaying the operation settlement itself.
func executeGrantAction(t *testing.T, ctx context.Context, id contract.OperationID, normalized platform.Normalized) (map[string]any, error) {
	t.Helper()
	result, err := normalized.Executor().Execute(ctx, platform.Execution{
		OperationID: id, Attempt: 1, Kind: normalized.Kind, Target: normalized.Target.String(),
		Actor: testActor, Input: normalized.Canonical,
	})
	if err != nil {
		return nil, err
	}
	data, ok := result.Data.(map[string]any)
	if !ok {
		t.Fatalf("grant executor data = %T, want map", result.Data)
	}
	return data, nil
}

// Retrying an access grant operation replays the original grant, conflicting
// input for the same operation identity is a conflict that changes nothing, and
// a revoked grant is never revived by a creation replay or a later revocation.
func TestIntegrationExtensionGrantRetryConflictAndNonRevival(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	ctx := context.Background()
	grants := run.Service.entities.Grants()

	audit := newTestEntities(t, pool).Audit()
	access, err := integrations.NewAccessController(integrations.AccessConfig{
		AdminSubjects: []string{testActor.Subject}, Audit: audit, Store: grants,
	})
	if err != nil {
		t.Fatal(err)
	}
	registry := platform.NewRegistry()
	if err := integrations.Configure(registry, integrations.RuntimeConfig{Audit: audit, Access: access}); err != nil {
		t.Fatal(err)
	}

	expires := time.Now().UTC().Add(2 * time.Hour)
	createInput := map[string]any{
		"subject": "alice", "device_id": "alpha", "routes": []string{"app.example"},
		"expires_at": expires, "note": "service retry",
	}
	createAction := normalizeGrantAction(t, registry, integrations.KindAccessGrant, createInput)

	// One real worker attempt creates the grant.
	createID, err := contract.NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	insertGrantOperation(t, ctx, pool, createID, integrations.KindAccessGrant, createAction)
	created := runGrantWorker(t, ctx, run, createID, 1, createAction)
	if created.State != contract.Succeeded {
		t.Fatalf("create operation = %s (%s)", created.State, created.ErrorCode)
	}
	createdData := grantResultData(t, created)
	grantID, ok := createdData["id"].(string)
	if !ok || !strings.HasPrefix(grantID, "grant_") || createdData["subject"] != "alice" {
		t.Fatalf("create result = %+v", createdData)
	}

	// The same operation identity replayed at the executor boundary returns the
	// original grant and inserts no second row.
	replayed, err := executeGrantAction(t, ctx, createID, createAction)
	if err != nil || replayed["id"] != grantID {
		t.Fatalf("create replay = %+v, %v", replayed, err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_access_grants WHERE id=$1`, grantID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("create replay rows = %d, %v; want 1", count, err)
	}

	// A durable grant created for an operation identity, followed by a worker
	// attempt that binds different input to the same identity, fails as a
	// conflict and leaves the grant untouched.
	conflictID, err := contract.NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	conflictGrant, err := access.CreateGrantForOperation(ctx, testActor, conflictID, integrations.GrantSpec{
		Subject: "alice", DeviceID: "gamma", Routes: []string{"conflict.example"}, ExpiresAt: expires,
	})
	if err != nil {
		t.Fatal(err)
	}
	conflictInput := map[string]any{
		"subject": "bob", "device_id": "gamma", "routes": []string{"conflict.example"},
		"expires_at": expires, "note": "conflicting retry",
	}
	conflictAction := normalizeGrantAction(t, registry, integrations.KindAccessGrant, conflictInput)
	insertGrantOperation(t, ctx, pool, conflictID, integrations.KindAccessGrant, conflictAction)
	conflicted := runGrantWorker(t, ctx, run, conflictID, 1, conflictAction)
	if conflicted.State != contract.Failed || conflicted.ErrorCode != contract.Conflict {
		t.Fatalf("conflicting operation = %s (%s), want failed conflict", conflicted.State, conflicted.ErrorCode)
	}
	stored, found, err := grants.Get(ctx, conflictGrant.ID)
	if err != nil || !found || stored.Subject != "alice" || stored.DeviceID != "gamma" {
		t.Fatalf("conflict changed the durable grant: %+v, %v, %v", stored, found, err)
	}

	// Revoke through the worker, then replay the same operation identity.
	revokeID, err := contract.NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	revokeInput := map[string]any{"grant_id": grantID}
	revokeAction := normalizeGrantAction(t, registry, integrations.KindAccessRevoke, revokeInput)
	insertGrantOperation(t, ctx, pool, revokeID, integrations.KindAccessRevoke, revokeAction)
	revoked := runGrantWorker(t, ctx, run, revokeID, 1, revokeAction)
	if revoked.State != contract.Succeeded {
		t.Fatalf("revoke operation = %s (%s)", revoked.State, revoked.ErrorCode)
	}
	revokedData := grantResultData(t, revoked)
	firstRevokedAt, ok := revokedData["revoked_at"].(string)
	if !ok || firstRevokedAt == "" || revokedData["id"] != grantID {
		t.Fatalf("revoke result = %+v", revokedData)
	}
	revokeReplay, err := executeGrantAction(t, ctx, revokeID, revokeAction)
	revokeReplayAt, ok := revokeReplay["revoked_at"].(*time.Time)
	if err != nil || !ok || revokeReplayAt == nil || revokeReplayAt.Format(time.RFC3339Nano) != firstRevokedAt {
		t.Fatalf("revoke replay = %+v, %v", revokeReplay, err)
	}

	// A creation replay after revocation is idempotent and cannot revive it.
	stored, found, err = grants.Get(ctx, grantID)
	if err != nil || !found || stored.RevokedAt == nil {
		t.Fatalf("revoked grant = %+v, %v, %v", stored, found, err)
	}
	creationReplay := stored
	creationReplay.RevokedAt = nil
	creationReplay.RevokedBy, creationReplay.RevokedByOperation = "", ""
	if err := grants.Put(ctx, creationReplay); err != nil {
		t.Fatalf("creation replay after revocation: %v", err)
	}
	afterReplay, found, err := grants.Get(ctx, grantID)
	if err != nil || !found || afterReplay.RevokedAt == nil || afterReplay.RevokedAt.Format(time.RFC3339Nano) != firstRevokedAt {
		t.Fatalf("creation replay revived the grant: %+v, %v, %v", afterReplay, found, err)
	}

	// A different revocation operation cannot replace the first revocation.
	secondRevokeID, err := contract.NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	secondRevokeAction := normalizeGrantAction(t, registry, integrations.KindAccessRevoke, revokeInput)
	insertGrantOperation(t, ctx, pool, secondRevokeID, integrations.KindAccessRevoke, secondRevokeAction)
	secondRevoked := runGrantWorker(t, ctx, run, secondRevokeID, 1, secondRevokeAction)
	if secondRevoked.State != contract.Succeeded {
		t.Fatalf("second revoke operation = %s (%s)", secondRevoked.State, secondRevoked.ErrorCode)
	}
	secondRevokedData := grantResultData(t, secondRevoked)
	if secondRevokedData["revoked_at"] != firstRevokedAt {
		t.Fatalf("second revoke replaced the first: %+v", secondRevokedData)
	}
	final, found, err := grants.Get(ctx, grantID)
	if err != nil || !found || final.RevokedByOperation != string(revokeID) || final.RevokedBy != testActor.Subject {
		t.Fatalf("final revocation = %+v, %v, %v", final, found, err)
	}
}
