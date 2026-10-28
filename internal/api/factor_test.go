package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

func TestHTTPSOperationsRefusesARealDeleteWhateverTheGrant(t *testing.T) {
	deletePrincipal := principal("operations:submit")
	deletePrincipal.ActionGrants = []ActionGrant{{Kind: platform.KindAppDelete, Target: "app/api"}}
	auth, err := NewAuth([]Principal{deletePrincipal})
	if err != nil {
		t.Fatal(err)
	}
	service := &fakeService{}
	h := NewHandler(auth, service, func(context.Context) error { return nil })
	body := `{"kind":"platform.app.delete","target":"app/api","input":{"app":"api","confirm":"api"}}`
	recorder, envelope := request(t, h, http.MethodPost, "/v1/operations", body, token)
	if recorder.Code != http.StatusForbidden || envelope.OK || envelope.Error == nil {
		t.Fatalf("delete submission = %d %+v, want forbidden", recorder.Code, envelope.Error)
	}
	if envelope.Error.Code != contract.Forbidden || !strings.Contains(envelope.Error.Hint, "eregion app delete") {
		t.Fatalf("delete refusal = %+v, want a forbidden fault naming the operator CLI", envelope.Error)
	}
	if service.called != 0 {
		t.Fatal("the HTTP delete reached the operation service")
	}
}

func TestHTTPSOperationsStillPreviewsADelete(t *testing.T) {
	p := principal("operations:submit")
	p.ActionGrants = []ActionGrant{{Kind: platform.KindAppDelete, Target: "app/api"}}
	auth, err := NewAuth([]Principal{p})
	if err != nil {
		t.Fatal(err)
	}
	h := NewHandler(auth, &fakeService{}, func(context.Context) error { return nil })
	body := `{"kind":"platform.app.delete","target":"app/api","input":{"app":"api","confirm":"api"},"dry_run":true}`
	recorder, envelope := request(t, h, http.MethodPost, "/v1/operations", body, token)
	if recorder.Code != http.StatusOK || !envelope.OK {
		t.Fatalf("delete preview = %d %+v, want 200", recorder.Code, envelope.Error)
	}
	raw, err := json.Marshal(envelope.Data)
	if err != nil {
		t.Fatal(err)
	}
	var preview platform.Preview
	if err := json.Unmarshal(raw, &preview); err != nil || !preview.DryRun || preview.Kind != platform.KindAppDelete {
		t.Fatalf("delete preview = %s, %v", raw, err)
	}
}
