package doctor

import (
	"strings"
	"testing"
)

func TestSummaryFailedAndMessage(t *testing.T) {
	results := []Result{
		{ID: "drift.pending", Status: Fail, Detail: "2 host steps pending"},
		{ID: "host.disk", Status: Warn, Detail: "18% free"},
		{ID: "tls.certificate", Status: Pass, Detail: "expires later"},
	}
	summary := Summary(results)
	if summary[Fail] != 1 || summary[Warn] != 1 || summary[Pass] != 1 || summary[Skip] != 0 {
		t.Fatalf("summary: %+v", summary)
	}
	failed := Failed(results)
	if len(failed) != 1 || failed[0].ID != "drift.pending" {
		t.Fatalf("failed: %+v", failed)
	}
	report := BuildReport(results)
	if report.Summary[Fail] != 1 || len(report.Results) != 3 {
		t.Fatalf("report: %+v", report)
	}
	message := Message(results)
	for _, want := range []string{"FAIL", "drift.pending", "WARN", "1 pass", "1 warn", "1 fail"} {
		if !strings.Contains(message, want) {
			t.Fatalf("message lacks %q: %s", want, message)
		}
	}
}

func TestRunAssignsCheckIdentity(t *testing.T) {
	checks := []Check{{ID: "host.disk", Category: CatHost, Run: func(Facts) []Result {
		return []Result{{Status: Pass, Detail: "ok"}}
	}}}
	results := Run(checks, Facts{})
	if len(results) != 1 || results[0].ID != "host.disk" || results[0].Category != CatHost {
		t.Fatalf("results: %+v", results)
	}
	if !ValidOnly("") || !ValidOnly(CatSecurity) || ValidOnly("made-up") {
		t.Fatalf("only validation: %v", Categories)
	}
}
