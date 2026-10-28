package controlplane

import (
	"context"
	"errors"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/platform"
)

// releaseRecoveryTransport answers only read-only status requests. A recovery
// must never dispatch the command again.
type releaseRecoveryTransport struct {
	status      engine.Status
	inputsHash  string
	statusErr   error
	queryFault  bool
	statusCalls int
	runCalls    int
}

func (*releaseRecoveryTransport) Name() string         { return "release-recovery" }
func (*releaseRecoveryTransport) NodeIdentity() string { return "main" }

func (t *releaseRecoveryTransport) Call(_ context.Context, request node.Request, _ func(node.Event)) (node.Response, error) {
	switch request.Method {
	case node.MethodStatus:
		t.statusCalls++
		if t.statusErr != nil {
			return node.Response{}, t.statusErr
		}
		if t.queryFault {
			// This is the SSH statusResponse shape when the journal cannot be read.
			return node.Response{Status: engine.StatusFailed, ErrorCode: string(contract.PreconditionFailed), Detail: "journal read failed"}, nil
		}
		return node.Response{Status: t.status, InputsHash: t.inputsHash, ExecutionID: request.Execution.ExecutionID()}, nil
	case node.MethodRun:
		t.runCalls++
		return node.Response{}, errors.New("recovery dispatched a second command")
	default:
		return node.Response{}, errors.New("unexpected recovery method")
	}
}

