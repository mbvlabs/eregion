package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/secrets"
)

func runPromoteCLI(t *testing.T, backend *definitionInventoryBackend, args ...string) (int, string, string) {
	t.Helper()
	a := newApp()
	a.backend = backend
	var stdout, stderr bytes.Buffer
	exit := runApp(context.Background(), args, panicReader{}, &stdout, &stderr, &secrets.Registry{}, a)
	return exit, stdout.String(), stderr.String()
}

func TestEnvironmentPromoteDryRunIsFlagOnlyAndTargetsProduction(t *testing.T) {
	backend := &definitionInventoryBackend{inventory: map[string]json.RawMessage{}}
	exit, stdout, stderr := runPromoteCLI(t, backend, "env", "promote", "api", "staging", "--dry-run", "--json")
	if exit != 0 || stderr != "" {
		t.Fatalf("exit=%d stderr=%q", exit, stderr)
	}
	if len(backend.submissions) != 1 {
		t.Fatalf("submissions = %+v", backend.submissions)
	}
	submission := backend.submissions[0]
	if submission.Kind != platform.KindPromote || submission.Target != "env/api/production" || !submission.DryRun {
		t.Fatalf("submission = %+v", submission)
	}
	var input platform.PromoteInput
	if err := json.Unmarshal(submission.Input, &input); err != nil {
		t.Fatal(err)
	}
	if input.App != "api" || input.SourceEnvironment != "staging" {
		t.Fatalf("input = %+v", input)
	}
	envelope := decodeOne(t, stdout)
	if !envelope.OK {
		t.Fatalf("envelope = %+v", envelope)
	}
}

func TestEnvironmentPromoteRefusesProductionAsSourceWithoutSubmitting(t *testing.T) {
	backend := &definitionInventoryBackend{inventory: map[string]json.RawMessage{}}
	exit, stdout, stderr := runPromoteCLI(t, backend, "env", "promote", "api", "production", "--json")
	if exit != 2 || stderr != "" || len(backend.submissions) != 0 {
		t.Fatalf("exit=%d stderr=%q submissions=%+v", exit, stderr, backend.submissions)
	}
	envelope := decodeOne(t, stdout)
	if envelope.OK || envelope.Error == nil || envelope.Error.Code != contract.ValidationFailed {
		t.Fatalf("envelope = %+v", envelope)
	}
}

// promoteKeyBackend records the preview key so the CLI regression can prove a
// dry run or interactive plan uses the operator's idempotency key.
type promoteKeyBackend struct {
	*definitionInventoryBackend
	previewKey string
}

func (b *promoteKeyBackend) SubmitAction(ctx context.Context, kind, target string, input json.RawMessage, key, confirm string, dryRun bool) (json.RawMessage, error) {
	b.previewKey = key
	return b.definitionInventoryBackend.SubmitAction(ctx, kind, target, input, key, confirm, dryRun)
}

func TestEnvironmentPromoteDryRunPassesIdempotencyKey(t *testing.T) {
	for _, args := range [][]string{
		{"env", "promote", "api", "staging", "--dry-run", "--idempotency-key", "stable-promote-json", "--json"},
		{"env", "promote", "api", "staging", "--dry-run", "--idempotency-key", "stable-promote-plain"},
	} {
		backend := &promoteKeyBackend{definitionInventoryBackend: &definitionInventoryBackend{inventory: map[string]json.RawMessage{}}}
		a := newApp()
		a.backend = backend
		var stdout, stderr bytes.Buffer
		exit := runApp(context.Background(), args, panicReader{}, &stdout, &stderr, &secrets.Registry{}, a)
		if exit != 0 || stderr.String() != "" {
			t.Fatalf("%v: exit=%d stderr=%q", args, exit, stderr.String())
		}
		want := "stable-promote-json"
		if !containsValue(args, "--json") {
			want = "stable-promote-plain"
		}
		if backend.previewKey != want {
			t.Fatalf("%v: preview key = %q, want %q", args, backend.previewKey, want)
		}
		if len(backend.submissions) != 1 || !backend.submissions[0].DryRun || backend.submissions[0].Kind != platform.KindPromote {
			t.Fatalf("%v: submissions = %+v", args, backend.submissions)
		}
	}
}

func TestEnvironmentPromoteCatalogMetadata(t *testing.T) {
	a := newApp()
	cmd, remaining, err := a.root.Find([]string{"env", "promote"})
	if err != nil || len(remaining) != 0 {
		t.Fatalf("find promote: %v %v", err, remaining)
	}
	meta := a.meta[cmd]
	if !meta.Mutating || !meta.DryRun || meta.Prompts || meta.Destructive || meta.Confirmation != "" {
		t.Fatalf("promote metadata = %+v", meta)
	}
	if !strings.Contains(cmd.Use, "promote <app> <environment>") {
		t.Fatalf("use = %q", cmd.Use)
	}
	flags := map[string]bool{}
	for _, flag := range []string{"dry-run", "detach", "idempotency-key"} {
		if cmd.Flags().Lookup(flag) == nil {
			t.Fatalf("missing --%s", flag)
		}
		flags[flag] = true
	}
	if len(flags) != 3 {
		t.Fatalf("flags = %+v", flags)
	}
}
