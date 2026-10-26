package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
)

const maxCIDeployBytes = 16 * 1024

type ciDeployRequest struct {
	Ref string `json:"ref"`
}

type ciDeployResponse struct {
	Accepted    bool                 `json:"accepted,omitempty"`
	Operation   *contract.Operation  `json:"operation,omitempty"`
	StatusURL   string               `json:"status_url,omitempty"`
	OperationID contract.OperationID `json:"operation_id,omitempty"`
	Kind        string               `json:"kind,omitempty"`
	Target      string               `json:"target,omitempty"`
	Message     string               `json:"message,omitempty"`
	Error       string               `json:"error,omitempty"`
	Hint        string               `json:"hint,omitempty"`
	Code        string               `json:"code,omitempty"`
}

type CIClient struct {
	auth    *Auth
	service operations.Service
}

// NewCIDeployHandler mounts POST /v1/environments/<app>/<env>/deployments and
// GET /v1/environments/<app>/<env>/deployments/<operation>. It authenticates a
// deploy-scoped token itself and never exposes another environment, another
// action, or a token listing.
func NewCIDeployHandler(auth *Auth, service operations.Service) http.Handler {
	handler := &CIClient{auth: auth, service: service}
	return http.HandlerFunc(handler.serve)
}

func (h *CIClient) serve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	defer func() {
		if recover() != nil {
			writeCIDeploy(w, http.StatusInternalServerError, ciDeployResponse{Error: "deploy request failed internally", Code: string(contract.Internal)})
		}
	}()
	app, environment, operationID, statusPath, ok := ciDeployPath(r.URL.Path)
	if !ok {
		writeCIDeploy(w, http.StatusNotFound, ciDeployResponse{Error: "deploy endpoint not found", Code: string(contract.NotFound)})
		return
	}
	target, err := platform.NewEnvironmentTarget(app, environment)
	if err != nil {
		writeCIDeploy(w, http.StatusNotFound, ciDeployResponse{Error: "deploy endpoint not found", Code: string(contract.NotFound)})
		return
	}
	switch {
	case r.Method == http.MethodPost && operationID == "":
		h.submit(w, r, target)
	case r.Method == http.MethodGet && operationID != "":
		h.status(w, r, target, operationID, statusPath)
	default:
		w.Header().Set("Allow", http.MethodPost)
		writeCIDeploy(w, http.StatusMethodNotAllowed, ciDeployResponse{Error: "use POST to deploy or GET for the returned operation", Code: string(contract.Usage)})
	}
}

func (h *CIClient) submit(w http.ResponseWriter, r *http.Request, target platform.Target) {
	if h.auth == nil || h.service == nil {
		writeCIDeploy(w, http.StatusServiceUnavailable, ciDeployResponse{Error: "the CI deploy endpoint is not configured", Code: string(contract.Unavailable)})
		return
	}
	if len(r.Header.Values("Authorization")) != 1 {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeCIDeploy(w, http.StatusUnauthorized, ciDeployResponse{Error: "exactly one Bearer token is required", Code: string(contract.Unauthenticated)})
		return
	}
	principal, err := h.auth.authenticateAction(r.Header.Get("Authorization"), "operations:submit", platform.KindDeploy, target.String())
	if err != nil {
		writeCIDeployFault(w, err)
		return
	}
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		writeCIDeploy(w, http.StatusBadRequest, ciDeployResponse{Error: "JSON content type required", Code: string(contract.ValidationFailed)})
		return
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxCIDeployBytes+1))
	if err != nil || len(raw) > maxCIDeployBytes {
		writeCIDeploy(w, http.StatusBadRequest, ciDeployResponse{Error: "request body is unreadable or too large", Code: string(contract.ValidationFailed)})
		return
	}
	var request ciDeployRequest
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || decoder.Decode(new(any)) != io.EOF {
		writeCIDeploy(w, http.StatusBadRequest, ciDeployResponse{Error: "invalid deploy body", Hint: "Send exactly {\"ref\":\"<commit or branch>\"}.", Code: string(contract.ValidationFailed)})
		return
	}
	if strings.TrimSpace(request.Ref) == "" {
		writeCIDeploy(w, http.StatusBadRequest, ciDeployResponse{Error: "ref is required", Hint: "Send the git commit or branch to deploy.", Code: string(contract.ValidationFailed)})
		return
	}
	if len(r.Header.Values("Idempotency-Key")) > 1 {
		writeCIDeploy(w, http.StatusBadRequest, ciDeployResponse{Error: "exactly one Idempotency-Key header is allowed", Code: string(contract.InputRequired)})
		return
	}
	key := r.Header.Get("Idempotency-Key")
	if key == "" {
		key = idempotencyKey("ci")
	}
	input, err := json.Marshal(struct {
		Ref string `json:"ref"`
	}{Ref: strings.TrimSpace(request.Ref)})
	if err != nil {
		writeCIDeploy(w, http.StatusInternalServerError, ciDeployResponse{Error: "could not encode the deploy input", Code: string(contract.Internal)})
		return
	}
	normalized, err := NormalizeAction(platform.KindDeploy, target.String(), input)
	if err != nil {
		writeCIDeployFault(w, err)
		return
	}
	operation, err := NewAppService(h.service, nil, nil).SubmitNormalized(r.Context(), principal.Actor, normalized, key, false)
	if err != nil {
		writeCIDeployFault(w, err)
		return
	}
	accepted, ok := operation.(contract.Operation)
	if !ok {
		writeCIDeploy(w, http.StatusInternalServerError, ciDeployResponse{Error: "the deploy was not accepted as an operation", Code: string(contract.Internal)})
		return
	}
	writeCIDeploy(w, http.StatusAccepted, ciDeployResponse{
		Accepted: true, OperationID: accepted.ID, Kind: accepted.Kind, Target: accepted.Target,
		StatusURL: statusURL(target.App, target.Environment, accepted.ID),
		Message:   "deploy accepted for durable processing",
	})
}

