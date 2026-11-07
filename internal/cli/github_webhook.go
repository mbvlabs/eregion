package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/mbvlabs/eregion/internal/cli/ui"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/spf13/cobra"
)

// GitHub repository webhook management is optional convenience around the
// manual push-to-deploy setup. Every gh invocation that carries the secret
// sends the request body on stdin; process arguments never contain it.

const (
	githubWebhookCreated   = "created"
	githubWebhookUpdated   = "updated"
	githubWebhookUnchanged = "unchanged"
	githubWebhookMissing   = "missing"
	githubWebhookSkipped   = "skipped"
	githubWebhookFailed    = "failed"
	githubWebhookAmbiguous = "ambiguous"
)

// githubWebhookReport is safe for JSON results and ordinary output: it never
// contains a secret value.
type githubWebhookReport struct {
	Status  string `json:"status"`
	HookID  int64  `json:"hook_id,omitempty"`
	Message string `json:"message,omitempty"`
	Next    string `json:"next,omitempty"`
}

func (r githubWebhookReport) configured() bool {
	switch r.Status {
	case githubWebhookCreated, githubWebhookUpdated, githubWebhookUnchanged:
		return true
	default:
		return false
	}
}

func (r githubWebhookReport) summary() string {
	switch r.Status {
	case githubWebhookCreated:
		if r.HookID > 0 {
			return fmt.Sprintf("GitHub webhook: created by gh (id %d).", r.HookID)
		}
		return "GitHub webhook: created by gh."
	case githubWebhookUpdated:
		if r.HookID > 0 {
			return fmt.Sprintf("GitHub webhook: updated by gh (id %d).", r.HookID)
		}
		return "GitHub webhook: updated by gh."
	case githubWebhookUnchanged:
		if r.HookID > 0 {
			return fmt.Sprintf("GitHub webhook: already configured for this URL (id %d); the secret was not changed.", r.HookID)
		}
		return "GitHub webhook: already configured for this URL; the secret was not changed."
	}
	text := "GitHub webhook: " + r.Status
	if r.Message != "" {
		text += ": " + strings.TrimSuffix(strings.TrimSpace(r.Message), ".")
	}
	if r.Next != "" {
		text += ". " + r.Next
	}
	return text
}

type githubWebhookAccess struct {
	OK      bool
	Message string
	Next    string
}

type githubHook struct {
	ID          int64    `json:"id"`
	URL         string   `json:"url"`
	Active      bool     `json:"active"`
	Events      []string `json:"events"`
	ContentType string   `json:"content_type"`
}

type ghCommandError struct {
	exitCode int
}

func (e *ghCommandError) Error() string { return "gh command failed" }

// validateGitHubWebhookFlags keeps the gh opt-in tied to a command that would
// actually create or rotate a secret.
func validateGitHubWebhookFlags(cmd *cobra.Command) error {
	if cmd == nil || !flagChanged(cmd, "github-webhook") || !boolFlag(cmd, "github-webhook") {
		return nil
	}
	autoSet := flagChanged(cmd, "auto-deploy")
	rotate := boolFlag(cmd, "rotate-webhook-secret")
	if !autoSet && !rotate {
		return contract.NewFault(contract.Usage, "--github-webhook needs --auto-deploy or --rotate-webhook-secret",
			"Use it only when enabling or rotating push to deploy; the manual webhook setup stays available without it.")
	}
	if autoSet && !boolFlag(cmd, "auto-deploy") {
		return contract.NewFault(contract.Usage, "--github-webhook cannot be combined with --auto-deploy=false",
			"Leave push to deploy on to manage its webhook, or disable it without --github-webhook.")
	}
	return nil
}

