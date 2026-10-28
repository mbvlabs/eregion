package controlplane

import (
	"context"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

// TestIntegrationReleaseCommandRuns proves the durable release-scoped command
// record: one row per release and digest, a stale running row is replaceable, a
// succeeded row is skipped, and the row goes with its release.
func TestIntegrationReleaseCommandRuns(t *testing.T) {
	pool, _ := testPool(t, true)
	service := &Service{pool: pool}
	ctx := context.Background()

	appID, err := platform.NewAppID()
	if err != nil {
		t.Fatal(err)
	}
	envID, err := platform.NewEnvironmentID()
	if err != nil {
		t.Fatal(err)
	}
	releaseID, err := platform.NewReleaseID()
	if err != nil {
		t.Fatal(err)
	}
	operationID, err := contract.NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_platform_nodes(name,roles,capabilities) VALUES('main',ARRAY['application'],ARRAY['docker'])`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_platform_apps(id,name) VALUES($1,'demo')`, appID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_platform_environments(id,app_id,name,node_name,host_port,candidate_port,container_port)
        VALUES($1,$2,'production','main',20001,30001,8080)`, envID, appID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_operations(id,kind,target,actor_kind,actor_subject,idempotency_key,input_hash,input,state)
        VALUES($1,'platform.deploy','env/demo/production','operator','tester','release-command',$2,'{}','running')`,
		operationID, "hash"); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_platform_releases(id,operation_id,app_id,environment_id,image,source,status)
        VALUES($1,$2,$3,$4,'demo:one','{}','ready')`, releaseID, operationID, appID, envID); err != nil {
		t.Fatal(err)
	}

	digest := platform.ReleaseCommandDigest(platform.CommandSpec{Argv: []string{"bin/migrate"}, TimeoutSeconds: 120})
	if err := service.startReleaseCommandRun(ctx, releaseID, digest, operationID); err != nil {
		t.Fatal(err)
	}
	row, err := service.releaseCommandRun(ctx, releaseID, digest)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != "running" || !row.Live || row.OperationID != operationID {
		t.Fatalf("running row = %+v", row)
	}
	if got := releaseCommandAction(row, platform.ActionDeploy, operationID); got != releaseCommandRunNow {
		t.Fatalf("own running command action = %v, want run", got)
	}
	createOperation := func(key string) contract.OperationID {
		id, err := contract.NewOperationID()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO eregion_operations(id,kind,target,actor_kind,actor_subject,idempotency_key,input_hash,input,state)
            VALUES($1,'platform.deploy','env/demo/production','operator','tester',$2,'hash','{}','failed')`, id, key); err != nil {
			t.Fatal(err)
		}
		return id
	}
	competing := createOperation("competing-release-command")
	if err := service.startReleaseCommandRun(ctx, releaseID, digest, competing); err == nil {
		t.Fatal("a running command owned by another operation was overwritten")
	}
	row, err = service.releaseCommandRun(ctx, releaseID, digest)
	if err != nil {
		t.Fatal(err)
	}
	if row.OperationID != operationID || row.State != "running" {
		t.Fatalf("running ownership changed after a refused claim: %+v", row)
	}
	if _, err := pool.Exec(ctx, `UPDATE eregion_operations SET state='failed' WHERE id=$1`, operationID); err != nil {
		t.Fatal(err)
	}
	row, err = service.releaseCommandRun(ctx, releaseID, digest)
	if err != nil {
		t.Fatal(err)
	}
	if row.Live {
		t.Fatalf("stale operation still reports live: %+v", row)
	}
	if got := releaseCommandAction(row, platform.ActionDeploy, competing); got != releaseCommandRecover {
		t.Fatalf("terminal owner action = %v, want recover", got)
	}
	if err := service.finishReleaseCommandRun(ctx, releaseID, digest, operationID,
		contract.NewFault(contract.PreconditionFailed, "command failed", "Fix the command.")); err != nil {
		t.Fatal(err)
	}
	row, err = service.releaseCommandRun(ctx, releaseID, digest)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != "failed" {
		t.Fatalf("confirmed failure row = %+v", row)
	}
	if err := service.startReleaseCommandRun(ctx, releaseID, digest, competing); err != nil {
		t.Fatalf("a confirmed failed row was not claimable: %v", err)
	}
	if err := service.startReleaseCommandRun(ctx, releaseID, digest, competing); err != nil {
		t.Fatalf("same-operation retry did not claim its own running row: %v", err)
	}
	if err := service.finishReleaseCommandRun(ctx, releaseID, digest, competing, nil); err != nil {
		t.Fatal(err)
	}
	succeeded := createOperation("succeeded-release-command")
	if err := service.startReleaseCommandRun(ctx, releaseID, digest, succeeded); err == nil {
		t.Fatal("a succeeded command was overwritten")
	}
	row, err = service.releaseCommandRun(ctx, releaseID, digest)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != "succeeded" {
		t.Fatalf("finished row = %+v", row)
	}
	if got := releaseCommandAction(row, platform.ActionRestart, ""); got != releaseCommandSkip {
		t.Fatalf("succeeded restart action = %v, want skip", got)
	}
	changed := platform.ReleaseCommandDigest(platform.CommandSpec{Argv: []string{"bin/migrate", "--force"}, TimeoutSeconds: 120})
	row, err = service.releaseCommandRun(ctx, releaseID, changed)
	if err != nil {
		t.Fatal(err)
	}
	if row.State != "" {
		t.Fatalf("changed digest reused the old row: %+v", row)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM eregion_platform_releases WHERE id=$1`, releaseID); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_platform_release_command_runs WHERE release_id=$1`, releaseID).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("release command rows survived the release: %d", remaining)
	}
}
