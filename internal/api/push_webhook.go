package api

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

// DefaultWebhookMaxBody bounds GitHub deliveries on every environment route.
const DefaultWebhookMaxBody = 1 << 20

// DefaultWebhookAdmissionTimeout keeps the resolver, request body read and
// durable admission comfortably below GitHub's 10 second delivery timeout. The
// same absolute deadline is applied to the transport read and the database
// context; a slow body or database returns a bounded retryable 503, and the
// handler never replies 202 before the admission transaction commits.
const DefaultWebhookAdmissionTimeout = 8 * time.Second

// PushWebhookTarget is one environment's verified push-to-deploy binding. The
// secret is the environment's HMAC secret, never a revision or operation input.
// The environment, revision and secret revision identities are non-secret
// snapshots used to detect a binding change before admission commits.
type PushWebhookTarget struct {
	App         string
	Environment string
	// EnvironmentID and RevisionID identify the observed environment row and
	// current immutable revision.
	EnvironmentID string
	RevisionID    string
	// Repository is the lower-case owner/repository the source binds.
	Repository string
	// Branch is the revision's bound branch without refs/heads/.
	Branch string
	Secret []byte
	// SecretRevision and SecretRevisionHash identify the exact vault revision
	// whose plaintext verified this delivery.
	SecretRevision     uint64
	SecretRevisionHash string
}

// PushWebhookResolver reads the current revision and its vault secret in one
// joined database read. The boolean is false when the environment exists but
// auto-deploy is off; then the delivery is ignored without revealing the
// secret. Implementations must read the secret per delivery so rotation takes
// effect immediately; a TTL cache would accept an old secret after rotation.
type PushWebhookResolver interface {
	ResolvePushWebhook(context.Context, string, string) (PushWebhookTarget, bool, error)
}

// PushWebhookConfig mounts per-environment GitHub push delivery.
type PushWebhookConfig struct {
	Resolver   PushWebhookResolver
	Admissions WebhookAdmissionStore
	MaxBody    int64
	// AdmissionTimeout bounds the resolver, transport body read and durable
	// admission. Zero uses DefaultWebhookAdmissionTimeout.
	AdmissionTimeout time.Duration
	// Logger records admission duration and outcome only. It never receives the
	// body, signature, secret, sender or raw delivery ID.
	Logger *slog.Logger
}

type PushWebhookHandler struct {
	config PushWebhookConfig
}

// NewPushWebhookHandler verifies X-Hub-Signature-256 over the exact body,
// matches the repository and bound branch, durably admits the delivery and its
// River processing job in one PostgreSQL transaction, and only then replies
// 202. It never submits the platform operation synchronously and never reads
// Authorization: the per-environment URL and HMAC secret authenticate GitHub.
func NewPushWebhookHandler(config PushWebhookConfig) (*PushWebhookHandler, error) {
	if config.Resolver == nil {
		return nil, contract.NewFault(contract.ValidationFailed, "push webhook needs an environment resolver", "Mount it with control-plane environment and vault access.")
	}
	if config.Admissions == nil {
		return nil, contract.NewFault(contract.ValidationFailed, "push webhook needs a durable admission store", "Share one PostgreSQL admission store across every control-plane process.")
	}
	if config.MaxBody == 0 {
		config.MaxBody = DefaultWebhookMaxBody
	}
	if config.MaxBody < 1024 || config.MaxBody > 5<<20 {
		return nil, contract.NewFault(contract.ValidationFailed, "invalid webhook body bound", "Use 1 KiB–5 MiB.")
	}
	if config.AdmissionTimeout == 0 {
		config.AdmissionTimeout = DefaultWebhookAdmissionTimeout
	}
	if config.AdmissionTimeout < 100*time.Millisecond || config.AdmissionTimeout > 9*time.Second {
		return nil, contract.NewFault(contract.ValidationFailed, "invalid webhook admission timeout", "Use 100 ms–9 s; the bound must stay below GitHub's delivery timeout.")
	}
	return &PushWebhookHandler{config: config}, nil
}

