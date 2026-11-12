package cli

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/appdef"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/secrets"
	"github.com/mbvlabs/eregion/internal/vault"
	"github.com/spf13/cobra"
)

const testDefinition = `version = 2
app = "api"
node = "main"
secret_names = ["DATABASE_URL"]

[source]
url = "https://github.com/acme/api"
ref = "main"
credential_secret = "GIT_READ_TOKEN"
context_path = "apps/api"
builder = "paketobuildpacks/builder-jammy-base"

[source.build_env]
BP_X = "y"

[runtime]
health_type = "http"
health_path = "/healthz"
health_timeout_seconds = 30
release_command = ["./migrate", "--yes"]

[config]
APP_ENV = "production"

[[processes]]
name = "web"
kind = "web"
command = "./api"
args = ["serve"]
container_port = 3000
replicas = 1

[environments.production]
[[environments.production.domains]]
name = "api.example.com"
primary = true
check_path = "/healthz"
`

func definitionCommand(t *testing.T, mode setupMode) *cobra.Command {
	t.Helper()
	cmd := &cobra.Command{Use: "create"}
	if mode == setupApp {
		cmd.Flags().String("name", "", "app")
		cmd.Flags().String("environment", "production", "environment")
	} else {
		cmd.Flags().String("app", "", "app")
		cmd.Flags().String("name", "", "environment")
	}
	cmd.Flags().Bool("no-deploy", false, "no deploy")
	cmd.Flags().Bool("detach", false, "detach")
	addSetupFlags(cmd, true)
	return cmd
}

func TestDefinitionFileAndFlagsBuildTheSameFirstRevision(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "eregion.toml")
	if err := os.WriteFile(path, []byte(testDefinition), 0o600); err != nil {
		t.Fatal(err)
	}
	credentialFile := writeSecretInput(t, "read-token")
	secretFile := writeSecretInput(t, "db-secret")

	fileCmd := definitionCommand(t, setupApp)
	if err := fileCmd.ParseFlags([]string{
		"--name", "api", "--environment", "production", "--from", path, "--no-deploy",
	}); err != nil {
		t.Fatal(err)
	}
	fileApp := &app{secrets: &secrets.Registry{}}
	fileAnswers, err := fileApp.readSetup(fileCmd, setupApp)
	if err != nil {
		t.Fatal(err)
	}
	defer fileAnswers.clearSecrets()
	fileSpec, err := fileAnswers.environmentSpec(fileCmd)
	if err != nil {
		t.Fatal(err)
	}
	if len(fileAnswers.PrebuiltSpecs) != 2 || fileAnswers.PrebuiltSpecs[0].Name != "production" || fileAnswers.PrebuiltSpecs[1].Name != "staging" {
		t.Fatalf("app definition did not resolve production and staging: %+v", fileAnswers.PrebuiltSpecs)
	}
	if !protectedSpec(fileAnswers.PrebuiltSpecs[0]) || !protectedSpec(fileAnswers.PrebuiltSpecs[1]) {
		t.Fatalf("definition environments were not protected by default: %+v", fileAnswers.PrebuiltSpecs)
	}
	if len(fileAnswers.PrebuiltSpecs[1].Domains) != 0 {
		t.Fatalf("staging inherited production's domain: %+v", fileAnswers.PrebuiltSpecs[1].Domains)
	}

	flagCmd := definitionCommand(t, setupApp)
	if err := flagCmd.ParseFlags([]string{
		"--name", "api",
		"--environment", "production",
		"--node", "main",
		"--git-url", "https://github.com/acme/api",
		"--git-ref", "main",
		"--git-credential-file", credentialFile,
		"--context-path", "apps/api",
		"--builder", "paketobuildpacks/builder-jammy-base",
		"--build-env", "BP_X=y",
		"--container-port", "3000",
		"--health-path", "/healthz",
		"--release-command", "./migrate --yes",
		"--process", "web=web:3000:1:./api serve",
		"--config", "APP_ENV=production",
		"--secret", "DATABASE_URL=" + secretFile,
		"--domain", "api.example.com",
		"--domain-check-path", "/healthz",
		"--no-deploy",
	}); err != nil {
		t.Fatal(err)
	}
	flagApp := &app{secrets: &secrets.Registry{}}
	flagAnswers, err := flagApp.readSetup(flagCmd, setupApp)
	if err != nil {
		t.Fatal(err)
	}
	defer flagAnswers.clearSecrets()
	flagSpec, err := flagAnswers.environmentSpec(flagCmd)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(fileSpec, flagSpec) {
		t.Fatalf("definition and flags produced different revisions:\nfile: %+v\nflags: %+v", fileSpec, flagSpec)
	}
}

