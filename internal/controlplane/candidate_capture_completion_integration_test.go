package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
)

// candidateCaptureFixture durably admits one deployment and prepares the
// exact owner/attempt fences a terminal cleanup capture uses. PostgreSQL and
// the application service are real; the node read is the narrow external
// result seam below.
type candidateCaptureFixture struct {
	service *Service
	pool    *pgxpool.Pool
	op      contract.Operation
	env     platform.Environment
	dep     platform.Deployment
	plan    []platform.PlannedContainer
}

func newCandidateCaptureFixture(
	t *testing.T,
	key string,
	processes []platform.ProcessSpec,
) candidateCaptureFixture {
	t.Helper()
	s, pool := gitDeployFixture(t)
	ctx := context.Background()
	if len(processes) > 0 {
		platformSubmit(
			t,
			s,
			platform.KindEnvironmentUpdate,
			"env/shop/production",
			key+"-processes",
			map[string]any{"processes": processes},
		)
	}
	s.resolveGitRef = func(context.Context, string, string, []byte) (string, error) {
		return submittedGitCommit, nil
	}
	op := platformSubmit(
		t,
		s,
		platform.KindDeploy,
		"env/shop/production",
		key+"-deploy",
		map[string]any{},
	)
	env, err := s.environment(ctx, "shop", "production")
	if err != nil {
		t.Fatal(err)
	}
	dep, ok, err := s.deploymentByOperation(ctx, op.ID)
	if err != nil || !ok {
		t.Fatalf("deployment: %v %v", ok, err)
	}
	if _, err := pool.Exec(
		ctx,
		`UPDATE eregion_operations SET state='running',attempt=1 WHERE id=$1`,
		op.ID,
	); err != nil {
		t.Fatal(err)
	}
	insertDeploymentOwner(
		t,
		pool,
		env,
		deploymentOwner{
			EnvironmentID: env.ID,
			OperationID:   op.ID,
			DeploymentID:  dep.ID,
			Attempt:       1,
			Phase:         deploymentOwnerPrepared,
		},
	)
	plan, err := platform.PlanDeployment(
		env.App,
		env.Name,
		env.Revision.RevisionState,
		dep.ReleaseID,
		dep.ID,
	)
	if err != nil || len(plan) == 0 {
		t.Fatalf("plan: %+v %v", plan, err)
	}
	return candidateCaptureFixture{
		service: s,
		pool:    pool,
		op:      op,
		env:     env,
		dep:     dep,
		plan:    plan,
	}
}

func (f candidateCaptureFixture) useTransport(t *testing.T, transport node.Transport) {
	t.Helper()
	f.service.resolver = newTestNodeResolver(
		t,
		f.pool,
		map[string]node.Transport{"local": transport},
	)
}

func (f candidateCaptureFixture) recordCleanupIntent(t *testing.T) {
	t.Helper()
	instances := make([]platform.DeploymentInstance, 0, len(f.plan))
	for _, item := range f.plan {
		instances = append(
			instances,
			platform.DeploymentInstance{
				Name:      item.Name,
				Process:   item.Process.Name,
				Kind:      item.Process.Kind,
				Replica:   item.Replica,
				ReleaseID: f.dep.ReleaseID,
				State:     "running",
			},
		)
	}
	encoded, err := json.Marshal(instances)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(
		context.Background(),
		`UPDATE eregion_platform_deployments SET instances=$2 WHERE id=$1`,
		f.dep.ID,
		encoded,
	); err != nil {
		t.Fatal(err)
	}
	dep, err := f.service.deploymentByID(context.Background(), f.dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.recordDeploymentCleanupCandidates(
		context.Background(),
		f.env,
		dep,
		1,
	); err != nil {
		t.Fatalf("capture failure vetoed cleanup: %v", err)
	}
	var cleanups int
	if err := f.pool.QueryRow(context.Background(), `SELECT count(*) FROM eregion_platform_container_cleanup WHERE deployment_id=$1 AND reason='candidate'`, f.dep.ID).
		Scan(&cleanups); err != nil {
		t.Fatal(err)
	}
	if cleanups != len(f.plan) {
		t.Fatalf("cleanup intents = %d, want %d", cleanups, len(f.plan))
	}
}

// candidateCaptureTransport is the narrow external-effect seam for candidate
// diagnostics: it returns test-owned typed observations, lines, blocking or a
// read failure. There is no host process, PATH shim, filesystem layout or
// dispatcher emulation here. PostgreSQL, the application service and the
// worker transitions that run the capture are real.
type candidateCaptureTransport struct {
	mu       sync.Mutex
	reads    int
	payloads []platform.CandidateDiagnosticPayload
	call     func(context.Context, platform.CandidateDiagnosticPayload) (platform.CandidateDiagnosticResult, error)
}

func (*candidateCaptureTransport) Name() string         { return "candidate-capture-result" }
func (*candidateCaptureTransport) NodeIdentity() string { return "local" }

func (t *candidateCaptureTransport) ReadCount() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.reads
}

func (t *candidateCaptureTransport) Payloads() []platform.CandidateDiagnosticPayload {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]platform.CandidateDiagnosticPayload(nil), t.payloads...)
}

func (t *candidateCaptureTransport) Call(
	ctx context.Context,
	request node.Request,
	_ func(node.Event),
) (node.Response, error) {
	var payload platform.CandidateDiagnosticPayload
	if err := json.Unmarshal(request.Payload, &payload); err != nil {
		return node.Response{}, err
	}
	t.mu.Lock()
	t.reads++
	t.payloads = append(t.payloads, payload)
	call := t.call
	t.mu.Unlock()
	var result platform.CandidateDiagnosticResult
	if call != nil {
		var err error
		result, err = call(ctx, payload)
		if err != nil {
			return node.Response{}, err
		}
	}
	raw, err := json.Marshal(result)
	if err != nil {
		return node.Response{}, err
	}
	return node.Response{
		Status:      engine.StatusCompleted,
		ExecutionID: request.Execution.ExecutionID(),
		Executed:    true,
		Result:      raw,
	}, nil
}

// candidateGatedCall completes the external read, then holds its result until
// the test has established the next database fault. Channels, not sleeps,
// order the read boundary against the completion transaction.
func candidateGatedCall(
	ready, release chan struct{},
	result platform.CandidateDiagnosticResult,
) func(context.Context, platform.CandidateDiagnosticPayload) (platform.CandidateDiagnosticResult, error) {
	var once sync.Once
	return func(
		ctx context.Context,
		_ platform.CandidateDiagnosticPayload,
	) (platform.CandidateDiagnosticResult, error) {
		once.Do(func() { close(ready) })
		select {
		case <-release:
			return result, nil
		case <-ctx.Done():
			return platform.CandidateDiagnosticResult{}, ctx.Err()
		}
	}
}

