package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/appdef"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/secrets"
	"github.com/mbvlabs/eregion/internal/vault"
	"github.com/spf13/cobra"
)

const testDefinition = `version = 1
app = "api"
environment = "production"
node = "main"
secret_names = ["DATABASE_URL"]

[source]
kind = "git"
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

[[domains]]
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
	raw := `version = 1
app = "api"
environment = "production"
node = "main"
[source]
kind = "image"
image = "ghcr.io/acme/api:1"
[[domains]]
name = "api.example.com"
primary = true
check_path = "/healthz"
[[domains]]
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

func TestDefinitionFileRejectsSecretValuesAndConflictingFlags(t *testing.T) {
	dir := t.TempDir()
	secretPath := filepath.Join(dir, "secret.toml")
	if err := os.WriteFile(secretPath, []byte("version = 1\n[secrets]\nDATABASE_URL = \"postgres://user:hunter2@example/db\"\n"), 0o600); err != nil {
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
	inventory   map[string]json.RawMessage
	submitted   string
	input       json.RawMessage
	dryRun      bool
	secretWrite int
	submissions []definitionSubmission
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
	return json.Marshal(preview)
}

func (f *definitionInventoryBackend) SubmitAction(_ context.Context, kind, target string, input json.RawMessage, _, confirm string, dryRun bool) (json.RawMessage, error) {
	f.record(kind, target, input, confirm, dryRun)
	return f.preview(kind, target)
}

func (f *definitionInventoryBackend) SubmitActionWithSecrets(_ context.Context, kind, target string, input json.RawMessage, _, confirm string, dryRun bool, _ map[string]string) (json.RawMessage, error) {
	f.record(kind, target, input, confirm, dryRun)
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

func (f *definitionInventoryBackend) SetSecret(context.Context, vault.Namespace, []byte, uint64) (vault.Metadata, error) {
	f.secretWrite++
	return vault.Metadata{}, nil
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

func TestPlanDomainChangesOrdersPromotionBeforeRemoval(t *testing.T) {
	current := []platform.DomainBinding{
		{Domain: "old.example.com", Primary: true},
		{Domain: "alias.example.com", Primary: false, CheckPath: "/healthz"},
	}
	want := []appdef.Domain{
		{Name: "alias.example.com", Primary: true, CheckPath: "/readyz"},
		{Name: "new.example.com", Primary: false},
	}
	plan, err := planDomainChanges(want, current)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Adds) != 1 || plan.Adds[0].Name != "new.example.com" {
		t.Fatalf("adds = %+v", plan.Adds)
	}
	if len(plan.Updates) != 1 || plan.Updates[0].Name != "alias.example.com" || !plan.Updates[0].Primary {
		t.Fatalf("updates = %+v", plan.Updates)
	}
	if len(plan.Removes) != 1 || plan.Removes[0].Domain != "old.example.com" {
		t.Fatalf("removes = %+v", plan.Removes)
	}
	if _, err := planDomainChanges(nil, current); err == nil {
		t.Fatal("allowed removal of every domain")
	}
	if err := validateDomainRemovalConfirmation(plan.Removes, nil); contract.Classify(err).Code != contract.ConfirmationRequired {
		t.Fatalf("missing domain confirmation = %v", err)
	}
	if err := validateDomainRemovalConfirmation(plan.Removes, []string{"old.example.com"}); err != nil {
		t.Fatal(err)
	}
}

func TestEnvUpdateFromFileRemovesNonPrimaryDomainWithConfirmation(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "eregion.toml")
	raw := `version = 1
app = "api"
environment = "production"
node = "main"

[source]
kind = "image"
image = "ghcr.io/acme/api:1"

[[domains]]
name = "api.example.com"
primary = true
`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	current := platform.Environment{
		App: "api", Name: "production", Node: "main",
		Domains: []string{"api.example.com", "alias.example.com"}, PrimaryDomain: "api.example.com",
		Revision: &platform.EnvironmentRevision{RevisionState: platform.RevisionState{
			Source:  platform.SourceBinding{Kind: "image", Image: "ghcr.io/acme/api:1"},
			Runtime: platform.RuntimeSpec{Node: "main", HealthType: "http", HealthPath: "/", HealthTimeoutSeconds: 30},
		}},
	}
	rawEnv, _ := json.Marshal(current)
	rawDomains, _ := json.Marshal(map[string]any{"items": []platform.DomainBinding{
		{Domain: "api.example.com", App: "api", Environment: "production", Primary: true},
		{Domain: "alias.example.com", App: "api", Environment: "production"},
	}})
	newBackend := func() *definitionInventoryBackend {
		return &definitionInventoryBackend{inventory: map[string]json.RawMessage{
			"environments": json.RawMessage(`{"items":[` + string(rawEnv) + `]}`),
			"domains":      rawDomains,
		}}
	}
	newCommand := func(flags ...string) *cobra.Command {
		cmd := &cobra.Command{Use: "update"}
		addEnvironmentUpdateFlags(cmd)
		if err := cmd.ParseFlags(append([]string{"--from", path}, flags...)); err != nil {
			t.Fatal(err)
		}
		return cmd
	}

	backend := newBackend()
	a := &app{secrets: &secrets.Registry{}, backend: backend}
	if _, err := a.envUpdate(newCommand(), []string{"api", "production"}); err == nil {
		t.Fatal("removed a domain without --confirm-domain")
	} else if contract.Classify(err).Code != contract.ConfirmationRequired {
		t.Fatalf("missing confirmation = %v", contract.Classify(err).Code)
	}
	if removes := backend.submissionsOfKind(platform.KindDomainRemove); len(removes) != 0 {
		t.Fatalf("unconfirmed domain removal was submitted: %+v", removes)
	}

	backend = newBackend()
	a = &app{secrets: &secrets.Registry{}, backend: backend}
	res, err := a.envUpdate(newCommand("--confirm-domain", "alias.example.com"), []string{"api", "production"})
	if err != nil {
		t.Fatal(err)
	}
	data, ok := res.data.(definitionUpdateResult)
	if !ok || len(data.DomainOperations) != 1 {
		t.Fatalf("domain operations = %+v", res.data)
	}
	removes := backend.submissionsOfKind(platform.KindDomainRemove)
	if len(removes) != 1 || removes[0].Confirm != "alias.example.com" {
		t.Fatalf("domain removals = %+v", removes)
	}
	if updates := backend.submissionsOfKind(platform.KindEnvironmentUpdate); len(updates) != 0 {
		t.Fatalf("domain-only change wrote a revision: %+v", updates)
	}
}

func TestEnvUpdateFromFileDetectsOmittedDomainRemoval(t *testing.T) {
	path := filepath.Join(t.TempDir(), "eregion.toml")
	raw := `version = 1
app = "api"
environment = "production"
node = "main"
[source]
kind = "image"
image = "ghcr.io/acme/api:1"
`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	current := platform.Environment{
		App: "api", Name: "production", Node: "main",
		RevisionID:    platform.RevisionID("erv_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"),
		Domains:       []string{"api.example.com"},
		PrimaryDomain: "api.example.com",
		Revision: &platform.EnvironmentRevision{RevisionState: platform.RevisionState{
			Source: platform.SourceBinding{Kind: "image", Image: "ghcr.io/acme/api:1"},
		}},
	}
	rawEnv, _ := json.Marshal(current)
	rawDomains, _ := json.Marshal(map[string]any{"items": []platform.DomainBinding{
		{Domain: "api.example.com", App: "api", Environment: "production", Primary: true},
	}})
	backend := &definitionInventoryBackend{inventory: map[string]json.RawMessage{
		"environments": json.RawMessage(`{"items":[` + string(rawEnv) + `]}`),
		"domains":      rawDomains,
	}}
	cmd := &cobra.Command{Use: "update"}
	addEnvironmentUpdateFlags(cmd)
	if err := cmd.ParseFlags([]string{"--from", path, "--dry-run"}); err != nil {
		t.Fatal(err)
	}
	a := &app{secrets: &secrets.Registry{}, backend: backend}
	if _, err := a.envUpdate(cmd, []string{"api", "production"}); err == nil {
		t.Fatal("an omitted domain was silently retained")
	} else if contract.Classify(err).Code != contract.ValidationFailed {
		t.Fatalf("domain-removal error = %v", contract.Classify(err).Code)
	}
}

func TestEnvUpdateFromFileRejectsWebhookFlags(t *testing.T) {
	path := filepath.Join(t.TempDir(), "eregion.toml")
	if err := os.WriteFile(path, []byte(testDefinition), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := &cobra.Command{Use: "update"}
	addEnvironmentUpdateFlags(cmd)
	if err := cmd.ParseFlags([]string{"--from", path, "--auto-deploy"}); err != nil {
		t.Fatal(err)
	}
	a := &app{secrets: &secrets.Registry{}}
	if _, err := a.envUpdate(cmd, []string{"api", "production"}); err == nil {
		t.Fatal("accepted --from with a webhook flag")
	} else if contract.Classify(err).Code != contract.ValidationFailed {
		t.Fatalf("webhook flag error = %v", contract.Classify(err).Code)
	}
}

func TestEnvUpdateFromFileDryRunBuildsTheSupportedPatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "eregion.toml")
	raw := `version = 1
app = "api"
environment = "production"
node = "main"
[config]
B = "3"
[source]
kind = "git"
url = "https://github.com/acme/api"
ref = "next"
[[processes]]
name = "web"
kind = "web"
container_port = 8080
replicas = 2
`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	revisionID := platform.RevisionID("erv_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	current := platform.Environment{
		App: "api", Name: "production", Node: "main", RevisionID: revisionID,
		Revision: &platform.EnvironmentRevision{ID: revisionID, RevisionState: platform.RevisionState{
			Source: platform.SourceBinding{
				Kind: "git", URL: "https://github.com/acme/api", Ref: "main", Builder: platform.DefaultBuilder,
			},
			Runtime:     platform.RuntimeSpec{Node: "main", HealthType: "http", HealthPath: "/", HealthTimeoutSeconds: 30},
			Processes:   []platform.ProcessSpec{{Name: "web", Kind: "web", Replicas: 1, ContainerPort: 8080}},
			Config:      map[string]string{"A": "1", "B": "2"},
			SecretNames: []string{"DATABASE_URL"},
		}},
	}
	rawEnv, _ := json.Marshal(current)
	backend := &definitionInventoryBackend{inventory: map[string]json.RawMessage{
		"environments": json.RawMessage(`{"items":[` + string(rawEnv) + `]}`),
	}}
	cmd := &cobra.Command{Use: "update"}
	addEnvironmentUpdateFlags(cmd)
	if err := cmd.ParseFlags([]string{"--from", path, "--dry-run"}); err != nil {
		t.Fatal(err)
	}
	a := &app{secrets: &secrets.Registry{}, backend: backend}
	res, err := a.envUpdate(cmd, []string{"api", "production"})
	if err != nil {
		t.Fatal(err)
	}
	data, ok := res.data.(definitionUpdateResult)
	if !ok {
		t.Fatalf("result = %T %+v", res.data, res.data)
	}
	if !data.DryRun || data.OperationID != "" {
		t.Fatalf("dry run result = %+v", data)
	}
	if got := strings.Join(data.Changed, ","); got != "source,processes,config,secret_names" {
		t.Fatalf("changed = %s", got)
	}
	if data.Patch.SecretNames == nil || len(*data.Patch.SecretNames) != 0 {
		t.Fatalf("secret names were not treated as full desired state: %+v", data.Patch.SecretNames)
	}
	if data.Patch.ExpectedRevisionID != revisionID {
		t.Fatalf("expected revision fence = %q, want %q", data.Patch.ExpectedRevisionID, revisionID)
	}
	if data.Patch.Source == nil || data.Patch.Source.Ref != "next" ||
		data.Patch.Source.URL != "https://github.com/acme/api" {
		t.Fatalf("source patch = %+v", data.Patch.Source)
	}
	if data.Patch.Processes == nil || len(*data.Patch.Processes) != 1 || (*data.Patch.Processes)[0].Replicas != 2 {
		t.Fatalf("process patch = %+v", data.Patch.Processes)
	}
	if data.Patch.Config == nil || (*data.Patch.Config)["B"] != "3" || len(*data.Patch.Config) != 1 {
		t.Fatalf("config patch = %+v", data.Patch.Config)
	}
	if backend.submitted != platform.KindEnvironmentUpdate || !backend.dryRun {
		t.Fatalf("preview submission = %q dry=%t", backend.submitted, backend.dryRun)
	}
	if strings.Contains(data.Plain, "No work was accepted") == false {
		t.Fatalf("dry-run text = %q", data.Plain)
	}
}

