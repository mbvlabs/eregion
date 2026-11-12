package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/secrets"
	"github.com/spf13/cobra"
)

func TestAcquireGitHubAuthTokenUsesGHInMemory(t *testing.T) {
	installFakeGH(t)
	token := "gho_fake-read-token-value"
	t.Setenv("GH_FAKE_TOKEN", token)

	value, err := acquireGitHubAuthToken(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if string(value) != token {
		t.Fatalf("token = %q", value)
	}
	clearBytes(value)

	calls := fakeGHCalls(t)
	if len(calls) != 1 {
		t.Fatalf("gh calls = %+v", calls)
	}
	if strings.Join(calls[0], " ") != "auth token --hostname github.com" {
		t.Fatalf("gh call = %+v", calls[0])
	}
	if strings.Contains(strings.Join(calls[0], " "), token) {
		t.Fatal("the token reached gh argv")
	}
	raw, err := os.ReadFile(os.Getenv("GH_FAKE_ARGS"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(token)) {
		t.Fatal("the token reached the fake gh call log")
	}
}

func TestAcquireGitHubAuthTokenMissingGH(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := acquireGitHubAuthToken(context.Background())
	if contract.Classify(err).Code != contract.MissingTool {
		t.Fatalf("error = %v", err)
	}
	if !strings.Contains(err.Error(), "gh") || !strings.Contains(err.Error(), "PATH") {
		t.Fatalf("error = %v", err)
	}
}

func TestAcquireGitHubAuthTokenSignedOut(t *testing.T) {
	installFakeGH(t)
	token := "gho_must-not-leak"
	t.Setenv("GH_FAKE_TOKEN", token)
	t.Setenv("GH_FAKE_TOKEN_EXIT", "1")
	_, err := acquireGitHubAuthToken(context.Background())
	fault := contract.Classify(err)
	if fault.Code != contract.PreconditionFailed {
		t.Fatalf("error = %v", err)
	}
	if !strings.Contains(fault.Hint, "auth login") {
		t.Fatalf("hint = %q", fault.Hint)
	}
	if strings.Contains(fault.Message, token) || strings.Contains(fault.Hint, token) {
		t.Fatal("the failed gh output reached the error")
	}
}

func TestApplyGitHubAuthRejectsNonGitHubSource(t *testing.T) {
	for _, rawURL := range []string{
		"https://gitlab.com/acme/api",
		"https://evil.example/acme/api",
	} {
		t.Run(rawURL, func(t *testing.T) {
			cmd := &cobra.Command{Use: "create"}
			addSetupFlags(cmd, true)
			if err := cmd.ParseFlags([]string{"--github-auth", "--git-url", rawURL}); err != nil {
				t.Fatal(err)
			}
			_, err := applyGitHubAuthToSpecs(cmd, &setupAnswers{}, []platform.EnvironmentSpec{{
				Name: "production", Source: platform.SourceBinding{URL: rawURL},
			}})
			if contract.Classify(err).Code != contract.ValidationFailed {
				t.Fatalf("error = %v", err)
			}
			if !strings.Contains(err.Error(), "GitHub") {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestApplyGitHubAuthNamesTheCredential(t *testing.T) {
	cmd := &cobra.Command{Use: "create"}
	addSetupFlags(cmd, true)
	if err := cmd.ParseFlags([]string{"--github-auth"}); err != nil {
		t.Fatal(err)
	}
	ans := &setupAnswers{SecretValues: map[string][]byte{}}
	specs, err := applyGitHubAuthToSpecs(cmd, ans, []platform.EnvironmentSpec{
		{Name: "production", Source: platform.SourceBinding{URL: "https://github.com/acme/api"}},
		{Name: "staging", Source: platform.SourceBinding{URL: "https://github.com/acme/api"}},
		{Name: "images", Source: platform.SourceBinding{URL: "https://github.com/acme/api"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if ans.CredentialName != defaultGitReadCredentialName {
		t.Fatalf("credential name = %q", ans.CredentialName)
	}
	for index, spec := range specs {
		want := ""
		if spec.Source.URL != "" {
			want = defaultGitReadCredentialName
		}
		if spec.Source.CredentialSecretName != want {
			t.Fatalf("spec %d credential = %q, want %q", index, spec.Source.CredentialSecretName, want)
		}
	}

	cmd = &cobra.Command{Use: "create"}
	addSetupFlags(cmd, true)
	if err := cmd.ParseFlags([]string{"--github-auth", "--git-credential-name", "PRIVATE_GIT"}); err != nil {
		t.Fatal(err)
	}
	specs, err = applyGitHubAuthToSpecs(cmd, ans, specs)
	if err != nil {
		t.Fatal(err)
	}
	if specs[0].Source.CredentialSecretName != "PRIVATE_GIT" || specs[1].Source.CredentialSecretName != "PRIVATE_GIT" {
		t.Fatalf("specs = %+v", specs)
	}
}

func TestApplyGitHubAuthRejectsConflictingCredentialInputs(t *testing.T) {
	cmd := &cobra.Command{Use: "create"}
	addSetupFlags(cmd, true)
	if err := cmd.ParseFlags([]string{"--github-auth", "--git-credential-file", "/tmp/token"}); err != nil {
		t.Fatal(err)
	}
	_, err := applyGitHubAuthToSpecs(cmd, &setupAnswers{}, []platform.EnvironmentSpec{{
		Name: "production", Source: platform.SourceBinding{URL: "https://github.com/acme/api"},
	}})
	if contract.Classify(err).Code != contract.Usage {
		t.Fatalf("error = %v", err)
	}
}

func TestReadUpdateSourceGitHubAuthUsesGitHubSource(t *testing.T) {
	base := platform.SourceBinding{
		URL: "https://github.com/acme/api", CredentialSecretName: "GIT_READ_TOKEN",
	}
	cmd := &cobra.Command{Use: "update"}
	addEnvironmentUpdateFlags(cmd)
	if err := cmd.ParseFlags([]string{"--github-auth"}); err != nil {
		t.Fatal(err)
	}
	source, changed, credential, name, err := (&app{}).readUpdateSource(cmd, base, platform.Environment{})
	if err != nil {
		t.Fatal(err)
	}
	if !changed || len(credential) != 0 || name != "GIT_READ_TOKEN" {
		t.Fatalf("changed=%v credential=%q name=%q", changed, credential, name)
	}
	if source.CredentialSecretName != "GIT_READ_TOKEN" || source.URL != base.URL {
		t.Fatalf("source = %+v", source)
	}

	cmd = &cobra.Command{Use: "update"}
	addEnvironmentUpdateFlags(cmd)
	if err := cmd.ParseFlags([]string{"--github-auth", "--git-url", "https://gitlab.com/acme/api"}); err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err := (&app{}).readUpdateSource(cmd, base, platform.Environment{}); contract.Classify(err).Code != contract.ValidationFailed {
		t.Fatalf("non-GitHub error = %v", err)
	}
}

func TestEnvUpdateGitHubAuthWritesCredential(t *testing.T) {
	installFakeGH(t)
	token := "gho_update-read-token"
	t.Setenv("GH_FAKE_TOKEN", token)
	current := platform.Environment{
		ID: "env_1", App: "api", Name: "production", Node: "main",
		Revision: &platform.EnvironmentRevision{RevisionState: platform.RevisionState{
			Source: platform.SourceBinding{
				URL: "https://github.com/acme/api", CredentialSecretName: "GIT_READ_TOKEN",
			},
			SecretNames: []string{"DATABASE_URL"},
			Runtime:     platform.RuntimeSpec{Node: "main", HealthType: "http", HealthPath: "/", HealthTimeoutSeconds: 30},
			Processes: []platform.ProcessSpec{
				{Name: "web", Kind: "web", Replicas: 1, ContainerPort: 8080},
			},
		}},
	}
	raw, err := json.Marshal(map[string]any{"items": []platform.Environment{current}})
	if err != nil {
		t.Fatal(err)
	}
	backend := &definitionInventoryBackend{inventory: map[string]json.RawMessage{"environments": raw}}
	a := newApp()
	a.backend = backend
	var stdout, stderr bytes.Buffer
	exit := runApp(context.Background(), []string{
		"env", "update", "api", "production", "--github-auth", "--json",
	}, panicReader{}, &stdout, &stderr, &secrets.Registry{}, a)
	if exit != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout.String(), stderr.String())
	}
	if got := backend.secretPayloads["api/production/GIT_READ_TOKEN"]; got != token {
		t.Fatalf("secret payload = %q; writes=%+v", got, backend.secretPayloads)
	}
	if strings.Contains(stdout.String(), token) || strings.Contains(stderr.String(), token) {
		t.Fatal("the token reached command output")
	}
	updates := backend.submissionsOfKind(platform.KindEnvironmentUpdate)
	var update *definitionSubmission
	for index := range updates {
		if !updates[index].DryRun {
			update = &updates[index]
		}
	}
	if update == nil {
		t.Fatalf("submissions = %+v", backend.submissions)
	}
	if bytes.Contains(update.Input, []byte(token)) {
		t.Fatal("the token reached the operation input")
	}
	var input platform.EnvironmentUpdateInput
	if err := json.Unmarshal(updates[0].Input, &input); err != nil {
		t.Fatal(err)
	}
	if input.Source == nil || input.Source.CredentialSecretName != "GIT_READ_TOKEN" {
		t.Fatalf("input source = %+v", input.Source)
	}
	calls := fakeGHCalls(t)
	if len(calls) != 1 || strings.Join(calls[0], " ") != "auth token --hostname github.com" {
		t.Fatalf("gh calls = %+v", calls)
	}
}

func TestEnvUpdateGitHubAuthReplayDoesNotReadGH(t *testing.T) {
	installFakeGH(t)
	current := platform.Environment{
		ID: "env_1", App: "api", Name: "production", Node: "main",
		Revision: &platform.EnvironmentRevision{RevisionState: platform.RevisionState{
			Source: platform.SourceBinding{
				URL: "https://github.com/acme/api", CredentialSecretName: "GIT_READ_TOKEN",
			},
			Runtime: platform.RuntimeSpec{Node: "main", HealthType: "http", HealthPath: "/", HealthTimeoutSeconds: 30},
			Processes: []platform.ProcessSpec{
				{Name: "web", Kind: "web", Replicas: 1, ContainerPort: 8080},
			},
		}},
	}
	raw, err := json.Marshal(map[string]any{"items": []platform.Environment{current}})
	if err != nil {
		t.Fatal(err)
	}
	backend := &definitionInventoryBackend{inventory: map[string]json.RawMessage{"environments": raw}, previewReplay: true}
	a := newApp()
	a.backend = backend
	var stdout, stderr bytes.Buffer
	exit := runApp(context.Background(), []string{
		"env", "update", "api", "production", "--github-auth", "--idempotency-key", "replay-key", "--json",
	}, panicReader{}, &stdout, &stderr, &secrets.Registry{}, a)
	if exit != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout.String(), stderr.String())
	}
	if calls := fakeGHCalls(t); len(calls) != 0 {
		t.Fatalf("replay called gh: %+v", calls)
	}
	if backend.secretWrite != 0 {
		t.Fatalf("replay wrote secrets: %+v", backend.secretNamespaces)
	}
}

func TestAppCreateGitHubAuthWritesTokenToBothEnvironments(t *testing.T) {
	installFakeGH(t)
	token := "gho_app-read-token"
	t.Setenv("GH_FAKE_TOKEN", token)
	backend := &definitionInventoryBackend{inventory: map[string]json.RawMessage{}}
	a := newApp()
	a.backend = backend
	var stdout, stderr bytes.Buffer
	exit := runApp(context.Background(), []string{
		"app", "create", "--name", "api", "--node", "main",
		"--git-url", "https://github.com/acme/api",
		"--github-auth", "--no-deploy", "--json",
	}, panicReader{}, &stdout, &stderr, &secrets.Registry{}, a)
	if exit != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout.String(), stderr.String())
	}
	want := map[string]string{
		"api/production/GIT_READ_TOKEN": token,
		"api/staging/GIT_READ_TOKEN":    token,
	}
	if len(backend.secretNamespaces) != len(want) {
		t.Fatalf("secret writes = %+v", backend.secretNamespaces)
	}
	for namespace, value := range want {
		if got := backend.secretPayloads[namespace]; got != value {
			t.Fatalf("secret %s = %q; writes=%+v", namespace, got, backend.secretPayloads)
		}
	}
	if strings.Contains(stdout.String(), token) || strings.Contains(stderr.String(), token) {
		t.Fatal("the token reached command output")
	}
	creates := backend.submissionsOfKind(platform.KindAppCreate)
	var accepted *definitionSubmission
	for index := range creates {
		if !creates[index].DryRun {
			accepted = &creates[index]
		}
	}
	if accepted == nil {
		t.Fatalf("submissions = %+v", backend.submissions)
	}
	if bytes.Contains(accepted.Input, []byte(token)) {
		t.Fatal("the token reached the operation input")
	}
	var input platform.AppCreateInput
	if err := json.Unmarshal(accepted.Input, &input); err != nil {
		t.Fatal(err)
	}
	for _, spec := range input.Environments {
		if spec.Source.CredentialSecretName != "GIT_READ_TOKEN" {
			t.Fatalf("credential name = %q", spec.Source.CredentialSecretName)
		}
	}
	calls := fakeGHCalls(t)
	if len(calls) != 1 || strings.Join(calls[0], " ") != "auth token --hostname github.com" {
		t.Fatalf("gh calls = %+v", calls)
	}
}

func TestEnvCreateGitHubAuthWritesCredential(t *testing.T) {
	installFakeGH(t)
	token := "gho_env-read-token"
	t.Setenv("GH_FAKE_TOKEN", token)
	backend := &definitionInventoryBackend{inventory: map[string]json.RawMessage{
		"apps": json.RawMessage(`{"items":[{"id":"app_1","name":"api"}]}`),
	}}
	a := newApp()
	a.backend = backend
	var stdout, stderr bytes.Buffer
	exit := runApp(context.Background(), []string{
		"env", "create", "--app", "api", "--name", "production", "--node", "main",
		"--git-url", "https://github.com/acme/api",
		"--github-auth", "--no-deploy", "--json",
	}, panicReader{}, &stdout, &stderr, &secrets.Registry{}, a)
	if exit != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout.String(), stderr.String())
	}
	if got := backend.secretPayloads["api/production/GIT_READ_TOKEN"]; got != token {
		t.Fatalf("secret payload = %q; writes=%+v", got, backend.secretPayloads)
	}
	if strings.Contains(stdout.String(), token) || strings.Contains(stderr.String(), token) {
		t.Fatal("the token reached command output")
	}
	creates := backend.submissionsOfKind(platform.KindEnvironmentCreate)
	var accepted *definitionSubmission
	for index := range creates {
		if !creates[index].DryRun {
			accepted = &creates[index]
		}
	}
	if accepted == nil {
		t.Fatalf("submissions = %+v", backend.submissions)
	}
	if bytes.Contains(accepted.Input, []byte(token)) {
		t.Fatal("the token reached the operation input")
	}
	if calls := fakeGHCalls(t); len(calls) != 1 {
		t.Fatalf("gh calls = %+v", calls)
	}
}

func TestAppCreateFromFileGitHubAuthUsesFileCredentialName(t *testing.T) {
	installFakeGH(t)
	token := "gho_file-read-token"
	t.Setenv("GH_FAKE_TOKEN", token)
	path := filepath.Join(t.TempDir(), "eregion.toml")
	if err := os.WriteFile(path, []byte(testDefinition), 0o600); err != nil {
		t.Fatal(err)
	}
	backend := &definitionInventoryBackend{inventory: map[string]json.RawMessage{}}
	a := newApp()
	a.backend = backend
	var stdout, stderr bytes.Buffer
	exit := runApp(context.Background(), []string{
		"app", "create", "--from", path, "--github-auth", "--no-deploy", "--json",
	}, panicReader{}, &stdout, &stderr, &secrets.Registry{}, a)
	if exit != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout.String(), stderr.String())
	}
	want := map[string]string{
		"api/production/GIT_READ_TOKEN": token,
		"api/staging/GIT_READ_TOKEN":    token,
	}
	for namespace, value := range want {
		if got := backend.secretPayloads[namespace]; got != value {
			t.Fatalf("secret %s = %q; writes=%+v", namespace, got, backend.secretPayloads)
		}
	}
	if strings.Contains(stdout.String(), token) || strings.Contains(stderr.String(), token) {
		t.Fatal("the token reached command output")
	}
	if calls := fakeGHCalls(t); len(calls) != 1 {
		t.Fatalf("gh calls = %+v", calls)
	}
}

func TestEnvUpdateGitHubAuthRejectsRuntimeSecretCollision(t *testing.T) {
	installFakeGH(t)
	t.Setenv("GH_FAKE_TOKEN", "gho_collision-token")
	current := githubAuthEnvUpdateEnvironment("", []string{"GIT_READ_TOKEN"}, "")
	backend := githubAuthEnvUpdateBackend(t, current, false)
	before := string(backend.inventory["environments"])

	exit, stdout, stderr := runGitHubAuthCommand(t, backend,
		"env", "update", "api", "production", "--github-auth", "--json")

	assertGitHubAuthRejected(t, exit, stdout, stderr, "runtime secret")
	if calls := fakeGHCalls(t); len(calls) != 0 {
		t.Fatalf("rejected collision called gh: %+v", calls)
	}
	if backend.secretWrite != 0 {
		t.Fatalf("rejected collision wrote secrets: %+v", backend.secretNamespaces)
	}
	if submissions := backend.submissionsOfKind(platform.KindEnvironmentUpdate); len(submissions) != 0 {
		t.Fatalf("rejected collision submitted work: %+v", submissions)
	}
	if string(backend.inventory["environments"]) != before {
		t.Fatal("rejected collision changed the environment inventory")
	}
}

func TestEnvUpdateGitHubAuthRejectsExplicitRuntimeSecretName(t *testing.T) {
	installFakeGH(t)
	t.Setenv("GH_FAKE_TOKEN", "gho_collision-token")
	current := githubAuthEnvUpdateEnvironment("GIT_READ_TOKEN", []string{"DATABASE_PASSWORD"}, "")
	backend := githubAuthEnvUpdateBackend(t, current, false)

	exit, stdout, stderr := runGitHubAuthCommand(t, backend,
		"env", "update", "api", "production", "--github-auth",
		"--git-credential-name", "DATABASE_PASSWORD", "--json")

	assertGitHubAuthRejected(t, exit, stdout, stderr, "runtime secret")
	if calls := fakeGHCalls(t); len(calls) != 0 {
		t.Fatalf("rejected collision called gh: %+v", calls)
	}
	if backend.secretWrite != 0 {
		t.Fatalf("rejected collision wrote secrets: %+v", backend.secretNamespaces)
	}
}

func TestEnvUpdateGitHubAuthRejectsWebhookSecretCollision(t *testing.T) {
	installFakeGH(t)
	t.Setenv("GH_FAKE_TOKEN", "gho_collision-token")
	current := githubAuthEnvUpdateEnvironment("", nil, "GIT_READ_TOKEN")
	backend := githubAuthEnvUpdateBackend(t, current, false)

	exit, stdout, stderr := runGitHubAuthCommand(t, backend,
		"env", "update", "api", "production", "--github-auth", "--json")

	assertGitHubAuthRejected(t, exit, stdout, stderr, "webhook secret")
	if calls := fakeGHCalls(t); len(calls) != 0 {
		t.Fatalf("rejected collision called gh: %+v", calls)
	}
	if backend.secretWrite != 0 {
		t.Fatalf("rejected collision wrote secrets: %+v", backend.secretNamespaces)
	}
}

func TestEnvUpdateCredentialFileStillReplacesTheSourceCredential(t *testing.T) {
	current := githubAuthEnvUpdateEnvironment("GIT_READ_TOKEN", []string{"DATABASE_URL"}, "EREGION_WEBHOOK_SECRET")
	backend := githubAuthEnvUpdateBackend(t, current, false)
	tokenFile := writeSecretInput(t, "file-read-token")

	exit, stdout, stderr := runGitHubAuthCommand(t, backend,
		"env", "update", "api", "production", "--git-credential-file", tokenFile, "--json")
	if exit != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	if got := backend.secretPayloads["api/production/GIT_READ_TOKEN"]; got != "file-read-token" {
		t.Fatalf("credential = %q; writes=%+v", got, backend.secretPayloads)
	}
	if calls := fakeGHCalls(t); len(calls) != 0 {
		t.Fatalf("explicit-file path called gh: %+v", calls)
	}
}

func TestEnvUpdateCredentialFileRejectsRuntimeSecretCollisionBeforeRead(t *testing.T) {
	current := githubAuthEnvUpdateEnvironment("GIT_READ_TOKEN", []string{"DATABASE_PASSWORD"}, "")
	backend := githubAuthEnvUpdateBackend(t, current, false)

	exit, stdout, stderr := runGitHubAuthCommand(t, backend,
		"env", "update", "api", "production",
		"--git-credential-file", filepath.Join(t.TempDir(), "missing-token"),
		"--git-credential-name", "DATABASE_PASSWORD", "--json")

	assertGitHubAuthRejected(t, exit, stdout, stderr, "runtime secret")
	if backend.secretWrite != 0 {
		t.Fatalf("rejected collision wrote secrets: %+v", backend.secretNamespaces)
	}
	if submissions := backend.submissionsOfKind(platform.KindEnvironmentUpdate); len(submissions) != 0 {
		t.Fatalf("rejected collision submitted work: %+v", submissions)
	}
}

func TestEnvUpdateFromFileRejectsEffectiveSourceCredentialAlias(t *testing.T) {
	for _, test := range []struct {
		name       string
		credential string
		secrets    string
		fragment   string
	}{
		{name: "runtime secret", credential: "GIT_READ_TOKEN", secrets: `["GIT_READ_TOKEN"]`, fragment: "source.credential_secret"},
		{name: "preserved webhook", credential: "EREGION_WEBHOOK_SECRET", secrets: `["DATABASE_URL"]`, fragment: "source.credential_secret"},
	} {
		t.Run(test.name, func(t *testing.T) {
			current := githubAuthEnvUpdateEnvironment("GIT_READ_TOKEN", []string{"DATABASE_URL"}, "EREGION_WEBHOOK_SECRET")
			current.Revision.AutoDeploy = true
			backend := githubAuthEnvUpdateBackend(t, current, false)
			before := string(backend.inventory["environments"])
			path := writeUpdateDefinition(t, test.credential, test.secrets)

			exit, stdout, stderr := runGitHubAuthCommand(t, backend,
				"env", "update", "api", "production", "--from", path, "--dry-run", "--json")

			assertGitHubAuthRejected(t, exit, stdout, stderr, test.fragment)
			if backend.secretWrite != 0 || backend.secretRead != 0 {
				t.Fatalf("rejected definition touched vault secrets: writes=%+v reads=%d", backend.secretNamespaces, backend.secretRead)
			}
			if submissions := backend.submissionsOfKind(platform.KindEnvironmentUpdate); len(submissions) != 0 {
				t.Fatalf("rejected definition previewed or submitted work: %+v", submissions)
			}
			if string(backend.inventory["environments"]) != before {
				t.Fatal("rejected definition changed the environment inventory")
			}
			if strings.Contains(stdout, "gho_") || strings.Contains(stderr, "gho_") {
				t.Fatal("a stored token reached command output")
			}
		})
	}
}

func TestEnvUpdateFromFilePreservesUnchangedSourceCredential(t *testing.T) {
	current := githubAuthEnvUpdateEnvironment("GIT_READ_TOKEN", []string{"DATABASE_URL"}, "EREGION_WEBHOOK_SECRET")
	current.Revision.AutoDeploy = true
	backend := githubAuthEnvUpdateBackend(t, current, false)
	path := writeUpdateDefinition(t, "GIT_READ_TOKEN", `["DATABASE_URL", "REDIS_URL"]`)

	exit, stdout, stderr := runGitHubAuthCommand(t, backend,
		"env", "update", "api", "production", "--from", path, "--dry-run", "--json")
	if exit != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	if backend.definitionPlans != 1 {
		t.Fatalf("expected one server plan, got %d", backend.definitionPlans)
	}
	patch := backend.definitionPatch
	if patch.Source != nil {
		t.Fatalf("an unchanged source was patched: %+v", patch.Source)
	}
	if patch.SecretNames == nil || len(*patch.SecretNames) != 2 {
		t.Fatalf("secret patch = %+v", patch.SecretNames)
	}
	if backend.secretWrite != 0 {
		t.Fatalf("file update wrote secrets: %+v", backend.secretNamespaces)
	}
}

func TestEnvUpdateWebhookEnableRejectsEffectiveWebhookAlias(t *testing.T) {
	installFakeGH(t)
	t.Setenv("GH_FAKE_TOKEN", "gho_webhook-token")
	current := githubAuthEnvUpdateEnvironment("PROD_GIT", nil, "")
	backend := githubAuthEnvUpdateBackend(t, current, false)

	exit, stdout, stderr := runGitHubAuthCommand(t, backend,
		"env", "update", "api", "production",
		"--github-auth", "--git-credential-name", "EREGION_WEBHOOK_SECRET",
		"--auto-deploy", "--dry-run", "--json")

	assertGitHubAuthRejected(t, exit, stdout, stderr, "webhook secret")
	if calls := fakeGHCalls(t); len(calls) != 0 {
		t.Fatalf("rejected webhook alias called gh: %+v", calls)
	}
	if backend.secretWrite != 0 {
		t.Fatalf("rejected webhook alias wrote secrets: %+v", backend.secretNamespaces)
	}
}

func TestEnvUpdateWebhookRotationRejectsPreservedWebhookAlias(t *testing.T) {
	installFakeGH(t)
	t.Setenv("GH_FAKE_TOKEN", "gho_webhook-token")
	current := githubAuthEnvUpdateEnvironment("PROD_GIT", nil, "EREGION_WEBHOOK_SECRET")
	current.Revision.AutoDeploy = true
	backend := githubAuthEnvUpdateBackend(t, current, false)

	exit, stdout, stderr := runGitHubAuthCommand(t, backend,
		"env", "update", "api", "production",
		"--github-auth", "--git-credential-name", "EREGION_WEBHOOK_SECRET",
		"--auto-deploy", "--rotate-webhook-secret", "--dry-run", "--json")

	assertGitHubAuthRejected(t, exit, stdout, stderr, "webhook secret")
	if calls := fakeGHCalls(t); len(calls) != 0 {
		t.Fatalf("rejected webhook alias called gh: %+v", calls)
	}
	if backend.secretWrite != 0 {
		t.Fatalf("rejected webhook alias wrote secrets: %+v", backend.secretNamespaces)
	}
}

func (f *definitionInventoryBackend) NodeInfo(context.Context) (rpcNodeInfoResult, error) {
	return rpcNodeInfoResult{Node: "main", Hostname: "main.example.com", Platform: "linux/amd64"}, nil
}

func writeUpdateDefinition(t *testing.T, credential, secretNames string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "eregion.toml")
	raw := fmt.Sprintf(`
version = 2
app = "api"
node = "main"
secret_names = %s

[source]
url = "https://github.com/acme/api"
ref = "main"
credential_secret = "%s"
builder = ""

[runtime]
health_path = "/"

[[processes]]
name = "web"
kind = "web"
container_port = 8080
replicas = 1
`, secretNames, credential)
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestAppCreateGitHubAuthRejectsRuntimeSecretCollision(t *testing.T) {
	installFakeGH(t)
	t.Setenv("GH_FAKE_TOKEN", "gho_collision-token")
	secretFile := writeSecretInput(t, "runtime-secret-value")
	backend := &definitionInventoryBackend{inventory: map[string]json.RawMessage{}}

	exit, stdout, stderr := runGitHubAuthCommand(t, backend,
		"app", "create", "--name", "api", "--node", "main",
		"--git-url", "https://github.com/acme/api",
		"--github-auth", "--secret", "GIT_READ_TOKEN="+secretFile,
		"--no-deploy", "--json")

	assertGitHubAuthRejected(t, exit, stdout, stderr, "git read credential")
	if calls := fakeGHCalls(t); len(calls) != 0 {
		t.Fatalf("rejected collision called gh: %+v", calls)
	}
	if backend.secretWrite != 0 {
		t.Fatalf("rejected collision wrote secrets: %+v", backend.secretNamespaces)
	}
	if submissions := backend.submissionsOfKind(platform.KindAppCreate); len(submissions) != 0 {
		t.Fatalf("rejected collision submitted work: %+v", submissions)
	}
}

func TestValidateEnvironmentGitCredentialRolesRejectsWebhookAlias(t *testing.T) {
	err := validateEnvironmentGitCredentialRoles([]platform.EnvironmentSpec{{
		Name: "production",
		Source: platform.SourceBinding{
			URL: "https://github.com/acme/api", CredentialSecretName: "DEPLOY_HOOK",
		},
		WebhookSecretName: "DEPLOY_HOOK",
	}})
	if contract.Classify(err).Code != contract.ValidationFailed {
		t.Fatalf("error = %v", err)
	}
	if !strings.Contains(err.Error(), "webhook secret") {
		t.Fatalf("error = %v", err)
	}
}

func TestAppCreateFromFileGitHubAuthWritesOnlyGitNamespace(t *testing.T) {
	installFakeGH(t)
	token := "gho_mixed-read-token"
	t.Setenv("GH_FAKE_TOKEN", token)
	definition := testDefinition + `
[environments.staging.source]
`
	path := filepath.Join(t.TempDir(), "eregion.toml")
	if err := os.WriteFile(path, []byte(definition), 0o600); err != nil {
		t.Fatal(err)
	}
	backend := &definitionInventoryBackend{inventory: map[string]json.RawMessage{}}
	a := newApp()
	a.backend = backend
	var stdout, stderr bytes.Buffer
	exit := runApp(context.Background(), []string{
		"app", "create", "--from", path, "--github-auth", "--no-deploy", "--json",
	}, panicReader{}, &stdout, &stderr, &secrets.Registry{}, a)
	if exit != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout.String(), stderr.String())
	}
	if len(backend.secretNamespaces) != 1 {
		t.Fatalf("secret writes = %+v, want only the git namespace", backend.secretNamespaces)
	}
	if got := backend.secretPayloads["api/production/GIT_READ_TOKEN"]; got != token {
		t.Fatalf("production credential = %q; writes=%+v", got, backend.secretPayloads)
	}
	if _, found := backend.secretPayloads["api/staging/GIT_READ_TOKEN"]; found {
		t.Fatalf("credential leaked into the image environment: %+v", backend.secretPayloads)
	}
}