func candidateContainerID(index int) string {
	return fmt.Sprintf("%064x", index+11)
}

// candidateContainers mirrors the planned identities a real node would read
// from the request payload. Result values are test-owned.
func candidateContainers(plan []platform.PlannedContainer) []platform.CandidateDiagnosticContainer {
	out := make([]platform.CandidateDiagnosticContainer, 0, len(plan))
	for _, item := range plan {
		out = append(out, platform.CandidateDiagnosticContainer{
			Name:    item.Name,
			Process: item.Process.Name,
			Kind:    item.Process.Kind,
			Replica: item.Replica,
		})
	}
	return out
}

func candidateObservation(
	container platform.CandidateDiagnosticContainer,
	index int,
	observedAt time.Time,
) platform.CandidateObservation {
	exitCode := 3 + index
	return platform.CandidateObservation{
		Process:     container.Process,
		Replica:     container.Replica,
		Container:   container.Name,
		ContainerID: candidateContainerID(index),
		ObservedAt:  observedAt,
		State:       "exited",
		Running:     false,
		StartedAt:   observedAt.Add(-time.Minute),
		FinishedAt:  observedAt,
		ExitCode:    &exitCode,
		OOMKilled:   false,
	}
}

func candidateObservations(
	containers []platform.CandidateDiagnosticContainer,
	observedAt time.Time,
) []platform.CandidateObservation {
	out := make([]platform.CandidateObservation, 0, len(containers))
	for index, container := range containers {
		out = append(out, candidateObservation(container, index, observedAt))
	}
	return out
}

func candidateLine(
	container platform.CandidateDiagnosticContainer,
	index int,
	timestamp time.Time,
	message string,
) platform.CandidateDiagnosticLine {
	return platform.CandidateDiagnosticLine{
		Process:     container.Process,
		Replica:     container.Replica,
		Container:   container.Name,
		ContainerID: candidateContainerID(index),
		Timestamp:   timestamp,
		Message:     message,
	}
}

func candidateProcessIndex(
	t *testing.T,
	containers []platform.CandidateDiagnosticContainer,
	process string,
) int {
	t.Helper()
	for index, container := range containers {
		if container.Process == process {
			return index
		}
	}
	t.Fatalf("process %q not in %+v", process, containers)
	return -1
}

type candidateCaptureEvents struct {
	claims     int
	summary    string
	observed   int
	captured   int
	processes  []string
	containers []string
	rendered   string
}

func readCandidateCaptureEvents(
	t *testing.T,
	pool *pgxpool.Pool,
	operationID contract.OperationID,
) candidateCaptureEvents {
	t.Helper()
	var events candidateCaptureEvents
	if err := pool.QueryRow(context.Background(), `SELECT
		count(*) FILTER (WHERE event->'candidate_diagnostic'->>'status'='unconfirmed'),
		coalesce(max(event->'candidate_diagnostic'->>'status') FILTER (WHERE event->>'message' LIKE 'Candidate diagnostic capture:%'),''),
		count(*) FILTER (WHERE event->'candidate_diagnostic'->>'status'='observed'),
		count(*) FILTER (WHERE event->'candidate_diagnostic'->>'status'='captured'),
		coalesce(array_agg(DISTINCT event->'candidate_diagnostic'->>'process') FILTER (WHERE event->'candidate_diagnostic'->>'status' IN ('observed','captured')),'{}'),
		coalesce(array_agg(DISTINCT event->'candidate_diagnostic'->>'container') FILTER (WHERE event->'candidate_diagnostic'->>'status' IN ('observed','captured')),'{}'),
		coalesce(string_agg(event::text,E'\n'),'')
		FROM eregion_operation_events WHERE operation_id=$1 AND event->>'stage'=$2`,
		operationID, platform.CandidateDiagnosticStage).Scan(
		&events.claims, &events.summary, &events.observed, &events.captured, &events.processes, &events.containers, &events.rendered); err != nil {
		t.Fatal(err)
	}
	return events
}

// A node read that consumes the complete five-second setup/read budget leaves
// the setup context expired. The durable completion summary must still be
// written under its own bounded budget; the old shared context silently left
// the claim unconfirmed. This is the actual beta.8 old-context reproduction.
func TestIntegrationCandidateCaptureCompletionAfterReadDeadline(t *testing.T) {
	f := newCandidateCaptureFixture(t, "capture-deadline", nil)
	transport := &candidateCaptureTransport{
		call: func(ctx context.Context, _ platform.CandidateDiagnosticPayload) (platform.CandidateDiagnosticResult, error) {
			<-ctx.Done()
			return platform.CandidateDiagnosticResult{}, ctx.Err()
		},
	}
	f.useTransport(t, transport)

	started := time.Now()
	f.service.captureCandidateDiagnostics(context.Background(), f.env, f.dep, 1)
	elapsed := time.Since(started)
	events := readCandidateCaptureEvents(t, f.pool, f.op.ID)
	if events.claims != 1 || events.summary != "unavailable" || events.observed != 0 ||
		events.captured != 0 {
		t.Fatalf("completion after exhausted read budget: %+v", events)
	}
	if transport.ReadCount() != 1 {
		t.Fatalf("node reads = %d, want one bounded read", transport.ReadCount())
	}
	// Five-second setup/read plus a fresh two-second completion budget is the
	// synchronous bound; the completion itself must not add a second read wait.
	if elapsed < 4*time.Second || elapsed > 7*time.Second+750*time.Millisecond {
		t.Fatalf(
			"capture elapsed %s outside the bounded setup/read plus completion budgets",
			elapsed,
		)
	}
	// A read timeout cannot veto the malformed/partial cleanup safety tail.
	f.recordCleanupIntent(t)
}

