package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
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
// response or partial failure. A claim without a completion summary remains
// unconfirmed. Nothing in this path can veto the safety tail.
func (s *Service) captureCandidateDiagnostics(parent context.Context, env platform.Environment, dep platform.Deployment, attempt uint32) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), 5*time.Second)
	defer cancel()
	op, _, err := s.operationSnapshot(ctx, dep.OperationID)
	if err != nil || op.Attempt != attempt {
		return
	}
	step, err := s.candidateCaptureStep(ctx, env, dep, op)
	if err != nil {
		return
	}
	claimed := false
	err = s.mutateCandidateDiagnostic(ctx, op, func(tx pgx.Tx, current *contract.Operation) error {
		var allowed, exists bool
		if err := tx.QueryRow(ctx, `SELECT true FROM eregion_platform_deployment_owners WHERE environment_id=$1 AND operation_id=$2 AND deployment_id=$3 AND owner_attempt=$4 FOR UPDATE`, env.ID, op.ID, dep.ID, attempt).Scan(&allowed); errors.Is(err, pgx.ErrNoRows) {
			return nil
		} else if err != nil {
			return err
		}
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM eregion_operation_events WHERE operation_id=$1 AND event->>'stage'=$2 AND event->>'attempt'=$3 AND event->>'step'=$4)`, op.ID, platform.CandidateDiagnosticStage, fmt.Sprint(op.Attempt), step).Scan(&exists); err != nil {
			return err
		}
		if exists {
			return nil
		}
		if err := appendEvent(ctx, tx, current, contract.Event{Kind: "warning", Stage: platform.CandidateDiagnosticStage, Step: step, Message: "Candidate diagnostic capture started; completion is unconfirmed until a summary follows.", CandidateDiagnostic: &contract.CandidateDiagnostic{DeploymentID: string(dep.ID), Status: "unconfirmed"}}); err != nil {
			return err
		}
		// Only the exact lifecycle owner may claim a Docker read.
		claimed = allowed
		return nil
	})
	if err != nil || !claimed {
		return
	}
	result := platform.CandidateDiagnosticResult{Unavailable: true}
	if captured, ok := s.readCandidateDiagnostics(ctx, env, dep, op, step); ok {
		result = captured
	}
	// The claim is already durable; write all output and its summary atomically.
	// If this fails, the claim remains honestly unconfirmed.
	_ = s.mutateCandidateDiagnostic(ctx, op, func(tx pgx.Tx, current *contract.Operation) error {
		var owned bool
		if err := tx.QueryRow(ctx, `SELECT true FROM eregion_platform_deployment_owners WHERE environment_id=$1 AND operation_id=$2 AND deployment_id=$3 AND owner_attempt=$4 FOR UPDATE`, env.ID, op.ID, dep.ID, attempt).Scan(&owned); errors.Is(err, pgx.ErrNoRows) {
			return errNotOwned
		} else if err != nil {
			return err
		}
		if !owned {
			return errNotOwned
		}
		for _, observation := range result.Observations {
			d := &contract.CandidateDiagnostic{DeploymentID: string(dep.ID), Process: observation.Process, Replica: observation.Replica, Container: observation.Container, ContainerID: observation.ContainerID, Timestamp: observation.ObservedAt, Status: "observed", Observation: &contract.CandidateLifecycleObservation{State: observation.State, Running: observation.Running, StartedAt: observation.StartedAt, FinishedAt: observation.FinishedAt, ExitCode: observation.ExitCode, OOMKilled: observation.OOMKilled, Health: observation.Health}}
			if err := appendEvent(ctx, tx, current, contract.Event{Kind: "warning", Stage: platform.CandidateDiagnosticStage, Step: step, Message: "Candidate lifecycle observed before cleanup.", CandidateDiagnostic: d}); err != nil {
				return err
			}
		}
		for _, line := range result.Lines {
			d := &contract.CandidateDiagnostic{DeploymentID: string(dep.ID), Process: line.Process, Replica: line.Replica, Container: line.Container, ContainerID: line.ContainerID, Timestamp: line.Timestamp, Status: "captured", RedactionContext: result.RedactionContext}
			if err := appendEvent(ctx, tx, current, contract.Event{Kind: "log", Stage: platform.CandidateDiagnosticStage, Step: step, Message: "[log read time; original line time unavailable] " + line.Message, CandidateDiagnostic: d}); err != nil {
				return err
			}
		}
		status := "complete"
		if result.Unavailable {
			status = "unavailable"
			if len(result.Observations) > 0 || len(result.Lines) > 0 {
				status = "partial"
			}
		}
		if result.Truncated {
			if status == "unavailable" || status == "partial" {
				status += "_truncated"
			} else {
				status = "truncated"
			}
		}
		return appendEvent(ctx, tx, current, contract.Event{Kind: "warning", Stage: platform.CandidateDiagnosticStage, Step: step, Message: "Candidate diagnostic capture: " + status + ".", CandidateDiagnostic: &contract.CandidateDiagnostic{DeploymentID: string(dep.ID), Status: status}})
	})
}

// candidateCaptureStep scopes a claim to immutable planned identities and
// recorded execution IDs, never a wall clock or a mutable diagnostic deadline.
func (s *Service) candidateCaptureStep(ctx context.Context, env platform.Environment, dep platform.Deployment, op contract.Operation) (string, error) {
	revision, err := s.revisionByID(ctx, dep.RevisionID)
	identities := []string{string(op.ID), fmt.Sprint(op.Attempt), string(dep.ID)}
	if err != nil || revision.App != env.App || revision.Environment != env.Name {
		identities = append(identities, "plan:unavailable")
	} else {
		plan, planErr := platform.PlanDeployment(env.App, env.Name, revision.RevisionState, dep.ReleaseID, dep.ID)
		if planErr != nil {
			identities = append(identities, "plan:unavailable")
		} else {
			for _, item := range plan {
				identities = append(identities, "container:"+item.Name)
			}
		}
	}
	records, err := s.stepRecords(ctx, op.ID)
	if err != nil {
		return "", err
	}
	for _, record := range records {
		if record.ExecutionID != "" && (strings.HasPrefix(record.Step, deploymentContainerStepPrefix) || strings.HasPrefix(record.Step, "platform-health-") || strings.HasPrefix(record.Step, "platform-command-") || strings.HasPrefix(record.Step, "platform-port-")) {
			identities = append(identities, "execution:"+record.ExecutionID)
		}
	}
	sort.Strings(identities)
	raw, err := json.Marshal(identities)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return fmt.Sprintf("platform-candidate-diagnostic-%x-v1", sum[:16]), nil
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

func (s *Service) readCandidateDiagnostics(ctx context.Context, env platform.Environment, dep platform.Deployment, op contract.Operation, step string) (platform.CandidateDiagnosticResult, bool) {
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
	if err != nil || revision.App != dep.App || revision.Environment != dep.Environment {
		return empty, false
	}
	env.Revision = &revision
	metadataOnly := false
	for _, name := range revision.SecretNames {
		if _, ok := dep.SecretRevisions[name]; !ok {
			metadataOnly = true
		}
	}
	if len(dep.SecretRevisions) == 0 && len(env.SecretRefs) > 0 {
		metadataOnly = true
	}
	env.SecretRefs = nil
	worker := &platformWorker{service: s}
	values := map[string]string{}
	if !metadataOnly {
		var key, credential string
		values, key, credential, _, err = worker.resolveDeploymentSecrets(ctx, env, dep)
		if err != nil {
			metadataOnly = true
			values = map[string]string{}
		} else if credential != "" {
			values[key] = credential
		}
	}
	for _, name := range revision.SecretNames {
		if values[name] == "" {
			metadataOnly = true
		}
	}
	for name := range dep.SecretRevisions {
		if values[name] == "" {
			metadataOnly = true
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
	payload := platform.CandidateDiagnosticPayload{Step: step, App: env.App, Environment: env.Name, ReleaseID: dep.ReleaseID, DeploymentID: dep.ID, Deadline: deadline, MetadataOnly: metadataOnly}
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
	// This marker comes from the caller's verified historical context, never
	// from the node's response or from current secret revisions.
	switch {
	case metadataOnly:
		result.RedactionContext = "unavailable"
		result.Lines = nil
		result.Unavailable = true
	case len(values) == 0:
		result.RedactionContext = "not_required"
	default:
		result.RedactionContext = "original"
	}
	if len(result.Lines) > 0 && result.LogTimestampSource != "observation_time" {
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
	if len(result.Observations) > len(expected) {
		return empty, false
	}
	seen := make(map[string]bool)
	for _, observation := range result.Observations {
		container, ok := expected[observation.Container]
		if !ok || seen[observation.Container] || observation.Process != container.Process || observation.Replica != container.Replica || observation.ContainerID == "" || observation.ObservedAt.IsZero() {
			return empty, false
		}
		switch observation.State {
		case "created", "running", "paused", "restarting", "removing", "exited", "dead":
		default:
			return empty, false
		}
		switch observation.Health {
		case "", "starting", "healthy", "unhealthy":
		default:
			return empty, false
		}
		seen[observation.Container] = true
	}
	counts := make(map[string]int)
	bytes := 0
	lines := make([]platform.CandidateDiagnosticLine, 0, len(result.Lines))
	for _, line := range result.Lines {
		container, ok := expected[line.Container]
		if !ok || line.Process != container.Process || line.Replica != container.Replica || line.ContainerID == "" || line.Timestamp.IsZero() {
			result.Unavailable = true
			continue
		}
		line.Message = registry.Redact(line.Message)
		counts[line.Container]++
		if counts[line.Container] > 200 || len(line.Message) > 32*1024 || bytes+len(line.Message) > 64*1024-512 || !utf8.ValidString(line.Message) {
			result.Truncated = true
			continue
		}
		bytes += len(line.Message)
		lines = append(lines, line)
	}
	result.Lines = lines
	result.Truncated = result.Truncated || truncated
	return result, true
}
