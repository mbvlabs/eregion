package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/riverqueue/river"
)

type deploymentHealthCase struct {
	name                                                           string
	run, wrongIdentity, otherPending, wrongInputs, normal, release bool
}

func TestIntegrationDeploymentHealthJournalHandoff(t *testing.T) {
	for _, tc := range []deploymentHealthCase{
		{"normal-dispatch", false, false, false, false, true, true},
		{"lost-reply", true, false, false, false, false, true},
		{"absent-reply", false, false, false, false, false, false},
		{"different-node-identity", true, true, false, false, false, false},
		{"different-journal-input", true, false, false, true, false, false},
		{"other-step-pending", true, false, true, false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			checkDeploymentHealthJournalHandoff(t, tc)
		})
	}
}

func checkDeploymentHealthJournalHandoff(t *testing.T, tc deploymentHealthCase) {
	t.Helper()
	s, id, target := seedDeploymentHandoffWithLocal(t, deploymentHandoffCase{phase: "prepared"}, true)
	ctx := t.Context()
	if err := s.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		_, err := tx.Nodes().InsertRegistration(ctx, "local", []string{platform.RoleApplication}, []string{platform.CapabilityDocker}, "")
		return err
	}); err != nil {
		t.Fatal(err)
	}
	tr, identity, err := s.buildTransportFor(ctx, "local")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	stepID := healthStepID("web", "1")
	exec := engine.Execution{OperationID: id, Attempt: 1, Step: stepID, RequestID: newRequestID()}
	raw, err := json.Marshal(platform.HealthPayload{Step: stepID, Type: "tcp", Target: listener.Addr().String()})
	if err != nil {
		t.Fatal(err)
	}
	step, err := node.DefaultRegistry().Build(exec, raw)
	if err != nil {
		t.Fatal(err)
	}
	inputs, err := step.Inputs()
	if err != nil {
		t.Fatal(err)
	}
	claims := claimFixture{t: t, ctx: ctx, a: s.entities, b: s.entities}
	claim := entities.TargetClaimID{OperationID: id, Token: "1/original"}
	if !claims.acquire(s.entities, claim, target) {
		t.Fatal("claim target")
	}
	workCtx := context.WithValue(ctx, buildTargetContextKey{}, buildTargetExecutor{claim: claim, target: target})
	workCtx = context.WithValue(workCtx, deploymentHealthDispatchKey{}, entities.PackPlacement{Node: "local", Identity: identity})
	hash := engine.InputsHash(step.ID(), inputs)
	if tc.wrongInputs {
		hash = "different"
	}
	if tc.normal {
		if _, err := (&platformWorker{service: s}).runStepOn(workCtx, tr, id, 1, stepID, json.RawMessage(raw), nil); err != nil {
			t.Fatal(err)
		}
	} else {
		if _, err := s.beginStep(workCtx, id, 1, stepID, hash, exec.ExecutionID()); err != nil {
			t.Fatal(err)
		}
	}
	if tc.run {
		response, err := tr.Call(ctx, node.Request{Method: node.MethodRun, Execution: exec, RequestID: exec.RequestID, Payload: raw}, nil)
		if err != nil || response.Status != engine.StatusCompleted {
			t.Fatalf("real health effect: %+v %v", response, err)
		}
	}
	if tc.otherPending {
		other := engine.Execution{OperationID: id, Attempt: 1, Step: "another-step-v1"}
		if _, err := s.beginStep(ctx, id, 1, other.Step, "hash", other.ExecutionID()); err != nil {
			t.Fatal(err)
		}
	}
	assertHealthHandoff(t, s, id, target, claim, claims, tc)
}

func assertHealthHandoff(t *testing.T, s *Service, id contract.OperationID, target string, claim entities.TargetClaimID, claims claimFixture, tc deploymentHealthCase) {
	t.Helper()
	ctx := t.Context()
	if tc.wrongIdentity {
		binding := *s.registryHostIdentity
		binding.HostKeyFingerprint = "different"
		s.registryHostIdentity = &binding
	}
	op, _, err := s.operationSnapshot(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.recoverDeploymentHealth(ctx, op, claim); err == nil {
		t.Fatal("unsealed claim allowed health observation")
	}
	err = runPlatformAttempt(ctx, s, id, 2)
	var snooze *river.JobSnoozeError
	if errors.As(err, &snooze) != tc.release {
		t.Fatalf("handoff error %v; release=%v", err, tc.release)
	}
	got, found := claims.inspect(claim.Token)
	if !found || !got.Sealed || got.Released != tc.release {
		t.Fatalf("claim: %+v found=%v", got, found)
	}
	competitor := claimID("health-competitor", "health-competitor-token")
	if acquired := claims.acquire(s.entities, competitor, target); acquired != tc.release {
		t.Fatalf("competitor acquired=%v", acquired)
	}
}
