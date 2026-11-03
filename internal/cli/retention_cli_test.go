package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/retention"
	"github.com/mbvlabs/eregion/internal/secrets"
)

type retentionCLIBackend struct {
	Backend
	node          string
	plan          retention.PlanResult
	view          retention.PolicyView
	submittedKind string
	submittedTo   string
	submittedRaw  json.RawMessage
	submits       int
	serverPolicy  retention.Policy
	serverSets    int
}

func (b *retentionCLIBackend) NodeInfo(context.Context) (rpcNodeInfoResult, error) {
	return rpcNodeInfoResult{Node: b.node, Hostname: b.node, Platform: "linux/amd64"}, nil
}

func (b *retentionCLIBackend) RetentionPlan(context.Context, string, string, string) (retention.PlanResult, error) {
	return b.plan, nil
}

func (b *retentionCLIBackend) RetentionShow(context.Context, string, string) (retention.PolicyView, error) {
	return b.view, nil
}

func (b *retentionCLIBackend) RetentionSetServer(_ context.Context, policy retention.Policy) (retention.PolicyView, error) {
	b.serverSets++
	b.serverPolicy = policy
	view := b.view
	view.Server, view.Effective = policy, policy
	return view, nil
}

func (b *retentionCLIBackend) SubmitAction(_ context.Context, kind, target string, input json.RawMessage, _, _ string, _ bool) (json.RawMessage, error) {
	b.submits++
	b.submittedKind, b.submittedTo, b.submittedRaw = kind, target, input
	op := guidedOperation(contract.OperationID("op_"+strings.Repeat("a", 32)), kind, target, contract.Queued, 1, time.Now().UTC())
	raw, err := json.Marshal(op)
	return raw, err
}

func (b *retentionCLIBackend) SubmitActionWithSecrets(ctx context.Context, kind, target string, input json.RawMessage, key, confirm string, dryRun bool, _ map[string]string) (json.RawMessage, error) {
	return b.SubmitAction(ctx, kind, target, input, key, confirm, dryRun)
}

func runRetentionCLI(t *testing.T, backend *retentionCLIBackend, args ...string) (int, string, string) {
	t.Helper()
	a := newApp()
	a.backend = backend
	var stdout, stderr bytes.Buffer
	exit := runApp(context.Background(), args, panicReader{}, &stdout, &stderr, &secrets.Registry{}, a)
	return exit, stdout.String(), stderr.String()
}