// A successful read followed by an unavailable completion transaction keeps
// the durable claim honestly unconfirmed. A test connection holds the exact
// operation row lock; the capture completion blocks for its bounded budget and
// returns through the static failure category.
func TestIntegrationCandidateCaptureCompletionPersistenceUnavailable(t *testing.T) {
	f := newCandidateCaptureFixture(t, "capture-blocked-completion", nil)
	containers := candidateContainers(f.plan)
	observedAt := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	result := platform.CandidateDiagnosticResult{
		Observations:       candidateObservations(containers, observedAt),
		Lines:              []platform.CandidateDiagnosticLine{candidateLine(containers[0], 0, observedAt, "web useful output")},
		LogTimestampSource: "observation_time",
	}
	ready := make(chan struct{})
	release := make(chan struct{})
	transport := &candidateCaptureTransport{call: candidateGatedCall(ready, release, result)}
	f.useTransport(t, transport)
	var logBuffer bytes.Buffer
	f.service.logger = slog.New(slog.NewTextHandler(&logBuffer, nil))

	done := make(chan struct{})
	go func() {
		f.service.captureCandidateDiagnostics(context.Background(), f.env, f.dep, 1)
		close(done)
	}()
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("candidate node read did not complete")
	}

	holder, err := f.pool.Begin(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Rollback(context.Background()) }()
	if _, err := holder.Exec(
		context.Background(),
		`SELECT id FROM eregion_operations WHERE id=$1 FOR UPDATE`,
		f.op.ID,
	); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	close(release)
	waitCtx, cancelWait := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelWait()
	waitPointerLockWaiter(t, waitCtx, f.pool, "WHERE id = $1")
	select {
	case <-done:
	case <-time.After(8 * time.Second):
		t.Fatal("capture did not return after the bounded completion budget")
	}
	elapsed := time.Since(started)

	events := readCandidateCaptureEvents(t, f.pool, f.op.ID)
	if events.claims != 1 || events.summary != "" || events.observed != 0 || events.captured != 0 {
		t.Fatalf("blocked completion changed durable state: %+v", events)
	}
	if elapsed < 1500*time.Millisecond || elapsed > 4*time.Second {
		t.Fatalf("completion transaction budget ran %s, want about two seconds", elapsed)
	}
	logged := logBuffer.String()
	if !strings.Contains(logged, "candidate_capture_completion_unconfirmed") ||
		!strings.Contains(logged, "reason=persistence") ||
		strings.Contains(logged, "context deadline") ||
		strings.Contains(logged, "SQLSTATE") ||
		strings.Contains(logged, "SELECT") ||
		strings.Contains(logged, "pgx") {
		t.Fatalf("completion failure log was not a bounded static category: %q", logged)
	}

	// A lost-response retry must not duplicate the claim or manufacture a
	// completion summary from output it never stored. Release the row lock
	// first, otherwise the retry would merely observe the same setup timeout.
	if err := holder.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	readsBeforeRetry := transport.ReadCount()
	f.service.captureCandidateDiagnostics(context.Background(), f.env, f.dep, 1)
	retry := readCandidateCaptureEvents(t, f.pool, f.op.ID)
	if retry.claims != 1 || retry.summary != "" || retry.observed != 0 || retry.captured != 0 {
		t.Fatalf("retry after lost response: %+v", retry)
	}
	if transport.ReadCount() != readsBeforeRetry {
		t.Fatalf("retry performed another node read: %d -> %d", readsBeforeRetry, transport.ReadCount())
	}

	// The safety tail records cleanup intent even though capture persistence
	// failed. Collection or persistence errors cannot veto cleanup.
	f.recordCleanupIntent(t)
}

// A quick read failure that is not a deadline overrun still completes
// honestly as unavailable when PostgreSQL is available.
func TestIntegrationCandidateCaptureUnavailableReadCompletion(t *testing.T) {
	f := newCandidateCaptureFixture(t, "capture-read-failed", nil)
	transport := &candidateCaptureTransport{
		call: func(context.Context, platform.CandidateDiagnosticPayload) (platform.CandidateDiagnosticResult, error) {
			return platform.CandidateDiagnosticResult{}, errors.New("candidate read unavailable")
		},
	}
	f.useTransport(t, transport)
	f.service.captureCandidateDiagnostics(context.Background(), f.env, f.dep, 1)
	events := readCandidateCaptureEvents(t, f.pool, f.op.ID)
	if events.claims != 1 || events.summary != "unavailable" || events.observed != 0 ||
		events.captured != 0 {
		t.Fatalf("failed read completion: %+v", events)
	}
	if transport.ReadCount() != 1 {
		t.Fatalf("node reads = %d, want one failed read", transport.ReadCount())
	}
}

// Caller cancellation during the node read cannot cancel the fresh completion
// context. The already-read observations and summary still commit atomically.
func TestIntegrationCandidateCaptureCompletionDetachedFromCaller(t *testing.T) {
	f := newCandidateCaptureFixture(t, "capture-caller-cancel", nil)
	containers := candidateContainers(f.plan)
	observedAt := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	result := platform.CandidateDiagnosticResult{
		Observations:       candidateObservations(containers, observedAt),
		Lines:              []platform.CandidateDiagnosticLine{candidateLine(containers[0], 0, observedAt, "original failure output")},
		LogTimestampSource: "observation_time",
	}
	ready := make(chan struct{})
	release := make(chan struct{})
	transport := &candidateCaptureTransport{call: candidateGatedCall(ready, release, result)}
	f.useTransport(t, transport)
	parent, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		f.service.captureCandidateDiagnostics(parent, f.env, f.dep, 1)
		close(done)
	}()
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("candidate node read did not complete")
	}
	cancel()
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled caller prevented capture completion")
	}
	events := readCandidateCaptureEvents(t, f.pool, f.op.ID)
	if events.claims != 1 || events.summary != "complete" || events.observed != 1 ||
		events.captured != 1 ||
		!strings.Contains(events.rendered, "original failure output") ||
		!strings.Contains(events.rendered, `"exit_code"`) {
		t.Fatalf("cancellation-detached completion: %+v", events)
	}
}

// A Rails-like formation has several processes and containers. A per-container
// inspect or log failure is partial, never a reason to drop the observations
// or lines that were actually read.
func TestIntegrationCandidateCaptureMultipleContainersPartialFailures(t *testing.T) {
	processes := []platform.ProcessSpec{
		{Name: "web", Kind: "web", Replicas: 1, ContainerPort: 8080},
		{Name: "worker", Kind: "worker", Command: "./worker", Replicas: 1},
	}
	t.Run("worker logs unavailable", func(t *testing.T) {
		f := newCandidateCaptureFixture(t, "capture-multi-logs", processes)
		if len(f.plan) != 2 {
			t.Fatalf("plan = %d containers, want web and worker", len(f.plan))
		}
		containers := candidateContainers(f.plan)
		observedAt := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
		web := candidateProcessIndex(t, containers, "web")
		result := platform.CandidateDiagnosticResult{
			Observations: candidateObservations(containers, observedAt),
			Lines: []platform.CandidateDiagnosticLine{
				candidateLine(containers[web], web, observedAt, "web output"),
			},
			LogTimestampSource: "observation_time",
			Unavailable:        true,
		}
		f.useTransport(t, &candidateCaptureTransport{
			call: func(context.Context, platform.CandidateDiagnosticPayload) (platform.CandidateDiagnosticResult, error) {
				return result, nil
			},
		})
		f.service.captureCandidateDiagnostics(context.Background(), f.env, f.dep, 1)
		events := readCandidateCaptureEvents(t, f.pool, f.op.ID)
		if events.claims != 1 || events.summary != "partial" || events.observed != 2 ||
			events.captured != 1 {
			t.Fatalf("worker log failure: %+v", events)
		}
		if !contains(events.processes, "web") || !contains(events.processes, "worker") {
			t.Fatalf("both processes must be observed: %+v", events)
		}
	})
	t.Run("worker inspect unavailable", func(t *testing.T) {
		f := newCandidateCaptureFixture(t, "capture-multi-inspect", processes)
		containers := candidateContainers(f.plan)
		observedAt := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
		web := candidateProcessIndex(t, containers, "web")
		result := platform.CandidateDiagnosticResult{
			Observations: []platform.CandidateObservation{
				candidateObservation(containers[web], web, observedAt),
			},
			Lines: []platform.CandidateDiagnosticLine{
				candidateLine(containers[web], web, observedAt, "web output"),
			},
			LogTimestampSource: "observation_time",
			Unavailable:        true,
		}
		f.useTransport(t, &candidateCaptureTransport{
			call: func(context.Context, platform.CandidateDiagnosticPayload) (platform.CandidateDiagnosticResult, error) {
				return result, nil
			},
		})
		f.service.captureCandidateDiagnostics(context.Background(), f.env, f.dep, 1)
		events := readCandidateCaptureEvents(t, f.pool, f.op.ID)
		if events.claims != 1 || events.summary != "partial" || events.observed != 1 ||
			events.captured != 1 {
			t.Fatalf("worker inspect failure: %+v", events)
		}
	})
}

