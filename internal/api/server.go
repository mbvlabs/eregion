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
		ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
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
	scope := "operations:read"
	if r.URL.Path == "/readyz" {
		scope = "system:read"
	} else if r.Method == http.MethodPost {
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
	if r.Method == http.MethodGet && r.URL.Path == "/readyz" {
		if err := s.ready(r.Context()); err != nil {
			return nil, 0, contract.NewFault(contract.Unavailable, "control plane is not ready", "Check database connectivity, migration versions, and binary compatibility.")
		}
		return map[string]bool{"ready": true}, http.StatusOK, nil
	}
	if r.Method == http.MethodPost && r.URL.Path == "/v1/operations" {
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
		input, err := operations.NormalizeTestInput(request.Input)
		if err != nil {
			return nil, 0, err
		}
		if request.DryRun {
			return operations.TestPreview{Kind: request.Kind, Target: request.Target, Input: input, DryRun: true}, http.StatusOK, nil
		}
		if len(r.Header.Values("Idempotency-Key")) != 1 || r.Header.Get("Idempotency-Key") == "" {
			return nil, 0, contract.NewFault(contract.InputRequired, "Idempotency-Key header is required", "Reuse one stable key for retries of this submission.")
		}
		canonical, _ := json.Marshal(input)
		op, err := s.service.Submit(r.Context(), operations.Submission{Kind: request.Kind, Target: request.Target, Actor: actor, IdempotencyKey: r.Header.Get("Idempotency-Key"), Input: canonical})
		return op, http.StatusAccepted, err
	}
	if r.Method == http.MethodGet && r.URL.Path == "/v1/platform" {
		return s.platformInventory(r)
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/"), "/")
	if len(parts) >= 3 && parts[0] == "v1" && parts[1] == "operations" {
		id := contract.OperationID(parts[2])
		if err := id.Validate(); err != nil {
			return nil, 0, err
		}
		if r.Method == http.MethodGet && len(parts) == 3 {
			op, err := s.service.Get(r.Context(), actor, id)
			if err != nil {
				return op, http.StatusOK, err
			}
			if err := s.authorizeExistingOperation(r, op); err != nil {
				return nil, 0, err
			}
			return op, http.StatusOK, nil
		}
		if r.Method == http.MethodGet && len(parts) == 4 && parts[3] == "events" {
			existing, err := s.service.Get(r.Context(), actor, id)
			if err != nil {
				return nil, 0, err
			}
			if err := s.authorizeExistingOperation(r, existing); err != nil {
				return nil, 0, err
			}
			after := uint64(0)
			limit := 100
			query, queryErr := url.ParseQuery(r.URL.RawQuery)
			if queryErr != nil {
				return nil, 0, contract.NewFault(contract.ValidationFailed, "invalid event query encoding", "Use decimal after and limit parameters.")
			}
			for name, values := range query {
				if (name != "after" && name != "limit") || len(values) != 1 {
					return nil, 0, contract.NewFault(contract.ValidationFailed, "invalid event query", "Use one after cursor and one limit parameter.")
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
					return nil, 0, contract.NewFault(contract.ValidationFailed, "invalid event cursor or limit", "Use an unsigned decimal after cursor and limit 1–200.")
				}
			}
			if limit < 1 || limit > 200 {
				return nil, 0, contract.NewFault(contract.ValidationFailed, "event limit out of range", "Use an event limit from 1 to 200.")
			}
			page, err := s.service.Events(r.Context(), actor, contract.Cursor{OperationID: id, After: after}, limit)
			return page, http.StatusOK, err
		}
		if r.Method == http.MethodPost && len(parts) == 4 && parts[3] == "cancel" {
			existing, err := s.service.Get(r.Context(), actor, id)
			if err != nil {
				return nil, 0, err
			}
			if err := s.authorizeExistingOperation(r, existing); err != nil {
				return nil, 0, err
			}
			op, err := s.service.RequestCancel(r.Context(), actor, id)
			return op, http.StatusAccepted, err
		}
	}
	return nil, 0, contract.NewFault(contract.NotFound, "API endpoint not found", "Use a documented method and /v1 endpoint.")
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
	default:
		return http.StatusInternalServerError
	}
}
