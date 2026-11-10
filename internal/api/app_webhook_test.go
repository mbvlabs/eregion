package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
)

type atomicReceiptService struct {
	fakeService
	submission operations.Submission
	result     operations.SubmissionResult
}

func (s *atomicReceiptService) SubmitWithReceipt(_ context.Context, submission operations.Submission) (operations.SubmissionResult, error) {
	s.submission = submission
	return s.result, nil
}

func TestAppServiceKeepsTransientWebhookSecretOutOfOrdinaryResult(t *testing.T) {
	operation := contract.Operation{
		ID: id, Kind: platform.KindEnvironmentUpdate, Target: "env/api/production",
		Actor: contract.Actor{Kind: "operator", Subject: "integration"},
		State: contract.Queued, Attempt: 1, LastSequence: 1,
	}
	service := &atomicReceiptService{result: operations.SubmissionResult{Operation: operation, WebhookSecretActivated: true}}
	app := AppService{Operations: service}
	actor := contract.Actor{Kind: "operator", Subject: "integration"}
	secret := []byte("transient-webhook-secret")
	result, err := app.PreviewOrSubmitWithReceipt(context.Background(), actor,
		platform.KindEnvironmentUpdate, "env/api/production", json.RawMessage(`{"rotate_webhook_secret":true}`),
		"webhook-transient", false, "", secret)
	if err != nil {
		t.Fatal(err)
	}
	receipt, ok := result.(operations.SubmissionResult)
	if !ok {
		t.Fatalf("result type = %T", result)
	}
	if receipt.Operation.ID != operation.ID || !receipt.WebhookSecretActivated {
		t.Fatalf("receipt = %+v", receipt)
	}
	if string(service.submission.WebhookSecret) != string(secret) {
		t.Fatal("the transient secret did not reach the atomic submission")
	}
	encoded, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), string(secret)) {
		t.Fatal("the ordinary submission result leaked the transient secret")
	}
}

func TestAppServiceHTTPSubmissionReceivesOperationNotTransientSecret(t *testing.T) {
	operation := contract.Operation{
		ID: id, Kind: platform.KindEnvironmentUpdate, Target: "env/api/production",
		Actor: contract.Actor{Kind: "operator", Subject: "integration"},
		State: contract.Queued, Attempt: 1, LastSequence: 1,
	}
	service := &atomicReceiptService{result: operations.SubmissionResult{Operation: operation}}
	app := AppService{Operations: service}
	normalized, err := NormalizeAction(platform.KindEnvironmentUpdate, "env/api/production",
		json.RawMessage(`{"rotate_webhook_secret":true}`))
	if err != nil {
		t.Fatal(err)
	}
	result, err := app.SubmitNormalizedWithFactor(context.Background(),
		contract.Actor{Kind: "operator", Subject: "integration"}, normalized, "webhook-http", false, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := result.(operations.SubmissionResult); ok {
		t.Fatal("the HTTP operation boundary returned a receipt struct instead of an operation")
	}
	if len(service.submission.WebhookSecret) != 0 {
		t.Fatal("the HTTP operation boundary carried a transient secret")
	}
}

func TestAppServiceReturnsAuthoritativeReplayForOrdinarySubmission(t *testing.T) {
	operation := contract.Operation{ID: id, Kind: platform.KindEnvironmentUpdate, Target: "env/api/production"}
	service := &atomicReceiptService{result: operations.SubmissionResult{Operation: operation, Replay: true}}
	app := AppService{Operations: service}
	actor := contract.Actor{Kind: "operator", Subject: "integration"}
	result, err := app.PreviewOrSubmitWithReceipt(context.Background(), actor,
		platform.KindEnvironmentUpdate, "env/api/production", json.RawMessage(`{"auto_deploy":false}`),
		"stable-key", false, "factor", nil)
	if err != nil {
		t.Fatal(err)
	}
	receipt, ok := result.(operations.SubmissionResult)
	if !ok || !receipt.Replay || receipt.Operation.ID != operation.ID {
		t.Fatalf("result = %#v", result)
	}
	if service.submission.Actor != actor || service.submission.IdempotencyKey != "stable-key" || service.submission.HumanFactor != "factor" {
		t.Fatalf("submission metadata was not forwarded: %+v", service.submission)
	}
	if service.fakeService.called != 0 {
		t.Fatal("ordinary submission used the operation-only path")
	}
}