// Count and byte truncation are reported as truncation while retaining only
// bounded output. They are not classified as complete.
func TestIntegrationCandidateCaptureTruncationClassification(t *testing.T) {
	t.Run("line count", func(t *testing.T) {
		f := newCandidateCaptureFixture(t, "capture-count-truncation", nil)
		containers := candidateContainers(f.plan)
		observedAt := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
		result := platform.CandidateDiagnosticResult{
			Observations:       candidateObservations(containers, observedAt),
			LogTimestampSource: "observation_time",
		}
		for index := 0; index < 300; index++ {
			result.Lines = append(
				result.Lines,
				candidateLine(containers[0], 0, observedAt, "useful line"),
			)
		}
		f.useTransport(t, &candidateCaptureTransport{
			call: func(context.Context, platform.CandidateDiagnosticPayload) (platform.CandidateDiagnosticResult, error) {
				return result, nil
			},
		})
		f.service.captureCandidateDiagnostics(context.Background(), f.env, f.dep, 1)
		events := readCandidateCaptureEvents(t, f.pool, f.op.ID)
		if events.claims != 1 || events.summary != "truncated" || events.observed != 1 ||
			events.captured != 200 {
			t.Fatalf(
				"count truncation: %+v claims=%d summary=%q observed=%d captured=%d",
				events,
				events.claims,
				events.summary,
				events.observed,
				events.captured,
			)
		}
	})
	t.Run("byte length", func(t *testing.T) {
		f := newCandidateCaptureFixture(t, "capture-byte-truncation", nil)
		containers := candidateContainers(f.plan)
		observedAt := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
		// Each line is within the node's 32 KiB message bound; the second one
		// exceeds the service's cumulative completion budget and is withheld.
		line := strings.Repeat("z", 32*1024)
		result := platform.CandidateDiagnosticResult{
			Observations: candidateObservations(containers, observedAt),
			Lines: []platform.CandidateDiagnosticLine{
				candidateLine(containers[0], 0, observedAt, line),
				candidateLine(containers[0], 0, observedAt, line),
			},
			LogTimestampSource: "observation_time",
		}
		f.useTransport(t, &candidateCaptureTransport{
			call: func(context.Context, platform.CandidateDiagnosticPayload) (platform.CandidateDiagnosticResult, error) {
				return result, nil
			},
		})
		f.service.captureCandidateDiagnostics(context.Background(), f.env, f.dep, 1)
		events := readCandidateCaptureEvents(t, f.pool, f.op.ID)
		if events.claims != 1 || events.summary != "truncated" || events.observed != 1 ||
			events.captured != 1 {
			t.Fatalf("byte truncation: %+v", events)
		}
	})
}