// TestIntegrationReleaseCommandRecovery proves a terminal owner's running
// command effect is resolved from durable step state and node status before any
// new ownership is allowed.
func TestIntegrationReleaseCommandRecovery(t *testing.T) {
	pool, _ := testPool(t, true)
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
	original, err := contract.NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	current, err := contract.NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	revisionID := platform.RevisionID("erv_" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_platform_nodes(name,roles,capabilities) VALUES('main',ARRAY['application'],ARRAY['docker'])`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_platform_apps(id,name) VALUES($1,'demo')`, appID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_platform_environments(id,app_id,name,node_name,host_port,candidate_port,container_port)
        VALUES($1,$2,'production','main',20003,30003,8080)`, envID, appID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_platform_environment_revisions(id,environment_id,number,operation,runtime)
        VALUES($1,$2,1,'env.changed','{"node":"main","health_path":"/"}')`, revisionID, envID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_operations(id,kind,target,actor_kind,actor_subject,idempotency_key,input_hash,input,state)
        VALUES($1,'platform.deploy','env/demo/production','operator','tester','original-command','hash','{}','failed')`, original); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_platform_releases(id,operation_id,app_id,environment_id,image,source,status)
        VALUES($1,$2,$3,$4,'demo:one','{}','ready')`, releaseID, original, appID, envID); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_platform_deployments(id,operation_id,app_id,environment_id,action,release_id,revision_id,state)
        VALUES('dep_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa',$1,$2,$3,'deploy',$4,$5,'succeeded')`, original, appID, envID, releaseID, revisionID); err != nil {
		t.Fatal(err)
	}
	digest := platform.ReleaseCommandDigest(platform.CommandSpec{Argv: []string{"/bin/migrate"}, TimeoutSeconds: 120})
	inputsHash := "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_platform_release_command_runs
        (release_id,command_digest,operation_id,state) VALUES($1,$2,$3,'running')`, releaseID, digest, original); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_operation_steps(operation_id,step,inputs_hash,state,attempt,execution_id)
        VALUES($1,$2,$3,'running',1,$4)`, original, deploymentCommandStep, inputsHash,
		engine.Execution{OperationID: original, Attempt: 1, Step: deploymentCommandStep}.ExecutionID()); err != nil {
		t.Fatal(err)
	}
	run := releaseCommandRun{State: "running", OperationID: original}

	tests := []struct {
		name         string
		stepState    string
		status       engine.Status
		statusHash   string
		statusErr    error
		queryFault   bool
		deleteStep   bool
		wantDecision releaseCommandDecision
		wantError    bool
		wantRow      string
		wantStatus   int
	}{
		{name: "completed effect recovers success", stepState: "running", status: engine.StatusCompleted, wantDecision: releaseCommandSkip, wantRow: "succeeded", wantStatus: 1},
		{name: "failed effect allows a new run", stepState: "running", status: engine.StatusFailed, wantDecision: releaseCommandRunNow, wantRow: "failed", wantStatus: 1},
		{name: "still running keeps ownership", stepState: "running", status: engine.StatusRunning, wantError: true, wantRow: "running", wantStatus: 1},
		{name: "absent effect keeps ownership", stepState: "running", status: engine.StatusAbsent, wantError: true, wantRow: "running", wantStatus: 1},
		{name: "unreachable node keeps ownership", stepState: "running", statusErr: errors.New("node unreachable"), wantError: true, wantRow: "running", wantStatus: 1},
		{name: "completed input mismatch keeps ownership", stepState: "running", status: engine.StatusCompleted, statusHash: "other-inputs", wantError: true, wantRow: "running", wantStatus: 1},
		{name: "running input mismatch keeps ownership", stepState: "running", status: engine.StatusRunning, statusHash: "other-inputs", wantError: true, wantRow: "running", wantStatus: 1},
		{name: "status query failure keeps ownership", stepState: "running", queryFault: true, wantError: true, wantRow: "running", wantStatus: 1},
		{name: "completed step record recovers without status", stepState: "completed", wantDecision: releaseCommandSkip, wantRow: "succeeded"},
		{name: "failed step record with failed node status allows a new run", stepState: "failed", status: engine.StatusFailed, wantDecision: releaseCommandRunNow, wantRow: "failed", wantStatus: 1},
		{name: "failed step record with absent node status keeps ownership", stepState: "failed", status: engine.StatusAbsent, wantError: true, wantRow: "running", wantStatus: 1},
		{name: "failed step record with completed node status recovers success", stepState: "failed", status: engine.StatusCompleted, wantDecision: releaseCommandSkip, wantRow: "succeeded", wantStatus: 1},
		{name: "missing step record is insufficient", stepState: "running", deleteStep: true, wantError: true, wantRow: "running"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := pool.Exec(ctx, `UPDATE eregion_platform_release_command_runs SET state='running',operation_id=$1,error_code='',error='',completed_at=NULL WHERE release_id=$2 AND command_digest=$3`,
				original, releaseID, digest); err != nil {
				t.Fatal(err)
			}
			if test.deleteStep {
				if _, err := pool.Exec(ctx, `DELETE FROM eregion_operation_steps WHERE operation_id=$1 AND step=$2`, original, deploymentCommandStep); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := pool.Exec(ctx, `UPDATE eregion_operation_steps SET state=$3,inputs_hash=$4 WHERE operation_id=$1 AND step=$2`,
					original, deploymentCommandStep, test.stepState, inputsHash); err != nil {
					t.Fatal(err)
				}
			}
			statusHash := test.statusHash
			if statusHash == "" {
				statusHash = inputsHash
			}
			fake := &releaseRecoveryTransport{status: test.status, inputsHash: statusHash, statusErr: test.statusErr, queryFault: test.queryFault}
			service := &Service{pool: pool, resolver: NewNodeResolver(pool, ResolverConfig{}, func() node.Transport { return fake })}
			worker := &platformWorker{service: service}
			decision, err := worker.recoverReleaseCommandRun(ctx, releaseID, digest, run, current)
			if test.wantError {
				if err == nil {
					t.Fatal("recovery accepted an unknown or running effect")
				}
			} else if err != nil {
				t.Fatalf("recovery failed: %v", err)
			}
			if decision != test.wantDecision && !test.wantError {
				t.Fatalf("recovery decision = %v, want %v", decision, test.wantDecision)
			}
			if fake.runCalls != 0 {
				t.Fatalf("recovery dispatched %d commands", fake.runCalls)
			}
			if fake.statusCalls != test.wantStatus {
				t.Fatalf("status calls = %d, want %d", fake.statusCalls, test.wantStatus)
			}
			row, err := service.releaseCommandRun(ctx, releaseID, digest)
			if err != nil {
				t.Fatal(err)
			}
			if row.State != test.wantRow || row.OperationID != original {
				t.Fatalf("row = %+v, want state %s owned by the original operation", row, test.wantRow)
			}
			switch test.wantRow {
			case "succeeded":
				if got := releaseCommandAction(row, platform.ActionDeploy, current); got != releaseCommandSkip {
					t.Fatalf("recovered success action = %v, want skip", got)
				}
			case "failed":
				if got := releaseCommandAction(row, platform.ActionDeploy, current); got != releaseCommandRunNow {
					t.Fatalf("recovered failure action = %v, want a new run", got)
				}
			}
		})
	}
}
