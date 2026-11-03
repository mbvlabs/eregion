package doctor

import (
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/retention"
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

func TestVersionSchemaReportsTheControlDatabaseFailure(t *testing.T) {
	results := Run([]Check{{ID: "version.schema", Category: CatVersions, AtInit: true, Run: versionSchema}}, Facts{
		SchemaKnown: false,
		SchemaError: "control database unreachable (dns_resolution) at db.example.com:5432: the database hostname did not resolve",
		SchemaFix:   "Check the database hostname and DNS resolution on the node.",
	})
	if len(results) != 1 || results[0].Status != Fail {
		t.Fatalf("version.schema = %+v", results)
	}
	if !strings.Contains(results[0].Detail, "dns_resolution") || !strings.Contains(results[0].Detail, "db.example.com:5432") {
		t.Fatalf("detail does not name the class and endpoint: %s", results[0].Detail)
	}
	if !strings.Contains(results[0].Fix, "DNS") {
		t.Fatalf("fix does not carry the class hint: %s", results[0].Fix)
	}
}

func TestMainNodeReportHasTwentyResults(t *testing.T) {
	// The documented alpha baseline is 20 pass / 0 warn / 0 fail on a healthy
	// main node: the previous 18 results plus host.retention and
	// host.build_cache.
	facts := Facts{
		Now:           time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC),
		Hostname:      "main.example.com",
		ServicesKnown: true,
		Services:      map[string]bool{"eregion-api.service": true, "eregion-worker.service": true, "wg-quick@wg0.service": true, "caddy.service": true, "docker.service": true},
		Retention:     RetentionFacts{Enabled: true, Known: true},
		BuildCache:    BuildCacheFacts{Applicable: true, Known: true, CapBytes: 10 << 30},
	}
	if results := Run(Checks(), facts); len(results) != 20 {
		t.Fatalf("main node doctor results = %d, want 20", len(results))
	}
}

func TestRetentionLastRunPassesOnAFreshHost(t *testing.T) {
	results := Run([]Check{{ID: "host.retention", Category: CatHost, Run: retentionLastRun}}, Facts{
		Now: time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC),
		Retention: RetentionFacts{
			Enabled: true, Known: true,
			WorkerAge: 12 * time.Hour, WorkerAgeKnown: true,
		},
	})
	if len(results) != 1 || results[0].Status != Pass {
		t.Fatalf("fresh retention row = %+v, want pass", results)
	}
	if !strings.Contains(results[0].Detail, "no retention run yet") {
		t.Fatalf("fresh detail = %q", results[0].Detail)
	}
}

func TestRetentionLastRunWarnsWhenTheWorkerNeverProducedARun(t *testing.T) {
	results := Run([]Check{{ID: "host.retention", Category: CatHost, Run: retentionLastRun}}, Facts{
		Now: time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC),
		Retention: RetentionFacts{
			Enabled: true, Known: true,
			WorkerAge: 3 * 24 * time.Hour, WorkerAgeKnown: true,
		},
	})
	if len(results) != 1 || results[0].Status != Warn {
		t.Fatalf("missing-run retention row = %+v, want warn", results)
	}
}

func TestRetentionLastRunReportsTheLastRun(t *testing.T) {
	finished := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	results := Run([]Check{{ID: "host.retention", Category: CatHost, Run: retentionLastRun}}, Facts{
		Now: time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC),
		Retention: RetentionFacts{
			Enabled: true, Known: true, HasRun: true,
			LastSuccessAt: finished,
			LastRun: RetentionRunFacts{
				Status: retention.RunSucceeded, StartedAt: finished.Add(-time.Minute), FinishedAt: &finished,
				RemovedItems: 3, ReclaimedBytes: 1 << 30, PlannedBytes: 2 << 30,
			},
		},
	})
	if len(results) != 1 || results[0].Status != Pass {
		t.Fatalf("successful retention row = %+v, want pass", results)
	}
	for _, want := range []string{"3 item(s) removed", "1.0 GiB reclaimed", "2.0 GiB"} {
		if !strings.Contains(results[0].Detail, want) {
			t.Fatalf("detail lacks %q: %s", want, results[0].Detail)
		}
	}
}

func TestRetentionLastRunWarnsOnFailureAndStaleSuccess(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	failed := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	results := Run([]Check{{ID: "host.retention", Category: CatHost, Run: retentionLastRun}}, Facts{
		Now: now,
		Retention: RetentionFacts{
			Enabled: true, Known: true, HasRun: true,
			LastSuccessAt: now.Add(-4 * 24 * time.Hour),
			LastRun: RetentionRunFacts{
				Status: retention.RunFailed, StartedAt: failed.Add(-time.Minute), FinishedAt: &failed,
				Errors: 1,
			},
		},
	})
	if len(results) != 1 || results[0].Status != Warn {
		t.Fatalf("failed retention row = %+v, want warn", results)
	}

	success := now.Add(-72 * time.Hour)
	results = Run([]Check{{ID: "host.retention", Category: CatHost, Run: retentionLastRun}}, Facts{
		Now: now,
		Retention: RetentionFacts{
			Enabled: true, Known: true, HasRun: true,
			LastSuccessAt: success,
			LastRun: RetentionRunFacts{
				Status: retention.RunSucceeded, StartedAt: success.Add(-time.Minute), FinishedAt: &success,
			},
		},
	})
	if len(results) != 1 || results[0].Status != Warn {
		t.Fatalf("stale retention row = %+v, want warn", results)
	}
}

func TestBuildpackCacheWarnsAboveThePolicyCap(t *testing.T) {
	results := Run([]Check{{ID: "host.build_cache", Category: CatHost, Run: buildpackCache}}, Facts{
		BuildCache: BuildCacheFacts{Applicable: true, Known: true, Bytes: 11 << 30, CapBytes: 10 << 30},
	})
	if len(results) != 1 || results[0].Status != Warn {
		t.Fatalf("oversized cache = %+v, want warn", results)
	}
	results = Run([]Check{{ID: "host.build_cache", Category: CatHost, Run: buildpackCache}}, Facts{
		BuildCache: BuildCacheFacts{Applicable: true, Known: true, Bytes: 2 << 30, CapBytes: 10 << 30},
	})
	if len(results) != 1 || results[0].Status != Pass {
		t.Fatalf("cache under cap = %+v, want pass", results)
	}
	results = Run([]Check{{ID: "host.build_cache", Category: CatHost, Run: buildpackCache}}, Facts{})
	if len(results) != 1 || results[0].Status != Pass {
		t.Fatalf("role without builds = %+v, want pass", results)
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