func definitionBackendForEnvironment(t *testing.T, current platform.Environment) *definitionInventoryBackend {
	t.Helper()
	rawEnv, err := json.Marshal(current)
	if err != nil {
		t.Fatal(err)
	}
	return &definitionInventoryBackend{inventory: map[string]json.RawMessage{
		"environments": json.RawMessage(`{"items":[` + string(rawEnv) + `]}`),
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
			Source:  platform.SourceBinding{Kind: "image", Image: "ghcr.io/acme/api:1"},
			Runtime: platform.RuntimeSpec{Node: "main", HealthType: "http", HealthPath: "/", HealthTimeoutSeconds: 30},
			Config:  map[string]string{"A": "1"},
		}},
	}
}

const resourceDefinition = `version = 1
app = "api"
environment = "production"
node = "main"

[source]
kind = "image"
image = "ghcr.io/acme/api:1"

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

func TestEnvUpdateFromFileAppliesDeclaredResources(t *testing.T) {
	path := writeDefinitionFile(t, resourceDefinition)
	backend := definitionBackendForEnvironment(t, resourceDefinitionEnvironment())
	cmd := &cobra.Command{Use: "update"}
	addEnvironmentUpdateFlags(cmd)
	if err := cmd.ParseFlags([]string{"--from", path}); err != nil {
		t.Fatal(err)
	}
	a := &app{secrets: &secrets.Registry{}, backend: backend}
	res, err := a.envUpdate(cmd, []string{"api", "production"})
	if err != nil {
		t.Fatal(err)
	}
	data, ok := res.data.(definitionUpdateResult)
	if !ok {
		t.Fatalf("result = %T %+v", res.data, res.data)
	}
	if data.OperationID == "" {
		t.Fatal("the environment revision was not accepted")
	}
	if data.Patch.Resources == nil || strings.Join(*data.Patch.Resources, ",") != "db" {
		t.Fatalf("revision attachments = %+v, want only db", data.Patch.Resources)
	}
	if len(data.ResourceOperations) != 2 {
		t.Fatalf("resource operations = %v", data.ResourceOperations)
	}
	applies := backend.submissionsOfKind(platform.KindResourceApply)
	if len(applies) != 2 {
		t.Fatalf("resource apply submissions = %+v", applies)
	}
	inputs := map[string]platform.ResourceApplyInput{}
	for _, submission := range applies {
		if submission.DryRun {
			t.Fatalf("resource apply was submitted as a dry run: %+v", submission)
		}
		var input platform.ResourceApplyInput
		if err := json.Unmarshal(submission.Input, &input); err != nil {
			t.Fatal(err)
		}
		inputs[input.Name] = input
	}
	db, ok := inputs["db"]
	if !ok {
		t.Fatalf("db resource apply missing: %+v", inputs)
	}
	if db.App != "api" || db.Environment != "production" || db.Image != "postgres:17" ||
		db.Port != 5432 || db.SecretEnv["POSTGRES_PASSWORD"] != "DATABASE_PASSWORD" ||
		len(db.Health.Command) != 1 || db.Health.Command[0] != "pg_isready" {
		t.Fatalf("db apply input = %+v", db)
	}
	cache, ok := inputs["cache"]
	if !ok || cache.Image != "redis:7" || cache.Health.Command[1] != "ping" {
		t.Fatalf("cache apply input = %+v", cache)
	}
}

func TestEnvUpdateFromFileDryRunDoesNotProvisionResources(t *testing.T) {
	path := writeDefinitionFile(t, resourceDefinition)
	backend := definitionBackendForEnvironment(t, resourceDefinitionEnvironment())
	cmd := &cobra.Command{Use: "update"}
	addEnvironmentUpdateFlags(cmd)
	if err := cmd.ParseFlags([]string{"--from", path, "--dry-run"}); err != nil {
		t.Fatal(err)
	}
	a := &app{secrets: &secrets.Registry{}, backend: backend}
	res, err := a.envUpdate(cmd, []string{"api", "production"})
	if err != nil {
		t.Fatal(err)
	}
	data, ok := res.data.(definitionUpdateResult)
	if !ok || !data.DryRun {
		t.Fatalf("dry-run result = %T %+v", res.data, res.data)
	}
	if len(data.ResourceOperations) != 0 {
		t.Fatalf("dry run accepted resource operations: %v", data.ResourceOperations)
	}
	if applies := backend.submissionsOfKind(platform.KindResourceApply); len(applies) != 0 {
		t.Fatalf("dry run submitted resource applies: %+v", applies)
	}
	if backend.submitted != platform.KindEnvironmentUpdate || !backend.dryRun {
		t.Fatalf("dry run last submission = %q dry=%t", backend.submitted, backend.dryRun)
	}
}

func TestEnvUpdateFromFileDryRunDoesNotProvisionAttachFalseOnlyResource(t *testing.T) {
	raw := `version = 1
