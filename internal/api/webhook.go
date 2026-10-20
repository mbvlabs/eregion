package api

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
)

const (
	// DefaultWebhookKind is the core release action. A deployment is a separate
	// core action, so a push never pretends that a deployment already exists.
	DefaultWebhookKind = platform.KindReleaseCreate
	// DefaultWebhookMaxBody bounds GitHub deliveries.
	DefaultWebhookMaxBody = 1 << 20
)

// AllowedPush is one exact repository/branch allowlist entry and its platform
// placement. The repository name in the payload is never used to choose an app.
type AllowedPush struct {
	Repository  string
	Branch      string
	App         string
	Environment string
}

// WebhookEvent is the validated, non-secret subset of a GitHub push delivery.
type WebhookEvent struct {
	DeliveryID string
	Repository string
	Branch     string
	CommitSHA  string
	Pusher     string
	Forced     bool
	Allowed    AllowedPush
}

// WebhookConfig is exported so the control-plane composition root can mount the
// handler. This handler is separate from the bearer-authenticated API handler:
// GitHub cannot send a bearer token, and adding a bypass inside server.go would
// weaken that boundary.
type WebhookConfig struct {
	Secret  []byte
	Allowed []AllowedPush
	// Kind defaults to DefaultWebhookKind (platform.release.create).
	Kind string
	// Target overrides the default env/<app>/<environment> target.
	Target func(WebhookEvent) (string, error)
	// Input overrides the default core release-create input. It is required for
	// kinds whose input cannot be derived from a push (for example a deploy that
	// needs an existing release ID).
	Input func(WebhookEvent) (json.RawMessage, error)
	// MaxBody defaults to 1 MiB and may not exceed 5 MiB.
	MaxBody int64
}

type WebhookHandler struct {
	config    WebhookConfig
	submitter operations.Service
	allowed   map[string]map[string]AllowedPush
}

// NewWebhookHandler verifies X-Hub-Signature-256 over the exact request body,
// allowlists repository and branch, derives a stable idempotency key from
// X-GitHub-Delivery, and submits through operations.Service.Submit. It never
// reads Authorization and never starts a queue.
func NewWebhookHandler(config WebhookConfig, submitter operations.Service) (*WebhookHandler, error) {
	if submitter == nil {
		return nil, contract.NewFault(contract.ValidationFailed, "webhook handler needs an operation service", "Pass the same operations.Service used by the API and worker.")
	}
	if len(config.Secret) < 16 || len(config.Secret) > 512 || strings.ContainsAny(string(config.Secret), "\x00") {
		return nil, contract.NewFault(contract.ValidationFailed, "webhook secret must be 16–512 bytes", "Configure a random GitHub webhook secret; never put it in a URL or query.")
	}
	if len(config.Allowed) == 0 {
		return nil, contract.NewFault(contract.ValidationFailed, "webhook allowlist is empty", "Allowlist at least one exact repository and branch; the handler fails closed.")
	}
	if config.Kind == "" {
		config.Kind = DefaultWebhookKind
	}
	if !webhookKindPattern.MatchString(config.Kind) {
		return nil, contract.NewFault(contract.ValidationFailed, "invalid webhook action kind", "Use a registered platform.<action> kind.")
	}
	if config.Kind != platform.KindReleaseCreate && config.Input == nil {
		return nil, contract.NewFault(contract.ValidationFailed, "webhook kind needs an explicit input mapping", "Only platform.release.create has a default push mapping because a deploy requires an existing release ID.")
	}
	if config.MaxBody == 0 {
		config.MaxBody = DefaultWebhookMaxBody
	}
	if config.MaxBody < 1024 || config.MaxBody > 5<<20 {
		return nil, contract.NewFault(contract.ValidationFailed, "invalid webhook body bound", "Use 1 KiB–5 MiB.")
	}
	handler := &WebhookHandler{config: config, submitter: submitter, allowed: map[string]map[string]AllowedPush{}}
	for _, entry := range config.Allowed {
		entry.Repository = strings.ToLower(strings.TrimSpace(entry.Repository))
		if !repositoryPattern.MatchString(entry.Repository) || strings.Count(entry.Repository, "/") != 1 {
			return nil, contract.NewFault(contract.ValidationFailed, "invalid webhook repository allowlist entry", "Use owner/repository with letters, digits, dots, underscores, and hyphens.")
		}
		if entry.Branch == "" || len(entry.Branch) > 255 || strings.ContainsAny(entry.Branch, "\x00\r\n\t ") || strings.HasPrefix(entry.Branch, "/") || strings.Contains(entry.Branch, "..") {
			return nil, contract.NewFault(contract.ValidationFailed, "invalid webhook branch allowlist entry", "Use an exact branch name without whitespace, a leading slash, or '..'.")
		}
		if err := platform.ValidateName(entry.App); err != nil {
			return nil, contract.NewFault(contract.ValidationFailed, "invalid webhook app mapping", "Map each allowlisted repository to a valid platform app name.")
		}
		if err := platform.ValidateName(entry.Environment); err != nil {
			return nil, contract.NewFault(contract.ValidationFailed, "invalid webhook environment mapping", "Map each allowlisted repository to a valid environment name.")
		}
		if handler.allowed[entry.Repository] == nil {
			handler.allowed[entry.Repository] = map[string]AllowedPush{}
		}
		if _, exists := handler.allowed[entry.Repository][entry.Branch]; exists {
			return nil, contract.NewFault(contract.ValidationFailed, "duplicate webhook allowlist entry", "Keep one exact repository/branch entry.")
		}
		handler.allowed[entry.Repository][entry.Branch] = entry
	}
	return handler, nil
}

