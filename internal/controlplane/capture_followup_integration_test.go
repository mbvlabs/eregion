package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/secrets"
	"github.com/mbvlabs/eregion/internal/vault"
)

// The shim supplies only test-owned Docker observations; the real application
// service, PostgreSQL, vault and node dispatcher execute the capture path.
func TestIntegrationCandidateCapturePinnedRotationAndClaims(t *testing.T) {
	t.Run("pinned rotation", func(t *testing.T) {
		checkCandidateCapture(t, "old-sensitive-value", "before\nold-sensitive-value\nafter\n", true, false, false)
	})
	t.Run("multiline tail fragment withheld", func(t *testing.T) {
		checkCandidateCapture(t, "old-line-one\nold-line-two", "old-line-two\nafter\n", false, false, false)
	})
	t.Run("missing pin retains metadata", func(t *testing.T) {
		checkCandidateCapture(t, "old-sensitive-value", "old-sensitive-value\n", false, true, false)
	})
	t.Run("created is not exited", func(t *testing.T) {
		checkCandidateCapture(t, "old-sensitive-value", "old-sensitive-value\n", true, false, true)
	})
}

func TestIntegrationNodePrivateMachineOutputAndLiveEvents(t *testing.T) {
	f := newPointerBuildFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if _, err := f.pool.Exec(ctx, `UPDATE eregion_operations SET state='running',attempt=1 WHERE id=$1`, f.op.ID); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	script := `#!/bin/sh
case "$1" in
  inspect) echo "no such container" >&2; exit 1;;
  rm) exit 0;;
  run) printf '%064d\n' 0 | tr 0 a;;
  port) echo "127.0.0.1:12345";;
  version) echo "safe progress"; while [ ! -f "$EVIDENCE_FIXTURE/unblock" ]; do sleep 0.02; done;;
  *) exit 1;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("EVIDENCE_FIXTURE", dir)
	dispatcher, err := node.NewDispatcher(t.TempDir(), "local", &secrets.Registry{})
	if err != nil {
		t.Fatal(err)
	}
	step := "platform-container-machine-v1"
	execution := engine.Execution{OperationID: f.op.ID, Attempt: 1, Step: step}
	image := "sha256:" + strings.Repeat("a", 64)
	payload := platform.ContainerPayload{Step: step, Name: "evidence-container", Image: image, Publish: true, ContainerPort: 8080, SecretEnvKeys: []string{"SHORT"}}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	response, err := dispatcher.Call(ctx, node.Request{Method: node.MethodRun, Execution: execution, Payload: raw, Secrets: map[string]string{"SHORT": "a"}}, nil)
	var result struct {
		Image    string `json:"image"`
		HostPort int    `json:"host_port"`
	}
	if err != nil || response.Status != engine.StatusCompleted || json.Unmarshal(response.Result, &result) != nil || result.Image != image || result.HostPort != 12345 {
		t.Fatalf("machine result=%+v response=%+v err=%v", result, response, err)
	}
	step = "platform-argv-live-v1"
	execution = engine.Execution{OperationID: f.op.ID, Attempt: 1, Step: step}
	raw, _ = json.Marshal(platform.ArgvPayload{Step: step, Argv: []string{"docker", "version"}})
	var callbackErr error
	response, err = dispatcher.Call(ctx, node.Request{Method: node.MethodRun, Execution: execution, Payload: raw, Secrets: map[string]string{"SHORT": "stdout"}}, func(event node.Event) {
		if strings.Contains(event.Message, "safe progress") {
			callbackErr = f.service.appendNodeEvents(ctx, f.op.ID, 1, []node.Event{event})
			if callbackErr == nil {
				callbackErr = os.WriteFile(filepath.Join(dir, "unblock"), []byte("ok"), 0600)
			}
		}
	})
	var stdout map[string]any
	if err != nil || callbackErr != nil || response.Status != engine.StatusCompleted || json.Unmarshal(response.Result, &stdout) != nil || stdout["stdout"] == nil {
		t.Fatalf("live result=%s response=%+v err=%v callback=%v", response.Result, response, err, callbackErr)
	}
	var persisted bool
	if err := f.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM eregion_operation_events WHERE operation_id=$1 AND event->>'message' LIKE '%safe progress%')`, f.op.ID).Scan(&persisted); err != nil || !persisted {
		t.Fatalf("live event persisted=%v err=%v", persisted, err)
	}
	if err := os.Remove(filepath.Join(dir, "unblock")); err != nil {
		t.Fatal(err)
	}
	step = "platform-argv-liveplain-v1"
	execution = engine.Execution{OperationID: f.op.ID, Attempt: 1, Step: step}
	raw, _ = json.Marshal(platform.ArgvPayload{Step: step, Argv: []string{"docker", "version"}})
	callbackErr = nil
	response, err = dispatcher.Call(ctx, node.Request{Method: node.MethodRun, Execution: execution, Payload: raw}, func(event node.Event) {
		if strings.Contains(event.Message, "safe progress") {
			callbackErr = f.service.appendNodeEvents(ctx, f.op.ID, 1, []node.Event{event})
			if callbackErr == nil {
				callbackErr = os.WriteFile(filepath.Join(dir, "unblock"), []byte("ok"), 0600)
			}
		}
	})
	if err != nil || callbackErr != nil || response.Status != engine.StatusCompleted {
		t.Fatalf("no-secret live response=%+v err=%v callback=%v", response, err, callbackErr)
	}
}