func TestDefinitionFileCarriesMultipleDomains(t *testing.T) {
	path := filepath.Join(t.TempDir(), "eregion.toml")
	raw := `version = 2
app = "api"
node = "main"
[source]
[environments.production]
[[environments.production.domains]]
name = "api.example.com"
primary = true
check_path = "/healthz"
[[environments.production.domains]]
name = "www.example.com"
check_path = "/readyz"
`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := definitionCommand(t, setupApp)
	if err := cmd.ParseFlags([]string{"--from", path, "--name", "api", "--no-deploy"}); err != nil {
		t.Fatal(err)
	}
	a := &app{secrets: &secrets.Registry{}}
	ans, err := a.readSetup(cmd, setupApp)
	if err != nil {
		t.Fatal(err)
	}
	defer ans.clearSecrets()
	if len(ans.Domains) != 2 || ans.Domain != "api.example.com" || ans.DomainCheckPath != "/healthz" {
		t.Fatalf("answers domains = %+v primary=%q check=%q", ans.Domains, ans.Domain, ans.DomainCheckPath)
	}
	spec, err := ans.environmentSpec(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Domains) != 2 || spec.Domains[1].Domain != "www.example.com" || spec.Domains[1].CheckPath != "/readyz" {
		t.Fatalf("spec domains = %+v", spec.Domains)
	}
}

// TestEnvironmentCreateFromSharedRecipeNeedsNoNamedOverride is the PR
// environment case: the shared recipe applies to a name that is not listed in
// the document, and the new environment is unprotected by default.
func TestEnvironmentCreateFromSharedRecipeNeedsNoNamedOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "eregion.toml")
	raw := `version = 2
app = "api"
node = "main"

[source]
url = "https://github.com/acme/api"
ref = "main"

[config]
APP_ENV = "shared"
`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := definitionCommand(t, setupEnvironment)
	if err := cmd.ParseFlags([]string{"--app", "api", "--name", "pr-42", "--from", path, "--no-deploy"}); err != nil {
		t.Fatal(err)
	}
	a := &app{secrets: &secrets.Registry{}}
	ans, err := a.readSetup(cmd, setupEnvironment)
	if err != nil {
		t.Fatal(err)
	}
	defer ans.clearSecrets()
	spec, err := ans.environmentSpec(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Name != "pr-42" || spec.Source.URL != "https://github.com/acme/api" || spec.Config["APP_ENV"] != "shared" {
		t.Fatalf("PR environment did not use the shared recipe: %+v", spec)
	}
	if spec.Protected == nil || *spec.Protected {
		t.Fatalf("PR environment protection = %+v, want unprotected by default", spec.Protected)
	}
}