func (h *PushWebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	started := time.Now()
	outcome := "rejected"
	targetLabel := ""
	bodyConsumed := false
	deadlineSet := false
	var controller *http.ResponseController
	if r.ProtoMajor < 2 {
		// A response written before the body is fully consumed must not let
		// net/http drain a stalled body after this handler returns. Closing the
		// HTTP/1 connection skips that drain; successful full-body reads remove
		// this header again to preserve keep-alive.
		w.Header().Set("Connection", "close")
	}
	defer func() {
		if recover() != nil {
			writeWebhook(w, http.StatusInternalServerError, webhookResponse{Message: "webhook submission failed internally"})
		}
		// Clear only after a fully consumed body. An unread body keeps the
		// transport deadline in place (HTTP/1 also keeps Connection: close), so
		// no post-handler drain can become unbounded and no completed request
		// poisons a keep-alive connection with a stale deadline.
		if deadlineSet && bodyConsumed && controller != nil {
			_ = controller.SetReadDeadline(time.Time{})
		}
		h.logAdmission(targetLabel, outcome, started)
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
	targetLabel = app + "/" + environment
	// One absolute deadline covers the resolver, the request body read and the
	// durable admission. The transport read deadline makes a stalled body
	// interruptible instead of relying on the server's 10 second ReadTimeout.
	deadline := started.Add(h.config.AdmissionTimeout)
	ctx, cancel := context.WithDeadline(r.Context(), deadline)
	defer cancel()
	controller = http.NewResponseController(w)
	if err := controller.SetReadDeadline(deadline); err != nil {
		if !errors.Is(err, http.ErrNotSupported) {
			writeWebhook(w, http.StatusInternalServerError, webhookResponse{Message: "webhook read deadline is unavailable"})
			return
		}
	} else {
		deadlineSet = true
	}
	target, enabled, err := h.config.Resolver.ResolvePushWebhook(ctx, app, environment)
	if err != nil {
		if contract.Classify(err).Code == contract.NotFound {
			writeWebhook(w, http.StatusNotFound, webhookResponse{Message: "environment not found"})
			return
		}
		h.writeError(w, err)
		return
	}
	if !enabled {
		outcome = "ignored"
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
		if ctx.Err() != nil || isNetworkTimeout(err) {
			writeWebhook(w, http.StatusServiceUnavailable, webhookResponse{
				Message: "webhook admission did not finish within its bound; retry the delivery",
				Code:    string(contract.Unavailable),
			})
			return
		}
		writeWebhook(w, http.StatusBadRequest, webhookResponse{Message: "delivery body is unreadable"})
		return
	}
	if int64(len(body)) > h.config.MaxBody {
		writeWebhook(w, http.StatusRequestEntityTooLarge, webhookResponse{Message: "delivery is too large"})
		return
	}
	// io.ReadAll reached EOF within the limit, so the body is fully consumed.
	// HTTP/1 can safely keep this connection alive without a post-handler
	// drain, and the deadline can be cleared on return.
	bodyConsumed = true
	if r.ProtoMajor < 2 {
		w.Header().Del("Connection")
	}
	signatures := r.Header.Values("X-Hub-Signature-256")
	if len(signatures) != 1 || !validGitHubSignature(target.Secret, body, signatures[0]) {
		w.Header().Set("WWW-Authenticate", "GitHub-HMAC-SHA256")
		writeWebhook(w, http.StatusUnauthorized, webhookResponse{Message: "invalid GitHub signature"})
		return
	}
	signature := signatures[0]
	if len(r.Header.Values("X-GitHub-Event")) != 1 {
		writeWebhook(w, http.StatusBadRequest, webhookResponse{Message: "exactly one GitHub event header is required"})
		return
	}
	switch eventName := r.Header.Get("X-GitHub-Event"); eventName {
	case "ping":
		outcome = "ignored"
		writeWebhook(w, http.StatusOK, webhookResponse{Message: "pong"})
		return
	case "push":
	default:
		outcome = "ignored"
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
		outcome = "ignored"
		writeWebhook(w, http.StatusOK, webhookResponse{Message: "repository is not bound to this environment"})
		return
	}
	if payload.Deleted {
		outcome = "ignored"
		writeWebhook(w, http.StatusOK, webhookResponse{Message: "branch deletion is not deployed"})
		return
	}
	branch, ok := pushBranch(payload.Ref)
	if !ok || !PushBranchMatches(target.Branch, branch) {
		outcome = "ignored"
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
	rawInput, err := json.Marshal(struct {
		Ref string `json:"ref"`
	}{Ref: sha})
	if err != nil {
		h.writeError(w, err)
		return
	}
	normalized, err := platform.Default().Normalize(platform.KindDeploy, targetName.String(), rawInput)
	if err != nil {
		h.writeError(w, err)
		return
	}
	deliveryHash := WebhookDeliveryHash(deliveryID)
	bodyHash := WebhookBodyHash(body)
	// incoming is this request's freshly verified snapshot. record is the
	// durable row Admit returns, which may be a legacy pending row with empty
	// snapshot columns; legacy reconciliation must use incoming, never record.
	var incoming WebhookAdmission
	var record WebhookAdmission
	created := false
	admissionOK := false
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 {
			// The admission transaction observed a changed environment,
			// revision or secret revision. Re-resolve and verify the same raw
			// bytes against the current secret before retrying once.
			target, enabled, err = h.config.Resolver.ResolvePushWebhook(ctx, app, environment)
			if err != nil {
				if contract.Classify(err).Code == contract.NotFound {
					writeWebhook(w, http.StatusNotFound, webhookResponse{Message: "environment not found"})
					return
				}
				h.writeError(w, err)
				return
			}
			if !enabled {
				outcome = "ignored"
				writeWebhook(w, http.StatusOK, webhookResponse{Message: "auto-deploy is not enabled"})
				return
			}
			if !validGitHubSignature(target.Secret, body, signature) {
				w.Header().Set("WWW-Authenticate", "GitHub-HMAC-SHA256")
				writeWebhook(w, http.StatusUnauthorized, webhookResponse{Message: "invalid GitHub signature"})
				return
			}
			if repository != target.Repository {
				outcome = "ignored"
				writeWebhook(w, http.StatusOK, webhookResponse{Message: "repository is not bound to this environment"})
				return
			}
			if !PushBranchMatches(target.Branch, branch) {
				outcome = "ignored"
				writeWebhook(w, http.StatusOK, webhookResponse{Message: "push is not to the bound branch"})
				return
			}
		}
		incoming = WebhookAdmission{
			DeliveryIDHash: deliveryHash, BodySHA256: bodyHash, Actor: actor,
			Kind: platform.KindDeploy, Target: targetName.String(),
			EnvironmentID: target.EnvironmentID, RevisionID: target.RevisionID,
			SecretRevision: target.SecretRevision, SecretRevisionHash: target.SecretRevisionHash,
			Repository: target.Repository, Branch: target.Branch, Commit: sha,
		}
		record, created, err = h.config.Admissions.Admit(ctx, incoming)
		if err == nil {
			admissionOK = true
			break
		}
		if errors.Is(err, ErrWebhookBindingChanged) {
			continue
		}
		outcome = "rejected"
		h.writeAdmissionError(w, err)
		return
	}
	if !admissionOK {
		outcome = "stale"
		writeWebhook(w, http.StatusServiceUnavailable, webhookResponse{
			Message: "the environment binding changed while the delivery was admitted; retry the delivery",
			Code:    string(contract.Unavailable),
		})
		return
	}
	switch record.State {
	case WebhookDeliverySubmitted:
		outcome = "duplicate"
		writeWebhook(w, http.StatusAccepted, webhookResponse{
			Accepted: true, OperationID: string(record.OperationID), Kind: record.Kind, Target: record.Target,
			Message: "duplicate delivery; the original operation was already accepted",
		})
	case WebhookDeliveryAdmitted:
		if created {
			outcome = "accepted"
			writeWebhook(w, http.StatusAccepted, webhookResponse{
				Accepted: true, Kind: record.Kind, Target: record.Target,
				Message: "delivery accepted for durable processing",
			})
			return
		}
		outcome = "duplicate"
		writeWebhook(w, http.StatusAccepted, webhookResponse{
			Accepted: true, Kind: record.Kind, Target: record.Target,
			Message: "duplicate delivery; it is already accepted for durable processing",
		})
	case WebhookDeliveryFailed:
		outcome = "failed"
		writeWebhook(w, http.StatusAccepted, webhookResponse{
			Message: "duplicate delivery; its submission previously failed and was not retried",
			Code:    record.ErrorCode,
		})
	case WebhookDeliveryPending:
		// A row from the synchronous handler has no recorded commit. Reconcile
		// the operation its old idempotency key may already have created; never
		// resubmit or invent an operation.
		updated, found, err := h.config.Admissions.ReconcileLegacy(ctx, incoming, "ghdelivery-"+deliveryID, normalized.Canonical)
		if err != nil {
			outcome = "rejected"
			h.writeAdmissionError(w, err)
			return
		}
		if found {
			switch updated.State {
			case WebhookDeliverySubmitted:
				outcome = "duplicate"
				writeWebhook(w, http.StatusAccepted, webhookResponse{
					Accepted: true, OperationID: string(updated.OperationID), Kind: updated.Kind, Target: updated.Target,
					Message: "duplicate delivery; the original operation was already accepted",
				})
			case WebhookDeliveryAdmitted:
				outcome = "duplicate"
				writeWebhook(w, http.StatusAccepted, webhookResponse{
					Accepted: true, Kind: updated.Kind, Target: updated.Target,
					Message: "duplicate delivery; it is already accepted for durable processing",
				})
			case WebhookDeliveryFailed:
				outcome = "failed"
				writeWebhook(w, http.StatusAccepted, webhookResponse{
					Message: "duplicate delivery; its submission previously failed and was not retried",
					Code:    updated.ErrorCode,
				})
			default:
				outcome = "ambiguous"
				writeWebhook(w, http.StatusAccepted, webhookResponse{
					Accepted: true, Ambiguous: true, Kind: updated.Kind, Target: updated.Target,
					Message: "duplicate delivery; its durable state cannot be interpreted",
				})
			}
			return
		}
		outcome = "ambiguous"
		writeWebhook(w, http.StatusAccepted, webhookResponse{
			Accepted: true, Ambiguous: true, Kind: record.Kind, Target: record.Target,
			Message: "duplicate delivery; the original operation is no longer retained, so its acceptance cannot be proven",
		})
	default:
		outcome = "unavailable"
		writeWebhook(w, http.StatusServiceUnavailable, webhookResponse{
			Message: "webhook delivery is in an unknown durable state", Code: string(contract.Unavailable),
		})
	}
}

func isNetworkTimeout(err error) bool {
	var networkError net.Error
	return errors.As(err, &networkError) && networkError.Timeout()
}

func (h *PushWebhookHandler) writeAdmissionError(w http.ResponseWriter, err error) {
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		writeWebhook(w, http.StatusServiceUnavailable, webhookResponse{
			Message: "webhook admission did not finish within its bound; retry the delivery",
			Code:    string(contract.Unavailable),
		})
		return
	}
	h.writeError(w, err)
}

func (h *PushWebhookHandler) logAdmission(target, outcome string, started time.Time) {
	if h.config.Logger == nil || target == "" {
		return
	}
	h.config.Logger.Info("github webhook admission", "target", target, "outcome", outcome, "duration_ms", time.Since(started).Milliseconds())
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
	Ambiguous   bool   `json:"ambiguous,omitempty"`
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
