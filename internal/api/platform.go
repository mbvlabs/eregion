package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
)

type platformInventoryService interface {
	Inventory(context.Context, contract.Actor, platform.InventoryQuery) (platform.Inventory, error)
}

type platformPreviewService interface {
	Preview(context.Context, contract.Actor, platform.Normalized) (platform.Preview, error)
}

const maxPlatformRequestBytes = platform.MaxInputBytes + 8192

// submitPlatformAction is called from the shared operation route after the JSON
// body is bounded. Authorization happens only after strict normalization, so a
// malformed target or unknown kind never reaches a grant decision.
func (s *Server) submitPlatformAction(r *http.Request, raw []byte) (any, int, error) {
	var request testRequest
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil || decoder.Decode(new(any)) != io.EOF {
		return nil, 0, contract.NewFault(contract.ValidationFailed, "invalid operation request", "Provide kind, target, input, and optional dry_run; identity comes from authentication.")
	}
	normalized, err := platform.Default().Normalize(request.Kind, request.Target, request.Input)
	if err != nil {
		return nil, 0, err
	}
	if err := s.auth.authorizeAction(r.Header.Get("Authorization"), normalized.Kind, normalized.Target.String()); err != nil {
		return nil, 0, err
	}
	actor, err := s.auth.authenticate(r.Header.Get("Authorization"), "operations:submit")
	if err != nil {
		return nil, 0, err
	}
	if request.DryRun {
		if previewer, ok := s.service.(platformPreviewService); ok {
			preview, err := previewer.Preview(r.Context(), actor, normalized)
			return preview, http.StatusOK, err
		}
		return platform.Preview{
			Kind: normalized.Kind, Target: normalized.Target.String(), DryRun: true,
			Input: normalized.Input.Preview(), Summary: normalized.Summary,
			Warnings: []string{"A detailed deployment plan requires a control-plane service."},
		}, http.StatusOK, nil
	}
	if len(r.Header.Values("Idempotency-Key")) != 1 || r.Header.Get("Idempotency-Key") == "" {
		return nil, 0, contract.NewFault(contract.InputRequired, "Idempotency-Key header is required", "Reuse one stable key for retries of this submission.")
	}
	op, err := s.service.Submit(r.Context(), operations.Submission{
		Kind: normalized.Kind, Target: normalized.Target.String(), Actor: actor,
		IdempotencyKey: r.Header.Get("Idempotency-Key"), Input: normalized.Canonical,
	})
	return op, http.StatusAccepted, err
}

func (s *Server) platformInventory(r *http.Request) (any, int, error) {
	query, err := parseInventoryQuery(r.URL.RawQuery)
	if err != nil {
		return nil, 0, err
	}
	if err := s.auth.authorizeInventory(r.Header.Get("Authorization"), query); err != nil {
		return nil, 0, err
	}
	actor, err := s.auth.authenticate(r.Header.Get("Authorization"), "operations:read")
	if err != nil {
		return nil, 0, err
	}
	service, ok := s.service.(platformInventoryService)
	if !ok {
		return nil, 0, contract.NewFault(contract.Unavailable, "platform inventory is not configured", "Use a control-plane service with durable platform state.")
	}
	inventory, err := service.Inventory(r.Context(), actor, query)
	return inventory, http.StatusOK, err
}

func parseInventoryQuery(raw string) (platform.InventoryQuery, error) {
	values, err := url.ParseQuery(raw)
	if err != nil {
		return platform.InventoryQuery{}, contract.NewFault(contract.ValidationFailed, "invalid inventory query", "Use collection, app, environment, node, state, and limit parameters.")
	}
	query := platform.InventoryQuery{Limit: 50}
	for name, entries := range values {
		if len(entries) != 1 {
			return query, contract.NewFault(contract.ValidationFailed, "duplicate inventory parameter", "Provide each inventory parameter at most once.")
		}
		value := entries[0]
		switch name {
		case "collection":
			query.Collection = value
		case "app":
			query.App = value
		case "environment":
			query.Environment = value
		case "node":
			query.Node = value
		case "state":
			query.State = value
		case "limit":
			limit, err := strconv.Atoi(value)
			if err != nil || limit < 1 || limit > 200 {
				return query, contract.NewFault(contract.ValidationFailed, "invalid inventory limit", "Use a limit from 1 to 200.")
			}
			query.Limit = limit
		default:
			return query, contract.NewFault(contract.ValidationFailed, "unknown inventory parameter", "Use only collection, app, environment, node, state, and limit.")
		}
	}
	if query.Collection == "" {
		return query, contract.NewFault(contract.InputRequired, "inventory collection is required", "Use collection=summary, nodes, apps, environments, releases, deployments, or changes.")
	}
	return query, nil
}

// authorizeExistingOperation prevents a credential from crossing its action
// grants by reading or cancelling another credential's accepted operation.
func (s *Server) authorizeExistingOperation(r *http.Request, op contract.Operation) error {
	return s.auth.authorizeAction(r.Header.Get("Authorization"), op.Kind, op.Target)
}

// SubmitAction submits or previews any registered action kind.
func (c *Client) SubmitAction(ctx context.Context, kind, target string, input json.RawMessage, idempotencyKey string, dryRun bool) (json.RawMessage, error) {
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	raw, err := c.request(ctx, http.MethodPost, "/v1/operations", testRequest{
		Kind: kind, Target: target, Input: input, DryRun: dryRun,
	}, idempotencyKey)
	if err != nil {
		return nil, err
	}
	if dryRun {
		var preview platform.Preview
		if json.Unmarshal(raw, &preview) != nil || !preview.DryRun || preview.Kind != kind || preview.Target != target {
			return nil, invalidData()
		}
		return raw, nil
	}
	return validOperationResult(raw, "")
}

// PlatformInventory reads one inventory collection.
func (c *Client) PlatformInventory(ctx context.Context, collection string, filters url.Values) (json.RawMessage, error) {
	if strings.TrimSpace(collection) == "" {
		return nil, contract.NewFault(contract.InputRequired, "inventory collection is required", "Use summary, nodes, apps, environments, releases, deployments, or changes.")
	}
	query := url.Values{}
	for key, values := range filters {
		if key == "collection" || len(values) != 1 {
			return nil, contract.NewFault(contract.ValidationFailed, "invalid inventory filter", "Pass collection separately and one value per filter.")
		}
		query.Set(key, values[0])
	}
	query.Set("collection", collection)
	raw, err := c.request(ctx, http.MethodGet, "/v1/platform?"+query.Encode(), nil, "")
	if err != nil {
		return nil, err
	}
	var inventory platform.Inventory
	if json.Unmarshal(raw, &inventory) != nil || inventory.Collection != collection || inventory.Items == nil {
		return nil, invalidData()
	}
	return raw, nil
}