func TestIntegrationNodeWholeStreamRedactionBeforePersistence(t *testing.T) {
	for _, overflow := range []bool{false, true} {
		t.Run(fmt.Sprintf("overflow=%t", overflow), func(t *testing.T) {
			f := newPointerBuildFixture(t)
			ctx := context.Background()
			if _, err := f.pool.Exec(ctx, `UPDATE eregion_operations SET state='running',attempt=1 WHERE id=$1`, f.op.ID); err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			text := "useful\nmultiline\nsecret\nprefix\x1bsuffix\n"
			if overflow {
				text = strings.Repeat("z", 1024*1024-3) + "prefix\x1bsuffix\n" + strings.Repeat("z", 1024*1024+1024)
			}
			if err := os.WriteFile(filepath.Join(dir, "raw"), []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\ncat \"$EVIDENCE_FIXTURE/raw\"\nexit 1\n"), 0700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
			t.Setenv("EVIDENCE_FIXTURE", dir)
			stateDir := t.TempDir()
			dispatcher, err := node.NewDispatcher(stateDir, "local", &secrets.Registry{})
			if err != nil {
				t.Fatal(err)
			}
			step := "platform-argv-wholestream-v1"
			execution := engine.Execution{OperationID: f.op.ID, Attempt: 1, Step: step}
			raw, _ := json.Marshal(platform.ArgvPayload{Step: step, Argv: []string{"docker", "version"}})
			var callbackErr error
			response, err := dispatcher.Call(ctx, node.Request{Method: node.MethodRun, Execution: execution, Payload: raw, Secrets: map[string]string{"MULTI": "multiline\nsecret", "CONTROL": `prefix\u001bsuffix`}}, func(event node.Event) {
				if callbackErr == nil {
					callbackErr = f.service.appendNodeEvents(ctx, f.op.ID, 1, []node.Event{event})
				}
			})
			if err != nil || callbackErr != nil || response.Status != engine.StatusFailed {
				t.Fatalf("response=%+v err=%v callback=%v", response, err, callbackErr)
			}
			var result platform.StepExecutionEvidence
			if err := json.Unmarshal(response.Result, &result); err != nil {
				t.Fatal(err)
			}
			if overflow && !result.StdoutTruncated {
				t.Fatal("overflow not reported")
			}
			journal, err := os.ReadFile(engine.JournalPath(stateDir))
			if err != nil {
				t.Fatal(err)
			}
			var events string
			if err := f.pool.QueryRow(ctx, `SELECT coalesce(string_agg(event::text,E'\n'),'') FROM eregion_operation_events WHERE operation_id=$1`, f.op.ID).Scan(&events); err != nil {
				t.Fatal(err)
			}
			for _, rendered := range []string{result.Stdout, string(journal), events} {
				if strings.Contains(rendered, "multiline") || strings.Contains(rendered, `prefix\u001bsuffix`) || strings.Contains(rendered, "prefix\x1bsuffix") {
					t.Fatalf("secret appeared in persisted evidence: %s", rendered)
				}
			}
			if !overflow && !strings.Contains(result.Stdout, "[REDACTED]") {
				t.Fatalf("useful output not redacted: %+v", result)
			}
		})
	}
}

