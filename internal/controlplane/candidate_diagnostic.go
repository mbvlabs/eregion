package controlplane

import (
	"context"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/secrets"
)

// captureCandidateDiagnostics is best effort and is called while the lifecycle
// owner is held, before terminal cleanup intent is released to maintenance. A
// durable claim prevents retries from duplicating output, including after a lost
// response or partial failure. An interrupted claim is itself an unavailable
// summary. Nothing in this path can veto the safety tail.
func (s *Service) captureCandidateDiagnostics(parent context.Context, env platform.Environment, dep platform.Deployment) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
	defer cancel()
	op, _, err := s.operationSnapshot(ctx, dep.OperationID)
	if err != nil {
		return
	}
	claimed := false
	err = s.mutateCandidateDiagnostic(ctx, op, func(tx pgx.Tx, current *contract.Operation) error {
		var allowed, exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM eregion_platform_deployment_owners WHERE environment_id=$1 AND operation_id=$2 AND deployment_id=$3)`, env.ID, op.ID, dep.ID).Scan(&allowed); err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM eregion_operation_events WHERE operation_id=$1 AND event->>'stage'=$2)`, op.ID, platform.CandidateDiagnosticStage).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return nil
		}
		if err := appendEvent(ctx, tx, current, contract.Event{Kind: "warning", Stage: platform.CandidateDiagnosticStage, Message: "Candidate diagnostic capture started; unavailable if no completion summary follows.", CandidateDiagnostic: &contract.CandidateDiagnostic{DeploymentID: string(dep.ID), Status: "unavailable"}}); err != nil {
			return err
		}
		// Capture requires the exact lifecycle owner. An ambiguous or absent
		// owner leaves only the unavailable marker, never a Docker read.
		claimed = allowed
		return nil
	})
	if err != nil || !claimed {
		return
	}
	result := platform.CandidateDiagnosticResult{Unavailable: true}
	if captured, ok := s.readCandidateDiagnostics(ctx, env, dep, op); ok {
		result = captured
	}
	// The claim is already durable; write all output and its summary atomically.
	// If this fails, the claim remains the honest unavailable summary.
	_ = s.mutateCandidateDiagnostic(ctx, op, func(tx pgx.Tx, current *contract.Operation) error {
		for _, line := range result.Lines {
			d := &contract.CandidateDiagnostic{DeploymentID: string(dep.ID), Process: line.Process, Replica: line.Replica, Container: line.Container, ContainerID: line.ContainerID, Timestamp: line.Timestamp, Status: "captured"}
			if err := appendEvent(ctx, tx, current, contract.Event{Kind: "log", Stage: platform.CandidateDiagnosticStage, Message: line.Message, CandidateDiagnostic: d}); err != nil {
				return err
			}
		}
		status := "complete"
		if result.Unavailable {
			status = "unavailable"
		}
		if result.Truncated {
			if status == "unavailable" {
				status = "unavailable_truncated"
			} else {
				status = "truncated"
			}
		}
		return appendEvent(ctx, tx, current, contract.Event{Kind: "warning", Stage: platform.CandidateDiagnosticStage, Message: "Candidate diagnostic capture: " + status + ".", CandidateDiagnostic: &contract.CandidateDiagnostic{DeploymentID: string(dep.ID), Status: status}})
	})
}