func (h *CIClient) status(w http.ResponseWriter, r *http.Request, target platform.Target, id contract.OperationID, statusPath string) {
	if h.auth == nil || h.service == nil {
		writeCIDeploy(w, http.StatusServiceUnavailable, ciDeployResponse{Error: "the CI deploy endpoint is not configured", Code: string(contract.Unavailable)})
		return
	}
	if len(r.Header.Values("Authorization")) != 1 {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeCIDeploy(w, http.StatusUnauthorized, ciDeployResponse{Error: "exactly one Bearer token is required", Code: string(contract.Unauthenticated)})
		return
	}
	principal, err := h.auth.authenticateAction(r.Header.Get("Authorization"), "operations:read", platform.KindDeploy, target.String())
	if err != nil {
		writeCIDeployFault(w, err)
		return
	}
	operation, err := h.service.Get(r.Context(), principal.Actor, id)
	if err != nil {
		writeCIDeployFault(w, err)
		return
	}
	if operation.Kind != platform.KindDeploy || operation.Target != target.String() {
		writeCIDeploy(w, http.StatusNotFound, ciDeployResponse{Error: "operation not found", Code: string(contract.NotFound)})
		return
	}
	writeCIDeploy(w, http.StatusOK, ciDeployResponse{Operation: &operation, OperationID: operation.ID, StatusURL: statusPath, Kind: operation.Kind, Target: operation.Target})
}

// ciDeployPath parses /v1/environments/<app>/<env>/deployments[/<operation>].
func ciDeployPath(path string) (app, environment string, id contract.OperationID, statusPath string, ok bool) {
	const prefix = "/v1/environments/"
	if !strings.HasPrefix(path, prefix) {
		return "", "", "", "", false
	}
	parts := strings.Split(strings.TrimPrefix(path, prefix), "/")
	if len(parts) < 3 || len(parts) > 4 || parts[2] != "deployments" {
		return "", "", "", "", false
	}
	if platform.ValidateName(parts[0]) != nil || platform.ValidateName(parts[1]) != nil {
		return "", "", "", "", false
	}
	app, environment = parts[0], parts[1]
	statusPath = "/v1/environments/" + app + "/" + environment + "/deployments"
	if len(parts) == 4 {
		id = contract.OperationID(parts[3])
		if err := id.Validate(); err != nil {
			return "", "", "", "", false
		}
		statusPath += "/" + string(id)
	}
	return app, environment, id, statusPath, true
}

func idempotencyKey(prefix string) string {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	return prefix + "-" + hex.EncodeToString(raw[:])
}

func statusURL(app, environment string, id contract.OperationID) string {
	return "/v1/environments/" + app + "/" + environment + "/deployments/" + string(id)
}

func writeCIDeployFault(w http.ResponseWriter, err error) {
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
	writeCIDeploy(w, status, ciDeployResponse{Error: fault.Message, Hint: fault.Hint, Code: string(fault.Code)})
}

func writeCIDeploy(w http.ResponseWriter, status int, response ciDeployResponse) {
	raw, err := json.Marshal(response)
	if err != nil {
		http.Error(w, `{"error":"internal error"}`, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(status)
	_, _ = w.Write(append(raw, '\n'))
}
