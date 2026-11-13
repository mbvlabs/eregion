package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/appdef"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/integrations"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/secrets"
)

type definitionCLIBackend struct {
	*ndjsonFollowBackend
	planInput      api.DefinitionPlanInput
	applyInput     api.DefinitionApplyInput
	plans, applies int
	blocked        bool
	requireConfirm bool
	confirmTarget  string
}

func (b *definitionCLIBackend) PlanDefinition(_ context.Context, in api.DefinitionPlanInput) (api.DefinitionPlan, error) {
	b.plans++
	b.planInput = in
	return api.DefinitionPlan{App: in.App, Environment: in.Environment, ConfirmTarget: b.confirmTarget, CanApply: !b.blocked, Digest: "approved-digest", PlanNonce: "server-nonce", Diff: []string{"runtime: old -> new"}, DNS: []integrations.DefinitionDNSPlan{{Host: "app.example", Before: []integrations.DomainDNSRecord{{Type: "CNAME", Content: "old.example", Ownership: "foreign"}}, Desired: []integrations.DNSRecord{{Type: "A", Content: "192.0.2.3"}}, ExternalSatisfaction: []integrations.DomainDNSRecord{{Type: "AAAA", Content: "2001:db8::1"}}, Warnings: []string{"CNAME replacement may cause a DNS gap"}}}, Blockers: func() []string {
		if b.blocked {
			return []string{"target missing"}
		}
		return nil
	}()}, nil
}

func TestDefinitionFileSizeBoundaryBeforeBackend(t *testing.T) {
	path := filepath.Join(t.TempDir(), "definition.toml")
	for _, test := range []struct {
		size    int
		allowed bool
	}{{appdef.MaxDefinitionBytes, true}, {appdef.MaxDefinitionBytes + 1, false}} {
		if err := os.WriteFile(path, []byte(strings.Repeat("x", test.size)), 0600); err != nil {
			t.Fatal(err)
		}
		for _, command := range []string{"plan", "apply"} {
			b := &definitionCLIBackend{ndjsonFollowBackend: newNDJSONBackend()}
			args := []string{command, "--from", path, "--app", "api", "--environment", "production"}
			if command == "apply" {
				args = append(args, "--plan-nonce", "nonce", "--confirm-plan", "digest", "--idempotency-key", "definition-key")
			}
			code, out, _ := definitionRun(t, b, append(args, "--json")...)
			if test.allowed {
				if code != 0 || b.plans+b.applies != 1 {
					t.Fatalf("%s size=%d exit=%d out=%s", command, test.size, code, out)
				}
			} else if code == 0 || b.plans != 0 || b.applies != 0 || !strings.Contains(out, "128 KiB") {
				t.Fatalf("%s size=%d exit=%d out=%s", command, test.size, code, out)
			}
		}
	}
}

func TestDefinitionPlanReportsExplicitRemovalConfirmation(t *testing.T) {
	b := &definitionCLIBackend{ndjsonFollowBackend: newNDJSONBackend(), confirmTarget: "api/production", requireConfirm: true}
	code, out, _ := definitionRun(t, b, definitionArgs("plan")...)
	if code != 0 || !strings.Contains(out, "required removal confirmation: api/production") || !strings.Contains(out, "--confirm 'api/production'") || strings.Count(out, "--confirm '") != 1 {
		t.Fatalf("plain plan exit=%d out=%s", code, out)
	}
	b = &definitionCLIBackend{ndjsonFollowBackend: newNDJSONBackend(), confirmTarget: "api/production", requireConfirm: true}
	code, out, _ = definitionRun(t, b, append(definitionArgs("plan"), "--json")...)
	if code != 0 || !strings.Contains(out, `"confirm_target":"api/production"`) || !strings.Contains(out, "--confirm 'api/production'") {
		t.Fatalf("json plan exit=%d out=%s", code, out)
	}
	args := append(definitionArgs("apply"), "--plan-nonce", "nonce", "--confirm-plan", "digest", "--idempotency-key", "definition-key", "--yes", "--json")
	b = &definitionCLIBackend{ndjsonFollowBackend: newNDJSONBackend(), confirmTarget: "api/production", requireConfirm: true}
	code, out, _ = definitionRun(t, b, args...)
	if code == 0 || b.applyInput.Confirm != "" || !strings.Contains(out, string(contract.ConfirmationRequired)) {
		t.Fatalf("unconfirmed apply exit=%d out=%s input=%+v", code, out, b.applyInput)
	}
	b = &definitionCLIBackend{ndjsonFollowBackend: newNDJSONBackend(), confirmTarget: "api/production", requireConfirm: true}
	args = append(args, "--confirm", "api/production")
	code, out, _ = definitionRun(t, b, args...)
	if code != 0 || b.applyInput.Confirm != "api/production" {
		t.Fatalf("confirmed apply exit=%d out=%s input=%+v", code, out, b.applyInput)
	}
}

