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
	if envelope.Error.Code != contract.Forbidden || !strings.Contains(envelope.Error.Hint, "human terminal") || !strings.Contains(envelope.Error.Hint, "unprotected non-last") {
		t.Fatalf("delete refusal = %+v, want a forbidden fault naming the human-only and agent-safe cases", envelope.Error)
	}
	if service.called != 0 {
		t.Fatal("the HTTP delete reached the operation service")
	}
}

type factorService struct {
	*fakeService
	required bool
}

func (s *factorService) RequiresHumanFactor(context.Context, platform.Normalized) (bool, error) {
	return s.required, nil
}

func TestHTTPSUnprotectedEnvironmentDeleteNeedsConfirmationOnly(t *testing.T) {
	p := principal("operations:submit")
	p.ActionGrants = []ActionGrant{{Kind: platform.KindEnvironmentDelete, Target: "env/api/pr-42"}}
	auth, err := NewAuth([]Principal{p})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"kind":"platform.environment.delete","target":"env/api/pr-42","input":{"app":"api","environment":"pr-42","confirm":"api/pr-42"}}`
	for _, tc := range []struct {
		name     string
		required bool
		status   int
	}{
		{name: "unprotected", required: false, status: http.StatusAccepted},
		{name: "protected or last", required: true, status: http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			service := &factorService{fakeService: &fakeService{}, required: tc.required}
			h := NewHandler(auth, service, func(context.Context) error { return nil })
			recorder, envelope := request(t, h, http.MethodPost, "/v1/operations", body, token)
			if recorder.Code != tc.status {
				t.Fatalf("status = %d, want %d; error=%+v", recorder.Code, tc.status, envelope.Error)
			}
			if tc.required && (envelope.OK || envelope.Error == nil || envelope.Error.Code != contract.Forbidden) {
				t.Fatalf("protected refusal = %+v", envelope.Error)
			}
			if !tc.required && (!envelope.OK || service.called != 1) {
				t.Fatalf("unprotected agent delete did not reach the service: %+v called=%d", envelope.Error, service.called)
			}
		})
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
