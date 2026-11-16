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
	run := &pushWebhookRun{h: h, w: w, r: r, started: time.Now(), outcome: "rejected"}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.ProtoMajor < 2 {
		// A response written before the body is fully consumed must not let
		// net/http drain a stalled body after this handler returns. Closing the
		// HTTP/1 connection skips that drain; successful full-body reads remove
		// this header again to preserve keep-alive.
		w.Header().Set("Connection", "close")
	}
	defer run.finish()
	if !run.checkEndpoint() {
		return
	}
	run.targetLabel = run.app + "/" + run.environment
	// One absolute deadline covers the resolver, the request body read and the
	// durable admission. The transport read deadline makes a stalled body
	// interruptible instead of relying on the server's 10 second ReadTimeout.
	if !run.setDeadline() {
		return
	}
	if !run.resolveTarget() {
		return
	}
	if !run.readBody() {
		return
	}
	if !run.verifySignature() {
		return
	}
	if !run.verifyEvent() {
		return
	}
	if !run.parsePayload() {
		return
	}
	if !run.prepareAdmission() {
		return
	}
	run.admit()
}

type pushWebhookRun struct {
	h            *PushWebhookHandler
	w            http.ResponseWriter
	r            *http.Request
	started      time.Time
	outcome      string
	targetLabel  string
	bodyConsumed bool
	deadlineSet  bool
	controller   *http.ResponseController
	ctx          context.Context
	cancel       context.CancelFunc
	app          string
	environment  string
	target       PushWebhookTarget
	body         []byte
	signature    string
	deliveryID   string
	repository   string
	branch       string
	sha          string
	actor        contract.Actor
	targetName   platform.Target
	normalized   platform.Normalized
}

func (run *pushWebhookRun) finish() {
	if run.cancel != nil {
		run.cancel()
	}
	if recover() != nil {
		writeWebhook(run.w, http.StatusInternalServerError, webhookResponse{Message: "webhook submission failed internally"})
	}
	// Clear only after a fully consumed body. An unread body keeps the
	// transport deadline in place (HTTP/1 also keeps Connection: close), so
	// no post-handler drain can become unbounded and no completed request
	// poisons a keep-alive connection with a stale deadline.
	if run.deadlineSet && run.bodyConsumed && run.controller != nil {
		_ = run.controller.SetReadDeadline(time.Time{})
	}
	run.h.logAdmission(run.targetLabel, run.outcome, run.started)
}

func (run *pushWebhookRun) checkEndpoint() bool {
	if run.r.Method != http.MethodPost {
		run.w.Header().Set("Allow", http.MethodPost)
		writeWebhook(run.w, http.StatusMethodNotAllowed, webhookResponse{Message: "use POST"})
		return false
	}
	app, environment, ok := pushWebhookPath(run.r.URL.Path)
	if !ok {
		writeWebhook(run.w, http.StatusNotFound, webhookResponse{Message: "webhook endpoint not found"})
		return false
	}
	run.app, run.environment = app, environment
	return true
}

func (run *pushWebhookRun) setDeadline() bool {
	deadline := run.started.Add(run.h.config.AdmissionTimeout)
	run.ctx, run.cancel = context.WithDeadline(run.r.Context(), deadline)
	run.controller = http.NewResponseController(run.w)
	if err := run.controller.SetReadDeadline(deadline); err != nil {
		if !errors.Is(err, http.ErrNotSupported) {
			writeWebhook(run.w, http.StatusInternalServerError, webhookResponse{Message: "webhook read deadline is unavailable"})
			return false
		}
		return true
	}
	run.deadlineSet = true
	return true
}

func (run *pushWebhookRun) resolveTarget() bool {
	target, enabled, err := run.h.config.Resolver.ResolvePushWebhook(run.ctx, run.app, run.environment)
	if err != nil {
		if contract.Classify(err).Code == contract.NotFound {
			writeWebhook(run.w, http.StatusNotFound, webhookResponse{Message: "environment not found"})
			return false
		}
		run.h.writeError(run.w, err)
		return false
	}
	if !enabled {
		run.outcome = "ignored"
		writeWebhook(run.w, http.StatusOK, webhookResponse{Message: "auto-deploy is not enabled"})
		return false
	}
	run.target = target
	return true
}