func TestEnvCreateGitHubAuthDryRunDoesNotReadGH(t *testing.T) {
	installFakeGH(t)
	backend := &definitionInventoryBackend{inventory: map[string]json.RawMessage{
		"apps": json.RawMessage(`{"items":[{"id":"app_1","name":"api"}]}`),
	}}
	a := newApp()
	a.backend = backend
	var stdout, stderr bytes.Buffer
	exit := runApp(context.Background(), []string{
		"env", "create", "--app", "api", "--name", "production", "--node", "main",
		"--git-url", "https://github.com/acme/api",
		"--github-auth", "--no-deploy", "--dry-run", "--json",
	}, panicReader{}, &stdout, &stderr, &secrets.Registry{}, a)
	if exit != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout.String(), stderr.String())
	}
	if calls := fakeGHCalls(t); len(calls) != 0 {
		t.Fatalf("dry run called gh: %+v", calls)
	}
	if backend.secretWrite != 0 {
		t.Fatalf("dry run wrote secrets: %+v", backend.secretNamespaces)
	}
	for _, submission := range backend.submissionsOfKind(platform.KindEnvironmentCreate) {
		if !submission.DryRun {
			t.Fatalf("dry run accepted work: %+v", submission)
		}
	}
}

func TestEnvCreateGitHubAuthReplayDoesNotReadGH(t *testing.T) {
	installFakeGH(t)
	backend := &definitionInventoryBackend{
		inventory: map[string]json.RawMessage{
			"apps": json.RawMessage(`{"items":[{"id":"app_1","name":"api"}]}`),
		},
		previewReplay: true,
	}
	a := newApp()
	a.backend = backend
	var stdout, stderr bytes.Buffer
	exit := runApp(context.Background(), []string{
		"env", "create", "--app", "api", "--name", "production", "--node", "main",
		"--git-url", "https://github.com/acme/api",
		"--github-auth", "--no-deploy", "--idempotency-key", "replay-key", "--json",
	}, panicReader{}, &stdout, &stderr, &secrets.Registry{}, a)
	if exit != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout.String(), stderr.String())
	}
	if calls := fakeGHCalls(t); len(calls) != 0 {
		t.Fatalf("replay called gh: %+v", calls)
	}
	if backend.secretWrite != 0 {
		t.Fatalf("replay wrote secrets: %+v", backend.secretNamespaces)
	}
}

