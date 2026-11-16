package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/output"
	"github.com/mbvlabs/eregion/internal/secrets"
)

type Server struct {
	auth    *Auth
	service operations.Service
	ready   func(context.Context) error
}

const (
	// requestTimeout bounds every API handler independently of connection and
	// server lifetimes.
	requestTimeout = 10 * time.Second
	// readinessTimeout is the request budget for /readyz. It stays below the
	// host readiness probe's 5s HTTP timeout so a slow database is reported as
	// one bounded generic failure instead of outliving the probe.
	readinessTimeout = 4 * time.Second
)

type testRequest struct {
	Kind   string          `json:"kind"`
	Target string          `json:"target"`
	Input  json.RawMessage `json:"input"`
	DryRun bool            `json:"dry_run"`
}

func NewHandler(auth *Auth, service operations.Service, ready func(context.Context) error) http.Handler {
	s := &Server{auth: auth, service: service, ready: ready}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Bound handlers independently from connection/server lifetimes.
		ctx, cancel := context.WithTimeout(r.Context(), requestTimeout)
		defer cancel()
		r = r.WithContext(ctx)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		registry := &secrets.Registry{}
		registry.Add(r.Header.Get("Authorization"), strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		data, status, err := s.dispatchSafe(r)
		command := r.Method + " API request" // Never reflect untrusted URLs/credentials.
		envelope := contract.Success(command, data, nil)
		if err != nil {
			envelope = contract.Failure(command, err)
			status = httpStatus(envelope.Error.Code)
			if status == http.StatusUnauthorized {
				w.Header().Set("WWW-Authenticate", "Bearer")
			}
		}
		// Serialize before headers, so failures cannot corrupt a partially sent JSON result.
		var body strings.Builder
		writer := output.Writer{Out: &body, Err: io.Discard, Mode: output.JSON, Secrets: registry}
		if err := writer.Write(envelope, ""); err != nil {
			status = http.StatusInternalServerError
			body.Reset()
			_ = writer.Write(contract.Failure(command, err), "")
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body.String())
	})
}

func (s *Server) dispatchSafe(r *http.Request) (data any, status int, err error) {
	defer func() {
		if recover() != nil {
			data, status, err = nil, 0, contract.NewFault(contract.Internal, "API request failed internally", "Inspect server diagnostics without resubmitting accepted work.")
		}
	}()
	return s.dispatch(r)
}

func (s *Server) dispatch(r *http.Request) (any, int, error) {
	if r.Method == http.MethodGet && r.URL.Path == "/healthz" {
		return map[string]bool{"alive": true}, http.StatusOK, nil
	}
	// Readiness is public (Caddy exposes it): it says ready or not, nothing else.
	// Only this route gets the tighter 4s child budget; an earlier parent
	// deadline is inherited rather than extended.
	if r.Method == http.MethodGet && r.URL.Path == "/readyz" {
		return s.readiness(r)
	}
	scope := "operations:read"
	if r.Method == http.MethodPost {
		scope = "operations:submit"
		if strings.HasSuffix(r.URL.Path, "/cancel") {
			scope = "operations:cancel"
		}
	}
	if len(r.Header.Values("Authorization")) != 1 {
		return nil, 0, contract.NewFault(contract.Unauthenticated, "exactly one authorization header is required", "Supply one Bearer token.")
	}
	actor, err := s.auth.authenticate(r.Header.Get("Authorization"), scope)
	if err != nil {
		return nil, 0, err
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/operations":
		return s.dispatchOperationSubmit(r, actor)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/platform":
		return s.platformInventory(r)
	default:
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
		if len(parts) >= 3 && parts[0] == "v1" && parts[1] == "operations" {
			return s.dispatchOperationRoute(r, actor, parts)
		}
		return nil, 0, apiNotFound()
	}
}

func (s *Server) readiness(r *http.Request) (any, int, error) {
	readyCtx, readyCancel := context.WithTimeout(r.Context(), readinessTimeout)
	defer readyCancel()
	if err := s.ready(readyCtx); err != nil {
		return nil, 0, contract.NewFault(contract.Unavailable, "control plane is not ready", "Check database connectivity, migration versions, and binary compatibility.")
	}
	return map[string]bool{"ready": true}, http.StatusOK, nil
}

func (s *Server) dispatchOperationSubmit(r *http.Request, actor contract.Actor) (any, int, error) {
	if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return nil, 0, contract.NewFault(contract.ValidationFailed, "JSON content type required", "Use Content-Type: application/json.")
	}
	raw, err := io.ReadAll(io.LimitReader(r.Body, maxPlatformRequestBytes+1))
	if err != nil || len(raw) > maxPlatformRequestBytes {
		return nil, 0, contract.NewFault(contract.ValidationFailed, "request body is unreadable or too large", "Limit JSON requests to 64 KiB of action input plus envelope overhead.")
	}
	var request testRequest
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || decoder.Decode(new(any)) != io.EOF {
		return nil, 0, contract.NewFault(contract.ValidationFailed, "invalid operation request", "Provide kind, target, input, and optional dry_run; identity comes from authentication.")
	}
	if strings.HasPrefix(request.Kind, "platform.") {
		return s.submitPlatformAction(r, raw)
	}
	if request.Kind != operations.TestKind || request.Target != operations.TestTarget {
		return nil, 0, contract.NewFault(contract.ValidationFailed, "unsupported operation kind or target", "Use diagnostic.test on control-plane.")
	}
	if len(r.Header.Values("Idempotency-Key")) > 1 {
		return nil, 0, contract.NewFault(contract.InputRequired, "exactly one Idempotency-Key header is required", "Reuse one stable key for retries of this submission.")
	}
	key := r.Header.Get("Idempotency-Key")
	data, err := NewAppService(s.service, nil, s.ready).SubmitDiagnostic(r.Context(), actor, request.Input, key, request.DryRun)
	status := http.StatusAccepted
	if request.DryRun {
		status = http.StatusOK
	}
	return data, status, err
}