// offerGitHubWebhook is the guided equivalent of --github-webhook. It never
// prompts when gh is missing or cannot reach an admin account, and it defaults
// to the existing manual path.
func (a *app) offerGitHubWebhook(cmd *cobra.Command, update *webhookUpdate) error {
	if update == nil || update.GitHub || flagChanged(cmd, "github-webhook") || dryRun(cmd) || !humanInteractive(cmd) || len(update.Secret) == 0 {
		return nil
	}
	access := inspectGitHubWebhookAccess(cmd.Context(), update.Repository)
	if !access.OK {
		return nil
	}
	use, err := ui.YesNoDefault("Create the GitHub webhook with gh?",
		"gh is signed in with repository admin access. The webhook secret is sent over stdin, never in process arguments. Choose No to paste the URL and secret manually.", false)
	if err != nil {
		return err
	}
	update.GitHub = use
	return nil
}

// setupWebhookWithGitHub runs only after the local vault write and (when
// present) the accepted environment operation, so an external failure cannot
// pretend that desired state was not saved.
func (a *app) setupWebhookWithGitHub(cmd *cobra.Command, update webhookUpdate) *githubWebhookReport {
	if !update.GitHub || dryRun(cmd) {
		return nil
	}
	if update.Repository == "" || update.URL == "" {
		report := githubWebhookReport{
			Status:  githubWebhookFailed,
			Message: "the repository or webhook URL is missing",
			Next:    "Paste the webhook URL and secret into GitHub manually.",
		}
		return &report
	}
	var report githubWebhookReport
	if len(update.Secret) == 0 {
		report = ensureGitHubWebhook(cmd.Context(), update.Repository, update.URL)
	} else {
		access := inspectGitHubWebhookAccess(cmd.Context(), update.Repository)
		report = configureGitHubWebhook(cmd.Context(), access, update.Repository, update.URL, update.Secret)
	}
	return &report
}

func inspectGitHubWebhookAccess(ctx context.Context, repository string) githubWebhookAccess {
	if _, err := exec.LookPath("gh"); err != nil {
		return githubWebhookAccess{
			Message: "the GitHub CLI (gh) is not installed",
			Next:    "Install gh and sign in, or paste the webhook URL and secret into GitHub manually.",
		}
	}
	if _, err := runGH(ctx, nil, "auth", "status", "--hostname", "github.com"); err != nil {
		return githubWebhookAccess{
			Message: "gh is not signed in to github.com or its session could not be verified",
			Next:    "Run gh auth login, or paste the webhook URL and secret into GitHub manually.",
		}
	}
	out, err := runGH(ctx, nil, "api", "--hostname", "github.com", "--jq", ".permissions.admin", "repos/"+repository)
	if err != nil {
		return githubWebhookAccess{
			Message: "gh could not confirm repository access or admin permission for " + repository,
			Next:    "Check that gh can read the repository, that the account can administer it, and that its token can manage hooks; or paste the webhook URL and secret into GitHub manually.",
		}
	}
	if !strings.EqualFold(strings.TrimSpace(string(out)), "true") {
		return githubWebhookAccess{
			Message: "the gh account does not have repository admin permission for " + repository,
			Next:    "Use an account that can administer the repository, or paste the webhook URL and secret into GitHub manually.",
		}
	}
	return githubWebhookAccess{OK: true}
}

func configureGitHubWebhook(ctx context.Context, access githubWebhookAccess, repository, webhookURL string, secret []byte) githubWebhookReport {
	if !access.OK {
		return githubWebhookReport{Status: githubWebhookSkipped, Message: access.Message, Next: access.Next}
	}
	if len(secret) == 0 {
		return githubWebhookReport{
			Status:  githubWebhookSkipped,
			Message: "no webhook secret is available",
			Next:    "Rerun with --rotate-webhook-secret --github-webhook.",
		}
	}
	hooks, failure := listGitHubWebhooks(ctx, repository)
	if failure != nil {
		return *failure
	}
	matches := matchingGitHubHooks(hooks, webhookURL)
	switch len(matches) {
	case 1:
		return updateGitHubHook(ctx, repository, webhookURL, matches[0], secret)
	case 0:
		return createGitHubHook(ctx, repository, webhookURL, secret)
	default:
		return duplicateGitHubHooks(len(matches))
	}
}