// A stale operation attempt is fenced before any claim or node read.
func TestIntegrationCandidateCaptureStaleAttemptWritesNothing(t *testing.T) {
	f := newCandidateCaptureFixture(t, "capture-stale-attempt", nil)
	transport := &candidateCaptureTransport{}
	f.useTransport(t, transport)
	if _, err := f.pool.Exec(
		context.Background(),
		`UPDATE eregion_operations SET attempt=2 WHERE id=$1`,
		f.op.ID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(
		context.Background(),
		`UPDATE eregion_platform_deployment_owners SET owner_attempt=2 WHERE operation_id=$1`,
		f.op.ID,
	); err != nil {
		t.Fatal(err)
	}
	f.service.captureCandidateDiagnostics(context.Background(), f.env, f.dep, 1)
	events := readCandidateCaptureEvents(t, f.pool, f.op.ID)
	if events.claims != 0 || events.summary != "" {
		t.Fatalf("stale attempt wrote capture events: %+v", events)
	}
	if transport.ReadCount() != 0 {
		t.Fatalf("stale attempt performed %d node reads", transport.ReadCount())
	}
}

// A newer deployment owner attempt is rejected before any claim, node read or
// capture event.
func TestIntegrationCandidateCaptureRejectsNewerOwner(t *testing.T) {
	f := newPointerBuildFixture(t)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `UPDATE eregion_operations SET state='running',attempt=1 WHERE id=$1`, f.op.ID); err != nil {
		t.Fatal(err)
	}
	insertDeploymentOwner(t, f.pool, f.env, deploymentOwner{
		EnvironmentID: f.env.ID, OperationID: f.op.ID, DeploymentID: f.dep,
		Attempt: 2, Phase: deploymentOwnerPrepared,
	})
	dep, err := f.service.deploymentByID(ctx, f.dep)
	if err != nil {
		t.Fatal(err)
	}
	transport := &candidateCaptureTransport{}
	f.service.resolver = newTestNodeResolver(t, f.pool, map[string]node.Transport{"local": transport})
	f.service.captureCandidateDiagnostics(ctx, f.env, dep, 1)
	var count int
	if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM eregion_operation_events WHERE operation_id=$1 AND event->>'stage'=$2`, f.op.ID, platform.CandidateDiagnosticStage).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("stale attempt wrote %d capture events", count)
	}
	if transport.ReadCount() != 0 {
		t.Fatalf("stale attempt performed %d node reads", transport.ReadCount())
	}
}

// runGatedCandidateCapture starts capture and returns once the external read
// has completed but before its result is released to the completion path.
func runGatedCandidateCapture(
	t *testing.T,
	f candidateCaptureFixture,
	result platform.CandidateDiagnosticResult,
) (*candidateCaptureTransport, chan struct{}, chan struct{}) {
	t.Helper()
	ready := make(chan struct{})
	release := make(chan struct{})
	transport := &candidateCaptureTransport{call: candidateGatedCall(ready, release, result)}
	f.useTransport(t, transport)
	done := make(chan struct{})
	go func() {
		f.service.captureCandidateDiagnostics(context.Background(), f.env, f.dep, 1)
		close(done)
	}()
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("candidate node read did not complete")
	}
	return transport, release, done
}

func waitCaptureDone(t *testing.T, done chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("capture completion did not return")
	}
}

func candidateUsefulResult(plan []platform.PlannedContainer) platform.CandidateDiagnosticResult {
	containers := candidateContainers(plan)
	observedAt := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	result := platform.CandidateDiagnosticResult{
		Observations:       candidateObservations(containers, observedAt),
		LogTimestampSource: "observation_time",
	}
	for index, container := range containers {
		result.Lines = append(
			result.Lines,
			candidateLine(container, index, observedAt, "post-claim output from "+container.Process),
		)
	}
	return result
}

// A stale operation attempt that appears only after the durable claim and node
// read must fence the completion transaction. The claim remains unconfirmed and
// none of the already-read observations, logs or summary may be written.
func TestIntegrationCandidateCaptureCompletionFencedByStaleAttempt(t *testing.T) {
	f := newCandidateCaptureFixture(t, "capture-post-claim-attempt", nil)
	transport, release, done := runGatedCandidateCapture(t, f, candidateUsefulResult(f.plan))
	if _, err := f.pool.Exec(
		context.Background(),
		`UPDATE eregion_operations SET attempt=2 WHERE id=$1`,
		f.op.ID,
	); err != nil {
		t.Fatal(err)
	}
	close(release)
	waitCaptureDone(t, done)
	events := readCandidateCaptureEvents(t, f.pool, f.op.ID)
	if events.claims != 1 || events.summary != "" || events.observed != 0 || events.captured != 0 {
		t.Fatalf("post-claim stale attempt wrote completion: %+v", events)
	}
	if transport.ReadCount() != 1 {
		t.Fatalf("node reads = %d, want the one pre-fence read", transport.ReadCount())
	}
}

// A newer deployment owner attempt that appears only after the claim and read
// must also fence completion: the owner row is checked independently of the
// operation attempt.
func TestIntegrationCandidateCaptureCompletionFencedByNewerOwner(t *testing.T) {
	f := newCandidateCaptureFixture(t, "capture-post-claim-owner", nil)
	transport, release, done := runGatedCandidateCapture(t, f, candidateUsefulResult(f.plan))
	if _, err := f.pool.Exec(
		context.Background(),
		`UPDATE eregion_platform_deployment_owners SET owner_attempt=2 WHERE operation_id=$1`,
		f.op.ID,
	); err != nil {
		t.Fatal(err)
	}
	close(release)
	waitCaptureDone(t, done)
	events := readCandidateCaptureEvents(t, f.pool, f.op.ID)
	if events.claims != 1 || events.summary != "" || events.observed != 0 || events.captured != 0 {
		t.Fatalf("post-claim newer owner wrote completion: %+v", events)
	}
	if transport.ReadCount() != 1 {
		t.Fatalf("node reads = %d, want the one pre-fence read", transport.ReadCount())
	}
}

// A failure while appending a later completion event must roll back every event
// already appended in that statement, along with the operation sequence
// advance. The durable claim remains the only record, so the completion is
// honestly unconfirmed rather than partial.
func TestIntegrationCandidateCaptureCompletionAtomicRollback(t *testing.T) {
	f := newCandidateCaptureFixture(t, "capture-mid-append-rollback", nil)
	ctx := context.Background()
	if _, err := f.pool.Exec(ctx, `CREATE FUNCTION eregion_test_reject_captured_event() RETURNS trigger
		LANGUAGE plpgsql AS $body$
		BEGIN
			IF NEW.event->'candidate_diagnostic'->>'status' = 'captured' THEN
				RAISE EXCEPTION 'test-only later capture event rejection';
			END IF;
			RETURN NEW;
		END
		$body$`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(ctx, `CREATE TRIGGER eregion_test_reject_captured_event
		BEFORE INSERT ON eregion_operation_events
		FOR EACH ROW EXECUTE FUNCTION eregion_test_reject_captured_event()`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.WithoutCancel(ctx), `DROP TRIGGER IF EXISTS eregion_test_reject_captured_event ON eregion_operation_events`)
		_, _ = f.pool.Exec(context.WithoutCancel(ctx), `DROP FUNCTION IF EXISTS eregion_test_reject_captured_event()`)
	})
	var logBuffer bytes.Buffer
	f.service.logger = slog.New(slog.NewTextHandler(&logBuffer, nil))
	transport, release, done := runGatedCandidateCapture(t, f, candidateUsefulResult(f.plan))
	var before uint64
	if err := f.pool.QueryRow(ctx, `SELECT last_sequence FROM eregion_operations WHERE id=$1`, f.op.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}
	close(release)
	waitCaptureDone(t, done)
	events := readCandidateCaptureEvents(t, f.pool, f.op.ID)
	if events.claims != 1 || events.summary != "" || events.observed != 0 || events.captured != 0 {
		t.Fatalf("mid-append rejection did not roll back the completion: %+v", events)
	}
	var after uint64
	if err := f.pool.QueryRow(ctx, `SELECT last_sequence FROM eregion_operations WHERE id=$1`, f.op.ID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("mid-append rejection advanced last_sequence %d -> %d", before, after)
	}
	if !strings.Contains(logBuffer.String(), "candidate_capture_completion_unconfirmed") ||
		!strings.Contains(logBuffer.String(), "reason=persistence") {
		t.Fatalf("mid-append rollback was not logged as unconfirmed: %q", logBuffer.String())
	}
	if transport.ReadCount() != 1 {
		t.Fatalf("node reads = %d, want one", transport.ReadCount())
	}
}

// A capture completion with the normal 200-line tail cap must fit the fixed
// two-second persistence budget even when advancing the operation sequence is
// expensive. The test-only trigger makes one sequence UPDATE cost 25 ms for
// this exact operation: the old per-event append path needs one UPDATE per
// event and exhausts the budget, while the batched capture path needs exactly
// one. PostgreSQL, the application service and the completion transaction are
// real; there is no simulated server or network.
func TestIntegrationCandidateCaptureCompletionBatchesSequenceAdvance(t *testing.T) {
	f := newCandidateCaptureFixture(t, "capture-batched-tail", nil)
	result := candidateBatchTailResult(f, 200)
	transport, release, done := runGatedCandidateCapture(t, f, result)
	ctx := context.Background()
	installCandidateCaptureBudgetTriggers(t, ctx, f)
	if _, err := f.pool.Exec(
		ctx,
		`INSERT INTO eregion_test_sequence_targets(operation_id) VALUES ($1)`,
		f.op.ID,
	); err != nil {
		t.Fatal(err)
	}
	var before uint64
	if err := f.pool.QueryRow(ctx, `SELECT last_sequence FROM eregion_operations WHERE id=$1`, f.op.ID).Scan(&before); err != nil {
		t.Fatal(err)
	}

	started := time.Now()
	close(release)
	waitCaptureDone(t, done)
	elapsed := time.Since(started)

	// One observation, the 200 accepted lines and the completion summary are
	// contiguous after the pre-existing sequence.
	events := readCandidateCaptureEvents(t, f.pool, f.op.ID)
	if events.claims != 1 || events.summary != "complete" || events.observed != 1 ||
		events.captured != 200 {
		t.Fatalf("batched tail completion: %+v", events)
	}
	stored := assertBatchedCaptureCompletion(t, ctx, f, before, elapsed, transport)
	assertCandidateCaptureReplay(t, ctx, f, stored)
}

func candidateBatchTailResult(f candidateCaptureFixture, lines int) platform.CandidateDiagnosticResult {
	containers := candidateContainers(f.plan)
	observedAt := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	result := platform.CandidateDiagnosticResult{
		Observations:       candidateObservations(containers, observedAt),
		LogTimestampSource: "observation_time",
	}
	for index := 0; index < lines; index++ {
		result.Lines = append(
			result.Lines,
			candidateLine(containers[0], 0, observedAt, fmt.Sprintf("candidate tail line %03d", index)),
		)
	}
	return result
}

func installCandidateCaptureBudgetTriggers(t *testing.T, ctx context.Context, f candidateCaptureFixture) {
	t.Helper()
	// The trigger is scoped by a row in a test-owned target table, never by an
	// ambient name or a broad pattern, and this test database is isolated.
	for _, statement := range []string{
		`CREATE TABLE eregion_test_sequence_targets (operation_id text PRIMARY KEY)`,
		`CREATE SEQUENCE eregion_test_sequence_advances START WITH 1`,
		`CREATE SEQUENCE eregion_test_event_inserts START WITH 1`,
		`CREATE FUNCTION eregion_test_slow_sequence_advance() RETURNS trigger
			LANGUAGE plpgsql AS $body$
			BEGIN
				IF EXISTS (SELECT 1 FROM eregion_test_sequence_targets WHERE operation_id = NEW.id) THEN
					PERFORM nextval('eregion_test_sequence_advances');
					PERFORM pg_sleep(0.025);
				END IF;
				RETURN NEW;
			END
			$body$`,
		`CREATE TRIGGER eregion_test_slow_sequence_advance
			BEFORE UPDATE OF last_sequence ON eregion_operations
			FOR EACH ROW EXECUTE FUNCTION eregion_test_slow_sequence_advance()`,
		`CREATE FUNCTION eregion_test_count_event_inserts() RETURNS trigger
			LANGUAGE plpgsql AS $body$
			BEGIN
				PERFORM nextval('eregion_test_event_inserts');
				RETURN NULL;
			END
			$body$`,
		`CREATE TRIGGER eregion_test_count_event_inserts
			BEFORE INSERT ON eregion_operation_events
			FOR EACH STATEMENT EXECUTE FUNCTION eregion_test_count_event_inserts()`,
	} {
		if _, err := f.pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(func() {
		_, _ = f.pool.Exec(context.WithoutCancel(ctx), `DROP TRIGGER IF EXISTS eregion_test_slow_sequence_advance ON eregion_operations`)
		_, _ = f.pool.Exec(context.WithoutCancel(ctx), `DROP TRIGGER IF EXISTS eregion_test_count_event_inserts ON eregion_operation_events`)
		_, _ = f.pool.Exec(context.WithoutCancel(ctx), `DROP FUNCTION IF EXISTS eregion_test_slow_sequence_advance()`)
		_, _ = f.pool.Exec(context.WithoutCancel(ctx), `DROP FUNCTION IF EXISTS eregion_test_count_event_inserts()`)
		_, _ = f.pool.Exec(context.WithoutCancel(ctx), `DROP TABLE IF EXISTS eregion_test_sequence_targets`)
		_, _ = f.pool.Exec(context.WithoutCancel(ctx), `DROP SEQUENCE IF EXISTS eregion_test_sequence_advances`)
		_, _ = f.pool.Exec(context.WithoutCancel(ctx), `DROP SEQUENCE IF EXISTS eregion_test_event_inserts`)
	})
}

func assertBatchedCaptureCompletion(
	t *testing.T,
	ctx context.Context,
	f candidateCaptureFixture,
	before uint64,
	elapsed time.Duration,
	transport *candidateCaptureTransport,
) contract.Operation {
	t.Helper()
	stored, err := f.service.Get(ctx, testActor, f.op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.LastSequence != before+202 {
		t.Fatalf("last sequence = %d, want %d (contiguous observation, lines and summary)", stored.LastSequence, before+202)
	}
	for _, probe := range []struct {
		name string
		want string
	}{
		{name: "sequence advances", want: "eregion_test_sequence_advances"},
		{name: "event inserts", want: "eregion_test_event_inserts"},
	} {
		var calls int64
		var called bool
		if err := f.pool.QueryRow(ctx, `SELECT last_value,is_called FROM `+probe.want).Scan(&calls, &called); err != nil {
			t.Fatal(err)
		}
		if !called || calls != 1 {
			t.Fatalf("%s = %d (called=%v), want exactly one batched statement", probe.name, calls, called)
		}
	}
	if elapsed >= 2*time.Second {
		t.Fatalf("batched completion took %s, outside its two-second budget", elapsed)
	}
	if transport.ReadCount() != 1 {
		t.Fatalf("node reads = %d, want one", transport.ReadCount())
	}
	return stored
}

func assertCandidateCaptureReplay(t *testing.T, ctx context.Context, f candidateCaptureFixture, stored contract.Operation) {
	t.Helper()
	// Replay the committed history through the application service and verify
	// every published cursor step is contiguous and carries the typed values.
	cursor := contract.Cursor{OperationID: f.op.ID}
	replayedCaptured, replayedObserved := 0, 0
	replayedSummary := ""
	for cursor.After < stored.LastSequence {
		page, err := f.service.Events(ctx, testActor, cursor, 200)
		if err != nil {
			t.Fatalf("replay at %d: %v", cursor.After, err)
		}
		want := cursor.After
		for _, event := range page.Events {
			want++
			if event.Sequence != want {
				t.Fatalf("replay sequence gap: got %d, want %d", event.Sequence, want)
			}
			if event.CandidateDiagnostic == nil {
				continue
			}
			switch event.CandidateDiagnostic.Status {
			case "captured":
				replayedCaptured++
			case "observed":
				replayedObserved++
			case "complete":
				replayedSummary = event.CandidateDiagnostic.Status
			}
		}
		if page.Cursor.After != want {
			t.Fatalf("page cursor %d, want %d", page.Cursor.After, want)
		}
		cursor = page.Cursor
	}
	if cursor.After != stored.LastSequence || replayedCaptured != 200 ||
		replayedObserved != 1 || replayedSummary != "complete" {
		t.Fatalf("replayed cursor=%d/%d captured=%d observed=%d summary=%q", cursor.After, stored.LastSequence, replayedCaptured, replayedObserved, replayedSummary)
	}
}

// A retained capture loss is classified from durable history, not from the
// requested event page. Later capture records and one-event pages cannot erase
// the honest unavailable classification.
func TestIntegrationCandidateCaptureLossClassificationIndependentOfPage(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	ctx := context.Background()
	op, err := run.Service.Submit(ctx, operations.Submission{
		Kind: operations.TestKind, Target: operations.TestTarget, Actor: testActor,
		IdempotencyKey: "capture-loss-page", Input: []byte(`{}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, ok, err := run.Service.claimAttempt(ctx, op.ID, 1, "test"); err != nil || !ok {
		t.Fatalf("claim: %v %v", ok, err)
	}
	_, _, err = run.Service.mutateOwned(
		ctx,
		op.ID,
		1,
		func(tx *entities.StoreTx, current *contract.Operation) error {
			if err := appendEvent(ctx, tx, current, captureUnconfirmedEvent()); err != nil {
				return err
			}
			if err := appendEvent(ctx, tx, current, captureLossEvent(op.ID)); err != nil {
				return err
			}
			return appendCaptureObservations(ctx, tx, current, 5)
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := run.Service.recordRetryResult(
		ctx,
		op.ID,
		1,
		contract.Unavailable,
		"original failure",
	); err != nil {
		t.Fatal(err)
	}
	got, err := run.Service.Get(ctx, testActor, op.ID)
	if err != nil || got.LatestResult == nil {
		t.Fatalf("result: %+v %v", got.LatestResult, err)
	}
	if got.LatestResult.Evidence != "interrupted" ||
		!hasEvidenceSource(got.LatestResult.EvidenceSources, "node_capture", "unavailable") {
		t.Fatalf("capture loss was not classified from durable history: %+v", got.LatestResult)
	}
	cursor := contract.Cursor{OperationID: op.ID}
	for cursor.After < got.LastSequence {
		page, err := run.Service.Events(ctx, testActor, cursor, 1)
		if err != nil || page.Cursor.After <= cursor.After {
			t.Fatalf("page at %d: %+v %v", cursor.After, page, err)
		}
		cursor = page.Cursor
	}
	after, err := run.Service.Get(ctx, testActor, op.ID)
	if err != nil || after.LatestResult == nil ||
		after.LatestResult.Evidence != got.LatestResult.Evidence ||
		len(after.LatestResult.EvidenceSources) != len(got.LatestResult.EvidenceSources) {
		t.Fatalf(
			"page traversal changed loss classification: before=%+v after=%+v err=%v",
			got.LatestResult,
			after.LatestResult,
			err,
		)
	}
}

func captureUnconfirmedEvent() contract.Event {
	return contract.Event{
		Kind:    "warning",
		Stage:   platform.CandidateDiagnosticStage,
		Message: "Candidate diagnostic capture started; completion is unconfirmed until a summary follows.",
		CandidateDiagnostic: &contract.CandidateDiagnostic{
			DeploymentID: "fixture",
			Status:       "unconfirmed",
		},
	}
}

func captureLossEvent(id contract.OperationID) contract.Event {
	return contract.Event{
		Kind:    "step",
		Stage:   "evidence",
		Message: "Node execution evidence could not be retained.",
		EvidenceLoss: &contract.NodeEvidenceLoss{
			ExecutionID: string(id) + ":1:capture-v1",
			Attempt:     1,
		},
	}
}

func appendCaptureObservations(
	ctx context.Context,
	tx *entities.StoreTx,
	current *contract.Operation,
	count int,
) error {
	for index := 0; index < count; index++ {
		if err := appendEvent(ctx, tx, current, contract.Event{
			Kind:    "warning",
			Stage:   platform.CandidateDiagnosticStage,
			Message: fmt.Sprintf("later capture observation %d", index),
			CandidateDiagnostic: &contract.CandidateDiagnostic{
				DeploymentID: "fixture",
				Status:       "captured",
				ContainerID:  fmt.Sprintf("%064x", index+1),
			},
		}); err != nil {
			return err
		}
	}
	return nil
}

func hasEvidenceSource(sources []contract.EvidenceSource, kind, status string) bool {
	for _, source := range sources {
		if source.Kind == kind && source.Status == status {
			return true
		}
	}
	return false
}

// workerCaptureTransport keeps every deployment effect on the existing
// scripted transport while the candidate read is the typed result seam above.
type workerCaptureTransport struct {
	*scriptedDeployTransport
	candidate     *candidateCaptureTransport
	failHealthFor contract.OperationID
}

func (t *workerCaptureTransport) Call(
	ctx context.Context,
	request node.Request,
	callback func(node.Event),
) (node.Response, error) {
	if strings.HasPrefix(request.Execution.Step, "platform-candidate-diagnostic-") {
		return t.candidate.Call(ctx, request, callback)
	}
	if request.Execution.OperationID == t.failHealthFor &&
		strings.HasPrefix(request.Execution.Step, "platform-health-") && request.Method == node.MethodRun {
		return node.Response{
			Status: engine.StatusFailed, ErrorCode: string(contract.Unavailable),
			Retryable: false, Detail: "candidate health check failed",
			ExecutionID: request.Execution.ExecutionID(),
		}, nil
	}
	return t.scriptedDeployTransport.Call(ctx, request, callback)
}

func assertWorkerRestoredServing(
	t *testing.T,
	fixture *deploymentRetryFixture,
	servingRelease platform.ReleaseID,
	servingDeployment platform.DeploymentID,
	servingRoutes []string,
) {
	t.Helper()
	env := fixture.environment(t)
	if env.ServingRelease != servingRelease || env.ServingDeploymentID != servingDeployment {
		t.Fatalf(
			"serving pointer changed: release=%s deployment=%s want release=%s deployment=%s",
			env.ServingRelease,
			env.ServingDeploymentID,
			servingRelease,
			servingDeployment,
		)
	}
	if got := routeNames(t, fixture); !reflect.DeepEqual(got, servingRoutes) {
		t.Fatalf("restored routes = %v, want %v", got, servingRoutes)
	}
}

func assertWorkerCandidateCleanup(
	t *testing.T,
	scripted *scriptedDeployTransport,
	candidate string,
) {
	t.Helper()
	scripted.mu.Lock()
	defer scripted.mu.Unlock()
	step := cleanupStepID(candidate)
	for key, runs := range scripted.runs {
		if strings.HasSuffix(key, "|"+step) {
			if runs != 1 {
				t.Fatalf("candidate cleanup step %q ran %d times: %v", step, runs, scripted.runs)
			}
			return
		}
	}
	t.Fatalf("candidate cleanup step %q did not run: %v", step, scripted.runs)
}

// TestIntegrationCandidateCaptureWorkerSafetyTail runs the actual platform
// worker attempt through candidate failure and cleanup. The candidate read
// consumes its bounded budget and returns no typed result; the same worker
// invocation must still persist the honest unavailable completion, restore the
// exact serving pointer/routes, release ownership and run the candidate cleanup
// step once.
func TestIntegrationCandidateCaptureWorkerSafetyTail(t *testing.T) {
	candidate := &candidateCaptureTransport{
		call: func(ctx context.Context, _ platform.CandidateDiagnosticPayload) (platform.CandidateDiagnosticResult, error) {
			<-ctx.Done()
			return platform.CandidateDiagnosticResult{}, ctx.Err()
		},
	}
	scripted := newScriptedDeployTransport()
	transport := &workerCaptureTransport{scriptedDeployTransport: scripted, candidate: candidate}
	fixture := newDeploymentRetryFixtureWithTransport(t, transport)
	fixture.deploy(t)
	servingRelease, servingDeployment := fixture.serving(t)
	servingRoutes := routeNames(t, fixture)
	const candidateName = "capture-worker-tail"
	op, _, env, _ := fixture.prepareOwnedCandidate(
		t,
		candidateName,
		"registry.example/live:tail",
		"running",
	)
	transport.failHealthFor = op.ID

	err := runPlatformAttempt(fixture.ctx, fixture.service, op.ID, 1)
	if err != nil {
		t.Fatalf("worker attempt: %v", err)
	}
	stored, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
	if err != nil || stored.State != contract.Failed {
		t.Fatalf("failed candidate state=%+v err=%v", stored, err)
	}
	if candidate.ReadCount() != 1 {
		t.Fatalf("candidate read count=%d, want one bounded read", candidate.ReadCount())
	}
	events := readCandidateCaptureEvents(t, fixture.pool, op.ID)
	await(t, 5*time.Second, func() bool {
		events = readCandidateCaptureEvents(t, fixture.pool, op.ID)
		return events.claims == 1 && events.summary != ""
	})
	if events.claims != 1 || events.summary != "unavailable" || events.observed != 0 || events.captured != 0 {
		t.Fatalf("worker capture after read timeout: %+v", events)
	}
	if owner, has := ownerForFixture(t, fixture, env); has {
		t.Fatalf("failed worker retained deployment owner: %+v", owner)
	}
	assertWorkerRestoredServing(t, fixture, servingRelease, servingDeployment, servingRoutes)
	assertWorkerCandidateCleanup(t, scripted, candidateName+"-candidate")
}

// TestIntegrationCandidateCaptureWorkerCompletionLockFailure exercises the same
// worker invocation with an unavailable completion transaction after a real,
// useful typed read. The claim must remain explicitly unconfirmed, no output
// may be stored, and the worker must still restore serving and run the exact
// candidate cleanup.
func TestIntegrationCandidateCaptureWorkerCompletionLockFailure(t *testing.T) {
	observedAt := time.Date(2026, 11, 14, 12, 0, 0, 0, time.UTC)
	ready := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	candidate := &candidateCaptureTransport{
		call: func(
			ctx context.Context,
			payload platform.CandidateDiagnosticPayload,
		) (platform.CandidateDiagnosticResult, error) {
			result := platform.CandidateDiagnosticResult{
				Observations:       candidateObservations(payload.Containers, observedAt),
				LogTimestampSource: "observation_time",
			}
			for index, container := range payload.Containers {
				result.Lines = append(
					result.Lines,
					candidateLine(container, index, observedAt, "worker output "+container.Name),
				)
			}
			once.Do(func() { close(ready) })
			select {
			case <-release:
				return result, nil
			case <-ctx.Done():
				return platform.CandidateDiagnosticResult{}, ctx.Err()
			}
		},
	}
	scripted := newScriptedDeployTransport()
	transport := &workerCaptureTransport{scriptedDeployTransport: scripted, candidate: candidate}
	fixture := newDeploymentRetryFixtureWithTransport(t, transport)
	fixture.deploy(t)
	servingRelease, servingDeployment := fixture.serving(t)
	servingRoutes := routeNames(t, fixture)
	const candidateName = "capture-worker-lock"
	op, _, env, _ := fixture.prepareOwnedCandidate(
		t,
		candidateName,
		"registry.example/live:lock",
		"running",
	)
	transport.failHealthFor = op.ID

	done := make(chan error, 1)
	go func() { done <- runPlatformAttempt(fixture.ctx, fixture.service, op.ID, 1) }()
	select {
	case <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("candidate node read did not complete")
	}
	if got := len(candidate.Payloads()); got != 1 {
		t.Fatalf("candidate read payloads = %d, want 1", got)
	}
	holder, err := fixture.pool.Begin(fixture.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := holder.Exec(fixture.ctx, `SELECT id FROM eregion_operations WHERE id=$1 FOR UPDATE`, op.ID); err != nil {
		t.Fatal(err)
	}
	close(release)
	waitCtx, cancelWait := context.WithTimeout(fixture.ctx, 5*time.Second)
	defer cancelWait()
	waitPointerLockWaiter(t, waitCtx, fixture.pool, "WHERE id = $1")
	time.AfterFunc(2500*time.Millisecond, func() { _ = holder.Rollback(context.Background()) })
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("worker attempt: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("worker did not return after completion lock failure")
	}
	events := readCandidateCaptureEvents(t, fixture.pool, op.ID)
	if events.claims != 1 || events.summary != "" || events.observed != 0 || events.captured != 0 {
		t.Fatalf("completion lock failure changed durable state: %+v", events)
	}
	stored, err := fixture.service.Get(fixture.ctx, testActor, op.ID)
	if err != nil || stored.State != contract.Failed {
		t.Fatalf("failed candidate state=%+v err=%v", stored, err)
	}
	if owner, has := ownerForFixture(t, fixture, env); has {
		t.Fatalf("failed worker retained deployment owner: %+v", owner)
	}
	assertWorkerRestoredServing(t, fixture, servingRelease, servingDeployment, servingRoutes)
	assertWorkerCandidateCleanup(t, scripted, candidateName+"-candidate")
}