func TestDefinitionRejectsInvalidTargetBeforeBackend(t *testing.T) {
	for _, target := range []struct{ app, env string }{{strings.Repeat("a", 41), "production"}, {"api", strings.Repeat("a", 41)}} {
		b := &definitionCLIBackend{ndjsonFollowBackend: newNDJSONBackend()}
		code, out, _ := definitionRun(t, b, "plan", "--from", "testdata/definition.toml", "--app", target.app, "--environment", target.env, "--json")
		if code == 0 || b.plans != 0 || b.applies != 0 {
			t.Fatalf("target=%+v exit=%d out=%s", target, code, out)
		}
	}
}

func (b *definitionCLIBackend) ApplyDefinition(ctx context.Context, in api.DefinitionApplyInput) (operations.SubmissionResult, error) {
	b.applies++
	b.applyInput = in
	if b.blocked {
		return operations.SubmissionResult{}, contract.NewFault(contract.PreconditionFailed, "blocked", "Plan first.")
	}
	if b.requireConfirm && in.Confirm != in.Plan.App+"/"+in.Plan.Environment {
		return operations.SubmissionResult{}, contract.NewFault(contract.ConfirmationRequired, "removal requires target confirmation", "Supply --confirm app/environment.")
	}
	return b.ndjsonFollowBackend.ApplyDefinition(ctx, in)
}

func definitionRun(t *testing.T, b *definitionCLIBackend, args ...string) (int, string, string) {
	t.Helper()
	a := newApp()
	a.backend = b
	var out, stderr bytes.Buffer
	exit := runApp(context.Background(), args, panicReader{}, &out, &stderr, &secrets.Registry{}, a)
	return exit, out.String(), stderr.String()
}

func definitionArgs(command string) []string {
	return []string{command, "--from", "testdata/definition.toml", "--app", "api", "--environment", "production"}
}

func TestDefinitionPlanAndDryRun(t *testing.T) {
	for _, command := range []string{"plan", "apply"} {
		b := &definitionCLIBackend{ndjsonFollowBackend: newNDJSONBackend()}
		args := definitionArgs(command)
		if command == "apply" {
			args = append(args, "--dry-run")
		}
		code, out, stderr := definitionRun(t, b, args...)
		if code != 0 || stderr != "" || b.plans != 1 || b.applies != 0 || b.planInput.Deploy {
			t.Fatalf("%s exit=%d out=%s err=%s backend=%+v", command, code, out, stderr, b)
		}
		for _, s := range []string{"approved-digest", "server-nonce", "CNAME old.example", "A 192.0.2.3", "unchanged external, unowned", "DNS gap", "eligibility, not readiness"} {
			if !strings.Contains(out, s) {
				t.Errorf("missing %q in %s", s, out)
			}
		}
	}
	b := &definitionCLIBackend{ndjsonFollowBackend: newNDJSONBackend()}
	code, out, _ := definitionRun(t, b, append(definitionArgs("plan"), "--json")...)
	if code != 0 || !strings.Contains(out, `"next_commands"`) || !strings.Contains(out, "server-nonce") || !strings.Contains(out, "approved-digest") {
		t.Fatalf("JSON plan exit=%d: %s", code, out)
	}
}

func TestDefinitionApplyRequiresExactApproval(t *testing.T) {
	for _, extra := range [][]string{{}, {"--yes"}, {"--plan-nonce", "n"}, {"--confirm-plan", "d"}} {
		b := &definitionCLIBackend{ndjsonFollowBackend: newNDJSONBackend()}
		args := append(definitionArgs("apply"), extra...)
		args = append(args, "--json")
		code, out, _ := definitionRun(t, b, args...)
		if code == 0 || b.applies != 0 || b.plans != 0 || !strings.Contains(out, string(contract.InputRequired)) {
			t.Fatalf("args=%v code=%d out=%s", args, code, out)
		}
	}
	b := &definitionCLIBackend{ndjsonFollowBackend: newNDJSONBackend()}
	args := append(definitionArgs("apply"), "--plan-nonce", "approved-nonce", "--confirm-plan", "approved-digest", "--idempotency-key", "definition-key", "--confirm", "api/production", "--json")
	code, out, stderr := definitionRun(t, b, args...)
	if code != 0 || stderr != "" || b.applies != 1 || b.plans != 0 || b.applyInput.Plan.PlanNonce != "approved-nonce" || b.applyInput.ApprovedDigest != "approved-digest" || b.applyInput.Confirm != "api/production" || b.applyInput.Plan.Deploy || !strings.Contains(out, string(followTestID)) {
		t.Fatalf("exit=%d out=%s err=%s input=%+v", code, out, stderr, b.applyInput)
	}
}

