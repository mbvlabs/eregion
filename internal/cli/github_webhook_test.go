package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/vault"
	"github.com/spf13/cobra"
)

const fakeGHScript = `#!/bin/sh
{
    printf 'CALL'
    for arg in "$@"; do
        printf '\t%s' "$arg"
    done
    printf '\n'
} >> "$GH_FAKE_ARGS"

if [ "$1" = "auth" ] && [ "$2" = "token" ]; then
    printf '%s' "${GH_FAKE_TOKEN:-fake-gh-token-value}"
    exit "${GH_FAKE_TOKEN_EXIT:-0}"
fi

if [ "$1" = "auth" ]; then
    exit "${GH_FAKE_AUTH_EXIT:-0}"
fi

if [ "$1" = "api" ]; then
    endpoint=""
    method=""
    for arg in "$@"; do
        case "$arg" in
            repos/*) endpoint="$arg" ;;
            POST|PATCH) method="$arg" ;;
        esac
    done
    case " $* " in
        *" --input - "*)
            cat > "$GH_FAKE_STDIN"
            ;;
    esac
    case "$endpoint" in
        repos/acme/api)
            if [ "${GH_FAKE_PERMISSION_EXIT:-0}" != "0" ]; then
                printf '%s\n' "${GH_FAKE_PERMISSION_STDERR:-}" >&2
                exit "${GH_FAKE_PERMISSION_EXIT}"
            fi
            printf '%s' "${GH_FAKE_PERMISSION:-true}"
            exit 0
            ;;
        repos/acme/api/hooks)
            if [ "$method" = "POST" ]; then
                if [ "${GH_FAKE_MUTATE_EXIT:-0}" != "0" ]; then
                    printf '%s\n' "${GH_FAKE_MUTATE_STDERR:-}" >&2
                    exit "${GH_FAKE_MUTATE_EXIT}"
                fi
                printf '{"id":42}'
                exit 0
            fi
            if [ "${GH_FAKE_LIST_EXIT:-0}" != "0" ]; then
                printf '%s\n' "${GH_FAKE_LIST_STDERR:-}" >&2
                exit "${GH_FAKE_LIST_EXIT}"
            fi
            printf '%s' "${GH_FAKE_HOOKS:-}"
            exit 0
            ;;
        repos/acme/api/hooks/*)
            if [ "${GH_FAKE_MUTATE_EXIT:-0}" != "0" ]; then
                printf '%s\n' "${GH_FAKE_MUTATE_STDERR:-}" >&2
                exit "${GH_FAKE_MUTATE_EXIT}"
            fi
            printf '{"id":7}'
            exit 0
            ;;
    esac
    exit 0
fi
exit 0
`

func installFakeGH(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "gh"), []byte(fakeGHScript), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GH_FAKE_ARGS", filepath.Join(dir, "args"))
	t.Setenv("GH_FAKE_STDIN", filepath.Join(dir, "stdin"))
	t.Setenv("PATH", dir+string(os.PathListSeparator)+"/usr/bin"+string(os.PathListSeparator)+"/bin")
}

func fakeGHCalls(t *testing.T) [][]string {
	t.Helper()
	raw, err := os.ReadFile(os.Getenv("GH_FAKE_ARGS"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	var calls [][]string
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "CALL") {
			t.Fatalf("unreadable fake gh call %q", line)
		}
		line = strings.TrimPrefix(line, "CALL")
		if line == "" {
			calls = append(calls, nil)
			continue
		}
		calls = append(calls, strings.Split(strings.TrimPrefix(line, "\t"), "\t"))
	}
	return calls
}