func (h *WebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	defer func() {
		if recover() != nil {
			writeWebhook(w, http.StatusInternalServerError, webhookResponse{Message: "webhook submission failed internally"})
		}
	}()
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeWebhook(w, http.StatusMethodNotAllowed, webhookResponse{Message: "use POST"})
		return
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		writeWebhook(w, http.StatusBadRequest, webhookResponse{Message: "JSON content type required"})
		return
	}
	if contentLength := r.ContentLength; contentLength > h.config.MaxBody {
		writeWebhook(w, http.StatusRequestEntityTooLarge, webhookResponse{Message: "delivery is too large"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, h.config.MaxBody+1))
	if err != nil {
		writeWebhook(w, http.StatusBadRequest, webhookResponse{Message: "delivery body is unreadable"})
		return
	}
	if int64(len(body)) > h.config.MaxBody {
		writeWebhook(w, http.StatusRequestEntityTooLarge, webhookResponse{Message: "delivery is too large"})
		return
	}
	signatures := r.Header.Values("X-Hub-Signature-256")
	if len(signatures) != 1 || !validGitHubSignature(h.config.Secret, body, signatures[0]) {
		w.Header().Set("WWW-Authenticate", "GitHub-HMAC-SHA256")
		writeWebhook(w, http.StatusUnauthorized, webhookResponse{Message: "invalid GitHub signature"})
		return
	}
	if len(r.Header.Values("X-GitHub-Event")) != 1 {
		writeWebhook(w, http.StatusBadRequest, webhookResponse{Message: "exactly one GitHub event header is required"})
		return
	}
	eventName := r.Header.Get("X-GitHub-Event")
	if eventName == "ping" {
		writeWebhook(w, http.StatusOK, webhookResponse{Message: "pong"})
		return
	}
	if eventName != "push" {
		writeWebhook(w, http.StatusOK, webhookResponse{Message: "event is not handled"})
		return
	}
	deliveries := r.Header.Values("X-GitHub-Delivery")
	if len(deliveries) != 1 || !deliveryPattern.MatchString(deliveries[0]) {
		writeWebhook(w, http.StatusBadRequest, webhookResponse{Message: "a valid X-GitHub-Delivery header is required"})
		return
	}
	deliveryID := deliveries[0]
	payload, err := decodePush(body)
	if err != nil {
		writeWebhook(w, http.StatusBadRequest, webhookResponse{Message: "invalid push payload"})
		return
	}
	repository := strings.ToLower(payload.Repository.FullName)
	entries := h.allowed[repository]
	if entries == nil {
		writeWebhook(w, http.StatusOK, webhookResponse{Message: "repository is not allowlisted"})
		return
	}
	if payload.Deleted {
		writeWebhook(w, http.StatusOK, webhookResponse{Message: "branch deletion is not deployed"})
		return
	}
	branch, ok := pushBranch(payload.Ref)
	if !ok {
		writeWebhook(w, http.StatusOK, webhookResponse{Message: "only branch pushes are deployed"})
		return
	}
	entry, ok := entries[branch]
	if !ok {
		writeWebhook(w, http.StatusOK, webhookResponse{Message: "branch is not allowlisted"})
		return
	}
	sha := strings.ToLower(payload.After)
	if !commitPattern.MatchString(sha) || strings.Trim(sha, "0") == "" {
		writeWebhook(w, http.StatusBadRequest, webhookResponse{Message: "invalid commit SHA"})
		return
	}
	event := WebhookEvent{
		DeliveryID: deliveryID, Repository: repository, Branch: branch, CommitSHA: sha,
		Pusher: payload.Sender.Login, Forced: payload.Forced, Allowed: entry,
	}
	target, err := h.target(event)
	if err != nil {
		h.writeError(w, err)
		return
	}
	input, err := h.input(event)
	if err != nil {
		h.writeError(w, err)
		return
	}
	if len(input) == 0 || len(input) > platform.MaxInputBytes || !json.Valid(input) || !strings.HasPrefix(strings.TrimSpace(string(input)), "{") {
		writeWebhook(w, http.StatusBadRequest, webhookResponse{Message: "webhook input mapping produced invalid data"})
		return
	}
	actor := contract.Actor{Kind: "webhook", Subject: "github:" + repository}
	if err := actor.Validate(); err != nil {
		h.writeError(w, err)
		return
	}
	idempotencyKey := "ghdelivery-" + deliveryID
	if err := contract.ValidateIdempotencyKey(idempotencyKey); err != nil {
		h.writeError(w, err)
		return
	}
	op, err := h.submitter.Submit(r.Context(), operations.Submission{
		Kind: h.config.Kind, Target: target, Actor: actor,
		IdempotencyKey: idempotencyKey, Input: input,
	})
	if err != nil {
		h.writeError(w, err)
		return
	}
	writeWebhook(w, http.StatusAccepted, webhookResponse{
		Accepted: true, OperationID: string(op.ID), Kind: op.Kind, Target: op.Target,
		Message: "delivery accepted for durable processing",
	})
}