// ensureGitHubWebhook is for an already-enabled environment. It verifies the
// existing hook's shape but never PATCHes it: GitHub clears a hook secret when
// a PATCH omits it, and an existing secret cannot be read back to re-send.
func ensureGitHubWebhook(ctx context.Context, repository, webhookURL string) githubWebhookReport {
	access := inspectGitHubWebhookAccess(ctx, repository)
	if !access.OK {
		return githubWebhookReport{Status: githubWebhookSkipped, Message: access.Message, Next: access.Next}
	}
	hooks, failure := listGitHubWebhooks(ctx, repository)
	if failure != nil {
		return *failure
	}
	matches := matchingGitHubHooks(hooks, webhookURL)
	switch len(matches) {
	case 1:
		hook := matches[0]
		if !strings.EqualFold(strings.TrimSpace(hook.ContentType), "json") {
			return githubWebhookReport{
				Status:  githubWebhookSkipped,
				Message: "the existing webhook does not use the application/json content type",
				Next:    "Rerun with --rotate-webhook-secret --github-webhook, or correct the webhook in GitHub.",
			}
		}
		if !hook.Active || !hasPushEvent(hook.Events) {
			return githubWebhookReport{
				Status:  githubWebhookSkipped,
				Message: "the existing webhook is inactive or does not include push events",
				Next:    "Rerun with --rotate-webhook-secret --github-webhook to update it with a fresh secret, or correct it in GitHub.",
			}
		}
		return githubWebhookReport{Status: githubWebhookUnchanged, HookID: hook.ID}
	case 0:
		return githubWebhookReport{
			Status:  githubWebhookMissing,
			Message: "no repository webhook uses this environment's URL",
			Next:    "Rerun with --rotate-webhook-secret --github-webhook to create it with a new secret, or add it manually.",
		}
	default:
		return duplicateGitHubHooks(len(matches))
	}
}

func listGitHubWebhooks(ctx context.Context, repository string) ([]githubHook, *githubWebhookReport) {
	const query = ".[] | {id: .id, url: .config.url, active: .active, events: .events, content_type: .config.content_type} | @json"
	out, err := runGH(ctx, nil, "api", "--hostname", "github.com", "--paginate", "--jq", query, "repos/"+repository+"/hooks")
	if err != nil {
		report := githubWebhookFailure("gh could not list the repository webhooks", err)
		return nil, &report
	}
	var hooks []githubHook
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var hook githubHook
		if err := json.Unmarshal([]byte(line), &hook); err != nil {
			report := githubWebhookReport{
				Status:  githubWebhookFailed,
				Message: "gh returned repository webhook data that could not be read",
				Next:    "Paste the webhook URL and secret into GitHub manually.",
			}
			return nil, &report
		}
		if hook.ID <= 0 {
			report := githubWebhookReport{
				Status:  githubWebhookFailed,
				Message: "gh returned a repository webhook without a usable id",
				Next:    "Paste the webhook URL and secret into GitHub manually.",
			}
			return nil, &report
		}
		hooks = append(hooks, hook)
	}
	return hooks, nil
}

func createGitHubHook(ctx context.Context, repository, webhookURL string, secret []byte) githubWebhookReport {
	body, err := githubWebhookBody(webhookURL, secret, true)
	if err != nil {
		return githubWebhookReport{Status: githubWebhookFailed, Message: "could not encode the webhook request", Next: "Retry, or paste the webhook URL and secret into GitHub manually."}
	}
	defer clearBytes(body)
	out, err := runGH(ctx, body, "api", "--hostname", "github.com", "--method", "POST", "-H", "Content-Type: application/json", "--input", "-", "--jq", "{id: .id}", "repos/"+repository+"/hooks")
	if err != nil {
		return githubWebhookFailure("gh could not create the repository webhook", err)
	}
	return githubWebhookReport{Status: githubWebhookCreated, HookID: hookIDFromResponse(out)}
}