func TestEnvUpdateGitHubAuthDryRunDoesNotReadGH(t *testing.T) {
	installFakeGH(t)
	current := githubAuthEnvUpdateEnvironment("GIT_READ_TOKEN", []string{"DATABASE_URL"}, "")
	backend := githubAuthEnvUpdateBackend(t, current, false)

	exit, stdout, stderr := runGitHubAuthCommand(t, backend,
		"env", "update", "api", "production", "--github-auth", "--dry-run", "--json")
	if exit != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	if calls := fakeGHCalls(t); len(calls) != 0 {
		t.Fatalf("dry run called gh: %+v", calls)
	}
	if backend.secretWrite != 0 {
		t.Fatalf("dry run wrote secrets: %+v", backend.secretNamespaces)
	}
	for _, submission := range backend.submissionsOfKind(platform.KindEnvironmentUpdate) {
		if !submission.DryRun {
			t.Fatalf("dry run accepted work: %+v", submission)
		}
	}
}

func TestReadUpdateSourceGitHubAuthRejectsMaliciousURL(t *testing.T) {
	for _, rawURL := range []string{
		"https://github.com@evil.example/acme/api",
		"https://github.com.evil.example/acme/api",
		"https://evil.example/github.com/acme/api",
		"https://github.com/acme/api/extra",
		"https://github.com/acme/api?next=evil",
	} {
		t.Run(rawURL, func(t *testing.T) {
			cmd := &cobra.Command{Use: "update"}
			addEnvironmentUpdateFlags(cmd)
			if err := cmd.ParseFlags([]string{"--github-auth", "--git-url", rawURL}); err != nil {
				t.Fatal(err)
			}
			base := platform.SourceBinding{
				CredentialSecretName: "GIT_READ_TOKEN",
			}
			if _, _, _, _, err := (&app{}).readUpdateSource(cmd, base, platform.Environment{}); contract.Classify(err).Code != contract.ValidationFailed {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func githubAuthEnvUpdateEnvironment(sourceCredential string, secretNames []string, webhookSecret string) platform.Environment {
	return platform.Environment{
		ID: "env_1", App: "api", Name: "production", Node: "main",
		Revision: &platform.EnvironmentRevision{RevisionState: platform.RevisionState{
			Source: platform.SourceBinding{
				URL: "https://github.com/acme/api", Ref: "main", CredentialSecretName: sourceCredential,
			},
			WebhookSecretName: webhookSecret,
			SecretNames:       append([]string(nil), secretNames...),
			Runtime:           platform.RuntimeSpec{Node: "main", HealthType: "http", HealthPath: "/", HealthTimeoutSeconds: 30},
			Processes: []platform.ProcessSpec{
				{Name: "web", Kind: "web", Replicas: 1, ContainerPort: 8080},
			},
		}},
	}
}

func githubAuthEnvUpdateBackend(t *testing.T, current platform.Environment, replay bool) *definitionInventoryBackend {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"items": []platform.Environment{current}})
	if err != nil {
		t.Fatal(err)
	}
	return &definitionInventoryBackend{
		inventory:     map[string]json.RawMessage{"environments": raw},
		previewReplay: replay,
	}
}

func runGitHubAuthCommand(t *testing.T, backend *definitionInventoryBackend, args ...string) (int, string, string) {
	t.Helper()
	a := newApp()
	a.backend = backend
	var stdout, stderr bytes.Buffer
	exit := runApp(context.Background(), args, panicReader{}, &stdout, &stderr, &secrets.Registry{}, a)
	return exit, stdout.String(), stderr.String()
}

func assertGitHubAuthRejected(t *testing.T, exit int, stdout, stderr, messageFragment string) {
	t.Helper()
	if exit == 0 {
		t.Fatalf("collision was accepted: stdout=%s stderr=%s", stdout, stderr)
	}
	envelope := decodeOne(t, stdout)
	if envelope.OK || envelope.Error == nil || envelope.Error.Code != contract.ValidationFailed {
		t.Fatalf("envelope = %+v", envelope)
	}
	if !strings.Contains(envelope.Error.Message+" "+envelope.Error.Hint, messageFragment) {
		t.Fatalf("error = %q / %q, want fragment %q", envelope.Error.Message, envelope.Error.Hint, messageFragment)
	}
	if strings.Contains(stdout, "gho_") || strings.Contains(stderr, "gho_") {
		t.Fatal("the gh token reached command output")
	}
}

func TestAppCreateGitHubAuthDryRunDoesNotReadGH(t *testing.T) {
	installFakeGH(t)
	backend := &definitionInventoryBackend{inventory: map[string]json.RawMessage{}}
	a := newApp()
	a.backend = backend
	var stdout, stderr bytes.Buffer
	exit := runApp(context.Background(), []string{
		"app", "create", "--name", "api", "--node", "main",
		"--git-url", "https://github.com/acme/api",
		"--github-auth", "--no-deploy", "--dry-run", "--json",
	}, panicReader{}, &stdout, &stderr, &secrets.Registry{}, a)
	if exit != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout.String(), stderr.String())
	}
	if calls := fakeGHCalls(t); len(calls) != 0 {
		t.Fatalf("dry run called gh: %+v", calls)
	}
	if backend.secretWrite != 0 {
		t.Fatalf("dry run wrote secrets: %+v", backend.secretNamespaces)
	}
}

func TestAppCreateGitHubAuthReplayDoesNotReadGH(t *testing.T) {
	installFakeGH(t)
	backend := &definitionInventoryBackend{inventory: map[string]json.RawMessage{}, previewReplay: true}
	a := newApp()
	a.backend = backend
	var stdout, stderr bytes.Buffer
	exit := runApp(context.Background(), []string{
		"app", "create", "--name", "api", "--node", "main",
		"--git-url", "https://github.com/acme/api",
		"--github-auth", "--no-deploy", "--idempotency-key", "replay-key", "--json",
	}, panicReader{}, &stdout, &stderr, &secrets.Registry{}, a)
	if exit != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout.String(), stderr.String())
	}
	if calls := fakeGHCalls(t); len(calls) != 0 {
		t.Fatalf("replay called gh: %+v", calls)
	}
	if backend.secretWrite != 0 {
		t.Fatalf("replay wrote secrets: %+v", backend.secretNamespaces)
	}
}