func (h *WebhookHandler) target(event WebhookEvent) (string, error) {
	if h.config.Target != nil {
		target, err := h.config.Target(event)
		if err != nil {
			return "", err
		}
		if _, err := platform.ParseTarget(target); err != nil {
			return "", contract.NewFault(contract.ValidationFailed, "webhook target mapping is invalid", "Return a canonical platform target.")
		}
		return target, nil
	}
	target, err := platform.NewEnvironmentTarget(event.Allowed.App, event.Allowed.Environment)
	if err != nil {
		return "", err
	}
	return target.String(), nil
}

func (h *WebhookHandler) input(event WebhookEvent) (json.RawMessage, error) {
	if h.config.Input != nil {
		return h.config.Input(event)
	}
	input := struct {
		App         string    `json:"app"`
		Environment string    `json:"environment"`
		Source      gitSource `json:"source"`
	}{
		App: event.Allowed.App, Environment: event.Allowed.Environment,
		Source: gitSource{Type: "git", URL: "https://github.com/" + event.Repository + ".git", Ref: event.CommitSHA},
	}
	return json.Marshal(input)
}

type gitSource struct {
	Type string `json:"type"`
	URL  string `json:"url"`
	Ref  string `json:"ref"`
}

func (h *WebhookHandler) writeError(w http.ResponseWriter, err error) {
	fault := contract.Classify(err)
	status := http.StatusInternalServerError
	switch fault.Code {
	case contract.InputRequired, contract.Usage, contract.ValidationFailed:
		status = http.StatusBadRequest
	case contract.Unauthenticated:
		status = http.StatusUnauthorized
	case contract.Forbidden:
		status = http.StatusForbidden
	case contract.NotFound:
		status = http.StatusNotFound
	case contract.Conflict, contract.PreconditionFailed, contract.ConfirmationRequired:
		status = http.StatusConflict
	case contract.Unavailable, contract.Cancelled:
		status = http.StatusServiceUnavailable
	}
	writeWebhook(w, status, webhookResponse{Error: fault.Message, Hint: fault.Hint, Code: string(fault.Code)})
}

type webhookResponse struct {
	Accepted    bool   `json:"accepted,omitempty"`
	OperationID string `json:"operation_id,omitempty"`
	Kind        string `json:"kind,omitempty"`
	Target      string `json:"target,omitempty"`
	Message     string `json:"message,omitempty"`
	Error       string `json:"error,omitempty"`
	Hint        string `json:"hint,omitempty"`
	Code        string `json:"code,omitempty"`
}

func writeWebhook(w http.ResponseWriter, status int, response webhookResponse) {
	raw, err := json.Marshal(response)
	if err != nil {
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(status)
	_, _ = w.Write(append(raw, '\n'))
}

func validGitHubSignature(secret, body []byte, header string) bool {
	if !strings.HasPrefix(header, "sha256=") {
		return false
	}
	provided, err := hex.DecodeString(strings.TrimPrefix(header, "sha256="))
	if err != nil || len(provided) != sha256.Size {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(body)
	return hmac.Equal(mac.Sum(nil), provided)
}

func pushBranch(ref string) (string, bool) {
	const prefix = "refs/heads/"
	if !strings.HasPrefix(ref, prefix) {
		return "", false
	}
	branch := strings.TrimPrefix(ref, prefix)
	if branch == "" || len(branch) > 255 || strings.ContainsAny(branch, "\x00\r\n") {
		return "", false
	}
	return branch, true
}

type pushPayload struct {
	Ref        string `json:"ref"`
	After      string `json:"after"`
	Deleted    bool   `json:"deleted"`
	Forced     bool   `json:"forced"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Sender struct {
		Login string `json:"login"`
	} `json:"sender"`
}

func decodePush(body []byte) (pushPayload, error) {
	var payload pushPayload
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	if err := decoder.Decode(&payload); err != nil {
		return payload, err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return payload, errors.New("trailing JSON")
	}
	return payload, nil
}

var (
	repositoryPattern  = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]{0,98}[a-z0-9])?/[a-z0-9]([a-z0-9._-]{0,98}[a-z0-9])?$`)
	deliveryPattern    = regexp.MustCompile(`^[A-Za-z0-9-]{1,100}$`)
	commitPattern      = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
	webhookKindPattern = regexp.MustCompile(`^platform\.[a-z0-9]+([.-][a-z0-9]+)*$`)
)
