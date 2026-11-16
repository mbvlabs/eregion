package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/retention"
	"github.com/mbvlabs/eregion/internal/vault"
)

// prerequisiteAppEnvironment creates the real node/app/environment graph every
// definition and webhook fact test needs. It uses the typed foundation only.
func prerequisiteAppEnvironment(t *testing.T, store *entities.Store) (platform.AppID, platform.EnvironmentID, string, string) {
	t.Helper()
	ctx := context.Background()
	appID, err := platform.NewAppID()
	if err != nil {
		t.Fatal(err)
	}
	envID, err := platform.NewEnvironmentID()
	if err != nil {
		t.Fatal(err)
	}
	const appName = "prerequisite-app"
	const envName = "production"
	const nodeName = "prerequisite-node"
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		if _, err := tx.Nodes().InsertRegistration(ctx, nodeName, []string{platform.RoleApplication}, []string{platform.CapabilityDocker}, "127.0.0.1"); err != nil {
			return err
		}
		if _, err := tx.Platform().Apps().Insert(ctx, entities.AppInsert{
			ID: appID, Name: appName, ImageRepository: "registry.example/prerequisite",
		}); err != nil {
			return err
		}
		if _, err := tx.Platform().Environments().Insert(ctx, entities.EnvironmentInsert{
			ID: envID, AppID: appID, Name: envName, Node: nodeName,
			HostPort: 22001, CandidatePort: 32001, ContainerPort: 8080,
			Bind: "127.0.0.1", HealthType: "http", HealthPath: "/", HealthTimeoutSeconds: 5,
			Env: map[string]string{}, SecretRefs: map[string]string{},
		}); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return appID, envID, appName, envName
}

