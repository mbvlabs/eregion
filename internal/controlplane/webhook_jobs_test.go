package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

func validWebhookAdmission() api.WebhookAdmission {
	return api.WebhookAdmission{
		DeliveryIDHash: strings.Repeat("a", 64), BodySHA256: strings.Repeat("b", 64),
		Actor: contract.Actor{Kind: "webhook", Subject: "github:mbvlabs/hello"},
		Kind:  platform.KindDeploy, Target: "env/shop/production",
		EnvironmentID: "env_" + strings.Repeat("1", 32), RevisionID: "erv_" + strings.Repeat("2", 32),
		SecretRevision: 7, SecretRevisionHash: strings.Repeat("c", 64),
		Repository: "mbvlabs/hello", Branch: "main", Commit: strings.Repeat("d", 40),
		State: api.WebhookDeliveryAdmitted,
	}
}

func TestValidateWebhookAdmissionRecord(t *testing.T) {
	valid := validWebhookAdmission()
	if err := validateWebhookAdmissionRecord(valid); err != nil {
		t.Fatalf("valid record: %v", err)
	}
	cases := map[string]func(*api.WebhookAdmission){
		"delivery hash": func(record *api.WebhookAdmission) { record.DeliveryIDHash = "short" },
		"body hash":     func(record *api.WebhookAdmission) { record.BodySHA256 = "short" },
		"actor":         func(record *api.WebhookAdmission) { record.Actor.Kind = "ci" },
		"kind":          func(record *api.WebhookAdmission) { record.Kind = platform.KindRollback },
		"target":        func(record *api.WebhookAdmission) { record.Target = "not-a-target" },
		"environment":   func(record *api.WebhookAdmission) { record.EnvironmentID = "" },
		"revision":      func(record *api.WebhookAdmission) { record.RevisionID = "" },
		"secret":        func(record *api.WebhookAdmission) { record.SecretRevision = 0 },
		"secret hash":   func(record *api.WebhookAdmission) { record.SecretRevisionHash = "" },
		"repository":    func(record *api.WebhookAdmission) { record.Repository = "" },
		"branch":        func(record *api.WebhookAdmission) { record.Branch = "" },
		"commit":        func(record *api.WebhookAdmission) { record.Commit = "main" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			record := validWebhookAdmission()
			mutate(&record)
			if err := validateWebhookAdmissionRecord(record); err == nil {
				t.Fatal("invalid record was accepted")
			}
		})
	}
}

func TestCompareWebhookAdmissionBindsIdentity(t *testing.T) {
	existing := validWebhookAdmission()
	if err := compareWebhookAdmission(existing, validWebhookAdmission()); err != nil {
		t.Fatalf("identical record: %v", err)
	}
	cases := map[string]func(*api.WebhookAdmission){
		"body":   func(record *api.WebhookAdmission) { record.BodySHA256 = strings.Repeat("e", 64) },
		"actor":  func(record *api.WebhookAdmission) { record.Actor.Subject = "github:other/repo" },
		"kind":   func(record *api.WebhookAdmission) { record.Kind = platform.KindRestart },
		"target": func(record *api.WebhookAdmission) { record.Target = "env/shop/staging" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			incoming := validWebhookAdmission()
			mutate(&incoming)
			err := compareWebhookAdmission(existing, incoming)
			if contract.Classify(err).Code != contract.Conflict {
				t.Fatalf("error = %v, want conflict", err)
			}
		})
	}
}

func TestWebhookAdmissionFailureClassification(t *testing.T) {
	code, detail, retry := webhookAdmissionFailure(unavailable(errors.New("database down")))
	if !retry || code != contract.Unavailable || detail != "" {
		t.Fatalf("database error = %s %q retry=%v", code, detail, retry)
	}
	code, detail, retry = webhookAdmissionFailure(contract.NewFault(contract.ValidationFailed, "environment is not deployable: domain", "Fix the revision."))
	if retry || code != contract.ValidationFailed || !strings.Contains(detail, "not deployable") {
		t.Fatalf("validation error = %s %q retry=%v", code, detail, retry)
	}
	code, _, retry = webhookAdmissionFailure(context.DeadlineExceeded)
	if !retry || code != contract.Unavailable {
		t.Fatalf("deadline = %s retry=%v", code, retry)
	}
	code, detail, retry = webhookAdmissionFailure(contract.NewFault(contract.Internal, "an internal error occurred", "Report it."))
	if retry || code != contract.Internal || detail != "an internal error occurred" {
		t.Fatalf("internal error = %s %q retry=%v", code, detail, retry)
	}
}

func TestSanitizeWebhookFailure(t *testing.T) {
	got := sanitizeWebhookFailure("line one\nline two\twith\rcontrol\x00bytes")
	if strings.ContainsAny(got, "\n\r\t\x00") || got != "line one line two with control bytes" {
		t.Fatalf("sanitized = %q", got)
	}
	long := sanitizeWebhookFailure(strings.Repeat("x", 700))
	if len(long) != 512 {
		t.Fatalf("long length = %d", len(long))
	}
}

func TestWebhookAdmissionDispositionFor(t *testing.T) {
	cases := map[string]webhookAdmissionDisposition{
		"":          webhookAdmissionRequeue,
		"completed": webhookAdmissionRequeue,
		"discarded": webhookAdmissionFail,
		"cancelled": webhookAdmissionFail,
		"running":   webhookAdmissionLeave,
		"available": webhookAdmissionLeave,
		"scheduled": webhookAdmissionLeave,
		"retryable": webhookAdmissionLeave,
	}
	for state, want := range cases {
		if got := webhookAdmissionDispositionFor(state); got != want {
			t.Errorf("state %q = %v, want %v", state, got, want)
		}
	}
}

func TestWebhookArgsCarryOnlyScopedDeliveryIdentity(t *testing.T) {
	hash := strings.Repeat("f", 64)
	environmentID := "env_" + strings.Repeat("1", 32)
	raw, err := json.Marshal(webhookArgs{EnvironmentID: environmentID, DeliveryIDHash: hash})
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"environment_id":"`+environmentID+`","delivery_id_hash":"`+hash+`"}` {
		t.Fatalf("args = %s", raw)
	}
}

func TestWebhookSourceBindingCurrent(t *testing.T) {
	record := validWebhookAdmission()
	env := platform.Environment{
		Revision: &platform.EnvironmentRevision{
			Source: platform.SourceBinding{Kind: "git", URL: "https://github.com/mbvlabs/hello.git", Ref: "refs/heads/main"},
		},
	}
	if err := webhookSourceBindingCurrent(env, record); err != nil {
		t.Fatalf("matching source: %v", err)
	}
	rebound := env
	revision := *env.Revision
	revision.Source.URL = "https://github.com/mbvlabs/other.git"
	rebound.Revision = &revision
	if err := webhookSourceBindingCurrent(rebound, record); err == nil {
		t.Fatal("repository rebind was accepted")
	}
	revision = *env.Revision
	revision.Source.Ref = "refs/heads/release"
	rebound.Revision = &revision
	if err := webhookSourceBindingCurrent(rebound, record); err == nil {
		t.Fatal("branch rebind was accepted")
	}
	image := env
	imageRevision := *env.Revision
	imageRevision.Source = platform.SourceBinding{Kind: "image", Image: "example/app:1"}
	image.Revision = &imageRevision
	if err := webhookSourceBindingCurrent(image, record); err == nil {
		t.Fatal("image source was accepted")
	}
	if err := webhookSourceBindingCurrent(platform.Environment{}, record); err == nil {
		t.Fatal("missing revision was accepted")
	}
}