func TestDefinitionHostFlagSeparation(t *testing.T) {
	b := &definitionCLIBackend{ndjsonFollowBackend: newNDJSONBackend()}
	for _, args := range [][]string{{"plan", "--app", "api"}, {"apply", "--from", ""}, {"apply", "--confirm-plan", "x"}, append(definitionArgs("plan"), "--auto-upgrade-nodes")} {
		code, out, _ := definitionRun(t, b, args...)
		if code == 0 || b.plans != 0 || b.applies != 0 {
			t.Fatalf("args=%v exit=%d out=%s", args, code, out)
		}
	}
}

func TestDefinitionBlockedPlanShowsBlockersWithoutAdmission(t *testing.T) {
	b := &definitionCLIBackend{ndjsonFollowBackend: newNDJSONBackend(), blocked: true}
	code, out, _ := definitionRun(t, b, append(definitionArgs("plan"), "--json")...)
	if code == 0 || b.applies != 0 || !strings.Contains(out, string(contract.PreconditionFailed)) || !strings.Contains(out, "target missing") || !strings.Contains(out, "server-nonce") {
		t.Fatalf("blocked plan: exit=%d out=%s", code, out)
	}
	b = &definitionCLIBackend{ndjsonFollowBackend: newNDJSONBackend(), blocked: true}
	args := append(definitionArgs("apply"), "--plan-nonce", "nonce", "--confirm-plan", "digest", "--idempotency-key", "definition-key", "--json")
	code, out, _ = definitionRun(t, b, args...)
	if code == 0 || b.applies != 1 || b.plans != 1 || len(b.submitted) != 0 || !strings.Contains(out, "target missing") {
		t.Fatalf("blocked apply: exit=%d out=%s backend=%+v", code, out, b)
	}
}

func TestDefinitionEnvironmentUpdateUsesSameServerPath(t *testing.T) {
	// The declaration includes both an attached and an unattached resource;
	// the CLI must transmit it intact rather than issuing child operations.
	path := writeDefinitionFile(t, resourceDefinition)
	args := []string{"env", "update", "api", "production", "--from", path}
	b := &definitionCLIBackend{ndjsonFollowBackend: newNDJSONBackend()}
	code, out, stderr := definitionRun(t, b, append(args, "--dry-run", "--json")...)
	if code != 0 || stderr != "" || b.plans != 1 || b.applies != 0 || b.calls != 0 || len(b.submitted) != 0 || b.planInput.Definition != resourceDefinition || b.planInput.App != "api" || b.planInput.Environment != "production" || b.planInput.Deploy || !strings.Contains(out, "server-nonce") {
		t.Fatalf("dry run exit=%d out=%s err=%s backend=%+v", code, out, stderr, b)
	}
	approved := append(args, "--plan-nonce", "nonce", "--confirm-plan", "digest", "--idempotency-key", "definition-key", "--confirm", "api/production", "--json")
	b = &definitionCLIBackend{ndjsonFollowBackend: newNDJSONBackend()}
	code, out, stderr = definitionRun(t, b, approved...)
	if code != 0 || stderr != "" || b.applies != 1 || b.plans != 0 || b.calls != 1 || len(b.submitted) != 1 || b.applyInput.Plan.Definition != resourceDefinition || b.applyInput.Plan.App != "api" || b.applyInput.Plan.Environment != "production" || b.applyInput.Plan.PlanNonce != "nonce" || b.applyInput.ApprovedDigest != "digest" || b.applyInput.Confirm != "api/production" || b.applyInput.Plan.Deploy || !strings.Contains(out, string(followTestID)) {
		t.Fatalf("apply exit=%d out=%s err=%s input=%+v", code, out, stderr, b.applyInput)
	}
	// A repeated unchanged definition is still admitted; the server may need
	// to repair managed DNS even if there is no local revision difference.
	b = &definitionCLIBackend{ndjsonFollowBackend: newNDJSONBackend()}
	code, out, _ = definitionRun(t, b, approved...)
	if code != 0 || b.applies != 1 || b.calls != 1 {
		t.Fatalf("unchanged apply exit=%d out=%s", code, out)
	}
}