func TestIntegrationReleaseCommandKeepsPrivateDockerID(t *testing.T) {
	f := newPointerBuildFixture(t)
	ctx := context.Background()
	dir := t.TempDir()
	id := strings.Repeat("a", 64)
	labels := platform.ReleaseCommandLabels(f.env.App, f.env.Name, f.release, f.dep)
	encodedLabels, _ := json.Marshal(labels)
	state := `{"Status":"exited","Running":false,"StartedAt":"2026-11-14T12:00:00Z","FinishedAt":"2026-11-14T12:00:01Z","ExitCode":0,"OOMKilled":false}`
	inspect := fmt.Sprintf("%q\n%s\n%s\n", id, encodedLabels, state)
	if err := os.WriteFile(filepath.Join(dir, "inspect"), []byte(inspect), 0600); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
case "$1" in
  inspect) if [ "$2" = "--type=image" ]; then echo '<no value>'; else cat "$EVIDENCE_FIXTURE/inspect"; fi;;
  create) printf '%064d\n' 0 | tr 0 a;;
  start) echo "command completed";;
  logs) echo "command completed";;
  rm) printf '%s\n' "$3" > "$EVIDENCE_FIXTURE/removed";;
  *) exit 1;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("EVIDENCE_FIXTURE", dir)
	dispatcher, err := node.NewDispatcher(t.TempDir(), "local", &secrets.Registry{})
	if err != nil {
		t.Fatal(err)
	}
	step := "platform-command-01-v1"
	execution := engine.Execution{OperationID: f.op.ID, Attempt: 1, Step: step}
	payload := platform.ReleaseCommandPayload{Step: step, Name: "release-command-test", Image: "sha256:" + strings.Repeat("b", 64), Network: "release-network-test", Command: []string{"/bin/true"}, Labels: labels, SecretEnvKeys: []string{"SHORT"}}
	raw, _ := json.Marshal(payload)
	response, err := dispatcher.Call(ctx, node.Request{Method: node.MethodRun, Execution: execution, Payload: raw, Secrets: map[string]string{"SHORT": "a"}}, nil)
	var result platform.StepExecutionEvidence
	if err != nil || response.Status != engine.StatusCompleted || json.Unmarshal(response.Result, &result) != nil || result.Container == nil || result.Container.ContainerID != id {
		t.Fatalf("response=%+v result=%+v err=%v", response, result, err)
	}
	removed, err := os.ReadFile(filepath.Join(dir, "removed"))
	if err != nil || strings.TrimSpace(string(removed)) != id {
		t.Fatalf("immutable cleanup ID=%q err=%v", removed, err)
	}
}

