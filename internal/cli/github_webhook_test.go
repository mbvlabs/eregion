package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
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