func (s *Server) dispatchOperationRoute(r *http.Request, actor contract.Actor, parts []string) (any, int, error) {
	id := contract.OperationID(parts[2])
	if err := id.Validate(); err != nil {
		return nil, 0, err
	}
	app := NewAppService(s.service, nil, s.ready)
	switch {
	case r.Method == http.MethodGet && len(parts) == 3:
		return s.getOperation(r, app, actor, id)
	case r.Method == http.MethodGet && len(parts) == 4 && parts[3] == "events":
		return s.getOperationEvents(r, app, actor, id)
	case r.Method == http.MethodPost && len(parts) == 4 && parts[3] == "cancel":
		return s.cancelOperation(r, app, actor, id)
	default:
		return nil, 0, apiNotFound()
	}
}

func (s *Server) getOperation(r *http.Request, app AppService, actor contract.Actor, id contract.OperationID) (any, int, error) {
	op, err := app.GetOperation(r.Context(), actor, id)
	if err != nil {
		return op, http.StatusOK, err
	}
	if err := s.authorizeExistingOperation(r, op); err != nil {
		return nil, 0, err
	}
	return op, http.StatusOK, nil
}

func (s *Server) getOperationEvents(r *http.Request, app AppService, actor contract.Actor, id contract.OperationID) (any, int, error) {
	existing, err := app.GetOperation(r.Context(), actor, id)
	if err != nil {
		return nil, 0, err
	}
	if err := s.authorizeExistingOperation(r, existing); err != nil {
		return nil, 0, err
	}
	after, limit, err := parseEventQuery(r)
	if err != nil {
		return nil, 0, err
	}
	page, err := app.OperationEvents(r.Context(), actor, contract.Cursor{OperationID: id, After: after}, limit)
	return page, http.StatusOK, err
}

func (s *Server) cancelOperation(r *http.Request, app AppService, actor contract.Actor, id contract.OperationID) (any, int, error) {
	existing, err := app.GetOperation(r.Context(), actor, id)
	if err != nil {
		return nil, 0, err
	}
	if err := s.authorizeExistingOperation(r, existing); err != nil {
		return nil, 0, err
	}
	op, err := s.service.RequestCancel(r.Context(), actor, id)
	return op, http.StatusAccepted, err
}

func parseEventQuery(r *http.Request) (uint64, int, error) {
	after := uint64(0)
	limit := 100
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		return 0, 0, contract.NewFault(contract.ValidationFailed, "invalid event query encoding", "Use decimal after and limit parameters.")
	}
	for name, values := range query {
		if (name != "after" && name != "limit") || len(values) != 1 {
			return 0, 0, contract.NewFault(contract.ValidationFailed, "invalid event query", "Use one after cursor and one limit parameter.")
		}
		if name == "after" {
			after, err = strconv.ParseUint(values[0], 10, 64)
			if err == nil && strconv.FormatUint(after, 10) != values[0] {
				err = strconv.ErrSyntax
			}
		} else {
			limit, err = strconv.Atoi(values[0])
		}
		if err != nil {
			return 0, 0, contract.NewFault(contract.ValidationFailed, "invalid event cursor or limit", "Use an unsigned decimal after cursor and limit 1–200.")
		}
	}
	if limit < 1 || limit > 200 {
		return 0, 0, contract.NewFault(contract.ValidationFailed, "event limit out of range", "Use an event limit from 1 to 200.")
	}
	return after, limit, nil
}

func apiNotFound() *contract.FaultError {
	return contract.NewFault(contract.NotFound, "API endpoint not found", "Use a documented method and /v1 endpoint.")
}

func httpStatus(code contract.Code) int {
	switch code {
	case contract.InputRequired, contract.Usage, contract.ValidationFailed, contract.InvalidOutputMode:
		return http.StatusBadRequest
	case contract.NotFound:
		return http.StatusNotFound
	case contract.Unauthenticated:
		return http.StatusUnauthorized
	case contract.Forbidden:
		return http.StatusForbidden
	case contract.Conflict:
		return http.StatusConflict
	case contract.PreconditionFailed, contract.ConfirmationRequired:
		return http.StatusPreconditionFailed
	case contract.Unavailable, contract.Cancelled:
		return http.StatusServiceUnavailable
	case contract.MissingTool, contract.DoctorFailed, contract.NodeVersionMismatch, contract.RemoteUnreachable, contract.Internal:
		return http.StatusInternalServerError
	default:
		return http.StatusInternalServerError
	}
}