func TestIntegrationContainerEvidencePrecedesReplacementAndPortCleanup(t *testing.T) {
	f := newPointerBuildFixture(t)
	ctx := context.Background()
	plan, err := platform.PlanDeployment(f.env.App, f.env.Name, f.env.Revision.RevisionState, f.release, f.dep)
	if err != nil || len(plan) != 1 {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	item := plan[0]
	dir := t.TempDir()
	id := strings.Repeat("a", 64)
	labels, _ := json.Marshal(item.Labels)
	state := `{"Status":"exited","Running":false,"StartedAt":"2026-11-14T12:00:00Z","FinishedAt":"2026-11-14T12:00:01Z","ExitCode":7,"OOMKilled":true}`
	if err := os.WriteFile(filepath.Join(dir, "inspect"), []byte(fmt.Sprintf("%q\n%s\n%s\n", id, labels, state)), 0600); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
printf '%s\n' "$1" >> "$EVIDENCE_FIXTURE/trace"
case "$1" in
  inspect) case "$4" in *State.Running*) echo 'false|old-image|old-hash';; *) cat "$EVIDENCE_FIXTURE/inspect";; esac;;
  logs) echo 'original failure output';;
  rm) exit 0;;
  run) printf '%064d\n' 0 | tr 0 a;;
  port) echo 'port unavailable' >&2; exit 1;;
  *) exit 1;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("EVIDENCE_FIXTURE", dir)
	dispatcher, err := node.NewDispatcher(t.TempDir(), "local", &secrets.Registry{})
	if err != nil {
		t.Fatal(err)
	}
	step := "platform-container-porttest-v1"
	execution := engine.Execution{OperationID: f.op.ID, Attempt: 1, Step: step}
	payload := platform.ContainerPayload{Step: step, Name: item.Name, Image: "sha256:" + strings.Repeat("b", 64), Labels: item.Labels, Publish: true, ContainerPort: 8080, SecretEnvKeys: []string{"SHORT"}}
	raw, _ := json.Marshal(payload)
	response, err := dispatcher.Call(ctx, node.Request{Method: node.MethodRun, Execution: execution, Payload: raw, Secrets: map[string]string{"SHORT": "a"}}, nil)
	var result platform.StepExecutionEvidence
	if err != nil || response.Status != engine.StatusFailed || json.Unmarshal(response.Result, &result) != nil || result.Stage != "container-port" || result.Previous == nil || result.Container == nil || result.Previous.ContainerID != id || result.Container.ExitCode == nil || *result.Container.ExitCode != 7 || !result.Container.OOMKilled {
		t.Fatalf("response=%+v result=%+v err=%v", response, result, err)
	}
	trace, err := os.ReadFile(filepath.Join(dir, "trace"))
	if err != nil {
		t.Fatal(err)
	}
	sequence := string(trace)
	firstLogs, firstRemove := strings.Index(sequence, "logs\n"), strings.Index(sequence, "rm\n")
	port, lastLogs, lastRemove := strings.Index(sequence, "port\n"), strings.LastIndex(sequence, "logs\n"), strings.LastIndex(sequence, "rm\n")
	if firstLogs < 0 || firstLogs > firstRemove || port < firstRemove || lastLogs < port || lastRemove < lastLogs {
		t.Fatalf("capture/cleanup sequence: %s", sequence)
	}
}

func TestIntegrationReleaseTimeoutCapturesBeforeCleanup(t *testing.T) {
	t.Run("timeout", func(t *testing.T) { checkReleaseInterruptedCapture(t, false) })
	t.Run("cancellation", func(t *testing.T) { checkReleaseInterruptedCapture(t, true) })
}