app = "api"
environment = "production"
node = "main"

[source]
kind = "image"
image = "ghcr.io/acme/api:1"

[config]
A = "1"

[[resources]]
name = "cache"
image = "redis:7"
attach = false

[resources.health]
command = ["redis-cli", "ping"]
`
	path := writeDefinitionFile(t, raw)
	backend := definitionBackendForEnvironment(t, resourceDefinitionEnvironment())
	cmd := &cobra.Command{Use: "update"}
	addEnvironmentUpdateFlags(cmd)
	if err := cmd.ParseFlags([]string{"--from", path, "--dry-run"}); err != nil {
		t.Fatal(err)
	}
	a := &app{secrets: &secrets.Registry{}, backend: backend}
	res, err := a.envUpdate(cmd, []string{"api", "production"})
	if err != nil {
		t.Fatal(err)
	}
	data, ok := res.data.(definitionUpdateResult)
	if !ok || !data.DryRun {
		t.Fatalf("dry-run result = %T %+v", res.data, res.data)
	}
	if len(data.ResourceOperations) != 0 {
		t.Fatalf("dry run accepted resource operations: %v", data.ResourceOperations)
	}
	if len(backend.submissions) != 0 {
		t.Fatalf("dry run submitted actions: %+v", backend.submissions)
	}
}

func TestEnvUpdateFromFileAppliesAttachFalseResourceWithoutRevisionChange(t *testing.T) {
	raw := `version = 1