func fakeGHStdin(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(os.Getenv("GH_FAKE_STDIN"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func callHas(call []string, value string) bool {
	for _, arg := range call {
		if arg == value {
			return true
		}
	}
	return false
}

func callFlagValue(call []string, flag string) string {
	for index, arg := range call {
		if arg == flag && index+1 < len(call) {
			return call[index+1]
		}
	}
	return ""
}

func TestGitHubWebhookWithoutGHKeepsManualFallback(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	access := inspectGitHubWebhookAccess(context.Background(), "acme/api")
	if access.OK {
		t.Fatal("missing gh was reported as usable")
	}
	if !strings.Contains(access.Message, "not installed") {
		t.Fatalf("access message = %q", access.Message)
	}
	report := configureGitHubWebhook(context.Background(), access, "acme/api", "https://node.example/v1/webhooks/github/api/production", []byte("secret-value"))
	if report.Status != githubWebhookSkipped {
		t.Fatalf("report = %+v", report)
	}
	if !strings.Contains(report.summary(), "GitHub webhook") || strings.Contains(report.summary(), "secret-value") {
		t.Fatalf("summary = %q", report.summary())
	}
}

func TestGitHubWebhookRequiresRepositoryAdmin(t *testing.T) {
	installFakeGH(t)
	t.Setenv("GH_FAKE_PERMISSION", "false")
	access := inspectGitHubWebhookAccess(context.Background(), "acme/api")
	if access.OK {
		t.Fatal("a non-admin account was reported as usable")
	}
	if !strings.Contains(access.Message, "admin permission") {
		t.Fatalf("access message = %q", access.Message)
	}
	for _, call := range fakeGHCalls(t) {
		if callHas(call, "repos/acme/api/hooks") {
			t.Fatalf("the helper listed hooks without admin permission: %v", call)
		}
	}
}

func TestGitHubWebhookPermissionCheckAmbiguity(t *testing.T) {
	installFakeGH(t)
	t.Setenv("GH_FAKE_PERMISSION_EXIT", "1")
	t.Setenv("GH_FAKE_PERMISSION_STDERR", `{"message":"untrusted 404 output","secret":"s3cr3t-webhook-value"}`)
	access := inspectGitHubWebhookAccess(context.Background(), "acme/api")
	if access.OK {
		t.Fatal("an unreadable repository was reported as usable")
	}
	if !strings.Contains(access.Message, "could not confirm repository access or admin permission") {
		t.Fatalf("access message = %q", access.Message)
	}
	if strings.Contains(access.Message, "untrusted") || strings.Contains(access.Message, "s3cr3t") {
		t.Fatalf("access message printed untrusted output: %q", access.Message)
	}
	for _, call := range fakeGHCalls(t) {
		if callHas(call, "repos/acme/api/hooks") {
			t.Fatalf("the helper listed hooks after an ambiguous permission check: %v", call)
		}
	}
}

func TestGitHubWebhookCreateSendsSecretOnlyOnStdin(t *testing.T) {
	installFakeGH(t)
	secret := []byte("s3cr3t-webhook-value")
	access := inspectGitHubWebhookAccess(context.Background(), "acme/api")
	if !access.OK {
		t.Fatalf("access = %+v", access)
	}
	report := configureGitHubWebhook(context.Background(), access, "acme/api",
		"https://node.example/v1/webhooks/github/api/production", secret)
	if report.Status != githubWebhookCreated || report.HookID != 42 {
		t.Fatalf("report = %+v", report)
	}
	foundCreate := false
	for _, call := range fakeGHCalls(t) {
		if strings.Contains(strings.Join(call, " "), string(secret)) {
			t.Fatalf("secret leaked into gh argv: %v", call)
		}
		if callHas(call, "POST") {
			foundCreate = true
			if callFlagValue(call, "--method") != "POST" {
				t.Fatalf("create did not explicitly use POST: %v", call)
			}
			if callFlagValue(call, "-H") != "Content-Type: application/json" {
				t.Fatalf("create did not set the JSON Content-Type header: %v", call)
			}
			if !callHas(call, "--input") || !callHas(call, "-") {
				t.Fatalf("create did not use stdin: %v", call)
			}
			if callHas(call, "--paginate") || callHas(call, "--slurp") {
				t.Fatalf("create combined input with pagination: %v", call)
			}
		}
	}
	if !foundCreate {
		t.Fatal("no create call was recorded")
	}
	body := fakeGHStdin(t)
	if !bytes.Contains(body, secret) {
		t.Fatalf("create body did not carry the secret on stdin: %q", body)
	}
	if !bytes.Contains(body, []byte(`"content_type":"json"`)) {
		t.Fatalf("create body did not request JSON content: %q", body)
	}
	if strings.Contains(report.summary(), string(secret)) {
		t.Fatalf("summary leaked the secret: %q", report.summary())
	}
}

func TestGitHubWebhookUpdatesOnlyTheMatchingHook(t *testing.T) {
	installFakeGH(t)
	t.Setenv("GH_FAKE_HOOKS", strings.Join([]string{
		`{"id":7,"url":"https://node.example/v1/webhooks/github/api/production","active":true,"events":["push"],"content_type":"json"}`,
		`{"id":8,"url":"https://example.com/unrelated","active":true,"events":["push"],"content_type":"json"}`,
	}, "\n"))
	secret := []byte("rotated-secret-value")
	access := inspectGitHubWebhookAccess(context.Background(), "acme/api")
	report := configureGitHubWebhook(context.Background(), access, "acme/api",
		"https://node.example/v1/webhooks/github/api/production", secret)
	if report.Status != githubWebhookUpdated || report.HookID != 7 {
		t.Fatalf("report = %+v", report)
	}
	foundUpdate := false
	foundList := false
	for _, call := range fakeGHCalls(t) {
		joined := strings.Join(call, " ")
		if strings.Contains(joined, string(secret)) {
			t.Fatalf("secret leaked into gh argv: %v", call)
		}
		if callHas(call, "POST") {
			t.Fatalf("an existing matching hook was duplicated: %v", call)
		}
		if strings.Contains(joined, "/hooks/8") {
			t.Fatalf("an unrelated hook was changed: %v", call)
		}
		if callHas(call, "repos/acme/api/hooks") {
			foundList = true
			if !callHas(call, "--paginate") {
				t.Fatalf("the hook list did not use pagination: %v", call)
			}
			if callHas(call, "--input") || callHas(call, "--slurp") {
				t.Fatalf("the hook list combined pagination with an input or slurp: %v", call)
			}
		}
		if callHas(call, "PATCH") {
			foundUpdate = true
			if !strings.Contains(joined, "/hooks/7") {
				t.Fatalf("wrong hook updated: %v", call)
			}
			if callFlagValue(call, "--method") != "PATCH" {
				t.Fatalf("update did not explicitly use PATCH: %v", call)
			}
			if callFlagValue(call, "-H") != "Content-Type: application/json" {
				t.Fatalf("update did not set the JSON Content-Type header: %v", call)
			}
			if callHas(call, "--paginate") || callHas(call, "--slurp") {
				t.Fatalf("update combined input with pagination: %v", call)
			}
		}
	}
	if !foundUpdate {
		t.Fatal("no update call was recorded")
	}
	if !foundList {
		t.Fatal("no hook list call was recorded")
	}
	if !bytes.Contains(fakeGHStdin(t), secret) {
		t.Fatal("the rotated secret did not travel on stdin")
	}
}

func TestGitHubWebhookRefusesDuplicateMatchingHooks(t *testing.T) {
	installFakeGH(t)
	t.Setenv("GH_FAKE_HOOKS", strings.Join([]string{
		`{"id":7,"url":"https://node.example/v1/webhooks/github/api/production","active":true,"events":["push"],"content_type":"json"}`,
		`{"id":9,"url":"https://node.example/v1/webhooks/github/api/production/","active":true,"events":["push"],"content_type":"json"}`,
	}, "\n"))
	access := inspectGitHubWebhookAccess(context.Background(), "acme/api")
	report := configureGitHubWebhook(context.Background(), access, "acme/api",
		"https://node.example/v1/webhooks/github/api/production", []byte("secret-value"))
	if report.Status != githubWebhookAmbiguous {
		t.Fatalf("report = %+v", report)
	}
	for _, call := range fakeGHCalls(t) {
		if callHas(call, "POST") || callHas(call, "PATCH") {
			t.Fatalf("duplicates caused a mutation: %v", call)
		}
	}
	if len(fakeGHStdin(t)) != 0 {
		t.Fatal("duplicates caused a secret-bearing request")
	}
}

func TestGitHubWebhookFailureDoesNotPrintUntrustedOutput(t *testing.T) {
	installFakeGH(t)
	secret := []byte("s3cr3t-webhook-value")
	t.Setenv("GH_FAKE_MUTATE_EXIT", "1")
	t.Setenv("GH_FAKE_MUTATE_STDERR", `{"message":"untrusted failure output","secret":"`+string(secret)+`"}`)
	access := inspectGitHubWebhookAccess(context.Background(), "acme/api")
	report := configureGitHubWebhook(context.Background(), access, "acme/api",
		"https://node.example/v1/webhooks/github/api/production", secret)
	if report.Status != githubWebhookFailed {
		t.Fatalf("report = %+v", report)
	}
	if strings.Contains(report.summary(), string(secret)) {
		t.Fatalf("failure leaked the secret: %q", report.summary())
	}
	if strings.Contains(report.summary(), "untrusted") {
		t.Fatalf("failure printed untrusted gh output: %q", report.summary())
	}
	if !strings.Contains(report.Message, "gh exit status 1") {
		t.Fatalf("failure did not report a safe exit status: %q", report.Message)
	}
}

func TestGitHubWebhookEnsureNeverPatchesWithoutSecret(t *testing.T) {
	installFakeGH(t)
	t.Setenv("GH_FAKE_HOOKS", `{"id":7,"url":"https://node.example/v1/webhooks/github/api/production","active":true,"events":["push"],"content_type":"json"}`)
	report := ensureGitHubWebhook(context.Background(), "acme/api", "https://node.example/v1/webhooks/github/api/production")
	if report.Status != githubWebhookUnchanged || report.HookID != 7 {
		t.Fatalf("report = %+v", report)
	}
	for _, call := range fakeGHCalls(t) {
		if callHas(call, "POST") || callHas(call, "PATCH") {
			t.Fatalf("an already-correct hook was mutated: %v", call)
		}
	}
	if len(fakeGHStdin(t)) != 0 {
		t.Fatal("an unchanged hook received a request body")
	}

	for _, hooks := range []string{
		`{"id":7,"url":"https://node.example/v1/webhooks/github/api/production","active":false,"events":["push"],"content_type":"json"}`,
		`{"id":7,"url":"https://node.example/v1/webhooks/github/api/production","active":true,"events":["release"],"content_type":"json"}`,
	} {
		t.Setenv("GH_FAKE_HOOKS", hooks)
		report = ensureGitHubWebhook(context.Background(), "acme/api", "https://node.example/v1/webhooks/github/api/production")
		if report.Status != githubWebhookSkipped || !strings.Contains(report.Message, "inactive or does not include push events") {
			t.Fatalf("repair without a secret report = %+v", report)
		}
	}
	for _, call := range fakeGHCalls(t) {
		if callHas(call, "POST") || callHas(call, "PATCH") {
			t.Fatalf("ensure patched a hook without a secret: %v", call)
		}
	}
	if len(fakeGHStdin(t)) != 0 {
		t.Fatal("ensure sent a request body without a secret")
	}
}

func TestGitHubWebhookEnsureRefusesNonJSONContentType(t *testing.T) {
	installFakeGH(t)
	t.Setenv("GH_FAKE_HOOKS", `{"id":7,"url":"https://node.example/v1/webhooks/github/api/production","active":true,"events":["push"],"content_type":"form"}`)
	report := ensureGitHubWebhook(context.Background(), "acme/api", "https://node.example/v1/webhooks/github/api/production")
	if report.Status != githubWebhookSkipped || !strings.Contains(report.Message, "application/json") {
		t.Fatalf("report = %+v", report)
	}
	for _, call := range fakeGHCalls(t) {
		if callHas(call, "POST") || callHas(call, "PATCH") {
			t.Fatalf("a non-JSON hook was mutated without a rotation: %v", call)
		}
	}
}

func TestGitHubWebhookSetupDoesNotRunOnDryRun(t *testing.T) {
	installFakeGH(t)
	cmd := &cobra.Command{Use: "update"}
	cmd.Flags().Bool("dry-run", false, "dry run")
	if err := cmd.Flags().Set("dry-run", "true"); err != nil {
		t.Fatal(err)
	}
	a := &app{}
	report := a.setupWebhookWithGitHub(cmd, webhookUpdate{
		GitHub: true, Repository: "acme/api",
		URL: "https://node.example/v1/webhooks/github/api/production", Secret: []byte("secret-value"),
	})
	if report != nil {
		t.Fatalf("dry run produced a report: %+v", report)
	}
	if calls := fakeGHCalls(t); len(calls) != 0 {
		t.Fatalf("dry run called gh: %v", calls)
	}
}

func TestValidateGitHubWebhookFlags(t *testing.T) {
	for _, test := range []struct {
		name string
		args []string
		code contract.Code
	}{
		{name: "needs a webhook change", args: []string{"--github-webhook"}, code: contract.Usage},
		{name: "rejects disabling", args: []string{"--auto-deploy=false", "--github-webhook"}, code: contract.Usage},
		{name: "enable", args: []string{"--auto-deploy", "--github-webhook"}},
		{name: "rotate", args: []string{"--rotate-webhook-secret", "--github-webhook"}},
		{name: "explicitly false", args: []string{"--github-webhook=false"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			cmd := &cobra.Command{Use: "update"}
			addEnvironmentUpdateFlags(cmd)
			if err := cmd.ParseFlags(test.args); err != nil {
				t.Fatal(err)
			}
			err := validateGitHubWebhookFlags(cmd)
			if test.code == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if contract.Classify(err).Code != test.code {
				t.Fatalf("error = %v, want %s", err, test.code)
			}
		})
	}
}

type githubWebhookUpdateData struct {
	OperationID      string               `json:"operation_id"`
	WebhookURL       string               `json:"webhook_url"`
	WebhookRotated   bool                 `json:"webhook_rotated"`
	WebhookReplay    bool                 `json:"webhook_replay"`
	WebhookActivated bool                 `json:"webhook_secret_activated"`
	WebhookMessage   string               `json:"webhook_message"`
	DryRun           bool                 `json:"dry_run"`
	GitHubWebhook    *githubWebhookReport `json:"github_webhook"`
}

func decodeGitHubWebhookUpdate(t *testing.T, envelope contract.Envelope) githubWebhookUpdateData {
	t.Helper()
	raw, err := json.Marshal(envelope.Data)
	if err != nil {
		t.Fatal(err)
	}
	var data githubWebhookUpdateData
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	return data
}

func assertNoSecretInOutputOrArgs(t *testing.T, secret, stdout, stderr string) {
	t.Helper()
	if secret == "" {
		t.Fatal("no generated secret was recorded")
	}
	if strings.Contains(stdout, secret) || strings.Contains(stderr, secret) {
		t.Fatal("the generated webhook secret reached command output")
	}
	for _, call := range fakeGHCalls(t) {
		if strings.Contains(strings.Join(call, "\x00"), secret) {
			t.Fatalf("the generated webhook secret reached gh argv: %v", call)
		}
	}
}

func mutateCurrentEnvironment(t *testing.T, backend *definitionInventoryBackend, mutate func(*platform.Environment)) {
	t.Helper()
	var envelope struct {
		Items []platform.Environment `json:"items"`
	}
	if err := json.Unmarshal(backend.inventory["environments"], &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Items) == 0 {
		t.Fatal("no current environment to mutate")
	}
	mutate(&envelope.Items[0])
	raw, err := json.Marshal(map[string]any{"items": envelope.Items})
	if err != nil {
		t.Fatal(err)
	}
	backend.inventory["environments"] = raw
}

func countWebhookGenerations(t *testing.T) *int {
	t.Helper()
	original := generateWebhookSecret
	count := 0
	generateWebhookSecret = func() ([]byte, error) {
		count++
		return original()
	}
	t.Cleanup(func() { generateWebhookSecret = original })
	return &count
}

func TestGitHubWebhookJSONEnableInstallsHookWithoutSecretOutput(t *testing.T) {
	installFakeGH(t)
	current := githubAuthEnvUpdateEnvironment("GIT_READ_TOKEN", nil, "")
	backend := githubAuthEnvUpdateBackend(t, current, false)

	exit, stdout, stderr := runGitHubAuthCommand(t, backend,
		"env", "update", "api", "production",
		"--auto-deploy", "--github-webhook", "--json")
	if exit != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	envelope := decodeOne(t, stdout)
	if !envelope.OK {
		t.Fatalf("envelope = %+v", envelope)
	}
	data := decodeGitHubWebhookUpdate(t, envelope)
	if data.GitHubWebhook == nil || data.GitHubWebhook.Status != githubWebhookCreated || data.GitHubWebhook.HookID != 42 {
		t.Fatalf("github webhook = %+v", data.GitHubWebhook)
	}
	if !data.WebhookActivated {
		t.Fatal("fresh enable did not report an activated secret")
	}
	if data.WebhookURL != "https://main.example.com/v1/webhooks/github/api/production" {
		t.Fatalf("webhook URL = %q", data.WebhookURL)
	}
	if backend.secretWrite != 1 {
		t.Fatalf("secret writes = %+v", backend.secretNamespaces)
	}
	secret := backend.secretPayloads["api/production/EREGION_WEBHOOK_SECRET"]
	assertNoSecretInOutputOrArgs(t, secret, stdout, stderr)
	for _, submission := range backend.submissions {
		if bytes.Contains(submission.Input, []byte(secret)) {
			t.Fatal("the webhook secret entered the operation input")
		}
	}
	if !bytes.Contains(fakeGHStdin(t), []byte(secret)) {
		t.Fatal("the generated secret did not travel to gh on stdin")
	}
	foundPost := false
	for _, call := range fakeGHCalls(t) {
		if callHas(call, "repos/acme/api") {
			if callFlagValue(call, "--jq") != ".permissions.admin" {
				t.Fatalf("preflight did not request exact admin permission: %v", call)
			}
		}
		if callHas(call, "POST") {
			foundPost = true
			if !callHas(call, "--input") || !callHas(call, "-") {
				t.Fatalf("create did not use stdin: %v", call)
			}
		}
	}
	if !foundPost {
		t.Fatal("no hook create call was recorded")
	}
	updates := backend.submissionsOfKind(platform.KindEnvironmentUpdate)
	var accepted *definitionSubmission
	for index := range updates {
		if !updates[index].DryRun {
			accepted = &updates[index]
		}
	}
	if accepted == nil {
		t.Fatalf("no accepted environment update: %+v", updates)
	}
	var patch platform.EnvironmentUpdateInput
	if err := json.Unmarshal(accepted.Input, &patch); err != nil {
		t.Fatal(err)
	}
	if patch.AutoDeploy == nil || !*patch.AutoDeploy {
		t.Fatalf("auto deploy patch = %+v", patch.AutoDeploy)
	}
	if patch.WebhookSecretName == nil || *patch.WebhookSecretName != "EREGION_WEBHOOK_SECRET" {
		t.Fatalf("webhook secret name patch = %+v", patch.WebhookSecretName)
	}
	if strings.Contains(stdout, "Webhook secret:") || strings.Contains(stderr, "Webhook secret:") {
		t.Fatal("the machine path printed a webhook secret label")
	}
}

func TestGitHubWebhookJSONPreflightFailuresDoNotTouchSecretOrHooks(t *testing.T) {
	for _, test := range []struct {
		name    string
		prepare func(*testing.T)
		code    contract.Code
		want    string
	}{
		{name: "missing gh", prepare: func(t *testing.T) { t.Setenv("PATH", t.TempDir()) }, code: contract.MissingTool, want: "not installed"},
		{name: "not signed in", prepare: func(t *testing.T) { t.Setenv("GH_FAKE_AUTH_EXIT", "1") }, code: contract.PreconditionFailed, want: "not signed in"},
		{
			name: "missing admin permission",
			prepare: func(t *testing.T) {
				t.Setenv("GH_FAKE_PERMISSION", "false")
			},
			code: contract.PreconditionFailed,
			want: "admin permission",
		},
		{
			name: "unverifiable permission",
			prepare: func(t *testing.T) {
				t.Setenv("GH_FAKE_PERMISSION_EXIT", "1")
				t.Setenv("GH_FAKE_PERMISSION_STDERR", "untrusted client output")
			},
			code: contract.PreconditionFailed,
			want: "could not confirm repository access",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			installFakeGH(t)
			test.prepare(t)
			current := githubAuthEnvUpdateEnvironment("GIT_READ_TOKEN", nil, "")
			backend := githubAuthEnvUpdateBackend(t, current, false)

			exit, stdout, stderr := runGitHubAuthCommand(t, backend,
				"env", "update", "api", "production",
				"--auto-deploy", "--github-webhook", "--json")
			if exit == 0 {
				t.Fatalf("preflight failure was accepted: stdout=%s stderr=%s", stdout, stderr)
			}
			envelope := decodeOne(t, stdout)
			if envelope.OK || envelope.Error == nil || envelope.Error.Code != test.code {
				t.Fatalf("envelope = %+v", envelope)
			}
			if !strings.Contains(envelope.Error.Message+" "+envelope.Error.Hint, test.want) {
				t.Fatalf("error = %q / %q, want %q", envelope.Error.Message, envelope.Error.Hint, test.want)
			}
			if strings.Contains(envelope.Error.Message, "untrusted") || strings.Contains(envelope.Error.Hint, "untrusted") {
				t.Fatalf("preflight failure exposed gh output: %+v", envelope.Error)
			}
			if backend.secretWrite != 0 {
				t.Fatalf("preflight failure wrote secrets: %+v", backend.secretNamespaces)
			}
			for _, submission := range backend.submissionsOfKind(platform.KindEnvironmentUpdate) {
				if !submission.DryRun {
					t.Fatalf("preflight failure submitted work: %+v", submission)
				}
			}
			if len(fakeGHStdin(t)) != 0 {
				t.Fatal("preflight failure sent a request body")
			}
			for _, call := range fakeGHCalls(t) {
				if callHas(call, "POST") || callHas(call, "PATCH") {
					t.Fatalf("preflight failure mutated a hook: %v", call)
				}
				if callHas(call, "repos/acme/api/hooks") {
					t.Fatalf("preflight failure listed or changed hooks: %v", call)
				}
			}
		})
	}
}

func TestGitHubWebhookJSONRotationUpdatesMatchingHook(t *testing.T) {
	installFakeGH(t)
	t.Setenv("GH_FAKE_HOOKS", `{"id":7,"url":"https://main.example.com/v1/webhooks/github/api/production","active":true,"events":["push"],"content_type":"json"}`)
	current := githubAuthEnvUpdateEnvironment("GIT_READ_TOKEN", nil, "EREGION_WEBHOOK_SECRET")
	current.Revision.AutoDeploy = true
	backend := githubAuthEnvUpdateBackend(t, current, false)

	exit, stdout, stderr := runGitHubAuthCommand(t, backend,
		"env", "update", "api", "production",
		"--rotate-webhook-secret", "--github-webhook", "--json")
	if exit != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	data := decodeGitHubWebhookUpdate(t, decodeOne(t, stdout))
	if data.GitHubWebhook == nil || data.GitHubWebhook.Status != githubWebhookUpdated || data.GitHubWebhook.HookID != 7 {
		t.Fatalf("github webhook = %+v", data.GitHubWebhook)
	}
	if !data.WebhookRotated {
		t.Fatal("rotation was not reported")
	}
	if !data.WebhookActivated {
		t.Fatal("rotation did not report an activated secret")
	}
	if backend.secretWrite != 1 {
		t.Fatalf("secret writes = %+v", backend.secretNamespaces)
	}
	secret := backend.secretPayloads["api/production/EREGION_WEBHOOK_SECRET"]
	assertNoSecretInOutputOrArgs(t, secret, stdout, stderr)
	if !bytes.Contains(fakeGHStdin(t), []byte(secret)) {
		t.Fatal("the rotated secret did not travel to gh on stdin")
	}
	foundPatch := false
	for _, call := range fakeGHCalls(t) {
		if callHas(call, "POST") {
			t.Fatalf("rotation duplicated the matching hook: %v", call)
		}
		if callHas(call, "PATCH") {
			foundPatch = true
			if !strings.Contains(strings.Join(call, " "), "/hooks/7") {
				t.Fatalf("rotation updated the wrong hook: %v", call)
			}
		}
	}
	if !foundPatch {
		t.Fatal("no hook update call was recorded")
	}
	updates := backend.submissionsOfKind(platform.KindEnvironmentUpdate)
	foundAccepted := false
	for _, submission := range updates {
		if !submission.DryRun {
			foundAccepted = true
		}
	}
	if !foundAccepted {
		t.Fatalf("rotation did not submit its canonical operation: %+v", updates)
	}
}

func TestGitHubWebhookDryRunAndReplayDoNotGenerateOrInstall(t *testing.T) {
	t.Run("dry run enable", func(t *testing.T) {
		installFakeGH(t)
		generated := countWebhookGenerations(t)
		current := githubAuthEnvUpdateEnvironment("GIT_READ_TOKEN", nil, "")
		backend := githubAuthEnvUpdateBackend(t, current, false)
		exit, stdout, stderr := runGitHubAuthCommand(t, backend,
			"env", "update", "api", "production",
			"--auto-deploy", "--github-webhook", "--dry-run", "--json")
		if exit != 0 {
			t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
		}
		if *generated != 0 {
			t.Fatalf("dry run generated %d secrets", *generated)
		}
		data := decodeGitHubWebhookUpdate(t, decodeOne(t, stdout))
		if !data.DryRun {
			t.Fatal("dry run was not reported")
		}
		if backend.secretWrite != 0 {
			t.Fatalf("dry run wrote secrets: %+v", backend.secretNamespaces)
		}
		if calls := fakeGHCalls(t); len(calls) != 0 {
			t.Fatalf("dry run called gh: %v", calls)
		}
		if len(fakeGHStdin(t)) != 0 {
			t.Fatal("dry run sent a request body")
		}
	})
	t.Run("dry run rotation", func(t *testing.T) {
		installFakeGH(t)
		current := githubAuthEnvUpdateEnvironment("GIT_READ_TOKEN", nil, "EREGION_WEBHOOK_SECRET")
		current.Revision.AutoDeploy = true
		backend := githubAuthEnvUpdateBackend(t, current, false)
		exit, stdout, stderr := runGitHubAuthCommand(t, backend,
			"env", "update", "api", "production",
			"--rotate-webhook-secret", "--github-webhook", "--dry-run", "--json")
		if exit != 0 {
			t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
		}
		if backend.secretWrite != 0 {
			t.Fatalf("dry-run rotation wrote secrets: %+v", backend.secretNamespaces)
		}
		if calls := fakeGHCalls(t); len(calls) != 0 {
			t.Fatalf("dry-run rotation called gh: %v", calls)
		}
	})
	t.Run("rotate replay after first call", func(t *testing.T) {
		installFakeGH(t)
		generated := countWebhookGenerations(t)
		t.Setenv("GH_FAKE_HOOKS", `{"id":7,"url":"https://main.example.com/v1/webhooks/github/api/production","active":true,"events":["push"],"content_type":"json"}`)
		current := githubAuthEnvUpdateEnvironment("GIT_READ_TOKEN", nil, "EREGION_WEBHOOK_SECRET")
		current.Revision.AutoDeploy = true
		backend := githubAuthEnvUpdateBackend(t, current, false)
		backend.webhookStateful = true
		firstExit, firstStdout, firstStderr := runGitHubAuthCommand(t, backend,
			"env", "update", "api", "production",
			"--rotate-webhook-secret", "--github-webhook", "--idempotency-key", "stable-rotate-replay", "--json")
		if firstExit != 0 {
			t.Fatalf("first exit=%d stdout=%s stderr=%s", firstExit, firstStdout, firstStderr)
		}
		if backend.secretWrite != 1 {
			t.Fatalf("first rotation writes = %+v", backend.secretNamespaces)
		}
		if *generated != 1 {
			t.Fatalf("first rotation generated %d secrets", *generated)
		}
		ghCalls := len(fakeGHCalls(t))
		firstSecret := backend.secretPayloads["api/production/EREGION_WEBHOOK_SECRET"]

		exit, stdout, stderr := runGitHubAuthCommand(t, backend,
			"env", "update", "api", "production",
			"--rotate-webhook-secret", "--github-webhook", "--idempotency-key", "stable-rotate-replay")
		if exit != 0 {
			t.Fatalf("replay exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
		}
		if !strings.Contains(stdout, "NOT retried or verified") {
			t.Fatalf("replay output does not state the hook was not retried or verified: %q", stdout)
		}
		if backend.secretWrite != 1 {
			t.Fatalf("rotate replay wrote another secret: %+v", backend.secretNamespaces)
		}
		if *generated != 1 {
			t.Fatalf("rotate replay generated another secret: %d", *generated)
		}
		if len(fakeGHCalls(t)) != ghCalls {
			t.Fatalf("rotate replay called gh: before=%d after=%d", ghCalls, len(fakeGHCalls(t)))
		}
		if strings.Contains(stdout, firstSecret) || strings.Contains(stderr, firstSecret) {
			t.Fatal("rotate replay printed the original secret")
		}
	})
	t.Run("enable replay after first call and current change", func(t *testing.T) {
		installFakeGH(t)
		generated := countWebhookGenerations(t)
		current := githubAuthEnvUpdateEnvironment("GIT_READ_TOKEN", nil, "")
		backend := githubAuthEnvUpdateBackend(t, current, false)
		backend.webhookStateful = true
		firstExit, firstStdout, firstStderr := runGitHubAuthCommand(t, backend,
			"env", "update", "api", "production",
			"--auto-deploy", "--github-webhook", "--idempotency-key", "stable-webhook-replay", "--json")
		if firstExit != 0 {
			t.Fatalf("first exit=%d stdout=%s stderr=%s", firstExit, firstStdout, firstStderr)
		}
		if *generated != 1 {
			t.Fatalf("first call generated %d secrets, want 1", *generated)
		}
		if backend.secretWrite != 1 {
			t.Fatalf("first enable writes = %+v", backend.secretNamespaces)
		}
		firstData := decodeGitHubWebhookUpdate(t, decodeOne(t, firstStdout))
		if firstData.OperationID == "" {
			t.Fatal("first enable returned no operation")
		}
		ghCalls := len(fakeGHCalls(t))
		mutateCurrentEnvironment(t, backend, func(env *platform.Environment) {
			env.Revision.Runtime.HealthPath = "/changed-after-first-call"
		})

		exit, stdout, stderr := runGitHubAuthCommand(t, backend,
			"env", "update", "api", "production",
			"--auto-deploy", "--github-webhook", "--idempotency-key", "stable-webhook-replay", "--json")
		if exit != 0 {
			t.Fatalf("replay exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
		}
		data := decodeGitHubWebhookUpdate(t, decodeOne(t, stdout))
		if !data.WebhookReplay || data.WebhookRotated {
			t.Fatalf("replay data = %+v", data)
		}
		if data.OperationID != firstData.OperationID {
			t.Fatalf("replay operation = %q, want %q", data.OperationID, firstData.OperationID)
		}
		if !strings.Contains(data.WebhookMessage, "NOT retried or verified") {
			t.Fatalf("replay message = %q", data.WebhookMessage)
		}
		if backend.secretWrite != 1 {
			t.Fatalf("replay wrote another secret: %+v", backend.secretNamespaces)
		}
		if *generated != 1 {
			t.Fatalf("replay generated another secret: %d", *generated)
		}
		if len(fakeGHCalls(t)) != ghCalls {
			t.Fatalf("replay called gh: before=%d after=%d", ghCalls, len(fakeGHCalls(t)))
		}
	})
	t.Run("changed input conflicts before gh or generation", func(t *testing.T) {
		installFakeGH(t)
		generated := countWebhookGenerations(t)
		current := githubAuthEnvUpdateEnvironment("GIT_READ_TOKEN", nil, "")
		backend := githubAuthEnvUpdateBackend(t, current, false)
		backend.webhookStateful = true
		exit, stdout, stderr := runGitHubAuthCommand(t, backend,
			"env", "update", "api", "production",
			"--auto-deploy", "--github-webhook", "--idempotency-key", "stable-conflict", "--json")
		if exit != 0 {
			t.Fatalf("first exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
		}
		if *generated != 1 {
			t.Fatalf("first call generated %d secrets", *generated)
		}
		ghCalls := len(fakeGHCalls(t))
		writes := backend.secretWrite
		exit, stdout, stderr = runGitHubAuthCommand(t, backend,
			"env", "update", "api", "production",
			"--auto-deploy", "--github-webhook", "--git-ref", "other",
			"--idempotency-key", "stable-conflict", "--json")
		if exit == 0 {
			t.Fatalf("changed input was accepted: stdout=%s stderr=%s", stdout, stderr)
		}
		envelope := decodeOne(t, stdout)
		if envelope.Error == nil || envelope.Error.Code != contract.Conflict {
			t.Fatalf("envelope = %+v", envelope)
		}
		if backend.secretWrite != writes {
			t.Fatalf("conflict wrote a secret: before=%d after=%d", writes, backend.secretWrite)
		}
		if *generated != 1 {
			t.Fatalf("conflict generated another secret: %d", *generated)
		}
		if len(fakeGHCalls(t)) != ghCalls {
			t.Fatalf("conflict called gh: before=%d after=%d", ghCalls, len(fakeGHCalls(t)))
		}
	})
}

func TestGitHubWebhookEnabledRepeatSubmitsCanonicalIntentWithoutSecret(t *testing.T) {
	installFakeGH(t)
	generated := countWebhookGenerations(t)
	t.Setenv("GH_FAKE_HOOKS", `{"id":7,"url":"https://main.example.com/v1/webhooks/github/api/production","active":true,"events":["push"],"content_type":"json"}`)
	current := githubAuthEnvUpdateEnvironment("GIT_READ_TOKEN", nil, "EREGION_WEBHOOK_SECRET")
	current.Revision.AutoDeploy = true
	backend := githubAuthEnvUpdateBackend(t, current, false)
	backend.webhookStateful = true

	exit, stdout, stderr := runGitHubAuthCommand(t, backend,
		"env", "update", "api", "production",
		"--auto-deploy", "--github-webhook", "--idempotency-key", "enabled-repeat", "--json")
	if exit != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	data := decodeGitHubWebhookUpdate(t, decodeOne(t, stdout))
	if data.GitHubWebhook == nil || data.GitHubWebhook.Status != githubWebhookUnchanged {
		t.Fatalf("github webhook = %+v", data.GitHubWebhook)
	}
	if data.WebhookActivated || *generated != 0 {
		t.Fatalf("enabled repeat activated=%v generated=%d", data.WebhookActivated, *generated)
	}
	if backend.secretWrite != 0 {
		t.Fatalf("enabled repeat wrote secrets: %+v", backend.secretNamespaces)
	}
	submissions := backend.submissionsOfKind(platform.KindEnvironmentUpdate)
	var accepted *definitionSubmission
	for index := range submissions {
		if !submissions[index].DryRun {
			accepted = &submissions[index]
		}
	}
	if accepted == nil {
		t.Fatalf("enabled repeat bypassed canonical submission: %+v", submissions)
	}
	var patch platform.EnvironmentUpdateInput
	if err := json.Unmarshal(accepted.Input, &patch); err != nil {
		t.Fatal(err)
	}
	if patch.AutoDeploy == nil || !*patch.AutoDeploy || patch.WebhookSecretName == nil ||
		*patch.WebhookSecretName != "EREGION_WEBHOOK_SECRET" || patch.RotateWebhookSecret {
		t.Fatalf("enabled repeat patch = %+v", patch)
	}
	for _, call := range fakeGHCalls(t) {
		if callHas(call, "POST") || callHas(call, "PATCH") {
			t.Fatalf("enabled repeat mutated a hook: %v", call)
		}
	}
	if strings.Contains(stdout, "Webhook secret:") || strings.Contains(stderr, "Webhook secret:") {
		t.Fatal("enabled repeat printed a secret")
	}
}

func TestGitHubWebhookRepeatAfterHookFailureDoesNotRetry(t *testing.T) {
	installFakeGH(t)
	generated := countWebhookGenerations(t)
	t.Setenv("GH_FAKE_MUTATE_EXIT", "1")
	current := githubAuthEnvUpdateEnvironment("GIT_READ_TOKEN", nil, "")
	backend := githubAuthEnvUpdateBackend(t, current, false)
	backend.webhookStateful = true

	exit, stdout, stderr := runGitHubAuthCommand(t, backend,
		"env", "update", "api", "production",
		"--auto-deploy", "--github-webhook", "--idempotency-key", "hook-failure-then-repeat", "--json")
	if exit == 0 {
		t.Fatalf("hook failure was reported as success: stdout=%s stderr=%s", stdout, stderr)
	}
	envelope := decodeOne(t, stdout)
	if envelope.Error == nil || envelope.Error.Code != contract.Unavailable {
		t.Fatalf("first envelope = %+v", envelope)
	}
	operationID, _ := envelope.Error.Details["operation_id"].(string)
	if operationID == "" {
		t.Fatalf("first failure details = %+v", envelope.Error.Details)
	}
	if backend.secretWrite != 1 {
		t.Fatalf("first call secret writes = %+v", backend.secretNamespaces)
	}
	if *generated != 1 {
		t.Fatalf("first call generated %d secrets", *generated)
	}
	ghCalls := len(fakeGHCalls(t))
	firstSecret := backend.secretPayloads["api/production/EREGION_WEBHOOK_SECRET"]
	if firstSecret == "" {
		t.Fatal("first call did not activate a secret")
	}

	exit, stdout, stderr = runGitHubAuthCommand(t, backend,
		"env", "update", "api", "production",
		"--auto-deploy", "--github-webhook", "--idempotency-key", "hook-failure-then-repeat")
	if exit != 0 {
		t.Fatalf("repeat exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	if !strings.Contains(stdout, operationID) {
		t.Fatalf("repeat did not return the original operation %s: %q", operationID, stdout)
	}
	if !strings.Contains(stdout, "NOT retried or verified") {
		t.Fatalf("repeat did not state the hook was not retried or verified: %q", stdout)
	}
	if backend.secretWrite != 1 {
		t.Fatalf("repeat wrote another secret: %+v", backend.secretNamespaces)
	}
	if *generated != 1 {
		t.Fatalf("repeat generated another secret: %d", *generated)
	}
	if len(fakeGHCalls(t)) != ghCalls {
		t.Fatalf("repeat retried gh: before=%d after=%d", ghCalls, len(fakeGHCalls(t)))
	}
	if strings.Contains(stdout, firstSecret) || strings.Contains(stderr, firstSecret) {
		t.Fatal("repeat printed the original secret")
	}
}

func TestGitHubWebhookConcurrentLoserDoesNotActivateOrInstall(t *testing.T) {
	installFakeGH(t)
	generated := countWebhookGenerations(t)
	current := githubAuthEnvUpdateEnvironment("GIT_READ_TOKEN", nil, "")
	backend := githubAuthEnvUpdateBackend(t, current, false)
	backend.webhookStateful = true
	backend.webhookForceReplay = true

	exit, stdout, stderr := runGitHubAuthCommand(t, backend,
		"env", "update", "api", "production",
		"--auto-deploy", "--github-webhook", "--idempotency-key", "concurrent-loser", "--json")
	if exit != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	data := decodeGitHubWebhookUpdate(t, decodeOne(t, stdout))
	if !data.WebhookReplay || data.WebhookActivated || data.WebhookRotated {
		t.Fatalf("loser receipt = %+v", data)
	}
	if !strings.Contains(data.WebhookMessage, "candidate") || !strings.Contains(data.WebhookMessage, "not activated") {
		t.Fatalf("loser message does not report the discarded candidate: %q", data.WebhookMessage)
	}
	if backend.secretWrite != 0 {
		t.Fatalf("concurrent loser activated a secret: %+v", backend.secretNamespaces)
	}
	if *generated != 1 {
		t.Fatalf("concurrent loser generated %d candidates, want the one unresolved candidate", *generated)
	}
	for _, call := range fakeGHCalls(t) {
		if callHas(call, "POST") || callHas(call, "PATCH") {
			t.Fatalf("concurrent loser mutated a hook: %v", call)
		}
	}
	if strings.Contains(stdout, "Webhook secret:") || strings.Contains(stderr, "Webhook secret:") {
		t.Fatal("concurrent loser printed a secret")
	}
}

func TestGitHubWebhookMachineManualNeedsExplicitOptIn(t *testing.T) {
	installFakeGH(t)
	generated := countWebhookGenerations(t)
	current := githubAuthEnvUpdateEnvironment("GIT_READ_TOKEN", nil, "")
	backend := githubAuthEnvUpdateBackend(t, current, false)
	exit, stdout, stderr := runGitHubAuthCommand(t, backend,
		"env", "update", "api", "production", "--auto-deploy", "--json")
	if exit == 0 {
		t.Fatalf("manual machine setup was accepted: stdout=%s stderr=%s", stdout, stderr)
	}
	envelope := decodeOne(t, stdout)
	if envelope.Error == nil || envelope.Error.Code != contract.InputRequired {
		t.Fatalf("envelope = %+v", envelope)
	}
	if *generated != 0 || backend.secretWrite != 0 {
		t.Fatalf("refused machine setup generated=%d writes=%d", *generated, backend.secretWrite)
	}
	if calls := fakeGHCalls(t); len(calls) != 0 {
		t.Fatalf("refused machine setup called gh: %v", calls)
	}
}

func TestGitHubWebhookDisableAlreadyDisabledReportsDisabled(t *testing.T) {
	installFakeGH(t)
	current := githubAuthEnvUpdateEnvironment("GIT_READ_TOKEN", nil, "")
	backend := githubAuthEnvUpdateBackend(t, current, false)
	backend.webhookStateful = true

	exit, stdout, stderr := runGitHubAuthCommand(t, backend,
		"env", "update", "api", "production", "--auto-deploy=false")
	if exit != 0 {
		t.Fatalf("exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	if !strings.Contains(stdout, "already disabled") {
		t.Fatalf("output does not report disabled state: %q", stdout)
	}
	if strings.Contains(stdout, "already enabled") {
		t.Fatalf("output falsely reports enabled state: %q", stdout)
	}
	if backend.secretWrite != 0 {
		t.Fatalf("already-disabled update wrote secrets: %+v", backend.secretNamespaces)
	}
}

func TestGitHubWebhookMixedCredentialReplayDoesNotReadOrWriteCredential(t *testing.T) {
	installFakeGH(t)
	generated := countWebhookGenerations(t)
	current := githubAuthEnvUpdateEnvironment("GIT_READ_TOKEN", nil, "")
	backend := githubAuthEnvUpdateBackend(t, current, false)
	backend.webhookStateful = true
	credentialFile := writeSecretInput(t, "mixed-file-read-token")

	args := []string{
		"env", "update", "api", "production",
		"--auto-deploy", "--github-webhook",
		"--git-credential-file", credentialFile,
		"--git-credential-name", "GIT_READ_TOKEN",
		"--idempotency-key", "mixed-webhook-credential",
	}
	firstExit, firstStdout, firstStderr := runGitHubAuthCommand(t, backend, append(args, "--json")...)
	if firstExit != 0 {
		t.Fatalf("first exit=%d stdout=%s stderr=%s", firstExit, firstStdout, firstStderr)
	}
	if got := backend.secretPayloads["api/production/GIT_READ_TOKEN"]; got != "mixed-file-read-token" {
		t.Fatalf("credential = %q; writes=%+v", got, backend.secretPayloads)
	}
	if got := backend.secretPayloads["api/production/EREGION_WEBHOOK_SECRET"]; got == "" {
		t.Fatalf("webhook secret was not activated: %+v", backend.secretPayloads)
	}
	if backend.secretWrite != 2 {
		t.Fatalf("first writes = %d namespaces=%+v", backend.secretWrite, backend.secretNamespaces)
	}
	writeCount := backend.secretWrite
	namespaces := append([]vault.Namespace(nil), backend.secretNamespaces...)
	ghCalls := len(fakeGHCalls(t))
	firstData := decodeGitHubWebhookUpdate(t, decodeOne(t, firstStdout))
	if firstData.OperationID == "" {
		t.Fatal("first call returned no operation")
	}
	if err := os.Remove(credentialFile); err != nil {
		t.Fatal(err)
	}
	mutateCurrentEnvironment(t, backend, func(env *platform.Environment) {
		env.Revision.Runtime.HealthPath = "/mixed-replay-change"
	})

	exit, stdout, stderr := runGitHubAuthCommand(t, backend, append(args, "--json")...)
	if exit != 0 {
		t.Fatalf("replay exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	data := decodeGitHubWebhookUpdate(t, decodeOne(t, stdout))
	if !data.WebhookReplay || data.OperationID != firstData.OperationID {
		t.Fatalf("replay data = %+v", data)
	}
	if backend.secretWrite != writeCount {
		t.Fatalf("replay wrote secrets: before=%d after=%d namespaces=%+v", writeCount, backend.secretWrite, backend.secretNamespaces)
	}
	if len(backend.secretNamespaces) != len(namespaces) {
		t.Fatalf("replay changed secret namespaces: before=%+v after=%+v", namespaces, backend.secretNamespaces)
	}
	for index := range namespaces {
		if backend.secretNamespaces[index] != namespaces[index] {
			t.Fatalf("replay changed secret namespace: before=%+v after=%+v", namespaces, backend.secretNamespaces)
		}
	}
	if *generated != 1 {
		t.Fatalf("replay generated %d webhook secrets", *generated)
	}
	if len(fakeGHCalls(t)) != ghCalls {
		t.Fatalf("replay called gh: before=%d after=%d", ghCalls, len(fakeGHCalls(t)))
	}
}

func TestGitHubWebhookReplayPrecedesHumanPrompt(t *testing.T) {
	installFakeGH(t)
	generated := countWebhookGenerations(t)
	oldInteractive, oldConfirm := webhookHumanInteractive, confirmGitHubWebhook
	t.Cleanup(func() { webhookHumanInteractive, confirmGitHubWebhook = oldInteractive, oldConfirm })
	webhookHumanInteractive = func(*cobra.Command) bool { return true }
	prompts := 0
	confirmGitHubWebhook = func(_, _ string, _ bool) (bool, error) {
		prompts++
		return false, nil
	}
	current := githubAuthEnvUpdateEnvironment("GIT_READ_TOKEN", nil, "")
	backend := githubAuthEnvUpdateBackend(t, current, false)
	backend.webhookStateful = true

	firstExit, _, firstStderr := runGitHubAuthCommand(t, backend,
		"env", "update", "api", "production", "--auto-deploy", "--idempotency-key", "prompt-replay")
	if firstExit != 0 {
		t.Fatalf("first exit=%d stderr=%s", firstExit, firstStderr)
	}
	if prompts != 1 {
		t.Fatalf("first call prompts = %d, want 1", prompts)
	}
	if *generated != 1 {
		t.Fatalf("first call generated %d secrets", *generated)
	}
	firstSecret := backend.secretPayloads["api/production/EREGION_WEBHOOK_SECRET"]
	if firstSecret == "" {
		t.Fatal("first call did not activate a secret")
	}
	ghCalls := len(fakeGHCalls(t))
	mutateCurrentEnvironment(t, backend, func(env *platform.Environment) {
		env.Revision.Runtime.HealthPath = "/changed-for-prompt-replay"
	})

	exit, stdout, stderr := runGitHubAuthCommand(t, backend,
		"env", "update", "api", "production", "--auto-deploy", "--idempotency-key", "prompt-replay")
	if exit != 0 {
		t.Fatalf("replay exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	if prompts != 1 {
		t.Fatalf("replay prompted again: prompts=%d", prompts)
	}
	if len(fakeGHCalls(t)) != ghCalls {
		t.Fatalf("replay interacted with gh: before=%d after=%d", ghCalls, len(fakeGHCalls(t)))
	}
	if backend.secretWrite != 1 {
		t.Fatalf("replay wrote another secret: %+v", backend.secretNamespaces)
	}
	if *generated != 1 {
		t.Fatalf("replay generated another secret: %d", *generated)
	}
	if strings.Contains(stdout, firstSecret) || strings.Contains(stderr, firstSecret) {
		t.Fatal("replay printed the original secret")
	}
	if !strings.Contains(stdout, "NOT retried or verified") {
		t.Fatalf("replay output = %q", stdout)
	}
}

func TestGitHubWebhookKnownReplaySkipsHumanGateWithoutGitHub(t *testing.T) {
	installFakeGH(t)
	generated := countWebhookGenerations(t)
	oldInteractive, oldConfirm := webhookHumanInteractive, confirmGitHubWebhook
	t.Cleanup(func() { webhookHumanInteractive, confirmGitHubWebhook = oldInteractive, oldConfirm })
	webhookHumanInteractive = func(*cobra.Command) bool { return true }
	prompts := 0
	confirmGitHubWebhook = func(_, _ string, _ bool) (bool, error) {
		prompts++
		return false, nil
	}
	current := githubAuthEnvUpdateEnvironment("GIT_READ_TOKEN", nil, "")
	backend := githubAuthEnvUpdateBackend(t, current, false)
	backend.webhookStateful = true
	firstExit, _, firstStderr := runGitHubAuthCommand(t, backend,
		"env", "update", "api", "production", "--auto-deploy", "--idempotency-key", "manual-replay")
	if firstExit != 0 {
		t.Fatalf("first exit=%d stderr=%s", firstExit, firstStderr)
	}
	if prompts != 1 || backend.secretWrite != 1 {
		t.Fatalf("first prompts=%d secretWrite=%d", prompts, backend.secretWrite)
	}
	firstSecret := backend.secretPayloads["api/production/EREGION_WEBHOOK_SECRET"]
	ghCalls := len(fakeGHCalls(t))

	webhookHumanInteractive = func(*cobra.Command) bool { return false }
	confirmGitHubWebhook = func(_, _ string, _ bool) (bool, error) {
		t.Fatal("replay reached the GitHub webhook prompt")
		return false, nil
	}
	exit, stdout, stderr := runGitHubAuthCommand(t, backend,
		"env", "update", "api", "production", "--auto-deploy", "--idempotency-key", "manual-replay", "--json")
	if exit != 0 {
		t.Fatalf("replay exit=%d stdout=%s stderr=%s", exit, stdout, stderr)
	}
	data := decodeGitHubWebhookUpdate(t, decodeOne(t, stdout))
	if !data.WebhookReplay || data.WebhookRotated || data.WebhookActivated {
		t.Fatalf("replay data = %+v", data)
	}
	if *generated != 1 || backend.secretWrite != 1 {
		t.Fatalf("replay generated=%d secretWrite=%d", *generated, backend.secretWrite)
	}
	if len(fakeGHCalls(t)) != ghCalls {
		t.Fatalf("replay called gh: before=%d after=%d", ghCalls, len(fakeGHCalls(t)))
	}
	if strings.Contains(stdout, firstSecret) || strings.Contains(stderr, firstSecret) {
		t.Fatal("replay printed the original secret")
	}
}

func TestGitHubWebhookPartialFailureIsReported(t *testing.T) {
	t.Run("activation accepted", func(t *testing.T) {
		installFakeGH(t)
		t.Setenv("GH_FAKE_MUTATE_EXIT", "1")
		t.Setenv("GH_FAKE_MUTATE_STDERR", `{"message":"untrusted failure output"}`)
		current := githubAuthEnvUpdateEnvironment("GIT_READ_TOKEN", nil, "")
		backend := githubAuthEnvUpdateBackend(t, current, false)

		exit, stdout, stderr := runGitHubAuthCommand(t, backend,
			"env", "update", "api", "production",
			"--auto-deploy", "--github-webhook", "--json")
		if exit == 0 {
			t.Fatalf("partial failure was reported as success: stdout=%s stderr=%s", stdout, stderr)
		}
		envelope := decodeOne(t, stdout)
		if envelope.OK || envelope.Error == nil || envelope.Error.Code != contract.Unavailable {
			t.Fatalf("envelope = %+v", envelope)
		}
		if !strings.Contains(envelope.Error.Message, "accepted") || !strings.Contains(envelope.Error.Message, "webhook was not installed") {
			t.Fatalf("message = %q", envelope.Error.Message)
		}
		if !strings.Contains(envelope.Error.Hint, "--rotate-webhook-secret --github-webhook") {
			t.Fatalf("hint = %q", envelope.Error.Hint)
		}
		if !strings.Contains(envelope.Error.Hint, "not printed") {
			t.Fatalf("hint does not state the secret was not printed: %q", envelope.Error.Hint)
		}
		details := envelope.Error.Details
		if details["platform_activation"] != "accepted" || details["secret_rotated"] != false {
			t.Fatalf("details = %+v", details)
		}
		if operationID, _ := details["operation_id"].(string); operationID == "" {
			t.Fatalf("operation id missing from details: %+v", details)
		}
		report, ok := details["github_webhook"].(map[string]any)
		if !ok || report["status"] != githubWebhookFailed {
			t.Fatalf("github webhook details = %+v", details["github_webhook"])
		}
		if backend.secretWrite != 1 {
			t.Fatalf("activation did not write exactly one secret: %+v", backend.secretNamespaces)
		}
		secret := backend.secretPayloads["api/production/EREGION_WEBHOOK_SECRET"]
		assertNoSecretInOutputOrArgs(t, secret, stdout, stderr)
		if strings.Contains(stdout, "Webhook secret:") || strings.Contains(stderr, "Webhook secret:") {
			t.Fatal("partial failure fell back to printing the secret")
		}
		if !bytes.Contains(fakeGHStdin(t), []byte(secret)) {
			t.Fatal("the activated secret did not reach the attempted hook create")
		}
	})
	t.Run("combined rotation and update", func(t *testing.T) {
		installFakeGH(t)
		t.Setenv("GH_FAKE_MUTATE_EXIT", "1")
		t.Setenv("GH_FAKE_HOOKS", `{"id":7,"url":"https://main.example.com/v1/webhooks/github/api/production","active":true,"events":["push"],"content_type":"json"}`)
		current := githubAuthEnvUpdateEnvironment("GIT_READ_TOKEN", nil, "EREGION_WEBHOOK_SECRET")
		current.Revision.AutoDeploy = true
		backend := githubAuthEnvUpdateBackend(t, current, false)

		exit, stdout, stderr := runGitHubAuthCommand(t, backend,
			"env", "update", "api", "production",
			"--rotate-webhook-secret", "--health-path", "/combined", "--github-webhook", "--json")
		if exit == 0 {
			t.Fatalf("combined partial failure was reported as success: stdout=%s stderr=%s", stdout, stderr)
		}
		envelope := decodeOne(t, stdout)
		if envelope.Error == nil || envelope.Error.Code != contract.Unavailable {
			t.Fatalf("envelope = %+v", envelope)
		}
		details := envelope.Error.Details
		if details["secret_rotated"] != true || details["platform_activation"] != "accepted" {
			t.Fatalf("details = %+v", details)
		}
		if operationID, _ := details["operation_id"].(string); operationID == "" {
			t.Fatalf("operation id missing from details: %+v", details)
		}
		if backend.secretWrite != 1 {
			t.Fatalf("combined rotation wrote %+v", backend.secretNamespaces)
		}
		var accepted *definitionSubmission
		for _, submission := range backend.submissionsOfKind(platform.KindEnvironmentUpdate) {
			if !submission.DryRun {
				accepted = &submission
			}
		}
		if accepted == nil || !bytes.Contains(accepted.Input, []byte(`"health_path":"/combined"`)) ||
			!bytes.Contains(accepted.Input, []byte(`"rotate_webhook_secret":true`)) {
			t.Fatalf("combined intent = %+v", accepted)
		}
		secret := backend.secretPayloads["api/production/EREGION_WEBHOOK_SECRET"]
		assertNoSecretInOutputOrArgs(t, secret, stdout, stderr)
	})
	t.Run("rotation happened", func(t *testing.T) {
		installFakeGH(t)
		t.Setenv("GH_FAKE_MUTATE_EXIT", "1")
		t.Setenv("GH_FAKE_HOOKS", `{"id":7,"url":"https://main.example.com/v1/webhooks/github/api/production","active":true,"events":["push"],"content_type":"json"}`)
		current := githubAuthEnvUpdateEnvironment("GIT_READ_TOKEN", nil, "EREGION_WEBHOOK_SECRET")
		current.Revision.AutoDeploy = true
		backend := githubAuthEnvUpdateBackend(t, current, false)

		exit, stdout, stderr := runGitHubAuthCommand(t, backend,
			"env", "update", "api", "production",
			"--rotate-webhook-secret", "--github-webhook", "--json")
		if exit == 0 {
			t.Fatalf("rotation partial failure was reported as success: stdout=%s stderr=%s", stdout, stderr)
		}
		envelope := decodeOne(t, stdout)
		if envelope.Error == nil || envelope.Error.Code != contract.Unavailable {
			t.Fatalf("envelope = %+v", envelope)
		}
		if !strings.Contains(envelope.Error.Message, "rotated") {
			t.Fatalf("message = %q", envelope.Error.Message)
		}
		details := envelope.Error.Details
		if details["secret_rotated"] != true || details["platform_activation"] != "accepted" {
			t.Fatalf("details = %+v", details)
		}
		if operationID, _ := details["operation_id"].(string); operationID == "" {
			t.Fatalf("operation id missing from details: %+v", details)
		}
		if backend.secretWrite != 1 {
			t.Fatalf("rotation wrote %+v", backend.secretNamespaces)
		}
		secret := backend.secretPayloads["api/production/EREGION_WEBHOOK_SECRET"]
		assertNoSecretInOutputOrArgs(t, secret, stdout, stderr)
		if strings.Contains(stderr, "Webhook secret:") {
			t.Fatal("rotation failure fell back to printing the secret")
		}
	})
}