func (run *pushWebhookRun) readBody() bool {
	if !strings.HasPrefix(run.r.Header.Get("Content-Type"), "application/json") {
		writeWebhook(run.w, http.StatusBadRequest, webhookResponse{Message: "JSON content type required"})
		return false
	}
	if contentLength := run.r.ContentLength; contentLength > run.h.config.MaxBody {
		writeWebhook(run.w, http.StatusRequestEntityTooLarge, webhookResponse{Message: "delivery is too large"})
		return false
	}
	body, err := io.ReadAll(io.LimitReader(run.r.Body, run.h.config.MaxBody+1))
	if err != nil {
		if run.ctx.Err() != nil || isNetworkTimeout(err) {
			writeWebhook(run.w, http.StatusServiceUnavailable, webhookResponse{
				Message: "webhook admission did not finish within its bound; retry the delivery",
				Code:    string(contract.Unavailable),
			})
			return false
		}
		writeWebhook(run.w, http.StatusBadRequest, webhookResponse{Message: "delivery body is unreadable"})
		return false
	}
	if int64(len(body)) > run.h.config.MaxBody {
		writeWebhook(run.w, http.StatusRequestEntityTooLarge, webhookResponse{Message: "delivery is too large"})
		return false
	}
	// io.ReadAll reached EOF within the limit, so the body is fully consumed.
	// HTTP/1 can safely keep this connection alive without a post-handler
	// drain, and the deadline can be cleared on return.
	run.body = body
	run.bodyConsumed = true
	if run.r.ProtoMajor < 2 {
		run.w.Header().Del("Connection")
	}
	return true
}

func (run *pushWebhookRun) verifySignature() bool {
	signatures := run.r.Header.Values("X-Hub-Signature-256")
	if len(signatures) != 1 || !validGitHubSignature(run.target.Secret, run.body, signatures[0]) {
		run.w.Header().Set("WWW-Authenticate", "GitHub-HMAC-SHA256")
		writeWebhook(run.w, http.StatusUnauthorized, webhookResponse{Message: "invalid GitHub signature"})
		return false
	}
	run.signature = signatures[0]
	return true
}

func (run *pushWebhookRun) verifyEvent() bool {
	if len(run.r.Header.Values("X-GitHub-Event")) != 1 {
		writeWebhook(run.w, http.StatusBadRequest, webhookResponse{Message: "exactly one GitHub event header is required"})
		return false
	}
	switch eventName := run.r.Header.Get("X-GitHub-Event"); eventName {
	case "ping":
		run.outcome = "ignored"
		writeWebhook(run.w, http.StatusOK, webhookResponse{Message: "pong"})
		return false
	case "push":
	default:
		run.outcome = "ignored"
		writeWebhook(run.w, http.StatusOK, webhookResponse{Message: "event is not handled"})
		return false
	}
	deliveries := run.r.Header.Values("X-GitHub-Delivery")
	if len(deliveries) != 1 || !deliveryPattern.MatchString(deliveries[0]) {
		writeWebhook(run.w, http.StatusBadRequest, webhookResponse{Message: "a valid X-GitHub-Delivery header is required"})
		return false
	}
	run.deliveryID = deliveries[0]
	return true
}

func (run *pushWebhookRun) parsePayload() bool {
	payload, err := decodePush(run.body)
	if err != nil {
		writeWebhook(run.w, http.StatusBadRequest, webhookResponse{Message: "invalid push payload"})
		return false
	}
	repository := strings.ToLower(payload.Repository.FullName)
	if !repositoryPattern.MatchString(repository) {
		writeWebhook(run.w, http.StatusBadRequest, webhookResponse{Message: "invalid repository name"})
		return false
	}
	if repository != run.target.Repository {
		run.outcome = "ignored"
		writeWebhook(run.w, http.StatusOK, webhookResponse{Message: "repository is not bound to this environment"})
		return false
	}
	if payload.Deleted {
		run.outcome = "ignored"
		writeWebhook(run.w, http.StatusOK, webhookResponse{Message: "branch deletion is not deployed"})
		return false
	}
	branch, ok := pushBranch(payload.Ref)
	if !ok || !PushBranchMatches(run.target.Branch, branch) {
		run.outcome = "ignored"
		writeWebhook(run.w, http.StatusOK, webhookResponse{Message: "push is not to the bound branch"})
		return false
	}
	sha := strings.ToLower(payload.After)
	if !commitPattern.MatchString(sha) || strings.Trim(sha, "0") == "" {
		writeWebhook(run.w, http.StatusBadRequest, webhookResponse{Message: "invalid commit SHA"})
		return false
	}
	run.repository, run.branch, run.sha = repository, branch, sha
	return true
}