func TestRetentionCommandsAppearInCatalog(t *testing.T) {
	code, stdout, stderr := invoke(t, "commands", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	envelope := decodeOne(t, stdout)
	raw, _ := json.Marshal(envelope.Data)
	var data struct {
		Commands []Command `json:"commands"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	byPath := map[string]Command{}
	for _, command := range data.Commands {
		byPath[command.Path] = command
	}
	prune, ok := byPath["eregion prune"]
	if !ok || !prune.Mutating || !prune.Destructive || !prune.DryRun || prune.Confirmation != "target" {
		t.Fatalf("prune metadata = %+v", prune)
	}
	show, ok := byPath["eregion retention show"]
	if !ok || show.Mutating || show.Output != "result" {
		t.Fatalf("retention show metadata = %+v", show)
	}
	set, ok := byPath["eregion retention set"]
	if !ok || !set.Mutating || !set.Prompts {
		t.Fatalf("retention set metadata = %+v", set)
	}
	flags := map[string]bool{}
	for _, flag := range prune.Flags {
		flags[flag.Name] = true
	}
	for _, name := range []string{"dry-run", "confirm", "detach", "idempotency-key", "poll-interval"} {
		if !flags[name] {
			t.Fatalf("prune is missing --%s", name)
		}
	}
}

func TestPruneDryRunListsItemsWithoutSubmitting(t *testing.T) {
	backend := &retentionCLIBackend{
		node: "main",
		plan: retention.PlanResult{
			Node: "main", PlannedBytes: 90,
			Items: []retention.Item{
				{Kind: retention.ItemRelease, Reference: "rel_old", Bytes: 40, Reason: "old"},
				{Kind: retention.ItemRegistrySkipped, Reference: "*", Reason: "inventory truncated"},
			},
		},
	}
	code, stdout, stderr := runRetentionCLI(t, backend, "prune", "--dry-run", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	envelope := decodeOne(t, stdout)
	if !envelope.OK {
		t.Fatalf("envelope=%+v", envelope)
	}
	raw, _ := json.Marshal(envelope.Data)
	var data prunePreviewResult
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	if !data.DryRun || data.Node != "main" || len(data.Items) != 2 || data.PlannedBytes != 90 {
		t.Fatalf("preview = %+v", data)
	}
	if backend.submits != 0 {
		t.Fatalf("dry run submitted %d operation(s)", backend.submits)
	}
}

func TestPruneRequiresTheServerConfirmation(t *testing.T) {
	backend := &retentionCLIBackend{node: "main"}
	for _, test := range []struct {
		name string
		args []string
		code contract.Code
	}{
		{name: "missing", args: []string{"prune", "--json"}, code: contract.ConfirmationRequired},
		{name: "wrong", args: []string{"prune", "--confirm", "other", "--json"}, code: contract.PreconditionFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			code, stdout, stderr := runRetentionCLI(t, backend, test.args...)
			if code != 5 || stderr != "" {
				t.Fatalf("code=%d stderr=%q", code, stderr)
			}
			envelope := decodeOne(t, stdout)
			if envelope.OK || envelope.Error == nil || envelope.Error.Code != test.code {
				t.Fatalf("envelope=%+v", envelope)
			}
		})
	}
	if backend.submits != 0 {
		t.Fatalf("refused prune submitted %d operation(s)", backend.submits)
	}
}

func TestPruneSubmitsTheNodeTarget(t *testing.T) {
	backend := &retentionCLIBackend{node: "main"}
	code, stdout, stderr := runRetentionCLI(t, backend, "prune", "--confirm", "main", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	if !decodeOne(t, stdout).OK {
		t.Fatal("prune did not return the accepted operation")
	}
	if backend.submittedKind != platform.KindRetentionPrune || backend.submittedTo != "node/main" {
		t.Fatalf("submitted %s on %s", backend.submittedKind, backend.submittedTo)
	}
	var input platform.RetentionPruneInput
	if err := json.Unmarshal(backend.submittedRaw, &input); err != nil || input.Node != "main" {
		t.Fatalf("input = %+v err=%v", input, err)
	}
}

func TestRetentionSetRefusesWithoutValuesInMachineMode(t *testing.T) {
	backend := &retentionCLIBackend{node: "main", view: retention.PolicyView{Scope: "server", Effective: retention.DefaultPolicy()}}
	for _, args := range [][]string{
		{"retention", "set", "--json"},
		{"retention", "set", "--app", "api", "--env", "production", "--json"},
	} {
		code, stdout, stderr := runRetentionCLI(t, backend, args...)
		if code != 2 || stderr != "" {
			t.Fatalf("%v: code=%d stderr=%q", args, code, stderr)
		}
		envelope := decodeOne(t, stdout)
		if envelope.OK || envelope.Error == nil || envelope.Error.Code != contract.InputRequired {
			t.Fatalf("%v: envelope=%+v", args, envelope)
		}
	}
}

func TestRetentionSetServerWritesTheDefault(t *testing.T) {
	backend := &retentionCLIBackend{
		node: "main",
		view: retention.PolicyView{Scope: "server", Server: retention.DefaultPolicy(), Effective: retention.DefaultPolicy()},
	}
	code, stdout, stderr := runRetentionCLI(t, backend,
		"retention", "set", "--keep-releases", "4", "--keep-days", "7",
		"--artifact-grace-days", "9", "--cache-cap-bytes", "2147483648",
		"--cache-cap-percent", "50", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	if !decodeOne(t, stdout).OK {
		t.Fatal("set did not return the updated policy")
	}
	if backend.serverSets != 1 {
		t.Fatalf("server sets = %d", backend.serverSets)
	}
	policy := backend.serverPolicy
	if policy.KeepReleases != 4 || policy.KeepNewerThanDays != 7 || policy.ArtifactGraceDays != 9 ||
		policy.BuildpackCacheMaxBytes != 2147483648 || policy.BuildpackCacheMaxPercent != 50 {
		t.Fatalf("policy = %+v", policy)
	}
}

func TestRetentionSetEnvironmentCommitsARevision(t *testing.T) {
	effective := retention.DefaultPolicy()
	effective.KeepReleases = 6
	backend := &retentionCLIBackend{
		node: "main",
		view: retention.PolicyView{
			Scope: "environment", App: "api", Environment: "production",
			Server: retention.DefaultPolicy(), Effective: effective,
		},
	}
	code, stdout, stderr := runRetentionCLI(t, backend,
		"retention", "set", "--app", "api", "--env", "production",
		"--keep-releases", "3", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	if !decodeOne(t, stdout).OK {
		t.Fatal("set did not return the accepted operation")
	}
	if backend.submittedKind != platform.KindEnvironmentUpdate || backend.submittedTo != "env/api/production" {
		t.Fatalf("submitted %s on %s", backend.submittedKind, backend.submittedTo)
	}
	var input platform.EnvironmentUpdateInput
	if err := json.Unmarshal(backend.submittedRaw, &input); err != nil {
		t.Fatal(err)
	}
	if input.Retention == nil || input.Retention.KeepReleases == nil || *input.Retention.KeepReleases != 3 {
		t.Fatalf("retention input = %+v", input.Retention)
	}
}

func TestRetentionSetNeverGuidesInMachineOutput(t *testing.T) {
	if retentionSetGuided("json", false, true, true) {
		t.Fatal("--json at a terminal entered the guided interview")
	}
	if retentionSetGuided("ndjson", false, true, true) {
		t.Fatal("--output ndjson at a terminal entered the guided interview")
	}
	if retentionSetGuided("plain", true, true, true) {
		t.Fatal("--json at a terminal entered the guided interview even with plain mode")
	}
	if !retentionSetGuided("plain", false, true, true) {
		t.Fatal("a plain terminal could not enter the guided interview")
	}
	if retentionSetGuided("plain", false, false, true) {
		t.Fatal("a piped stdin entered the guided interview")
	}
}

func TestRetentionShowReturnsFieldSources(t *testing.T) {
	keep := 2
	backend := &retentionCLIBackend{
		node: "main",
		view: retention.PolicyView{
			Scope: "environment", App: "api", Environment: "production",
			Server: retention.DefaultPolicy(), Effective: retention.DefaultPolicy(),
			Override: retention.Override{KeepReleases: &keep},
			Sources: map[string]string{
				"keep_releases": "environment", "keep_newer_than_days": "server",
				"artifact_grace_days": "server", "buildpack_cache_max_bytes": "server",
				"buildpack_cache_max_percent": "server",
			},
		},
	}
	code, stdout, stderr := runRetentionCLI(t, backend, "retention", "show", "--app", "api", "--env", "production", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	envelope := decodeOne(t, stdout)
	raw, _ := json.Marshal(envelope.Data)
	var view retention.PolicyView
	if err := json.Unmarshal(raw, &view); err != nil {
		t.Fatal(err)
	}
	if view.Scope != "environment" || view.Sources["keep_releases"] != "environment" || view.Sources["keep_newer_than_days"] != "server" {
		t.Fatalf("view = %+v", view)
	}
}
