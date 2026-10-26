package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
)

type grantService struct {
	fakeService
	op             contract.Operation
	inventoryCalls int
	eventCalls     int
	cancelCalls    int
}

func (s *grantService) Submit(_ context.Context, sub operations.Submission) (contract.Operation, error) {
	s.called++
	op := fakeOperation(contract.Queued)
	op.Kind, op.Target, op.Actor = sub.Kind, sub.Target, sub.Actor
	return op, nil
}

func (s *grantService) Get(context.Context, operations.Authority, contract.OperationID) (contract.Operation, error) {
	return s.op, nil
}

func (s *grantService) Events(_ context.Context, _ operations.Authority, cursor contract.Cursor, _ int) (operations.EventPage, error) {
	s.eventCalls++
	return operations.EventPage{Events: []contract.Event{}, Cursor: cursor}, nil
}

func (s *grantService) RequestCancel(context.Context, operations.Authority, contract.OperationID) (contract.Operation, error) {
	s.cancelCalls++
	return s.op, nil
}

func (s *grantService) Inventory(_ context.Context, _ contract.Actor, query platform.InventoryQuery) (platform.Inventory, error) {
	s.inventoryCalls++
	return platform.Inventory{Collection: query.Collection, Items: []any{}, Limit: query.Limit}, nil
}

func grantHandler(t *testing.T, service *grantService) http.Handler {
	t.Helper()
	p := principal("operations:submit", "operations:read", "operations:cancel")
	p.ActionGrants = []ActionGrant{{Kind: platform.KindDeploy, Target: "env/api/production"}}
	auth, err := NewAuth([]Principal{p})
	if err != nil {
		t.Fatal(err)
	}
	return NewHandler(auth, service, func(context.Context) error { return nil })
}

func TestHTTPGrantDenialPreventsSubmissionAndPreview(t *testing.T) {
	s := &grantService{}
	h := grantHandler(t, s)
	for _, dryRun := range []bool{false, true} {
		for _, target := range []string{"env/other/production", "env/api/staging"} {
			body, _ := json.Marshal(map[string]any{
				"kind": platform.KindDeploy, "target": target,
				"input": map[string]string{"release_id": "rel_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}, "dry_run": dryRun,
			})
			w, envelope := request(t, h, http.MethodPost, "/v1/operations", string(body), token)
			if w.Code != http.StatusForbidden || envelope.Error == nil || envelope.Error.Code != contract.Forbidden || s.called != 0 {
				t.Fatalf("cross-target dry_run=%t accepted: HTTP %d %+v", dryRun, w.Code, envelope)
			}
		}
	}
}

func TestHTTPGrantDenialProtectsOperationReadEventsAndCancel(t *testing.T) {
	s := &grantService{op: fakeOperation(contract.Running)}
	s.op.Kind, s.op.Target = platform.KindDeploy, "env/other/production"
	h := grantHandler(t, s)
	for _, tc := range []struct{ method, suffix string }{
		{http.MethodGet, ""}, {http.MethodGet, "/events"}, {http.MethodPost, "/cancel"},
	} {
		w, envelope := request(t, h, tc.method, "/v1/operations/"+string(id)+tc.suffix, "", token)
		if w.Code != http.StatusForbidden || envelope.Error == nil || envelope.Error.Code != contract.Forbidden {
			t.Fatalf("cross-target %s %s: HTTP %d %+v", tc.method, tc.suffix, w.Code, envelope)
		}
	}
	if s.eventCalls != 0 || s.cancelCalls != 0 {
		t.Fatal("unauthorized event read/cancel crossed application boundary")
	}
}

func TestHTTPGrantInventoryDeniesGlobalAndExtensionReads(t *testing.T) {
	s := &grantService{}
	h := grantHandler(t, s)
	for _, query := range []string{
		"collection=summary", "collection=nodes", "collection=apps",
		"collection=secrets&app=api&environment=production",
		"collection=releases&app=other&environment=production",
		"collection=releases&app=api",
	} {
		w, envelope := request(t, h, http.MethodGet, "/v1/platform?"+query, "", token)
		if w.Code != http.StatusForbidden || envelope.Error == nil || envelope.Error.Code != contract.Forbidden {
			t.Fatalf("inventory scope bypass %s: HTTP %d %+v", query, w.Code, envelope)
		}
	}
	if s.inventoryCalls != 0 {
		t.Fatal("unauthorized inventory provider ran")
	}
	w, envelope := request(t, h, http.MethodGet, "/v1/platform?collection=releases&app=api&environment=production", "", token)
	if w.Code != http.StatusOK || !envelope.OK || s.inventoryCalls != 1 {
		t.Fatalf("authorized bounded inventory failed: HTTP %d %+v", w.Code, envelope)
	}
}
