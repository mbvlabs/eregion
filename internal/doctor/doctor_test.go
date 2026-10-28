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

func TestRunHonoursInitExemptions(t *testing.T) {
	checks := []Check{
		{ID: "init.ready", Category: CatHost, AtInit: true, Run: func(Facts) []Result {
			return one(Pass, "ready during init", "")
		}},
		{ID: "after.handoff", Category: CatHost, Run: func(Facts) []Result {
			return one(Fail, "needs the stored handoff", "confirm the handoff")
		}},
	}
	results := Run(checks, Facts{Init: true})
	if len(results) != 1 || results[0].ID != "init.ready" {
		t.Fatalf("init results = %+v, want only the AtInit check", results)
	}
	if got := Run(checks, Facts{}); len(got) != 2 {
		t.Fatalf("normal results = %+v, want both checks", got)
	}
}

func TestPendingCertificateStaysAWarningAtInit(t *testing.T) {
	results := Run([]Check{{ID: "tls.certificate", Category: CatTLS, AtInit: true, Run: certificate}}, Facts{
		Init: true, Hostname: "app.example.com", CertKnown: false,
	})
	if len(results) != 1 || results[0].Status != Warn {
		t.Fatalf("pending certificate = %+v, want one warning", results)
	}
	if len(Failed(results)) != 0 {
		t.Fatalf("pending certificate failed the gate: %+v", results)
	}
}

func TestSummaryLineNamesTheInitCounts(t *testing.T) {
	line := SummaryLine(map[Status]int{Pass: 12, Warn: 1, Fail: 0, Skip: 2})
	if line != "12 ok · 1 warn · 2 skip" {
		t.Fatalf("summary line = %q", line)
	}
	if got := SummaryLine(map[Status]int{Pass: 14}); got != "14 ok" {
		t.Fatalf("healthy summary line = %q", got)
	}
}