// Terminal-owner recovery may be the first opportunity to capture. The owner
// lock still serializes the lifecycle even after terminalization, so diagnostic
// history may be appended without changing terminal state or ownership.
func (s *Service) mutateCandidateDiagnostic(ctx context.Context, expected contract.Operation, change func(pgx.Tx, *contract.Operation) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	op, err := scanOperation(tx.QueryRow(ctx, "SELECT "+columns+" FROM eregion_operations WHERE id=$1 FOR UPDATE", expected.ID))
	if err != nil {
		return err
	}
	if op.Attempt != expected.Attempt {
		return errNotOwned
	}
	if err := change(tx, &op); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *Service) readCandidateDiagnostics(ctx context.Context, env platform.Environment, dep platform.Deployment, op contract.Operation) (platform.CandidateDiagnosticResult, bool) {
	var empty platform.CandidateDiagnosticResult
	// Re-read the deployment after secret snapshot/instance writes; only the
	// immutable pinned revision and pinned secret revisions can prove redaction.
	var has bool
	var err error
	expectedID := dep.ID
	dep, has, err = s.deploymentByOperation(ctx, op.ID)
	if err != nil || !has || dep.ID != expectedID || dep.App != env.App || dep.Environment != env.Name || dep.RevisionID == "" || s.resolver == nil {
		return empty, false
	}
	revision, err := s.revisionByID(ctx, dep.RevisionID)
	if err != nil {
		return empty, false
	}
	env.Revision = &revision
	for _, name := range revision.SecretNames {
		if _, ok := dep.SecretRevisions[name]; !ok {
			return empty, false
		}
	}
	for name := range env.SecretRefs {
		if _, ok := dep.SecretRevisions[name]; !ok {
			return empty, false
		}
	}
	worker := &platformWorker{service: s}
	values, key, credential, _, err := worker.resolveDeploymentSecrets(ctx, env, dep)
	if err != nil {
		return empty, false
	}
	for _, name := range revision.SecretNames {
		if values[name] == "" {
			return empty, false
		}
	}
	if credential != "" {
		values[key] = credential
	}
	for _, value := range values {
		if strings.ContainsAny(value, "\r\n") {
			return empty, false
		}
	}
	plan, err := platform.PlanDeployment(env.App, env.Name, revision.RevisionState, dep.ReleaseID, dep.ID)
	if err != nil {
		return empty, false
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		return empty, false
	}
	payload := platform.CandidateDiagnosticPayload{Step: platform.CandidateDiagnosticStep, App: env.App, Environment: env.Name, ReleaseID: dep.ReleaseID, DeploymentID: dep.ID, Deadline: deadline}
	truncated := len(plan) > 64
	if truncated {
		plan = plan[:64]
	}
	for _, p := range plan {
		payload.Containers = append(payload.Containers, platform.CandidateDiagnosticContainer{Name: p.Name, Process: p.Process.Name, Kind: p.Process.Kind, Replica: p.Replica})
	}
	nodeName, err := deploymentCleanupNode(ctx, s, dep, env.Node)
	if err != nil || nodeName == "" {
		return empty, false
	}
	tr, err := s.resolver.TransportFor(ctx, nodeName)
	if err != nil {
		return empty, false
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return empty, false
	}
	execution := engine.Execution{OperationID: op.ID, Attempt: op.Attempt, Step: payload.Step, RequestID: newRequestID()}
	response, err := tr.Call(ctx, node.Request{Method: node.MethodRun, RequestID: execution.RequestID, Execution: execution, Payload: raw, Secrets: values}, nil)
	if err != nil || response.Status != engine.StatusCompleted {
		return empty, false
	}
	var result platform.CandidateDiagnosticResult
	if json.Unmarshal(response.Result, &result) != nil {
		return empty, false
	}
	if len(result.Lines) > 512 {
		return empty, false
	}
	registry := &secrets.Registry{}
	for _, value := range values {
		registry.Add(value)
	}
	expected := make(map[string]platform.CandidateDiagnosticContainer, len(payload.Containers))
	for _, container := range payload.Containers {
		expected[container.Name] = container
	}
	counts := make(map[string]int)
	bytes := 0
	for i := range result.Lines {
		line := &result.Lines[i]
		container, ok := expected[line.Container]
		if !ok || line.Process != container.Process || line.Replica != container.Replica || line.ContainerID == "" || line.Timestamp.IsZero() {
			return empty, false
		}
		line.Message = registry.Redact(line.Message)
		counts[line.Container]++
		bytes += len(line.Message)
		if counts[line.Container] > 200 || len(line.Message) > 32*1024 || bytes > 64*1024-512 || !utf8.ValidString(line.Message) {
			return empty, false
		}
	}
	result.Truncated = result.Truncated || truncated
	return result, true
}