app = "api"
environment = "production"
node = "main"

[source]
kind = "image"
image = "ghcr.io/acme/api:1"

[config]
A = "1"

[[resources]]
name = "cache"
image = "redis:7"
attach = false

[resources.health]
command = ["redis-cli", "ping"]
`
	path := writeDefinitionFile(t, raw)
	backend := definitionBackendForEnvironment(t, resourceDefinitionEnvironment())
	cmd := &cobra.Command{Use: "update"}
	addEnvironmentUpdateFlags(cmd)
	if err := cmd.ParseFlags([]string{"--from", path}); err != nil {
		t.Fatal(err)
	}
	a := &app{secrets: &secrets.Registry{}, backend: backend}
	res, err := a.envUpdate(cmd, []string{"api", "production"})
	if err != nil {
		t.Fatal(err)
	}
	data, ok := res.data.(definitionUpdateResult)
	if !ok {
		t.Fatalf("result = %T %+v", res.data, res.data)
	}
	if data.OperationID != "" {
		t.Fatalf("an unchanged revision was submitted again: %s", data.OperationID)
	}
	if len(data.ResourceOperations) != 1 {
		t.Fatalf("attach=false resource was not provisioned: %+v", data)
	}
	if envUpdates := backend.submissionsOfKind(platform.KindEnvironmentUpdate); len(envUpdates) != 0 {
		t.Fatalf("unchanged revision submitted an update: %+v", envUpdates)
	}
	applies := backend.submissionsOfKind(platform.KindResourceApply)
	if len(applies) != 1 {
		t.Fatalf("resource applies = %+v", applies)
	}
	var input platform.ResourceApplyInput
	if err := json.Unmarshal(applies[0].Input, &input); err != nil {
		t.Fatal(err)
	}
	if input.Name != "cache" || input.Image != "redis:7" {
		t.Fatalf("resource apply input = %+v", input)
	}
	if !strings.Contains(data.Plain, "No revision changes") {
		t.Fatalf("plain result = %q", data.Plain)
	}
}

func TestEnvUpdateFromFileRequiresConfirmationForResourceRemoval(t *testing.T) {
	raw := `version = 1