func TestDefinitionEnvironmentUpdateRejectsMixedOrUnapprovedInputs(t *testing.T) {
	base := []string{"env", "update", "api", "production", "--from", "testdata/definition.toml"}
	for _, extra := range [][]string{
		{}, {"--plan-nonce", "nonce"}, {"--confirm-plan", "digest"},
		{"--confirm-domain", "old.example"}, {"--git-ref", "main", "--dry-run"},
		{"--auto-deploy=false", "--dry-run"}, {"--github-webhook", "--dry-run"},
		{"--github-auth", "--dry-run"}, {"--deploy", "--dry-run"},
	} {
		b := &definitionCLIBackend{ndjsonFollowBackend: newNDJSONBackend()}
		args := append(append([]string{}, base...), extra...)
		code, out, _ := definitionRun(t, b, append(args, "--json")...)
		if code == 0 || b.plans != 0 || b.applies != 0 || b.calls != 0 || len(b.submitted) != 0 {
			t.Fatalf("args=%v exit=%d out=%s backend=%+v", args, code, out, b)
		}
	}
}

func TestDefinitionEnvironmentUpdatePassesFullDomainAndEmptyCollections(t *testing.T) {
	for _, raw := range []string{
		"version = 1\napp = \"api\"\nnode = \"main\"\n[source]\n[[domains]]\nname = \"api.example.com\"\nprimary = true\n",
		"version = 1\napp = \"api\"\nnode = \"main\"\nsecret_names = []\n[source]\n[config]\n",
	} {
		path := writeDefinitionFile(t, raw)
		b := &definitionCLIBackend{ndjsonFollowBackend: newNDJSONBackend()}
		code, out, _ := definitionRun(t, b, "env", "update", "api", "production", "--from", path, "--dry-run", "--json")
		if code != 0 || b.plans != 1 || b.applies != 0 || b.planInput.Definition != raw {
			t.Fatalf("exit=%d out=%s input=%+v", code, out, b.planInput)
		}
	}
}

func TestDefinitionEnvironmentUpdateRemovalNeedsTargetConfirmation(t *testing.T) {
	base := []string{"env", "update", "api", "production", "--from", "testdata/definition.toml", "--plan-nonce", "nonce", "--confirm-plan", "digest", "--idempotency-key", "definition-key"}
	for _, confirm := range []string{"", "api/other", "api/production"} {
		b := &definitionCLIBackend{ndjsonFollowBackend: newNDJSONBackend(), requireConfirm: true}
		args := append([]string{}, base...)
		if confirm != "" {
			args = append(args, "--confirm", confirm)
		}
		code, out, _ := definitionRun(t, b, append(args, "--json")...)
		if confirm == "api/production" {
			if code != 0 || len(b.submitted) != 1 || b.applyInput.Confirm != confirm {
				t.Fatalf("confirmed: exit=%d out=%s", code, out)
			}
		} else if code == 0 || len(b.submitted) != 0 || !strings.Contains(out, string(contract.ConfirmationRequired)) {
			t.Fatalf("unconfirmed %q: exit=%d out=%s", confirm, code, out)
		}
	}
}

func TestDefinitionEnvironmentUpdateAutomationIsOnlyServerInput(t *testing.T) {
	raw := "version = 1\napp = \"api\"\nnode = \"main\"\n[source]\nurl = \"https://github.com/acme/api\"\n[automation]\nauto_deploy = false\n"
	path := writeDefinitionFile(t, raw)
	b := &definitionCLIBackend{ndjsonFollowBackend: newNDJSONBackend()}
	args := []string{"env", "update", "api", "production", "--from", path, "--plan-nonce", "nonce", "--confirm-plan", "digest", "--idempotency-key", "definition-key", "--json"}
	code, out, _ := definitionRun(t, b, args...)
	if code != 0 || b.applies != 1 || b.calls != 1 || len(b.submitted) != 1 || b.applyInput.Plan.Definition != raw {
		t.Fatalf("exit=%d out=%s backend=%+v", code, out, b)
	}
}