func updateGitHubHook(ctx context.Context, repository, webhookURL string, hook githubHook, secret []byte) githubWebhookReport {
	body, err := githubWebhookBody(webhookURL, secret, false)
	if err != nil {
		return githubWebhookReport{Status: githubWebhookFailed, Message: "could not encode the webhook request", Next: "Retry, or paste the webhook URL and secret into GitHub manually."}
	}
	defer clearBytes(body)
	_, err = runGH(ctx, body, "api", "--hostname", "github.com", "--method", "PATCH", "-H", "Content-Type: application/json", "--input", "-", "--jq", "{id: .id}", "repos/"+repository+"/hooks/"+strconv.FormatInt(hook.ID, 10))
	if err != nil {
		return githubWebhookFailure("gh could not update the repository webhook", err)
	}
	return githubWebhookReport{Status: githubWebhookUpdated, HookID: hook.ID}
}

func duplicateGitHubHooks(count int) githubWebhookReport {
	return githubWebhookReport{
		Status:  githubWebhookAmbiguous,
		Message: fmt.Sprintf("%d repository webhooks already use this environment's URL", count),
		Next:    "Remove the duplicate webhooks in GitHub, then rerun with --rotate-webhook-secret --github-webhook. No webhook was changed; if a secret was rotated it is already active and was shown once.",
	}
}

func matchingGitHubHooks(hooks []githubHook, webhookURL string) []githubHook {
	want := normalizeWebhookURL(webhookURL)
	var matches []githubHook
	for _, hook := range hooks {
		if normalizeWebhookURL(hook.URL) == want {
			matches = append(matches, hook)
		}
	}
	return matches
}

func normalizeWebhookURL(value string) string {
	return strings.TrimRight(strings.TrimSpace(value), "/")
}

func hasPushEvent(events []string) bool {
	for _, event := range events {
		if strings.EqualFold(strings.TrimSpace(event), "push") {
			return true
		}
	}
	return false
}

func githubWebhookBody(webhookURL string, secret []byte, create bool) ([]byte, error) {
	type config struct {
		URL         string `json:"url"`
		ContentType string `json:"content_type"`
		Secret      string `json:"secret"`
		InsecureSSL string `json:"insecure_ssl"`
	}
	type payload struct {
		Name   string   `json:"name,omitempty"`
		Active bool     `json:"active"`
		Events []string `json:"events"`
		Config config   `json:"config"`
	}
	body := payload{
		Active: true,
		Events: []string{"push"},
		Config: config{
			URL:         webhookURL,
			ContentType: "json",
			Secret:      string(secret),
			InsecureSSL: "0",
		},
	}
	if create {
		body.Name = "web"
	}
	return json.Marshal(body)
}

func hookIDFromResponse(raw []byte) int64 {
	var response struct {
		ID int64 `json:"id"`
	}
	if json.Unmarshal(bytes.TrimSpace(raw), &response) != nil {
		return 0
	}
	return response.ID
}

func githubWebhookFailure(message string, err error) githubWebhookReport {
	exit := ""
	var commandErr *ghCommandError
	if errors.As(err, &commandErr) && commandErr.exitCode > 0 {
		exit = fmt.Sprintf(" (gh exit status %d)", commandErr.exitCode)
	}
	return githubWebhookReport{
		Status:  githubWebhookFailed,
		Message: message + exit,
		Next:    "Paste the webhook URL and secret into GitHub manually, or fix gh and rerun with --rotate-webhook-secret --github-webhook.",
	}
}

func runGH(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "gh", args...) //nolint:gosec // G204: fixed executable; values are constants or validated command inputs.
	command.Env = append(os.Environ(),
		"GH_PROMPT_DISABLED=1",
		"GH_NO_UPDATE_NOTIFIER=1",
		"GH_PAGER=cat",
	)
	if stdin != nil {
		command.Stdin = bytes.NewReader(stdin)
	}
	var stdout bytes.Buffer
	command.Stdout = &stdout
	if err := command.Run(); err != nil {
		exitCode := -1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		}
		return nil, &ghCommandError{exitCode: exitCode}
	}
	return stdout.Bytes(), nil
}