func (run *pushWebhookRun) prepareAdmission() bool {
	actor := contract.Actor{Kind: "webhook", Subject: "github:" + run.repository}
	if err := actor.Validate(); err != nil {
		run.h.writeError(run.w, err)
		return false
	}
	targetName, err := platform.NewEnvironmentTarget(run.target.App, run.target.Environment)
	if err != nil {
		run.h.writeError(run.w, err)
		return false
	}
	rawInput, err := json.Marshal(struct {
		Ref string `json:"ref"`
	}{Ref: run.sha})
	if err != nil {
		run.h.writeError(run.w, err)
		return false
	}
	normalized, err := platform.Default().Normalize(platform.KindDeploy, targetName.String(), rawInput)
	if err != nil {
		run.h.writeError(run.w, err)
		return false
	}
	run.actor, run.targetName, run.normalized = actor, targetName, normalized
	return true
}

func (run *pushWebhookRun) admit() {
	deliveryHash := WebhookDeliveryHash(run.deliveryID)
	bodyHash := WebhookBodyHash(run.body)
	// incoming is this request's freshly verified snapshot. record is the
	// durable row Admit returns, which may be a legacy pending row with empty
	// snapshot columns; legacy reconciliation must use incoming, never record.
	var incoming WebhookAdmission
	var record WebhookAdmission
	created := false
	admissionOK := false
	for attempt := 0; attempt < 2; attempt++ {
		if attempt > 0 && !run.refreshTarget() {
			return
		}
		incoming = WebhookAdmission{
			DeliveryIDHash: deliveryHash, BodySHA256: bodyHash, Actor: run.actor,
			Kind: platform.KindDeploy, Target: run.targetName.String(),
			EnvironmentID: run.target.EnvironmentID, RevisionID: run.target.RevisionID,
			SecretRevision: run.target.SecretRevision, SecretRevisionHash: run.target.SecretRevisionHash,
			Repository: run.target.Repository, Branch: run.target.Branch, Commit: run.sha,
		}
		var err error
		record, created, err = run.h.config.Admissions.Admit(run.ctx, incoming)
		if err == nil {
			admissionOK = true
			break
		}
		if errors.Is(err, ErrWebhookBindingChanged) {
			continue
		}
		run.outcome = "rejected"
		run.h.writeAdmissionError(run.w, err)
		return
	}
	if !admissionOK {
		run.outcome = "stale"
		writeWebhook(run.w, http.StatusServiceUnavailable, webhookResponse{
			Message: "the environment binding changed while the delivery was admitted; retry the delivery",
			Code:    string(contract.Unavailable),
		})
		return
	}
	run.respond(record, created, incoming)
}

// refreshTarget re-resolves and verifies the binding after the admission
// transaction observed a changed environment, revision or secret revision.
func (run *pushWebhookRun) refreshTarget() bool {
	target, enabled, err := run.h.config.Resolver.ResolvePushWebhook(run.ctx, run.app, run.environment)
	if err != nil {
		if contract.Classify(err).Code == contract.NotFound {
			writeWebhook(run.w, http.StatusNotFound, webhookResponse{Message: "environment not found"})
			return false
		}
		run.h.writeError(run.w, err)
		return false
	}
	if !enabled {
		run.outcome = "ignored"
		writeWebhook(run.w, http.StatusOK, webhookResponse{Message: "auto-deploy is not enabled"})
		return false
	}
	if !validGitHubSignature(target.Secret, run.body, run.signature) {
		run.w.Header().Set("WWW-Authenticate", "GitHub-HMAC-SHA256")
		writeWebhook(run.w, http.StatusUnauthorized, webhookResponse{Message: "invalid GitHub signature"})
		return false
	}
	if run.repository != target.Repository {
		run.outcome = "ignored"
		writeWebhook(run.w, http.StatusOK, webhookResponse{Message: "repository is not bound to this environment"})
		return false
	}
	if !PushBranchMatches(target.Branch, run.branch) {
		run.outcome = "ignored"
		writeWebhook(run.w, http.StatusOK, webhookResponse{Message: "push is not to the bound branch"})
		return false
	}
	run.target = target
	return true
}