func checkReleaseInterruptedCapture(t *testing.T, cancelled bool) {
	t.Helper()
	f := newPointerBuildFixture(t)
	ctx := context.Background()
	if cancelled {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, time.Second)
		defer cancel()
	}
	dir := t.TempDir()
	id := strings.Repeat("b", 64)
	labels, _ := json.Marshal(platform.ReleaseCommandLabels(f.env.App, f.env.Name, f.release, f.dep))
	state := `{"Status":"running","Running":true,"StartedAt":"2026-11-14T12:00:00Z","FinishedAt":"0001-01-01T00:00:00Z","ExitCode":0,"OOMKilled":false}`
	if err := os.WriteFile(filepath.Join(dir, "inspect"), []byte(fmt.Sprintf("%q\n%s\n%s\n", id, labels, state)), 0600); err != nil {
		t.Fatal(err)
	}
	script := `#!/bin/sh
printf '%s\n' "$1" >> "$EVIDENCE_FIXTURE/trace"
case "$1" in
  inspect) if [ "$2" = "--type=image" ]; then echo '<no value>'; else cat "$EVIDENCE_FIXTURE/inspect"; fi;;
  create) printf '%064d\n' 0 | tr 0 b;;
  start) exec sleep 10;;
  logs) echo 'timed out command output';;
  rm) printf '%s\n' "$3" > "$EVIDENCE_FIXTURE/removed";;
  *) exit 1;;
esac
`
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("EVIDENCE_FIXTURE", dir)
	dispatcher, err := node.NewDispatcher(t.TempDir(), "local", &secrets.Registry{})
	if err != nil {
		t.Fatal(err)
	}
	step := "platform-command-01-v1"
	execution := engine.Execution{OperationID: f.op.ID, Attempt: 1, Step: step}
	payload := platform.ReleaseCommandPayload{Step: step, Name: "release-command-test", Image: "sha256:" + strings.Repeat("b", 64), Network: "release-network-test", Command: []string{"/bin/true"}, Labels: platform.ReleaseCommandLabels(f.env.App, f.env.Name, f.release, f.dep), TimeoutSeconds: 1}
	if cancelled {
		payload.TimeoutSeconds = 30
	}
	raw, _ := json.Marshal(payload)
	response, err := dispatcher.Call(ctx, node.Request{Method: node.MethodRun, Execution: execution, Payload: raw}, nil)
	var result platform.StepExecutionEvidence
	if err != nil || response.Status != engine.StatusFailed || json.Unmarshal(response.Result, &result) != nil || result.Container == nil || !result.Container.Running || result.Container.ExitCode != nil || !strings.Contains(result.Container.Logs, "timed out") {
		t.Fatalf("response=%+v result=%+v err=%v", response, result, err)
	}
	trace, err := os.ReadFile(filepath.Join(dir, "trace"))
	if err != nil {
		t.Fatal(err)
	}
	logs, remove := strings.Index(string(trace), "logs\n"), strings.LastIndex(string(trace), "rm\n")
	if logs < 0 || remove < logs {
		t.Fatalf("capture must precede cleanup: %s", trace)
	}
	removed, err := os.ReadFile(filepath.Join(dir, "removed"))
	if err != nil || strings.TrimSpace(string(removed)) != id {
		t.Fatalf("cleanup ID=%q err=%v", removed, err)
	}
}

