package controlplane

import (
	"context"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/totp"
)

// TestIntegrationAppCreateMaterialisesProductionAndStaging atomically records
// both environments, their revisions and the protection defaults. The second
// attempt against the same app name must fail without adding a second set.
func TestIntegrationAppCreateMaterialisesProductionAndStaging(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 1)
	startWorkers(t, run)
	s := run.Service
	ctx := context.Background()
	platformSubmit(t, s, platform.KindNodeRegister, "platform", "protect-node", map[string]any{
		"name": "local", "roles": []string{platform.RoleApplication}, "capabilities": []string{platform.CapabilityDocker},
	})
	input := map[string]any{
		"name": "protected",
		"environments": []map[string]any{
			{"name": "production", "node": "local"},
			{"name": "staging", "node": "local"},
		},
	}
	op := platformSubmit(t, s, platform.KindAppCreate, platform.PlatformTarget().String(), "protected-create", input)
	platformAwait(t, s, op, contract.Succeeded)
	for _, name := range []string{"production", "staging"} {
		var protected bool
		var revisions int
		if err := pool.QueryRow(ctx, `SELECT e.protected,(SELECT count(*) FROM eregion_platform_environment_revisions r WHERE r.environment_id=e.id)
            FROM eregion_platform_environments e JOIN eregion_platform_apps a ON a.id=e.app_id
            WHERE a.name='protected' AND e.name=$1`, name).Scan(&protected, &revisions); err != nil {
			t.Fatal(err)
		}
		if !protected || revisions != 1 {
			t.Fatalf("%s protected=%t revisions=%d, want protected with one revision", name, protected, revisions)
		}
	}
	replayed, err := s.Submit(ctx, platformSubmission(platform.KindAppCreate, platform.PlatformTarget().String(), "protected-create", input))
	if err != nil || replayed.ID != op.ID {
		t.Fatalf("same-key replay = %v/%s, want original %s", err, replayed.ID, op.ID)
	}
	if _, err := s.Submit(ctx, platformSubmission(platform.KindAppCreate, platform.PlatformTarget().String(), "protected-create-2", input)); err == nil {
		t.Fatal("a second app create with the same name was accepted")
	}
	var environments int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM eregion_platform_environments e JOIN eregion_platform_apps a ON a.id=e.app_id WHERE a.name='protected'`).Scan(&environments); err != nil {
		t.Fatal(err)
	}
	if environments != 2 {
		t.Fatalf("environments = %d, want the original two", environments)
	}
	// A failure while recording the second environment must roll back the app
	// and the first environment too. The duplicate hostname is rejected by the
	// same transaction that inserts them.
	_, err = s.Submit(ctx, platformSubmission(platform.KindAppCreate, platform.PlatformTarget().String(), "protected-bad", map[string]any{
		"name": "protectedbad",
		"environments": []map[string]any{
			{"name": "production", "node": "local", "domain": "same.example.com"},
			{"name": "staging", "node": "local", "domain": "same.example.com"},
		},
	}))
	if err == nil {
		t.Fatal("duplicate domain across production and staging was accepted")
	}
	var apps, badEnvironments int
	if err := pool.QueryRow(ctx, `SELECT
        (SELECT count(*) FROM eregion_platform_apps WHERE name='protectedbad'),
        (SELECT count(*) FROM eregion_platform_environments e JOIN eregion_platform_apps a ON a.id=e.app_id WHERE a.name='protectedbad')`).Scan(&apps, &badEnvironments); err != nil {
		t.Fatal(err)
	}
	if apps != 0 || badEnvironments != 0 {
		t.Fatalf("failed app create left app=%d environments=%d", apps, badEnvironments)
	}
}

// TestIntegrationDeleteFactorIsDecidedServerSide proves the node transport is
// not the authorization boundary: direct Service.Submit calls without a code
// fail for an app delete, a protected environment, and the last environment,
// while an unprotected non-last environment needs only exact confirmation.
func TestIntegrationDeleteFactorIsDecidedServerSide(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	s := run.Service
	ctx := context.Background()
	platformSubmit(t, s, platform.KindNodeRegister, "platform", "factor-node", map[string]any{
		"name": "local", "roles": []string{platform.RoleApplication}, "capabilities": []string{platform.CapabilityDocker},
	})
	makeApp := func(name string, environments ...map[string]any) {
		t.Helper()
		platformSubmit(t, s, platform.KindAppCreate, platform.PlatformTarget().String(), name+"-create", map[string]any{
			"name": name, "environments": environments,
		})
	}
	env := func(name string, protected bool) map[string]any {
		return map[string]any{
			"name": name, "node": "local", "protected": protected,
		}
	}
	makeApp("factor", env("production", true), env("staging", true))
	makeApp("agent", env("production", false), env("staging", false))
	makeApp("last", env("pr-42", false))

	noFactor := func(kind, target, key string, input map[string]any) error {
		_, err := s.Submit(ctx, platformSubmission(kind, target, key, input))
		return err
	}
	if err := noFactor(platform.KindAppDelete, "app/factor", "factor-app-delete", map[string]any{"confirm": "factor"}); err == nil || contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("app delete without factor = %v, want precondition_failed", err)
	}
	if err := noFactor(platform.KindEnvironmentDelete, "env/factor/staging", "factor-protected-delete", map[string]any{"confirm": "factor/staging"}); err == nil || contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("protected environment delete without factor = %v, want precondition_failed", err)
	}
	if err := noFactor(platform.KindEnvironmentDelete, "env/last/pr-42", "factor-last-delete", map[string]any{"confirm": "last/pr-42"}); err == nil || contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("last environment delete without factor = %v, want precondition_failed", err)
	}
	if err := noFactor(platform.KindEnvironmentDelete, "env/agent/staging", "factor-agent-delete", map[string]any{"confirm": "agent/staging"}); err != nil {
		t.Fatalf("unprotected non-last environment delete was refused: %v", err)
	}
	// A sibling delete accepted before the first worker ran must not let the
	// second deletion look non-last and remove the app without the factor.
	if err := noFactor(platform.KindEnvironmentDelete, "env/agent/production", "factor-agent-delete-sibling", map[string]any{"confirm": "agent/production"}); err == nil || contract.Classify(err).Code != contract.Conflict {
		t.Fatalf("sibling environment delete = %v, want conflict", err)
	}

	run.Service.humanFactor = func(code string) error {
		if code != "123456" {
			return contract.NewFault(contract.PreconditionFailed, "invalid code", "Use the current code.")
		}
		return nil
	}
	withFactor := platformSubmission(platform.KindEnvironmentDelete, "env/factor/staging", "factor-protected-with-code", map[string]any{"confirm": "factor/staging"})
	withFactor.HumanFactor = "123456"
	if _, err := s.Submit(ctx, withFactor); err != nil {
		t.Fatalf("protected environment delete with the factor was refused: %v", err)
	}
}

func TestIntegrationUnprotectRequiresFactorAndProtectDoesNot(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	s := run.Service
	ctx := context.Background()
	platformSubmit(t, s, platform.KindNodeRegister, "platform", "unprotect-node", map[string]any{
		"name": "local", "roles": []string{platform.RoleApplication}, "capabilities": []string{platform.CapabilityDocker},
	})
	platformSubmit(t, s, platform.KindAppCreate, platform.PlatformTarget().String(), "unprotect-create", map[string]any{
		"name": "unprotect",
		"environments": []map[string]any{{
			"name": "production", "node": "local",
		}},
	})
	run.Service.humanFactor = func(code string) error {
		if code != "654321" {
			return contract.NewFault(contract.PreconditionFailed, "invalid code", "Use the current code.")
		}
		return nil
	}
	if _, err := s.Submit(ctx, platformSubmission(platform.KindEnvironmentProtect, "env/unprotect/production", "unprotect-no-factor", map[string]any{"protected": false})); err == nil || contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("unprotect without factor = %v, want precondition_failed", err)
	}
	withFactor := platformSubmission(platform.KindEnvironmentProtect, "env/unprotect/production", "unprotect-with-factor", map[string]any{"protected": false})
	withFactor.HumanFactor = "654321"
	if _, err := s.Submit(ctx, withFactor); err != nil {
		t.Fatalf("unprotect with the factor was refused: %v", err)
	}
	// Protecting again is agent-safe and needs no code.
	if _, err := s.Submit(ctx, platformSubmission(platform.KindEnvironmentProtect, "env/unprotect/production", "protect-again", map[string]any{"protected": true})); err != nil {
		t.Fatalf("protect was refused: %v", err)
	}
}

// TestIntegrationDirectDeleteWithRealTOTPCode exercises the direct service
// submission path with a generated local TOTP secret and a real current code.
// The verifier is local to the test; no live protect store or code is read.
func TestIntegrationDirectDeleteWithRealTOTPCode(t *testing.T) {
	pool, _ := testPool(t, true)
	run := testRuntime(t, pool, 0)
	s := run.Service
	ctx := context.Background()
	platformSubmit(t, s, platform.KindNodeRegister, "platform", "real-totp-node", map[string]any{
		"name": "local", "roles": []string{platform.RoleApplication}, "capabilities": []string{platform.CapabilityDocker},
	})
	platformSubmit(t, s, platform.KindAppCreate, platform.PlatformTarget().String(), "real-totp-create", map[string]any{
		"name": "realtotp",
		"environments": []map[string]any{
			{"name": "production", "node": "local"},
			{"name": "staging", "node": "local"},
		},
	})

	secret, err := totp.NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	key, err := totp.DecodeSecret(secret)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	var last int64
	s.humanFactor = func(code string) error {
		step, err := totp.Verify(key, code, now, last)
		if err != nil {
			return contract.NewFault(contract.PreconditionFailed, "invalid local TOTP code", "Use the current code.")
		}
		last = step
		return nil
	}
	code := totp.Code(key, now)

	delete := platformSubmission(platform.KindEnvironmentDelete, "env/realtotp/production", "real-totp-delete", map[string]any{"confirm": "realtotp/production"})
	delete.HumanFactor = code
	op, err := s.Submit(ctx, delete)
	if err != nil {
		t.Fatalf("real TOTP delete was refused: %v", err)
	}
	if op.State != contract.Queued {
		t.Fatalf("delete state = %s, want queued with workers stopped", op.State)
	}
	// The same TOTP step must not authorize a second deletion.
	replay := platformSubmission(platform.KindEnvironmentDelete, "env/realtotp/staging", "real-totp-replay", map[string]any{"confirm": "realtotp/staging"})
	replay.HumanFactor = code
	if _, err := s.Submit(ctx, replay); err == nil || contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("reused local TOTP = %v, want precondition_failed", err)
	}
}