func (run *pushWebhookRun) respond(record WebhookAdmission, created bool, incoming WebhookAdmission) {
	switch record.State {
	case WebhookDeliverySubmitted:
		run.outcome = "duplicate"
		writeWebhook(run.w, http.StatusAccepted, webhookResponse{
			Accepted: true, OperationID: string(record.OperationID), Kind: record.Kind, Target: record.Target,
			Message: "duplicate delivery; the original operation was already accepted",
		})
	case WebhookDeliveryAdmitted:
		if created {
			run.outcome = "accepted"
			writeWebhook(run.w, http.StatusAccepted, webhookResponse{
				Accepted: true, Kind: record.Kind, Target: record.Target,
				Message: "delivery accepted for durable processing",
			})
			return
		}
		run.outcome = "duplicate"
		writeWebhook(run.w, http.StatusAccepted, webhookResponse{
			Accepted: true, Kind: record.Kind, Target: record.Target,
			Message: "duplicate delivery; it is already accepted for durable processing",
		})
	case WebhookDeliveryFailed:
		run.outcome = "failed"
		writeWebhook(run.w, http.StatusAccepted, webhookResponse{
			Message: "duplicate delivery; its submission previously failed and was not retried",
			Code:    record.ErrorCode,
		})
	case WebhookDeliveryPending:
		// A row from the synchronous handler has no recorded commit. Reconcile
		// the operation its old idempotency key may already have created; never
		// resubmit or invent an operation.
		run.respondLegacy(record, incoming)
	default:
		run.outcome = "unavailable"
		writeWebhook(run.w, http.StatusServiceUnavailable, webhookResponse{
			Message: "webhook delivery is in an unknown durable state", Code: string(contract.Unavailable),
		})
	}
}

func (run *pushWebhookRun) respondLegacy(record, incoming WebhookAdmission) {
	updated, found, err := run.h.config.Admissions.ReconcileLegacy(run.ctx, incoming, "ghdelivery-"+run.deliveryID, run.normalized.Canonical)
	if err != nil {
		run.outcome = "rejected"
		run.h.writeAdmissionError(run.w, err)
		return
	}
	if found {
		switch updated.State {
		case WebhookDeliverySubmitted:
			run.outcome = "duplicate"
			writeWebhook(run.w, http.StatusAccepted, webhookResponse{
				Accepted: true, OperationID: string(updated.OperationID), Kind: updated.Kind, Target: updated.Target,
				Message: "duplicate delivery; the original operation was already accepted",
			})
		case WebhookDeliveryAdmitted:
			run.outcome = "duplicate"
			writeWebhook(run.w, http.StatusAccepted, webhookResponse{
				Accepted: true, Kind: updated.Kind, Target: updated.Target,
				Message: "duplicate delivery; it is already accepted for durable processing",
			})
		case WebhookDeliveryFailed:
			run.outcome = "failed"
			writeWebhook(run.w, http.StatusAccepted, webhookResponse{
				Message: "duplicate delivery; its submission previously failed and was not retried",
				Code:    updated.ErrorCode,
			})
		case WebhookDeliveryPending:
			run.writeAmbiguous(updated.Kind, updated.Target, "duplicate delivery; its durable state cannot be interpreted")
		default:
			run.writeAmbiguous(updated.Kind, updated.Target, "duplicate delivery; its durable state cannot be interpreted")
		}
		return
	}
	run.writeAmbiguous(record.Kind, record.Target, "duplicate delivery; the original operation is no longer retained, so its acceptance cannot be proven")
}

func (run *pushWebhookRun) writeAmbiguous(kind, target, message string) {
	run.outcome = "ambiguous"
	writeWebhook(run.w, http.StatusAccepted, webhookResponse{
		Accepted: true, Ambiguous: true, Kind: kind, Target: target, Message: message,
	})
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
	case contract.InvalidOutputMode, contract.MissingTool, contract.DoctorFailed, contract.NodeVersionMismatch, contract.RemoteUnreachable, contract.Internal:
		status = http.StatusInternalServerError
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