func TestIntegrationImageDeploymentCaptureDoesNotRequireGitCredential(t *testing.T) {
	f := newPointerBuildFixture(t)
	ctx := context.Background()
	if err := f.service.markBuildSucceeded(ctx, f.build, f.release, "registry.example/shop@sha256:"+strings.Repeat("b", 64), submittedGitCommit, "sha256:"+strings.Repeat("c", 64)); err != nil {
		t.Fatal(err)
	}
	platformSubmit(t, f.service, platform.KindEnvironmentUpdate, "env/shop/production", "credential-recipe", map[string]any{"source": map[string]any{"url": "https://github.com/acme/app.git", "ref": "main", "builder": "paketobuildpacks/builder-jammy-base", "credential_secret_name": "GIT_READ"}})
	op := platformSubmit(t, f.service, platform.KindDeploy, "env/shop/production", "capture-ready-image", map[string]any{"release_id": f.release})
	env, err := f.service.environment(ctx, "shop", "production")
	if err != nil {
		t.Fatal(err)
	}
	dep, ok, err := f.service.deploymentByOperation(ctx, op.ID)
	if err != nil || !ok {
		t.Fatalf("deployment err=%v", err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE eregion_operations SET state='running',attempt=1 WHERE id=$1`, op.ID); err != nil {
		t.Fatal(err)
	}
	insertDeploymentOwner(t, f.pool, env, deploymentOwner{EnvironmentID: env.ID, OperationID: op.ID, DeploymentID: dep.ID, Attempt: 1, Phase: deploymentOwnerPrepared})
	plan, err := platform.PlanDeployment(env.App, env.Name, env.Revision.RevisionState, dep.ReleaseID, dep.ID)
	if err != nil || len(plan) != 1 {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	dir := t.TempDir()
	values := []any{strings.Repeat("a", 64), "/" + plan[0].Name, "sha256:" + strings.Repeat("c", 64), plan[0].Labels, map[string]any{"Status": "created", "Running": false, "StartedAt": "0001-01-01T00:00:00Z", "FinishedAt": "0001-01-01T00:00:00Z", "ExitCode": 0, "OOMKilled": false}}
	var lines []string
	for _, value := range values {
		raw, _ := json.Marshal(value)
		lines = append(lines, string(raw))
	}
	if err := os.WriteFile(filepath.Join(dir, "inspect"), []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\ncase \"$1\" in inspect) cat \"$EVIDENCE_FIXTURE/inspect\";; logs) echo image-evidence;; *) exit 1;; esac\n"
	if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+":"+os.Getenv("PATH"))
	t.Setenv("EVIDENCE_FIXTURE", dir)
	local, err := node.NewLocal(t.TempDir(), "local", &secrets.Registry{})
	if err != nil {
		t.Fatal(err)
	}
	f.service.resolver = newTestNodeResolver(t, f.pool, map[string]node.Transport{"local": local})
	f.service.captureCandidateDiagnostics(ctx, env, dep, 1)
	var observed, logged bool
	if err := f.pool.QueryRow(ctx, `SELECT bool_or(event->'candidate_diagnostic'->>'status'='observed'),bool_or(event->>'message' LIKE '%image-evidence%') FROM eregion_operation_events WHERE operation_id=$1 AND event->>'stage'=$2`, op.ID, platform.CandidateDiagnosticStage).Scan(&observed, &logged); err != nil || !observed || !logged {
		t.Fatalf("observed=%v logged=%v err=%v", observed, logged, err)
	}
}

func checkCandidateCapture(t *testing.T, old, logs string, wantLogs, missingPin, neverStarted bool) {
	t.Helper()
	s, pool := gitDeployFixture(t)
	ctx := context.Background()
	ring, err := vault.GenerateKeyRing()
	if err != nil {
		t.Fatal(err)
	}
	store, err := vault.NewService(pool, ring)
	if err != nil {
		t.Fatal(err)
	}
	s.secrets = vault.NewEnvironmentResolver(store)
	ns, err := vault.NewNamespace("shop", "production", "PASSWORD")
	if err != nil {
		t.Fatal(err)
	}
	meta, err := store.Set(ctx, ns, []byte(old), 0, testActor)
	if err != nil {
		t.Fatal(err)
	}
	platformSubmit(t, s, platform.KindEnvironmentUpdate, "env/shop/production", "capture-secret-revision", map[string]any{"secret_names": []string{"PASSWORD"}})
	s.resolveGitRef = func(context.Context, string, string, []byte) (string, error) { return submittedGitCommit, nil }
	op := platformSubmit(t, s, platform.KindDeploy, "env/shop/production", "capture-pinned", map[string]any{})
	env, err := s.environment(ctx, "shop", "production")
	if err != nil {
		t.Fatal(err)
	}
	dep, ok, err := s.deploymentByOperation(ctx, op.ID)
	if err != nil || !ok {
		t.Fatalf("deployment: %v", err)
	}
	ref := meta.Revision
	pins := map[string]platform.SecretRevisionRef{"PASSWORD": {Revision: ref.Revision, Hash: ref.Hash, KeyID: ref.KeyID, CreatedAt: ref.CreatedAt}}
	if !missingPin {
		if err := s.recordDeploymentSecretSnapshot(ctx, op.ID, env.RevisionID, pins); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Set(ctx, ns, []byte("new-rotated-value"), meta.LatestRevision, testActor); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE eregion_operations SET state='running',attempt=1 WHERE id=$1`, op.ID); err != nil {
		t.Fatal(err)
	}
	insertDeploymentOwner(t, pool, env, deploymentOwner{EnvironmentID: env.ID, OperationID: op.ID, DeploymentID: dep.ID, Attempt: 1, Phase: deploymentOwnerPrepared})

	plan, err := platform.PlanDeployment(env.App, env.Name, env.Revision.RevisionState, dep.ReleaseID, dep.ID)
	if err != nil || len(plan) != 1 {
		t.Fatalf("plan: %+v %v", plan, err)
	}
	item := plan[0]
	tmp := t.TempDir()
	state := map[string]any{"Status": "exited", "Running": false, "StartedAt": "2026-11-14T12:00:00Z", "FinishedAt": "2026-11-14T12:00:01Z", "ExitCode": 3, "OOMKilled": false}
	if neverStarted {
		state["Status"], state["StartedAt"], state["FinishedAt"], state["ExitCode"] = "created", "0001-01-01T00:00:00Z", "0001-01-01T00:00:00Z", 0
	}
	metadata := []any{strings.Repeat("a", 64), "/" + item.Name, "sha256:" + strings.Repeat("b", 64), item.Labels, state}
	var lines []string
	for _, value := range metadata {
		b, _ := json.Marshal(value)
		lines = append(lines, string(b))
	}
	if err := os.WriteFile(filepath.Join(tmp, "inspect"), []byte(strings.Join(lines, "\n")+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "logs"), []byte(logs), 0600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\ncase \"$1\" in inspect) cat \"$EVIDENCE_FIXTURE/inspect\";; logs) cat \"$EVIDENCE_FIXTURE/logs\";; *) exit 1;; esac\n"
	if err := os.WriteFile(filepath.Join(tmp, "docker"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", tmp+":"+os.Getenv("PATH"))
	t.Setenv("EVIDENCE_FIXTURE", tmp)
	local, err := node.NewLocal(t.TempDir(), "local", &secrets.Registry{})
	if err != nil {
		t.Fatal(err)
	}
	s.resolver = newTestNodeResolver(t, pool, map[string]node.Transport{"local": local})
	s.captureCandidateDiagnostics(ctx, env, dep, 1)
	s.captureCandidateDiagnostics(ctx, env, dep, 1)
	var claims, observations int
	var events string
	if err := pool.QueryRow(ctx, `SELECT count(*) FILTER (WHERE event->'candidate_diagnostic'->>'status'='unconfirmed'), count(*) FILTER (WHERE event->'candidate_diagnostic'->>'status'='observed'), string_agg(event::text, E'\n') FROM eregion_operation_events WHERE operation_id=$1 AND event->>'stage'=$2`, op.ID, platform.CandidateDiagnosticStage).Scan(&claims, &observations, &events); err != nil {
		t.Fatal(err)
	}
	if claims != 1 || observations != 1 || strings.Contains(events, old) || strings.Contains(events, "old-line-two") || (neverStarted && strings.Contains(events, `"exit_code"`)) || (!neverStarted && !strings.Contains(events, `"exit_code": 3`)) || (wantLogs && !strings.Contains(events, "[REDACTED]")) || (!wantLogs && !strings.Contains(events, `"status": "partial"`)) {
		t.Fatalf("claims=%d observations=%d events=%s", claims, observations, events)
	}
	// A newly recorded container execution in the same attempt produces a
	// distinct capture claim, rather than suppressing the later observation.
	step := "platform-container-evidence-v1"
	execution := fmt.Sprintf("%s:1:%s", op.ID, step)
	if _, err := pool.Exec(ctx, `INSERT INTO eregion_operation_steps(operation_id,step,inputs_hash,state,attempt,execution_id,started_at,error,error_code) VALUES($1,$2,'test','failed',1,$3,clock_timestamp(),'','')`, op.ID, step, execution); err != nil {
		t.Fatal(err)
	}
	s.captureCandidateDiagnostics(ctx, env, dep, 1)
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_operation_events WHERE operation_id=$1 AND event->'candidate_diagnostic'->>'status'='unconfirmed'`, op.ID).Scan(&claims); err != nil || claims != 2 {
		t.Fatalf("distinct execution claims=%d err=%v", claims, err)
	}
}