func TestDefinitionFileRejectsSecretValuesAndConflictingFlags(t *testing.T) {
	dir := t.TempDir()
	secretPath := filepath.Join(dir, "secret.toml")
	if err := os.WriteFile(secretPath, []byte("version = 2\n[secrets]\nDATABASE_URL = \"postgres://user:hunter2@example/db\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := definitionCommand(t, setupApp)
	if err := cmd.ParseFlags([]string{"--from", secretPath}); err != nil {
		t.Fatal(err)
	}
	a := &app{secrets: &secrets.Registry{}}
	_, err := a.readSetup(cmd, setupApp)
	if err == nil {
		t.Fatal("accepted a secret value field")
	}
	if contract.Classify(err).Code != contract.ValidationFailed {
		t.Fatalf("error code = %v", contract.Classify(err).Code)
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("error echoed the secret value: %v", err)
	}

	validPath := filepath.Join(dir, "eregion.toml")
	if err := os.WriteFile(validPath, []byte(testDefinition), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd = definitionCommand(t, setupApp)
	if err := cmd.ParseFlags([]string{"--from", validPath, "--git-ref", "other"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.readSetup(cmd, setupApp); err == nil {
		t.Fatal("accepted --from with a conflicting revision flag")
	}

	cmd = definitionCommand(t, setupApp)
	if err := cmd.ParseFlags([]string{"--from", validPath, "--name", "other"}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.readSetup(cmd, setupApp); err == nil {
		t.Fatal("accepted a definition app that disagrees with --name")
	}

	largePath := filepath.Join(dir, "large.toml")
	if err := os.WriteFile(largePath, []byte(strings.Repeat("#", 1<<20+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd = definitionCommand(t, setupApp)
	if err := cmd.ParseFlags([]string{"--from", largePath}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.readSetup(cmd, setupApp); err == nil {
		t.Fatal("accepted a definition above the input size limit")
	} else if contract.Classify(err).Code != contract.ValidationFailed {
		t.Fatalf("size-limit error = %v", contract.Classify(err).Code)
	}
}

// definitionSubmission records one action the CLI submitted or previewed.
type definitionSubmission struct {
	Kind    string
	Target  string
	Input   json.RawMessage
	Confirm string
	DryRun  bool
}

// definitionInventoryBackend serves inventory reads and records action
// submissions for the definition CLI tests.
type definitionInventoryBackend struct {
	Backend
	inventory        map[string]json.RawMessage
	submitted        string
	input            json.RawMessage
	dryRun           bool
	secretWrite      int
	secretRead       int
	secretNamespaces []vault.Namespace
	secretPayloads   map[string]string
	submissions      []definitionSubmission
	definitionPatch  platform.EnvironmentUpdateInput
	definitionPlans  int
	previewReplay    bool
	// webhookStateful models the control plane's canonical-hash operation
	// table for first-call-then-repeat CLI tests.
	webhookStateful       bool
	webhookOps            map[string]storedWebhookOperation
	webhookForceReplay    bool
	webhookSkipActivation bool
}

// Model the server-side definition planner for tests that exercise the
// canonical patch and credential alias checks; no inventory RPC is made.
func (f *definitionInventoryBackend) PlanDefinition(_ context.Context, input api.DefinitionPlanInput) (api.DefinitionPlan, error) {
	f.definitionPlans++
	definition, err := appdef.Parse([]byte(input.Definition))
	if err != nil {
		return api.DefinitionPlan{}, definitionFault(err)
	}
	resolved, err := definition.Resolve(input.Environment)
	if err != nil {
		return api.DefinitionPlan{}, definitionFault(err)
	}
	resolved.App = input.App
	var page struct {
		Items []platform.Environment `json:"items"`
	}
	if err := json.Unmarshal(f.inventory["environments"], &page); err != nil || len(page.Items) == 0 {
		return api.DefinitionPlan{}, contract.NewFault(contract.NotFound, "environment not found", "Use an existing environment.")
	}
	patch, changed, diff, err := resolved.UpdatePatch(page.Items[0], nil)
	if err != nil {
		return api.DefinitionPlan{}, definitionFault(err)
	}
	f.definitionPatch = patch
	return api.DefinitionPlan{App: input.App, Environment: input.Environment, CanApply: true, Digest: "digest", PlanNonce: "nonce", Changes: changed, Diff: diff}, nil
}

func (f *definitionInventoryBackend) ApplyDefinition(context.Context, api.DefinitionApplyInput) (operations.SubmissionResult, error) {
	return operations.SubmissionResult{}, contract.NewFault(contract.Unavailable, "fake admission not configured", "Use a planning test.")
}

type storedWebhookOperation struct {
	hash string
	op   contract.Operation
}

func (f *definitionInventoryBackend) PlatformInventory(_ context.Context, collection string, _ url.Values) (json.RawMessage, error) {
	if raw, ok := f.inventory[collection]; ok {
		return raw, nil
	}
	return json.RawMessage(`{"items":[]}`), nil
}

func (f *definitionInventoryBackend) record(kind, target string, input json.RawMessage, confirm string, dryRun bool) {
	f.submitted, f.input, f.dryRun = kind, input, dryRun
	f.submissions = append(f.submissions, definitionSubmission{
		Kind: kind, Target: target, Input: input, Confirm: confirm, DryRun: dryRun,
	})
}

func (f *definitionInventoryBackend) preview(kind, target string) (json.RawMessage, error) {
	preview := platform.Preview{Kind: kind, Target: target, DryRun: true, Summary: "test preview", Steps: []string{"revision"}}
	if f.previewReplay {
		now := time.Unix(1700000000, 0).UTC()
		preview.Replay = true
		preview.Operation = &contract.Operation{
			ID:           "op_replay",
			Kind:         kind,
			Target:       target,
			Actor:        contract.Actor{Kind: "operator", Subject: "definition-test"},
			State:        contract.Succeeded,
			Attempt:      1,
			LastSequence: 1,
			CreatedAt:    now,
			UpdatedAt:    now,
		}
	}
	return json.Marshal(preview)
}

func (f *definitionInventoryBackend) SubmitAction(_ context.Context, kind, target string, input json.RawMessage, key, confirm string, dryRun bool) (json.RawMessage, error) {
	f.record(kind, target, input, confirm, dryRun)
	if dryRun && f.webhookStateful && kind == platform.KindEnvironmentUpdate {
		return f.previewWebhook(kind, target, input, key)
	}
	return f.preview(kind, target)
}

func (f *definitionInventoryBackend) SubmitActionWithSecrets(_ context.Context, kind, target string, input json.RawMessage, key, confirm string, dryRun bool, _ map[string]string) (json.RawMessage, error) {
	f.record(kind, target, input, confirm, dryRun)
	if dryRun && f.webhookStateful && kind == platform.KindEnvironmentUpdate {
		return f.previewWebhook(kind, target, input, key)
	}
	if dryRun {
		return f.preview(kind, target)
	}
	now := time.Unix(1700000000, 0).UTC()
	return json.Marshal(contract.Operation{
		ID:           contract.OperationID(fmt.Sprintf("op_%032x", len(f.submissions))),
		Kind:         kind,
		Target:       target,
		Actor:        contract.Actor{Kind: "operator", Subject: "definition-test"},
		State:        contract.Succeeded,
		Attempt:      1,
		LastSequence: 1,
		CreatedAt:    now,
		UpdatedAt:    now,
	})
}

func (f *definitionInventoryBackend) SubmitWebhookAction(_ context.Context, kind, target string, input json.RawMessage, key, _ string, dryRun bool, secretValues map[string]string) (json.RawMessage, submissionReceipt, error) {
	f.record(kind, target, input, "", dryRun)
	if dryRun {
		data, err := f.previewWebhook(kind, target, input, key)
		return data, submissionReceipt{}, err
	}
	if kind != platform.KindEnvironmentUpdate {
		return nil, submissionReceipt{}, contract.NewFault(contract.ValidationFailed, "test backend only models environment webhook updates", "Report the command and binary version.")
	}
	normalized, err := platform.Default().Normalize(kind, target, input)
	if err != nil {
		return nil, submissionReceipt{}, err
	}
	hash := webhookInputHash(normalized.Canonical)
	if key == "" {
		return nil, submissionReceipt{}, contract.NewFault(contract.InputRequired, "an idempotency key is required", "Reuse one stable key for retries.")
	}
	if f.webhookOps == nil {
		f.webhookOps = map[string]storedWebhookOperation{}
	}
	if stored, ok := f.webhookOps[key]; ok {
		if stored.hash != hash {
			return nil, submissionReceipt{}, contract.NewFault(contract.Conflict, "idempotency key already binds different inputs", "Use the original inputs to recover the existing operation; use a new key only for deliberately new work.")
		}
		raw, err := json.Marshal(stored.op)
		return raw, submissionReceipt{Replay: true}, err
	}
	if f.webhookForceReplay {
		op := f.webhookOperation(kind, target, key)
		return mustWebhookJSON(op), submissionReceipt{Replay: true}, nil
	}
	activated := false
	if encoded := secretValues[rpcWebhookSecretKey]; encoded != "" {
		secret, err := base64.StdEncoding.DecodeString(encoded)
		if err != nil || len(secret) == 0 {
			return nil, submissionReceipt{}, contract.NewFault(contract.ValidationFailed, "invalid test webhook secret", "Report the command and binary version.")
		}
		if !f.webhookSkipActivation {
			update, _ := normalized.Input.(*platform.EnvironmentUpdateInput)
			name := platform.DefaultWebhookSecretName
			if update != nil && update.WebhookSecretName != nil && *update.WebhookSecretName != "" {
				name = *update.WebhookSecretName
			}
			namespace := vault.Namespace{App: "api", Environment: "production", Name: name}
			f.secretWrite++
			f.secretNamespaces = append(f.secretNamespaces, namespace)
			if f.secretPayloads == nil {
				f.secretPayloads = map[string]string{}
			}
			f.secretPayloads[namespace.String()] = string(secret)
			activated = true
		}
	}
	op := f.webhookOperation(kind, target, key)
	f.webhookOps[key] = storedWebhookOperation{hash: hash, op: op}
	if update, ok := normalized.Input.(*platform.EnvironmentUpdateInput); ok {
		f.applyWebhookEnvironment(update)
	}
	raw, err := json.Marshal(op)
	return raw, submissionReceipt{WebhookSecretActivated: activated}, err
}

func (f *definitionInventoryBackend) webhookOperation(kind, target, key string) contract.Operation {
	now := time.Unix(1700000000, 0).UTC()
	return contract.Operation{
		ID: contract.OperationID(fmt.Sprintf("op_%032x", len(f.submissions))), Kind: kind, Target: target,
		Actor: contract.Actor{Kind: "operator", Subject: "definition-test"},
		State: contract.Succeeded, Attempt: 1, LastSequence: 1, CreatedAt: now, UpdatedAt: now,
	}
}

func webhookInputHash(canonical json.RawMessage) string {
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

func (f *definitionInventoryBackend) previewWebhook(kind, target string, input json.RawMessage, key string) (json.RawMessage, error) {
	normalized, err := platform.Default().Normalize(kind, target, input)
	if err != nil {
		return nil, err
	}
	if key != "" {
		if stored, ok := f.webhookOps[key]; ok {
			if stored.hash != webhookInputHash(normalized.Canonical) {
				return nil, contract.NewFault(contract.Conflict, "idempotency key already binds different inputs", "Use the original inputs to recover the existing operation; use a new key only for deliberately new work.")
			}
			preview := platform.Preview{
				Kind: kind, Target: target, DryRun: true, Replay: true, Operation: &stored.op,
				Summary: "Replay the accepted operation " + string(stored.op.ID) + "; the same submission already exists.",
			}
			return json.Marshal(preview)
		}
	}
	return f.preview(kind, target)
}

func (f *definitionInventoryBackend) applyWebhookEnvironment(update *platform.EnvironmentUpdateInput) {
	if update == nil {
		return
	}
	var envelope struct {
		Items []platform.Environment `json:"items"`
	}
	if err := json.Unmarshal(f.inventory["environments"], &envelope); err != nil {
		return
	}
	for index := range envelope.Items {
		env := &envelope.Items[index]
		if env.App != update.App || env.Name != update.Environment || env.Revision == nil {
			continue
		}
		next := platform.ApplyEnvironmentPatch(env.Revision.RevisionState, *update)
		platform.ResolveWebhookSecretName(&next)
		env.Revision.RevisionState = next
		env.Revision.Number++
		if data, err := json.Marshal(map[string]any{"items": envelope.Items}); err == nil {
			f.inventory["environments"] = data
		}
		return
	}
}

func mustWebhookJSON(value any) json.RawMessage {
	raw, _ := json.Marshal(value)
	return raw
}

func (f *definitionInventoryBackend) SetSecret(_ context.Context, namespace vault.Namespace, value []byte, _ uint64) (vault.Metadata, error) {
	f.secretWrite++
	f.secretNamespaces = append(f.secretNamespaces, namespace)
	if f.secretPayloads == nil {
		f.secretPayloads = map[string]string{}
	}
	f.secretPayloads[namespace.String()] = string(value)
	return vault.Metadata{}, nil
}

func (f *definitionInventoryBackend) GetSecretMetadata(context.Context, vault.Namespace) (vault.Metadata, error) {
	f.secretRead++
	return vault.Metadata{}, contract.NewFault(contract.NotFound, "secret not found", "Set the secret first.")
}

func (f *definitionInventoryBackend) submissionsOfKind(kind string) []definitionSubmission {
	var out []definitionSubmission
	for _, submission := range f.submissions {
		if submission.Kind == kind {
			out = append(out, submission)
		}
	}
	return out
}

func definitionBackendForEnvironment(t *testing.T, current platform.Environment) *definitionInventoryBackend {
	t.Helper()
	rawEnv, err := json.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	return &definitionInventoryBackend{inventory: map[string]json.RawMessage{
		"environments": json.RawMessage(`{"items":[` + string(rawEnv) + `]}`),
		"apps":         json.RawMessage(`{"items":[{"id":"app_1","name":"` + current.App + `","image_repository":"` + current.App + `"}]}`),
	}}
}

func writeDefinitionFile(t *testing.T, raw string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "eregion.toml")
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func resourceDefinitionEnvironment() platform.Environment {
	return platform.Environment{
		App: "api", Name: "production", Node: "main",
		RevisionID: platform.RevisionID("erv_cccccccccccccccccccccccccccccccc"),
		Revision: &platform.EnvironmentRevision{RevisionState: platform.RevisionState{
			Source:  platform.SourceBinding{URL: "https://github.com/acme/api"},
			Runtime: platform.RuntimeSpec{Node: "main", HealthType: "http", HealthPath: "/", HealthTimeoutSeconds: 30},
			Config:  map[string]string{"A": "1"},
		}},
	}
}

const resourceDefinition = `version = 2
app = "api"
node = "main"

[source]

[config]
A = "1"

[[resources]]
name = "db"
image = "postgres:17"
port = 5432

[resources.health]
command = ["pg_isready"]

[resources.secret_env]
POSTGRES_PASSWORD = "DATABASE_PASSWORD"

[[resources]]
name = "cache"
image = "redis:7"
attach = false

[resources.health]
command = ["redis-cli", "ping"]
`

func TestDefinitionEnvironmentPlannerKeepsResourceDeclarations(t *testing.T) {
	path := writeDefinitionFile(t, resourceDefinition)
	backend := definitionBackendForEnvironment(t, resourceDefinitionEnvironment())
	exit, stdout, stderr := runGitHubAuthCommand(t, backend, "env", "update", "api", "production", "--from", path, "--dry-run", "--json")
	if exit != 0 || stderr != "" || backend.definitionPlans != 1 || len(backend.submissions) != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s plans=%d submissions=%+v", exit, stdout, stderr, backend.definitionPlans, backend.submissions)
	}
	definition, err := appdef.Parse([]byte(resourceDefinition))
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := definition.Resolve("production")
	if err != nil {
		t.Fatal(err)
	}
	resolved.App = "api"
	specs, err := resolved.ResourceApplySpecs()
	if err != nil {
		t.Fatal(err)
	}
	attached, set := resolved.ResourceAttachments()
	if len(specs) != 2 || specs[0].Name != "db" || specs[1].Name != "cache" || !set || len(attached) != 1 || attached[0] != "db" {
		t.Fatalf("specs=%+v attached=%v", specs, attached)
	}
	if backend.definitionPatch.Resources == nil || len(*backend.definitionPatch.Resources) != 1 || (*backend.definitionPatch.Resources)[0] != "db" {
		t.Fatalf("patch resources = %+v", backend.definitionPatch.Resources)
	}
}

func TestDefinitionEnvironmentPlannerKeepsEmptyCollectionRemovals(t *testing.T) {
	raw := `version = 2
app = "api"
node = "main"
secret_names = []
resources = []
[source]
[config]
`
	current := resourceDefinitionEnvironment()
	current.Revision.Processes = []platform.ProcessSpec{{Name: "worker", Kind: "worker", Command: "./worker", Replicas: 1}}
	current.Revision.SecretNames = []string{"DATABASE_URL"}
	current.Revision.Resources = []string{"db"}
	backend := definitionBackendForEnvironment(t, current)
	path := writeDefinitionFile(t, raw)
	exit, stdout, stderr := runGitHubAuthCommand(t, backend, "env", "update", "api", "production", "--from", path, "--dry-run", "--json")
	if exit != 0 || stderr != "" || backend.definitionPlans != 1 || len(backend.submissions) != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	patch := backend.definitionPatch
	if patch.Processes == nil || len(*patch.Processes) != 0 || patch.Config == nil || len(*patch.Config) != 0 || patch.SecretNames == nil || len(*patch.SecretNames) != 0 || patch.Resources == nil || len(*patch.Resources) != 0 {
		t.Fatalf("removal patch = %+v", patch)
	}
	encoded, err := json.Marshal(patch)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"processes", "secret_names", "resources"} {
		if string(fields[name]) != "[]" {
			t.Fatalf("%s = %s", name, fields[name])
		}
	}
	if string(fields["config"]) != "{}" {
		t.Fatalf("config = %s", fields["config"])
	}
}

func TestDefinitionCreateAttachesOnlyAttachTrueResources(t *testing.T) {
	path := writeDefinitionFile(t, resourceDefinition)
	cmd := definitionCommand(t, setupApp)
	if err := cmd.ParseFlags([]string{"--from", path, "--no-deploy"}); err != nil {
		t.Fatal(err)
	}
	a := &app{secrets: &secrets.Registry{}}
	ans, err := a.readSetup(cmd, setupApp)
	if err != nil {
		t.Fatal(err)
	}
	defer ans.clearSecrets()
	spec, err := ans.environmentSpec(cmd)
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Resources) != 1 || spec.Resources[0] != "db" {
		t.Fatalf("first revision resources = %v, want only db", spec.Resources)
	}
	if len(ans.ResourceSpecs) != 2 {
		t.Fatalf("declared resource specs = %+v", ans.ResourceSpecs)
	}
}

func TestAppCreateFromFileDryRunDoesNotWriteSecrets(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "eregion.toml")
	raw := `version = 2
app = "api"
node = "main"
secret_names = ["DATABASE_URL"]
[source]

[[resources]]
name = "db"
image = "postgres:17"

[resources.health]
command = ["pg_isready"]

[[resources]]
name = "cache"
image = "redis:7"
attach = false

[resources.health]
command = ["redis-cli", "ping"]
`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	backend := &definitionInventoryBackend{inventory: map[string]json.RawMessage{
		"apps": json.RawMessage(`{"items":[]}`),
	}}
	cmd := &cobra.Command{Use: "create"}
	cmd.Flags().String("name", "", "app")
	cmd.Flags().String("environment", "production", "environment")
	cmd.Flags().Bool("no-deploy", false, "no deploy")
	cmd.Flags().Bool("detach", false, "detach")
	addSetupFlags(cmd, true)
	if err := cmd.ParseFlags([]string{
		"--from", path, "--name", "api", "--no-deploy", "--detach", "--dry-run",
	}); err != nil {
		t.Fatal(err)
	}
	a := &app{secrets: &secrets.Registry{}, backend: backend}
	res, err := a.appCreate(cmd, nil)
	if err != nil {
		t.Fatal(err)
	}
	data, ok := res.data.(setupResult)
	if !ok || !data.DryRun {
		t.Fatalf("dry-run result = %T %+v", res.data, res.data)
	}
	if backend.secretWrite != 0 {
		t.Fatalf("dry run wrote %d secret values", backend.secretWrite)
	}
	if backend.submitted != platform.KindAppCreate || !backend.dryRun {
		t.Fatalf("preview submission = %q dry=%t", backend.submitted, backend.dryRun)
	}
	if len(data.SecretNames) != 1 || data.SecretNames[0] != "DATABASE_URL" {
		t.Fatalf("secret names = %v", data.SecretNames)
	}
	if len(data.ResolvedResourceIDs) != 0 {
		t.Fatalf("dry run accepted resource operations: %v", data.ResolvedResourceIDs)
	}
	if applies := backend.submissionsOfKind(platform.KindResourceApply); len(applies) != 0 {
		t.Fatalf("dry run submitted resource applies: %+v", applies)
	}
}

func TestAppExportRejectsUnknownRevisionResource(t *testing.T) {
	revision := &platform.EnvironmentRevision{RevisionState: platform.RevisionState{
		Source:    platform.SourceBinding{URL: "https://github.com/acme/api"},
		Resources: []string{"db"},
	}}
	current := platform.Environment{App: "api", Name: "production", Node: "main", Revision: revision}
	backend := definitionBackendForEnvironment(t, current)
	cmd := &cobra.Command{Use: "export"}
	cmd.Flags().String("out", "", "out")
	a := &app{secrets: &secrets.Registry{}, backend: backend}
	if _, err := a.appExport(cmd, []string{"api", "production"}); err == nil {
		t.Fatal("exported a revision whose resource definition is missing")
	} else if fault := contract.Classify(err); fault.Code != contract.PreconditionFailed {
		t.Fatalf("unknown-resource error = %v", fault.Code)
	} else if !strings.Contains(fault.Hint, "db") || !strings.Contains(fault.Hint, "unavailable") {
		t.Fatalf("unknown-resource refusal = %v", fault.Hint)
	}
}

func TestResourceApplyRejectsDuplicateMountDestination(t *testing.T) {
	a := &app{meta: make(registry)}
	var apply *cobra.Command
	for _, parent := range a.resourceSpecs() {
		for _, child := range parent.children {
			if strings.HasPrefix(child.use, "apply ") {
				apply = a.command(child)
			}
		}
	}
	if apply == nil {
		t.Fatal("resource apply command not found")
	}
	if err := apply.ParseFlags([]string{
		"--image", "postgres:17",
		"--mount", "data:/var/lib/postgresql/data",
		"--mount", "backup:/var/lib/postgresql/data/.",
		"--health-command", "pg_isready",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.resourceApply(apply, []string{"api", "production", "db"}); err == nil {
		t.Fatal("accepted two mounts with the same container destination")
	} else if contract.Classify(err).Code != contract.ValidationFailed {
		t.Fatalf("duplicate mount error = %v", contract.Classify(err).Code)
	} else if !strings.Contains(err.Error(), "destination") {
		t.Fatalf("duplicate mount refusal = %v", err)
	}
}

func TestAppExportUsesAuthoritativeCustomRepository(t *testing.T) {
	current := platform.Environment{
		App: "api", Name: "production", Node: "main",
		Revision: &platform.EnvironmentRevision{RevisionState: platform.RevisionState{
			Source:    platform.SourceBinding{URL: "https://github.com/acme/api"},
			Runtime:   platform.RuntimeSpec{Node: "main", HealthType: "http", HealthPath: "/", HealthTimeoutSeconds: 30},
			Processes: []platform.ProcessSpec{{Name: "web", Kind: "web", Replicas: 1, ContainerPort: 8080}},
		}},
	}
	rawEnv, _ := json.Marshal(current)
	backend := &definitionInventoryBackend{inventory: map[string]json.RawMessage{
		"environments": json.RawMessage(`{"items":[` + string(rawEnv) + `]}`),
		"apps":         json.RawMessage(`{"items":[{"id":"app_1","name":"api","image_repository":"mbvlabs/custom-api"}]}`),
	}}
	a := &app{secrets: &secrets.Registry{}, backend: backend}
	cmd := &cobra.Command{Use: "export"}
	cmd.Flags().String("out", "", "out")
	res, err := a.appExport(cmd, []string{"api", "production"})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(res.data)
	if err != nil {
		t.Fatal(err)
	}
	var data struct {
		Document string `json:"document"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(data.Document, `image_repository = "mbvlabs/custom-api"`) {
		t.Fatalf("export did not use the authoritative repository:\n%s", data.Document)
	}
}

func TestAppExportFailsWhenAuthoritativeAppLookupIsMissing(t *testing.T) {
	current := platform.Environment{
		App: "api", Name: "production", Node: "main",
		Revision: &platform.EnvironmentRevision{RevisionState: platform.RevisionState{
			Source:    platform.SourceBinding{URL: "https://github.com/acme/api"},
			Runtime:   platform.RuntimeSpec{Node: "main", HealthType: "http", HealthPath: "/", HealthTimeoutSeconds: 30},
			Processes: []platform.ProcessSpec{{Name: "web", Kind: "web", Replicas: 1, ContainerPort: 8080}},
		}},
	}
	rawEnv, _ := json.Marshal(current)
	backend := &definitionInventoryBackend{inventory: map[string]json.RawMessage{
		"environments": json.RawMessage(`{"items":[` + string(rawEnv) + `]}`),
	}}
	a := &app{secrets: &secrets.Registry{}, backend: backend}
	cmd := &cobra.Command{Use: "export"}
	if _, err := a.appExport(cmd, []string{"api", "production"}); err == nil || contract.Classify(err).Code != contract.NotFound {
		t.Fatalf("missing app identity = %v, want not_found", err)
	}
}

func TestAppExportProducesARoundTrippableDefinition(t *testing.T) {
	revision := &platform.EnvironmentRevision{RevisionState: platform.RevisionState{
		Source: platform.SourceBinding{
			URL: "https://github.com/acme/api", Ref: "main",
		},
		Processes: []platform.ProcessSpec{
			{Name: "web", Kind: "web", Replicas: 1, ContainerPort: 8080},
		},
		Config:      map[string]string{"APP_ENV": "production"},
		SecretNames: []string{"DATABASE_URL"},
		Resources:   []string{"db"},
		Runtime:     platform.RuntimeSpec{Node: "main", HealthType: "http", HealthPath: "/", HealthTimeoutSeconds: 30},
		AutoDeploy:  true, WebhookSecretName: "HOOK",
	}}
	current := platform.Environment{App: "api", Name: "production", ImageRepository: "mbvlabs/api", Node: "main", BuildNode: "builder", Revision: revision}
	rawEnv, _ := json.Marshal(current)
	rawDomains, _ := json.Marshal(map[string]any{"items": []platform.DomainBinding{
		{Domain: "api.example.com", App: "api", Environment: "production", Primary: true, CheckPath: "/readyz"},
	}})
	rawResources, _ := json.Marshal(map[string]any{"items": []platform.Resource{
		{
			Name: "db", App: "api", Environment: "production",
			Spec: platform.ResourceSpec{
				Image: "postgres:17", Port: 5432,
				Env:       map[string]string{"POSTGRES_DB": "app"},
				SecretEnv: map[string]string{"POSTGRES_PASSWORD": "DATABASE_URL"},
				Mounts:    []platform.ResourceMount{{Name: "data", Destination: "/var/lib/postgresql/data"}},
				Health:    platform.ResourceHealthSpec{Type: "exec", Command: []string{"pg_isready"}, TimeoutSeconds: 30, IntervalMS: 500},
			},
		},
		{
			Name: "cache", App: "api", Environment: "production",
			Spec: platform.ResourceSpec{
				Image:  "redis:7",
				Health: platform.ResourceHealthSpec{Type: "exec", Command: []string{"redis-cli", "ping"}, TimeoutSeconds: 30, IntervalMS: 500},
			},
		},
	}})
	backend := &definitionInventoryBackend{inventory: map[string]json.RawMessage{
		"environments": json.RawMessage(`{"items":[` + string(rawEnv) + `]}`),
		"domains":      rawDomains,
		"resources":    rawResources,
		"apps":         json.RawMessage(`{"items":[{"id":"app_1","name":"api","image_repository":"mbvlabs/api"}]}`),
	}}
	cmd := &cobra.Command{Use: "export"}
	cmd.Flags().String("out", "", "out")
	a := &app{secrets: &secrets.Registry{}, backend: backend}
	res, err := a.appExport(cmd, []string{"api", "production"})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(res.data)
	if err != nil {
		t.Fatal(err)
	}
	var data struct {
		Format   string `json:"format"`
		Document string `json:"document"`
	}
	if err := json.Unmarshal(encoded, &data); err != nil {
		t.Fatal(err)
	}
	if data.Format != "toml" {
		t.Fatalf("format = %q", data.Format)
	}
	for _, want := range []string{`name = "db"`, `name = "cache"`, `attach = false`, `destination = "/var/lib/postgresql/data"`, `auto_deploy = true`, `webhook_secret_name = "HOOK"`} {
		if !strings.Contains(data.Document, want) {
			t.Fatalf("export missing %q:\n%s", want, data.Document)
		}
	}
	definition, err := appdef.Parse([]byte(data.Document))
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := definition.Resolve("production")
	if err != nil {
		t.Fatal(err)
	}
	patch, changed, _, err := resolved.UpdatePatch(current, []platform.DomainBinding{
		{Domain: "api.example.com", Primary: true, CheckPath: "/readyz"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(changed) != 0 || patch.Node != nil || patch.Source != nil || patch.Processes != nil || patch.AutoDeploy != nil || patch.WebhookSecretName != nil {
		t.Fatalf("export did not round-trip: changed=%v patch=%+v", changed, patch)
	}
	if patch.Resources != nil {
		t.Fatalf("export round-trip rewrote resource attachments: %+v", patch.Resources)
	}
	cmd.Flags().Bool("inspect", true, "inspect")
	inspection, err := a.appExport(cmd, []string{"api", "production"})
	if err != nil {
		t.Fatal(err)
	}
	if len(inspection.next) != 1 || strings.Contains(inspection.next[0], "--from") {
		t.Fatalf("unsafe inspection hint: %v", inspection.next)
	}
	inspectionData, _ := json.Marshal(inspection.data)
	var snapshot struct {
		Document string `json:"document"`
	}
	if err := json.Unmarshal(inspectionData, &snapshot); err != nil {
		t.Fatal(err)
	}
	if _, err := appdef.Parse([]byte(snapshot.Document)); err == nil {
		t.Fatal("inspection snapshot is applicable")
	}
	specs, err := resolved.ResourceApplySpecs()
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 2 {
		t.Fatalf("round-trip resource specs = %+v", specs)
	}
	if _, err := resolved.CreateSpec(nil); err == nil {
		t.Fatal("create accepted enabled automation")
	}
	createResolved := *resolved
	createResolved.Automation = nil
	created, err := createResolved.CreateSpec(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(created.Resources) != 1 || created.Resources[0] != "db" {
		t.Fatalf("round-trip first revision resources = %v", created.Resources)
	}
	if !strings.Contains(data.Document, `build_node = "builder"`) {
		t.Fatalf("build node is missing:\n%s", data.Document)
	}

	out := filepath.Join(t.TempDir(), "eregion.toml")
	cmd = &cobra.Command{Use: "export"}
	cmd.Flags().String("out", "", "out")
	if err := cmd.ParseFlags([]string{"--out", out}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.appExport(cmd, []string{"api", "production"}); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if string(written) != data.Document {
		t.Fatal("file export differs from the document result")
	}
	if _, err := a.appExport(cmd, []string{"api", "production"}); err == nil {
		t.Fatal("overwrote an existing export file")
	}
}