app = "api"
environment = "production"
node = "main"

[source]
kind = "image"
image = "ghcr.io/acme/api:1"

[config]
A = "1"
`
	path := writeDefinitionFile(t, raw)
	current := resourceDefinitionEnvironment()
	current.Revision.Resources = []string{"db"}
	backend := definitionBackendForEnvironment(t, current)
	cmd := &cobra.Command{Use: "update"}
	addEnvironmentUpdateFlags(cmd)
	if err := cmd.ParseFlags([]string{"--from", path}); err != nil {
		t.Fatal(err)
	}
	a := &app{secrets: &secrets.Registry{}, backend: backend}
	if _, err := a.envUpdate(cmd, []string{"api", "production"}); err == nil {
		t.Fatal("a resource dependency removal was accepted without confirmation")
	} else if contract.Classify(err).Code != contract.ConfirmationRequired {
		t.Fatalf("removal error = %v", contract.Classify(err).Code)
	}
	for _, submission := range backend.submissions {
		if !submission.DryRun {
			t.Fatalf("an unconfirmed removal reached submission: %+v", submission)
		}
	}

	backend = definitionBackendForEnvironment(t, current)
	cmd = &cobra.Command{Use: "update"}
	addEnvironmentUpdateFlags(cmd)
	if err := cmd.ParseFlags([]string{"--from", path, "--confirm", "api/production"}); err != nil {
		t.Fatal(err)
	}
	a = &app{secrets: &secrets.Registry{}, backend: backend}
	res, err := a.envUpdate(cmd, []string{"api", "production"})
	if err != nil {
		t.Fatal(err)
	}
	data, ok := res.data.(definitionUpdateResult)
	if !ok {
		t.Fatalf("result = %T %+v", res.data, res.data)
	}
	if data.Patch.Resources == nil || len(*data.Patch.Resources) != 0 {
		t.Fatalf("confirmed removal patch = %+v", data.Patch.Resources)
	}
	if data.Patch.Confirm != "api/production" {
		t.Fatalf("confirmation was not carried into the patch: %q", data.Patch.Confirm)
	}
	if data.OperationID == "" {
		t.Fatal("confirmed removal was not accepted")
	}
	if applies := backend.submissionsOfKind(platform.KindResourceApply); len(applies) != 0 {
		t.Fatalf("removal applied resources: %+v", applies)
	}
}

func TestEnvUpdateFromFileRemovingLastCollectionsSubmitsEmptyArrays(t *testing.T) {
	raw := `version = 1