func prerequisiteOperation(t *testing.T, store *entities.Store, kind, target, key string, input json.RawMessage) contract.OperationID {
	t.Helper()
	id, err := contract.NewOperationID()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WithinTx(context.Background(), func(tx *entities.StoreTx) error {
		_, err := tx.Operations().Insert(context.Background(), entities.OperationInsert{
			ID: id,
			Key: entities.OperationKey{
				Kind: kind, Target: target, Actor: testActor, IdempotencyKey: key,
			},
			InputHash: "hash-" + key,
			Input:     input,
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

func prerequisiteRevision(t *testing.T, store *entities.Store, envID platform.EnvironmentID, sourceURL, secretName string) platform.RevisionID {
	t.Helper()
	revisionID, err := platform.NewEnvironmentRevisionID()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WithinTx(context.Background(), func(tx *entities.StoreTx) error {
		if _, err := tx.Platform().Revisions().Insert(context.Background(), entities.RevisionInsert{
			ID: revisionID, EnvironmentID: envID, Number: 1, Actor: testActor,
			Operation: "environment.created", Summary: "prerequisite revision",
			State: platform.RevisionState{
				Source:     platform.SourceBinding{URL: sourceURL, Ref: "refs/heads/main"},
				AutoDeploy: true, WebhookSecretName: secretName,
				Processes: []platform.ProcessSpec{}, Config: map[string]string{},
				SecretNames: []string{}, Resources: []string{},
			},
		}); err != nil {
			return err
		}
		_, err := tx.Platform().Environments().SetCurrentRevision(context.Background(), envID, revisionID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return revisionID
}

func TestIntegrationEntitiesDomainPrerequisitesPushWebhookBinding(t *testing.T) {
	store := newTestEntities(t, prerequisitePool(t))
	_, envID, appName, envName := prerequisiteAppEnvironment(t, store)
	const secretName = "webhook-secret"
	revisionID := prerequisiteRevision(t, store, envID, "https://github.com/acme/app.git", secretName)
	namespace := vault.Namespace{App: appName, Environment: envName, Name: secretName}
	if err := store.WithinTx(context.Background(), func(tx *entities.StoreTx) error {
		if err := tx.Secrets().InsertSecretNamespace(context.Background(), namespace, testActor); err != nil {
			return err
		}
		return tx.Secrets().InsertSecretRevision(context.Background(), namespace, vault.SecretRevisionInput{
			Revision: 1, KeyID: "key-prerequisite", Ciphertext: []byte{0x01, 0x02},
			CiphertextSHA256: "sha-prerequisite", RevisionHash: "revision-hash-prerequisite", Actor: testActor,
		})
	}); err != nil {
		t.Fatal(err)
	}

	binding, found, err := store.Platform().Outcomes().PushWebhookBinding(context.Background(), appName, envName)
	if err != nil || !found {
		t.Fatalf("push binding found=%v err=%v", found, err)
	}
	if binding.EnvironmentID != envID || binding.App != appName || binding.Environment != envName {
		t.Fatalf("push binding identity=%+v, want environment=%s app=%s env=%s", binding, envID, appName, envName)
	}
	if binding.CurrentRevisionID != revisionID || !binding.AutoDeploy || binding.WebhookSecretName != secretName {
		t.Fatalf("push binding revision=%+v, want revision=%s secret=%s", binding, revisionID, secretName)
	}
	var source platform.SourceBinding
	if err := json.Unmarshal(binding.Source, &source); err != nil {
		t.Fatal(err)
	}
	if source.URL != "https://github.com/acme/app.git" || source.Ref != "refs/heads/main" {
		t.Fatalf("push binding source=%+v", source)
	}
	if binding.SecretState != string(vault.StateActive) || binding.LatestRevision != 1 ||
		binding.Revision != 1 || binding.KeyID != "key-prerequisite" ||
		string(binding.Ciphertext) != string([]byte{0x01, 0x02}) ||
		binding.CiphertextSHA256 != "sha-prerequisite" ||
		binding.RevisionHash != "revision-hash-prerequisite" {
		t.Fatalf("push binding secret facts=%+v", binding)
	}
	if _, found, err := store.Platform().Outcomes().PushWebhookBinding(context.Background(), appName, "missing"); err != nil || found {
		t.Fatalf("missing push binding found=%v err=%v", found, err)
	}
}

func TestIntegrationEntitiesDomainPrerequisitesOperationTxProjections(t *testing.T) {
	store := newTestEntities(t, prerequisitePool(t))
	ctx := context.Background()
	op := prerequisiteOperation(t, store, "diagnostic.test", "control-plane", "tx-projections", json.RawMessage(`{"steps":1}`))
	through := uint64(0)
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		current, found, err := tx.Operations().Lock(ctx, op)
		if err != nil || !found {
			t.Fatalf("operation lock found=%v err=%v", found, err)
		}
		if err := tx.Operations().SetAttempt(ctx, op, 1); err != nil {
			return err
		}
		current.Attempt = 1
		append := func(event contract.Event) error {
			return tx.Operations().AppendEvent(ctx, &current, event)
		}
		if err := append(contract.Event{Kind: "log", Stage: "projection", Message: "older projection message"}); err != nil {
			return err
		}
		if err := append(contract.Event{Kind: "warning", Stage: "projection", Message: "newest projection message"}); err != nil {
			return err
		}
		if err := append(contract.Event{Kind: "step", Stage: "projection-result", Result: &contract.ActionResult{
			OperationID: op, Action: "diagnostic.test", Target: "control-plane", Attempt: 1,
			Status: contract.Failed, Final: true, History: contract.Cursor{OperationID: op},
		}}); err != nil {
			return err
		}
		if err := append(contract.Event{Kind: "step", Stage: "action-output", ActionOutputStatus: "available", ActionOutput: json.RawMessage(`{"ok":true}`)}); err != nil {
			return err
		}
		if err := append(contract.Event{Kind: "step", Stage: "deploy-step", Step: "deploy-step", NodeEvidence: &contract.NodeExecutionEvidence{
			ExecutionID: "exec-failed-prerequisite", Attempt: 1, Outcome: "failed", Stage: "deploy-step", OutputAvailability: "partial",
		}}); err != nil {
			return err
		}
		if err := append(contract.Event{Kind: "warning", Stage: "evidence", Message: "Node execution evidence could not be retained.", EvidenceLoss: &contract.NodeEvidenceLoss{
			ExecutionID: "exec-lost-prerequisite", Attempt: 1,
		}}); err != nil {
			return err
		}
		through = current.LastSequence

		started, found, err := tx.Operations().AttemptStartedAt(ctx, op, 1)
		if err != nil || !found || started.IsZero() {
			t.Fatalf("tx attempt started=%v found=%v err=%v", started, found, err)
		}
		sequence, found, err := tx.Operations().LastSequence(ctx, op)
		if err != nil || !found || sequence != through {
			t.Fatalf("tx last sequence=%d found=%v err=%v, want %d", sequence, found, err, through)
		}
		exists, err := tx.Operations().ResultExists(ctx, op, 1, contract.Failed, true)
		if err != nil || !exists {
			t.Fatalf("tx result exists=%v err=%v", exists, err)
		}
		if exists, err := tx.Operations().ResultExists(ctx, op, 1, contract.Succeeded, true); err != nil || exists {
			t.Fatalf("tx mismatched result exists=%v err=%v", exists, err)
		}
		output, status, found, err := tx.Operations().LatestActionOutput(ctx, op)
		if err != nil || !found || status != "available" || !json.Valid(output) {
			t.Fatalf("tx action output=%q status=%q found=%v err=%v", output, status, found, err)
		}
		if exists, err := tx.Operations().ActionOutputExists(ctx, op); err != nil || !exists {
			t.Fatalf("tx action output exists=%v err=%v", exists, err)
		}
		evidenceSequence, rawEvidence, found, err := tx.Operations().FailedNodeEvidence(ctx, op, through, 1, "deploy-step")
		if err != nil || !found || evidenceSequence == 0 || !strings.Contains(string(rawEvidence), "exec-failed-prerequisite") {
			t.Fatalf("tx failed node evidence sequence=%d raw=%s found=%v err=%v", evidenceSequence, rawEvidence, found, err)
		}
		if exists, err := tx.Operations().NodeEvidenceExists(ctx, op, "exec-failed-prerequisite", "failed"); err != nil || !exists {
			t.Fatalf("tx node evidence exists=%v err=%v", exists, err)
		}
		if lost, err := tx.Operations().EvidenceLost(ctx, op, through, 1); err != nil || !lost {
			t.Fatalf("tx evidence lost=%v err=%v", lost, err)
		}
		if message, err := tx.Operations().LatestEventMessageForStage(ctx, op, "projection"); err != nil || message != "newest projection message" {
			t.Fatalf("tx latest message=%q err=%v", message, err)
		}
		if exists, err := tx.Operations().EventStageExists(ctx, op, "projection"); err != nil || !exists {
			t.Fatalf("tx stage exists=%v err=%v", exists, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestIntegrationEntitiesDomainPrerequisitesOperationSavepointProjection(t *testing.T) {
	store := newTestEntities(t, prerequisitePool(t))
	ctx := context.Background()
	op := prerequisiteOperation(t, store, "diagnostic.test", "control-plane", "tx-savepoint-projections", json.RawMessage(`{"steps":1}`))
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		current, found, err := tx.Operations().Lock(ctx, op)
		if err != nil || !found {
			t.Fatalf("operation lock found=%v err=%v", found, err)
		}
		if err := tx.Operations().AppendEvent(ctx, &current, contract.Event{
			Kind: "warning", Stage: "outer-step", Message: "Node step failed: outer",
		}); err != nil {
			return err
		}
		if err := tx.Retention().InsertRun(ctx, entities.RetentionRunInsert{
			ID: "run_projection_outer", OperationID: op, Status: string(retention.RunRunning),
			PlannedItems: json.RawMessage(`[]`), RemovedItems: json.RawMessage(`[]`), Errors: json.RawMessage(`[]`),
			StartedAt: time.Now().UTC().Add(-time.Second), GCGeneration: 1,
		}); err != nil {
			return err
		}
		scopeErr := tx.WithinSavepoint(ctx, func(nested *entities.StoreTx) error {
			var nestedOp contract.Operation
			nestedOp, found, err := nested.Operations().Lock(ctx, op)
			if err != nil || !found {
				t.Fatalf("nested lock found=%v err=%v", found, err)
			}
			if err := nested.Operations().AppendEvent(ctx, &nestedOp, contract.Event{
				Kind: "warning", Stage: "nested-step", Message: "Node step failed: nested",
			}); err != nil {
				return err
			}
			if err := nested.Retention().InsertRun(ctx, entities.RetentionRunInsert{
				ID: "run_projection_nested", OperationID: op, Status: string(retention.RunRunning),
				PlannedItems: json.RawMessage(`[]`), RemovedItems: json.RawMessage(`[]`), Errors: json.RawMessage(`[]`),
				StartedAt: time.Now().UTC(), GCGeneration: 1,
			}); err != nil {
				return err
			}
			if stage, err := nested.Results().FailedStage(ctx, op, 0); err != nil || stage != "outer-step" {
				t.Fatalf("nested failed stage=%q err=%v", stage, err)
			}
			if run, found, err := nested.Retention().LatestRunForOperation(ctx, op); err != nil || !found || run.ID != "run_projection_nested" {
				t.Fatalf("nested retention run=%+v found=%v err=%v", run, found, err)
			}
			return errProjectionRollback
		})
		if scopeErr != errProjectionRollback {
			return scopeErr
		}
		if stage, err := tx.Results().FailedStage(ctx, op, 0); err != nil || stage != "outer-step" {
			t.Fatalf("post-savepoint stage=%q err=%v", stage, err)
		}
		if run, found, err := tx.Retention().LatestRunForOperation(ctx, op); err != nil || !found || run.ID != "run_projection_outer" {
			t.Fatalf("post-savepoint run=%+v found=%v err=%v", run, found, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestIntegrationEntitiesDomainPrerequisitesDefinitionJournal(t *testing.T) {
	store := newTestEntities(t, prerequisitePool(t))
	_, envID, appName, envName := prerequisiteAppEnvironment(t, store)
	parent := prerequisiteOperation(t, store, platform.KindDefinitionApply, "env/"+appName+"/"+envName, "definition-prerequisite-parent", json.RawMessage(`{}`))
	const stepKind = "resource.apply"
	target := "resource/" + appName + "/" + envName + "/db"
	input := json.RawMessage(`{"a":1,"b":2}`)
	child := prerequisiteOperation(t, store, stepKind, target, "definition-prerequisite-child", input)
	if err := store.WithinTx(context.Background(), func(tx *entities.StoreTx) error {
		if err := tx.DefinitionRuns().Insert(context.Background(), entities.DefinitionRunInsert{
			OperationID: parent, PlanNonce: "prerequisite-nonce", EnvironmentID: string(envID),
			Approved: json.RawMessage(`{"input":{}}`),
		}); err != nil {
			return err
		}
		return tx.DefinitionSteps().Insert(context.Background(), entities.DefinitionStepInsert{
			ParentOperationID: parent, Ordinal: 0, Kind: stepKind, Target: target, Input: input,
		})
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.WithinTx(context.Background(), func(tx *entities.StoreTx) error {
		ready, err := tx.DefinitionSteps().JournalReady(context.Background(), parent, 0, string(envID), stepKind, target, input)
		if err != nil || !ready {
			t.Fatalf("journal ready before link=%v err=%v", ready, err)
		}
		if linked, err := tx.DefinitionSteps().SetChild(context.Background(), parent, 0, child); err != nil || !linked {
			t.Fatalf("set child=%v err=%v", linked, err)
		}
		ready, err = tx.DefinitionSteps().JournalReady(context.Background(), parent, 0, string(envID), stepKind, target, input)
		if err != nil || ready {
			t.Fatalf("journal ready after link=%v err=%v", ready, err)
		}
		linked, found, err := tx.DefinitionSteps().LinkedChild(context.Background(), child, stepKind, target, json.RawMessage(`{"b":2,"a":1}`), testActor)
		if err != nil || !found || !linked.Matches || linked.State != contract.Queued || linked.ErrorCode != "" {
			t.Fatalf("linked child=%+v found=%v err=%v", linked, found, err)
		}
		if linked, found, err := tx.DefinitionSteps().LinkedChild(context.Background(), child, stepKind, target, input, contract.Actor{Kind: "ci", Subject: "wrong"}); err != nil || !found || linked.Matches {
			t.Fatalf("wrong actor linked child=%+v found=%v err=%v", linked, found, err)
		}
		dnsTarget, found, err := tx.DefinitionDNS().ChildTarget(context.Background(), child)
		if err != nil || !found || dnsTarget.App != appName || dnsTarget.Environment != envName ||
			dnsTarget.EnvironmentID != envID || dnsTarget.ParentID != parent || dnsTarget.Ordinal != 0 {
			t.Fatalf("dns child target=%+v found=%v err=%v", dnsTarget, found, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestIntegrationEntitiesDomainPrerequisitesDefinitionActiveOperations(t *testing.T) {
	store := newTestEntities(t, prerequisitePool(t))
	appID, envID, appName, envName := prerequisiteAppEnvironment(t, store)
	buildID, err := platform.NewBuildID()
	if err != nil {
		t.Fatal(err)
	}
	buildOp := prerequisiteOperation(t, store, platform.KindDeploy, "build/"+string(buildID), "definition-active-build", json.RawMessage(`{}`))
	if err := store.WithinTx(context.Background(), func(tx *entities.StoreTx) error {
		_, err := tx.Platform().Builds().Insert(context.Background(), entities.BuildInsert{
			ID: buildID, OperationID: buildOp, AppID: appID, EnvironmentID: envID,
			Source: json.RawMessage(`{}`), CommitSHA: strings.Repeat("a", 40),
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	appOp := prerequisiteOperation(t, store, platform.KindAppCreate, "app/"+appName, "definition-active-app", json.RawMessage(`{}`))
	envOp := prerequisiteOperation(t, store, platform.KindEnvironmentUpdate, "env/"+appName+"/"+envName, "definition-active-env", json.RawMessage(`{}`))
	resourceOp := prerequisiteOperation(t, store, platform.KindResourceApply, "resource/"+appName+"/"+envName+"/db", "definition-active-resource", json.RawMessage(`{}`))
	unrelated := prerequisiteOperation(t, store, "node.check", "node/other", "definition-active-unrelated", json.RawMessage(`{}`))

	active, err := store.Operations().DefinitionActiveOperations(context.Background(), appID, "app/"+appName, "env/"+appName+"/", "resource/"+appName+"/")
	if err != nil {
		t.Fatal(err)
	}
	seen := map[contract.OperationID]bool{}
	for _, operation := range active {
		seen[operation.ID] = true
	}
	for _, want := range []contract.OperationID{appOp, envOp, resourceOp, buildOp} {
		if !seen[want] {
			t.Fatalf("active definition operations missing %s in %+v", want, active)
		}
	}
	if seen[unrelated] {
		t.Fatalf("unrelated operation appeared in active definition set %+v", active)
	}
}

func TestIntegrationEntitiesDomainPrerequisitesNodeShareLock(t *testing.T) {
	pool := prerequisitePool(t)
	store := newTestEntities(t, pool)
	prerequisiteAppEnvironment(t, store)
	ctx := context.Background()
	firstReady := make(chan struct{})
	secondReady := make(chan struct{})
	release := make(chan struct{})
	firstDone := make(chan error, 1)
	secondDone := make(chan error, 1)
	go func() {
		firstDone <- store.WithinTx(ctx, func(tx *entities.StoreTx) error {
			if _, err := tx.Nodes().LockExistingNames(ctx, []string{"prerequisite-node"}); err != nil {
				return err
			}
			close(firstReady)
			<-release
			return nil
		})
	}()
	<-firstReady
	go func() {
		secondDone <- store.WithinTx(ctx, func(tx *entities.StoreTx) error {
			if _, err := tx.Nodes().LockExistingNames(ctx, []string{"prerequisite-node"}); err != nil {
				return err
			}
			close(secondReady)
			<-release
			return nil
		})
	}()
	<-secondReady

	blocked, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := blocked.Exec(ctx, `SET LOCAL lock_timeout = '250ms'`); err != nil {
		_ = blocked.Rollback(ctx)
		t.Fatal(err)
	}
	_, err = blocked.Exec(ctx, `UPDATE eregion_platform_nodes SET status='ready' WHERE name=$1`, "prerequisite-node")
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "55P03" {
		_ = blocked.Rollback(ctx)
		t.Fatalf("update under share locks error=%v, want SQLSTATE 55P03", err)
	}
	if err := blocked.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-secondDone; err != nil {
		t.Fatal(err)
	}
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		_, err := tx.Nodes().RecordCheckReady(ctx, "prerequisite-node")
		return err
	}); err != nil {
		t.Fatalf("node write after release: %v", err)
	}
}

func TestIntegrationEntitiesDomainPrerequisitesDomainAndEnvironmentFacts(t *testing.T) {
	store := newTestEntities(t, prerequisitePool(t))
	appID, envID, _, _ := prerequisiteAppEnvironment(t, store)
	revisionID := prerequisiteRevision(t, store, envID, "https://github.com/acme/app.git", "")
	domainID, err := platform.NewDomainID()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WithinTx(context.Background(), func(tx *entities.StoreTx) error {
		_, err := tx.Platform().Domains().Insert(context.Background(), entities.DomainInsert{
			ID: domainID, Domain: "app.example.com", EnvironmentID: envID, Primary: true,
			Addresses: []string{"192.0.2.10"}, CheckPath: "/health",
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	binding, found, err := store.Platform().Domains().BindingByNameAndEnvironment(context.Background(), "app.example.com", envID)
	if err != nil || !found || binding.ID != domainID || !binding.Primary {
		t.Fatalf("domain primary binding=%+v found=%v err=%v", binding, found, err)
	}
	secondID, err := platform.NewEnvironmentID()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.WithinTx(context.Background(), func(tx *entities.StoreTx) error {
		if _, err := tx.Platform().Environments().Insert(context.Background(), entities.EnvironmentInsert{
			ID: secondID, AppID: appID, Name: "staging", Node: "prerequisite-node",
			HostPort: 22002, CandidatePort: 32002, ContainerPort: 8080,
			Bind: "127.0.0.1", HealthType: "http", HealthPath: "/", HealthTimeoutSeconds: 5,
			Env: map[string]string{}, SecretRefs: map[string]string{},
		}); err != nil {
			return err
		}
		_, err := tx.Platform().Environments().SetCurrentRevision(context.Background(), secondID, revisionID)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	foreign, err := store.Platform().Environments().ReferencedByAnother(context.Background(), envID)
	if err != nil || !foreign {
		t.Fatalf("environment foreign reference=%v err=%v", foreign, err)
	}
}

func TestIntegrationEntitiesDomainPrerequisitesWorkspaceCatalogAndLock(t *testing.T) {
	unmigrated := newTestEntities(t, prerequisiteUnmigratedPool(t))
	if exists, err := unmigrated.Workspace().SchemaExists(context.Background()); err != nil || exists {
		t.Fatalf("unmigrated workspace schema exists=%v err=%v", exists, err)
	}
	store := newTestEntities(t, prerequisitePool(t))
	if exists, err := store.Workspace().SchemaExists(context.Background()); err != nil || !exists {
		t.Fatalf("migrated workspace schema exists=%v err=%v", exists, err)
	}
	ctx := context.Background()
	const lockKey int64 = 424242
	locked := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- store.WithinTx(ctx, func(tx *entities.StoreTx) error {
			if err := tx.Workspace().LockAssignment(ctx, lockKey); err != nil {
				return err
			}
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked
	blockedCtx, cancel := context.WithTimeout(ctx, 250*time.Millisecond)
	err := store.WithinTx(blockedCtx, func(tx *entities.StoreTx) error {
		return tx.Workspace().LockAssignment(blockedCtx, lockKey)
	})
	cancel()
	if err == nil {
		t.Fatalf("workspace assignment lock did not block")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := store.WithinTx(ctx, func(tx *entities.StoreTx) error {
		return tx.Workspace().LockAssignment(ctx, lockKey)
	}); err != nil {
		t.Fatalf("workspace assignment lock after release: %v", err)
	}
}

func prerequisitePool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, _ := testPool(t, true)
	return pool
}

func prerequisiteUnmigratedPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, _ := testPool(t, false)
	return pool
}
