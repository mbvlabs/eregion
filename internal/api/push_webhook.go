package api

import (
	"context"
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

// DefaultWebhookMaxBody bounds GitHub deliveries on every environment route.
const DefaultWebhookMaxBody = 1 << 20

// PushWebhookTarget is one environment's verified push-to-deploy binding. The
// secret is the environment's HMAC secret, never a revision or operation input.
type PushWebhookTarget struct {
	App         string
	Environment string
	// Repository is the lower-case owner/repository the source binds.
	Repository string
	// Branch is the revision's bound branch without refs/heads/.
	Branch string
	Secret []byte
}

// PushWebhookResolver reads the current revision and its vault secret. The
// boolean is false when the environment exists but auto-deploy is off; then
// the delivery is ignored without revealing the secret.
type PushWebhookResolver interface {
	ResolvePushWebhook(context.Context, string, string) (PushWebhookTarget, bool, error)
}

// PushWebhookConfig mounts per-environment GitHub push delivery.
type PushWebhookConfig struct {
	Resolver   PushWebhookResolver
	Deliveries WebhookDeliveryStore
	MaxBody    int64
}

type PushWebhookHandler struct {
	config    PushWebhookConfig
	submitter operations.Service
}

// NewPushWebhookHandler verifies X-Hub-Signature-256 over the exact body,
// matches the repository and bound branch, deduplicates by X-GitHub-Delivery,
// and submits platform.deploy with the pushed commit SHA. It never reads
// Authorization: the per-environment URL and HMAC secret authenticate GitHub.
func NewPushWebhookHandler(config PushWebhookConfig, submitter operations.Service) (*PushWebhookHandler, error) {
	if submitter == nil {
		return nil, contract.NewFault(contract.ValidationFailed, "push webhook needs an operation service", "Pass the same operations.Service used by the API and worker.")
	}
	if config.Resolver == nil {
		return nil, contract.NewFault(contract.ValidationFailed, "push webhook needs an environment resolver", "Mount it with control-plane environment and vault access.")
	}
	if config.Deliveries == nil {
		return nil, contract.NewFault(contract.ValidationFailed, "push webhook needs a durable delivery store", "Share one PostgreSQL delivery store across every control-plane process.")
	}
	if config.MaxBody == 0 {
		config.MaxBody = DefaultWebhookMaxBody
	}
	if config.MaxBody < 1024 || config.MaxBody > 5<<20 {
		return nil, contract.NewFault(contract.ValidationFailed, "invalid webhook body bound", "Use 1 KiB–5 MiB.")
	}
	return &PushWebhookHandler{config: config, submitter: submitter}, nil
}

func (h *PushWebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
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
	app, environment, ok := pushWebhookPath(r.URL.Path)
	if !ok {
		writeWebhook(w, http.StatusNotFound, webhookResponse{Message: "webhook endpoint not found"})
		return
	}
	target, enabled, err := h.config.Resolver.ResolvePushWebhook(r.Context(), app, environment)
	if err != nil {
		if target.App == "" && contract.Classify(err).Code == contract.NotFound {
			writeWebhook(w, http.StatusNotFound, webhookResponse{Message: "environment not found"})
			return
		}
		h.writeError(w, err)
		return
	}
	if !enabled {
		writeWebhook(w, http.StatusOK, webhookResponse{Message: "auto-deploy is not enabled"})
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
	if len(signatures) != 1 || !validGitHubSignature(target.Secret, body, signatures[0]) {
		w.Header().Set("WWW-Authenticate", "GitHub-HMAC-SHA256")
		writeWebhook(w, http.StatusUnauthorized, webhookResponse{Message: "invalid GitHub signature"})
		return
	}
	if len(r.Header.Values("X-GitHub-Event")) != 1 {
		writeWebhook(w, http.StatusBadRequest, webhookResponse{Message: "exactly one GitHub event header is required"})
		return
	}
	switch eventName := r.Header.Get("X-GitHub-Event"); eventName {
	case "ping":
		writeWebhook(w, http.StatusOK, webhookResponse{Message: "pong"})
		return
	case "push":
	default:
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
	if !repositoryPattern.MatchString(repository) {
		writeWebhook(w, http.StatusBadRequest, webhookResponse{Message: "invalid repository name"})
		return
	}
	if repository != target.Repository {
		writeWebhook(w, http.StatusOK, webhookResponse{Message: "repository is not bound to this environment"})
		return
	}
	if payload.Deleted {
		writeWebhook(w, http.StatusOK, webhookResponse{Message: "branch deletion is not deployed"})
		return
	}
	branch, ok := pushBranch(payload.Ref)
	if !ok || !PushBranchMatches(target.Branch, branch) {
		writeWebhook(w, http.StatusOK, webhookResponse{Message: "push is not to the bound branch"})
		return
	}
	sha := strings.ToLower(payload.After)
	if !commitPattern.MatchString(sha) || strings.Trim(sha, "0") == "" {
		writeWebhook(w, http.StatusBadRequest, webhookResponse{Message: "invalid commit SHA"})
		return
	}
	actor := contract.Actor{Kind: "webhook", Subject: "github:" + repository}
	if err := actor.Validate(); err != nil {
		h.writeError(w, err)
		return
	}
	targetName, err := platform.NewEnvironmentTarget(target.App, target.Environment)
	if err != nil {
		h.writeError(w, err)
		return
	}
	input, err := json.Marshal(struct {
		Ref string `json:"ref"`
	}{Ref: sha})
	if err != nil {
		h.writeError(w, err)
		return
	}
	idempotencyKey := "ghdelivery-" + deliveryID
	if err := contract.ValidateIdempotencyKey(idempotencyKey); err != nil {
		h.writeError(w, err)
		return
	}
	deliveryHash := WebhookDeliveryHash(deliveryID)
	bodyHash := WebhookBodyHash(body)
	record, created, err := h.config.Deliveries.Reserve(r.Context(), WebhookDelivery{
		DeliveryIDHash: deliveryHash, BodySHA256: bodyHash, Actor: actor,
		Kind: platform.KindDeploy, Target: targetName.String(),
	})
	if err != nil {
		if contract.Classify(err).Code == contract.Conflict {
			h.writeError(w, err)
			return
		}
		writeWebhook(w, http.StatusServiceUnavailable, webhookResponse{
			Message: "webhook delivery could not be recorded durably", Code: string(contract.Unavailable),
		})
		return
	}
	if !created && record.Submitted {
		// A redelivery returns the originally accepted operation; it never
		// submits new work even after operation retention purged the row.
		writeWebhook(w, http.StatusAccepted, webhookResponse{
			Accepted: true, OperationID: string(record.OperationID), Kind: record.Kind, Target: record.Target,
			Message: "duplicate delivery; the original operation was already accepted",
		})
		return
	}
	op, err := h.submitter.Submit(r.Context(), operations.Submission{
		Kind: platform.KindDeploy, Target: targetName.String(), Actor: actor,
		IdempotencyKey: idempotencyKey, Input: input,
	})
	if err != nil {
		h.writeError(w, err)
		return
	}
	if err := h.config.Deliveries.Complete(r.Context(), deliveryHash, op.ID); err != nil {
		// The operation is already durable; only the replay record failed.
		// Ask GitHub to retry so the same delivery is completed, never
		// resubmitted as new work.
		writeWebhook(w, http.StatusServiceUnavailable, webhookResponse{
			Message: "operation was accepted but the delivery replay record is not durable", Code: string(contract.Unavailable),
			OperationID: string(op.ID),
		})
		return
	}
	writeWebhook(w, http.StatusAccepted, webhookResponse{
		Accepted: true, OperationID: string(op.ID), Kind: op.Kind, Target: op.Target,
		Message: "delivery accepted for durable processing",
	})
}

func (h *PushWebhookHandler) writeError(w http.ResponseWriter, err error) {
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

// pushWebhookPath splits /v1/webhooks/github/<app>/<env>.
func pushWebhookPath(path string) (string, string, bool) {
	const prefix = "/v1/webhooks/github/"
	if !strings.HasPrefix(path, prefix) {
		return "", "", false
	}
	parts := strings.Split(strings.TrimPrefix(path, prefix), "/")
	if len(parts) != 2 {
		return "", "", false
	}
	if platform.ValidateName(parts[0]) != nil || platform.ValidateName(parts[1]) != nil {
		return "", "", false
	}
	return parts[0], parts[1], true
}

// PushBranchMatches reports whether a pushed branch is the revision's bound
// branch. A revision may record the full refs/heads/ form.
func PushBranchMatches(bound, pushed string) bool {
	bound = strings.TrimPrefix(strings.TrimSpace(bound), "refs/heads/")
	return bound != "" && bound == strings.TrimSpace(pushed)
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
	repositoryPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9._-]{0,98}[a-z0-9])?/[a-z0-9]([a-z0-9._-]{0,98}[a-z0-9])?$`)
	deliveryPattern   = regexp.MustCompile(`^[A-Za-z0-9-]{1,100}$`)
	commitPattern     = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)
)