app = "api"
environment = "production"
node = "main"
secret_names = []
resources = []

[source]
kind = "image"
image = "ghcr.io/acme/api:1"

[config]
`
	path := writeDefinitionFile(t, raw)
	current := resourceDefinitionEnvironment()
	current.Revision.Processes = []platform.ProcessSpec{{Name: "worker", Kind: "worker", Command: "./worker", Replicas: 1}}
	current.Revision.SecretNames = []string{"DATABASE_URL"}
	current.Revision.Resources = []string{"db"}
	current.Revision.Config = map[string]string{"A": "1"}
	backend := definitionBackendForEnvironment(t, current)
	cmd := &cobra.Command{Use: "update"}
	addEnvironmentUpdateFlags(cmd)
	if err := cmd.ParseFlags([]string{"--from", path, "--confirm", "api/production"}); err != nil {
		t.Fatal(err)
	}
	a := &app{secrets: &secrets.Registry{}, backend: backend}
	res, err := a.envUpdate(cmd, []string{"api", "production"})
	if err != nil {
		t.Fatal(err)
	}
	data, ok := res.data.(definitionUpdateResult)
	if !ok || data.OperationID == "" {
		t.Fatalf("result = %T %+v", res.data, res.data)
	}
	if got := strings.Join(data.Changed, ","); got != "processes,config,secret_names,resources" {
		t.Fatalf("changed = %s", got)
	}
	if data.Patch.Processes == nil || len(*data.Patch.Processes) != 0 ||
		data.Patch.Config == nil || len(*data.Patch.Config) != 0 ||
		data.Patch.SecretNames == nil || len(*data.Patch.SecretNames) != 0 ||
		data.Patch.Resources == nil || len(*data.Patch.Resources) != 0 {
		t.Fatalf("removal patch = %+v", data.Patch)
	}

	updates := backend.submissionsOfKind(platform.KindEnvironmentUpdate)
	if len(updates) != 2 {
		t.Fatalf("environment update submissions = %+v, want preview and accepted action", updates)
	}
	var accepted definitionSubmission
	for _, submission := range updates {
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(submission.Input, &fields); err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"processes", "secret_names", "resources"} {
			if got := string(fields[name]); got != "[]" {
				t.Fatalf("%s on the wire = %s, want an explicit empty array; input = %s", name, got, submission.Input)
			}
		}
		if got := string(fields["config"]); got != "{}" {
			t.Fatalf("config on the wire = %s, want an explicit empty object; input = %s", got, submission.Input)
		}
		if !submission.DryRun {
			accepted = submission
		}
	}
	if accepted.Input == nil {
		t.Fatal("the accepted environment update was not captured")
	}

	normalized, err := platform.Default().Normalize(platform.KindEnvironmentUpdate, "env/api/production", accepted.Input)
	if err != nil {
		t.Fatalf("the node refused the CLI's accepted input: %v", err)
	}
	patch, ok := normalized.Input.(*platform.EnvironmentUpdateInput)
	if !ok {
		t.Fatalf("normalized input = %T", normalized.Input)
	}
	next := platform.ApplyEnvironmentPatch(platform.CloneRevisionState(current.Revision.RevisionState), *patch)
	if err := platform.NormalizeRevisionState(&next); err != nil {
		t.Fatal(err)
	}
	if len(next.Processes) != 0 || len(next.Config) != 0 || len(next.SecretNames) != 0 || len(next.Resources) != 0 {
		t.Fatalf("the accepted revision retained removed collections: %+v", next)
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
	raw := `version = 1
app = "api"
environment = "production"
node = "main"
secret_names = ["DATABASE_URL"]
[source]
kind = "image"
image = "ghcr.io/acme/api:1"

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
		Source:    platform.SourceBinding{Kind: "image", Image: "ghcr.io/acme/api:1"},
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

func TestAppExportProducesARoundTrippableDefinition(t *testing.T) {
	revision := &platform.EnvironmentRevision{RevisionState: platform.RevisionState{
		Source: platform.SourceBinding{
			Kind: "image", Image: "ghcr.io/acme/api:1",
		},
		Runtime: platform.RuntimeSpec{Node: "main", HealthType: "http", HealthPath: "/healthz", HealthTimeoutSeconds: 30},
		Processes: []platform.ProcessSpec{
			{Name: "web", Kind: "web", Replicas: 1, ContainerPort: 8080},
		},
		Config:      map[string]string{"APP_ENV": "production"},
		SecretNames: []string{"DATABASE_URL"},
		Resources:   []string{"db"},
	}}
	current := platform.Environment{App: "api", Name: "production", Node: "main", BuildNode: "builder", Revision: revision}
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
	for _, want := range []string{`name = "db"`, `name = "cache"`, `attach = false`, `destination = "/var/lib/postgresql/data"`} {
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
	if len(changed) != 0 || patch.Node != nil || patch.Source != nil || patch.Processes != nil {
		t.Fatalf("export did not round-trip: changed=%v patch=%+v", changed, patch)
	}
	if patch.Resources != nil {
		t.Fatalf("export round-trip rewrote resource attachments: %+v", patch.Resources)
	}
	specs, err := resolved.ResourceApplySpecs()
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 2 {
		t.Fatalf("round-trip resource specs = %+v", specs)
	}
	created, err := resolved.CreateSpec(nil)
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
