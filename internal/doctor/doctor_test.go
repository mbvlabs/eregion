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

func telemetryTestFloat(value float64) *float64 { return &value }

func telemetryTestPath(path string) TelemetryPathFacts {
	return TelemetryPathFacts{
		Path: path, Exists: true, Directory: true, OwnerKnown: true,
		UID: 0, GID: 0, Mode: 0o755,
	}
}

func TestValidateTelemetryDirectoryRejectsSpecialModeBits(t *testing.T) {
	for _, mode := range []uint32{0o1755, 0o2755, 0o4755, 0o7755} {
		path := telemetryTestPath("/var/lib/node_exporter/textfile")
		path.Mode = mode
		if err := ValidateTelemetryDirectory(path); err == nil {
			t.Fatalf("mode %04o was accepted; setuid/setgid/sticky must fail", mode)
		}
	}
	path := telemetryTestPath("/var/lib/node_exporter/textfile")
	if err := ValidateTelemetryDirectory(path); err != nil {
		t.Fatalf("0755 rejected: %v", err)
	}
}

func TestTelemetryLocalHealth(t *testing.T) {
	base := Facts{
		WireGuardKnown: true, WireGuardActive: true,
		Telemetry: TelemetryFacts{
			Known: true, Endpoint: "http://10.78.0.1:9100/metrics",
			Parent:              telemetryTestPath("/var/lib/node_exporter"),
			Textfile:            telemetryTestPath("/var/lib/node_exporter/textfile"),
			Collectors:          map[string]bool{"textfile": true, "systemd": true},
			TextfileScrapeError: telemetryTestFloat(0),
		},
	}
	if got := telemetryLocal(Facts{}); got[0].Status != Skip {
		t.Fatalf("unconfigured telemetry = %+v, want skip", got)
	}
	wireGuardDown := base
	wireGuardDown.WireGuardActive = false
	if got := telemetryLocal(wireGuardDown); got[0].Status != Warn || !strings.Contains(got[0].Detail, "not observed") {
		t.Fatalf("WireGuard down = %+v, want warn/unobserved", got)
	}
	wireGuardDownMissingDir := wireGuardDown
	wireGuardDownMissingDir.Telemetry.Textfile.Exists = false
	if got := telemetryLocal(wireGuardDownMissingDir); got[0].Status != Fail || !strings.Contains(got[0].Detail, "missing") {
		t.Fatalf("WireGuard down with missing directory = %+v, want independent fail", got)
	}
	wireGuardUnknown := base
	wireGuardUnknown.WireGuardKnown = false
	wireGuardUnknown.Telemetry.EndpointError = "node_exporter is unreachable on the WireGuard address"
	if got := telemetryLocal(wireGuardUnknown); got[0].Status != Warn || !strings.Contains(got[0].Detail, "WireGuard state is unknown") {
		t.Fatalf("unknown WireGuard = %+v, want warn", got)
	}
	missingDir := base
	missingDir.Telemetry.Textfile.Exists = false
	if got := telemetryLocal(missingDir); got[0].Status != Fail || !strings.Contains(got[0].Detail, "missing") {
		t.Fatalf("missing directory = %+v", got)
	}
	for name, mutate := range map[string]func(*TelemetryFacts){
		"parent wrong mode": func(t *TelemetryFacts) { t.Parent.Mode = 0o775 },
		"parent wrong owner": func(t *TelemetryFacts) {
			t.Parent.UID = 1000
		},
		"parent symlink":      func(t *TelemetryFacts) { t.Parent.Exists, t.Parent.Symlink, t.Parent.Directory = true, true, false },
		"textfile wrong mode": func(t *TelemetryFacts) { t.Textfile.Mode = 0o750 },
		"textfile wrong owner": func(t *TelemetryFacts) {
			t.Textfile.GID = 1000
		},
		"textfile symlink": func(t *TelemetryFacts) {
			t.Textfile.Exists, t.Textfile.Symlink, t.Textfile.Directory = true, true, false
		},
	} {
		t.Run(name, func(t *testing.T) {
			facts := base
			mutate(&facts.Telemetry)
			if got := telemetryLocal(facts); got[0].Status != Fail || !strings.Contains(got[0].Detail, "unsafe or missing") {
				t.Fatalf("strict directory = %+v, want fail", got)
			}
		})
	}
	endpointDown := base
	endpointDown.Telemetry.EndpointError = "node_exporter is unreachable on the WireGuard address"
	if got := telemetryLocal(endpointDown); got[0].Status != Fail || !strings.Contains(got[0].Detail, "does not answer") {
		t.Fatalf("unreachable endpoint = %+v", got)
	}
	missingCollector := base
	missingCollector.Telemetry.Collectors = map[string]bool{"textfile": true}
	if got := telemetryLocal(missingCollector); got[0].Status != Fail || !strings.Contains(got[0].Detail, "systemd") {
		t.Fatalf("missing systemd collector = %+v", got)
	}
	failedCollector := base
	failedCollector.Telemetry.Collectors = map[string]bool{"textfile": true, "systemd": false}
	if got := telemetryLocal(failedCollector); got[0].Status != Fail || !strings.Contains(got[0].Detail, "reported failure") {
		t.Fatalf("failed systemd collector = %+v", got)
	}
	missingTextfileError := base
	missingTextfileError.Telemetry.TextfileScrapeError = nil
	if got := telemetryLocal(missingTextfileError); got[0].Status != Fail || !strings.Contains(got[0].Detail, "node_textfile_scrape_error") {
		t.Fatalf("missing textfile scrape error = %+v", got)
	}
	ambiguousTextfileError := base
	ambiguousTextfileError.Telemetry.TextfileScrapeError = nil
	ambiguousTextfileError.Telemetry.TextfileScrapeErrorAmbiguous = true
	if got := telemetryLocal(ambiguousTextfileError); got[0].Status != Fail || !strings.Contains(got[0].Detail, "ambiguous") {
		t.Fatalf("ambiguous textfile scrape error = %+v", got)
	}
	failedTextfile := base
	failedTextfile.Telemetry.TextfileScrapeError = telemetryTestFloat(1)
	if got := telemetryLocal(failedTextfile); got[0].Status != Fail || !strings.Contains(got[0].Detail, "node_textfile_scrape_error=1") {
		t.Fatalf("failed textfile scrape = %+v", got)
	}
	healthy := base
	healthy.Telemetry.ExporterVersion = "1.11.1"
	healthy.Telemetry.Collectors = map[string]bool{"textfile": true, "systemd": true, "cpu": true}
	if got := telemetryLocal(healthy); got[0].Status != Pass ||
		!strings.Contains(got[0].Detail, "1.11.1") || !strings.Contains(got[0].Detail, "node_textfile_scrape_error=0") {
		t.Fatalf("healthy telemetry = %+v", got)
	}
}

func TestMainNodeReportCoversEveryCheck(t *testing.T) {
	// Service checks expand to one result per configured unit; with
	// node-exporter.service in the main role the report has one more row than
	// the older five-service fixtures. Do not encode a total count here.
	facts := Facts{
		Now:           time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC),
		Hostname:      "main.example.com",
		ServicesKnown: true,
		Services: map[string]bool{
			"eregion-api.service": true, "eregion-worker.service": true,
			"wg-quick@wg0.service": true, "caddy.service": true,
			"docker.service": true, "node-exporter.service": true,
		},
		Retention:  RetentionFacts{Enabled: true, Known: true},
		BuildCache: BuildCacheFacts{Applicable: true, Known: true, CapBytes: 10 << 30},
	}
	results := Run(Checks(), facts)
	seen := map[string]bool{}
	for _, result := range results {
		seen[result.ID] = true
	}
	for _, check := range Checks() {
		if !seen[check.ID] {
			t.Fatalf("check %s produced no doctor result; results = %+v", check.ID, results)
		}
	}
	if len(results) < len(Checks()) {
		t.Fatalf("doctor results = %d for %d checks", len(results), len(Checks()))
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

func TestRetentionLastRunPassesWithSkipNote(t *testing.T) {
	finished := time.Date(2026, 11, 3, 10, 0, 0, 0, time.UTC)
	results := Run([]Check{{ID: "host.retention", Category: CatHost, Run: retentionLastRun}}, Facts{
		Now: time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC),
		Retention: RetentionFacts{
			Enabled: true, Known: true, HasRun: true,
			LastSuccessAt: finished,
			LastRun: RetentionRunFacts{
				Status: retention.RunSucceededWithSkips, StartedAt: finished.Add(-time.Minute), FinishedAt: &finished,
				RemovedItems: 3, ReclaimedBytes: 1 << 30, Errors: 1, Skipped: 1,
			},
		},
	})
	if len(results) != 1 || results[0].Status != Pass {
		t.Fatalf("succeeded-with-skips retention row = %+v, want pass", results)
	}
	if !strings.Contains(results[0].Detail, "1 item(s) skipped") {
		t.Fatalf("skip note missing from detail: %s", results[0].Detail)
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

func TestRetentionLastRunPointsAtTheRegistryStorageFix(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	failed := now.Add(-time.Hour)
	for _, message := range []string{
		"retention did not complete: the node's registry storage is unreadable",
		"retention did not complete: the node's registry storage at /var/lib/eregion/registry is not readable; run eregion apply to apply host-directories-v2",
	} {
		facts := Facts{
			Now: now,
			Retention: RetentionFacts{
				Enabled: true, Known: true, HasRun: true,
				LastRun: RetentionRunFacts{
					Status: retention.RunFailed, StartedAt: failed.Add(-time.Minute), FinishedAt: &failed,
					Errors: 1, Error: message,
				},
			},
		}
		results := Run([]Check{{ID: "host.retention", Category: CatHost, Run: retentionLastRun}}, facts)
		if len(results) != 1 || results[0].Status != Warn {
			t.Fatalf("registry storage failure %q = %+v, want warn", message, results)
		}
		if !strings.Contains(results[0].Fix, "eregion apply") || !strings.Contains(results[0].Fix, "host-directories-v2") {
			t.Fatalf("registry storage failure fix = %q", results[0].Fix)
		}
	}

	facts := Facts{
		Now: now,
		Retention: RetentionFacts{
			Enabled: true, Known: true, HasRun: true,
			LastRun: RetentionRunFacts{
				Status: retention.RunFailed, StartedAt: failed.Add(-time.Minute), FinishedAt: &failed,
				Errors: 1, Error: "the node could not list dangling images",
			},
		},
	}
	results := Run([]Check{{ID: "host.retention", Category: CatHost, Run: retentionLastRun}}, facts)
	if len(results) != 1 || strings.Contains(results[0].Fix, "host-directories-v2") {
		t.Fatalf("an unrelated failure points at the registry permission fix: %+v", results)
	}
}

func TestBuildCacheMeasurementReadsOnlyFreshDurableValues(t *testing.T) {
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	fresh := now.Add(-time.Hour)
	stale := now.Add(-49 * time.Hour)
	zero := int64(0)
	known := int64(42)
	for _, test := range []struct {
		name       string
		bytes      *int64
		measuredAt *time.Time
		wantKnown  bool
		wantBytes  int64
	}{
		{name: "never measured", measuredAt: &fresh},
		{name: "stale", bytes: &known, measuredAt: &stale},
		{name: "fresh", bytes: &known, measuredAt: &fresh, wantKnown: true, wantBytes: 42},
		{name: "healthy empty", bytes: &zero, measuredAt: &fresh, wantKnown: true, wantBytes: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			facts := BuildCacheFacts{}
			applyBuildCacheMeasurement(&facts, test.bytes, test.measuredAt, "", now)
			if facts.Known != test.wantKnown || facts.Bytes != test.wantBytes {
				t.Fatalf("measurement = known=%t bytes=%d, want known=%t bytes=%d (error %q)",
					facts.Known, facts.Bytes, test.wantKnown, test.wantBytes, facts.Error)
			}
			if !test.wantKnown && facts.Error == "" {
				t.Fatal("an unknown measurement had no explanation")
			}
		})
	}
}

func TestBuildpackCacheUnknownIsNotZeroHealthy(t *testing.T) {
	results := Run([]Check{{ID: "host.build_cache", Category: CatHost, Run: buildpackCache}}, Facts{
		BuildCache: BuildCacheFacts{
			Applicable: true, Known: false, Error: "the latest Buildpacks cache measurement is older than 48 hours",
			CapBytes: 10 << 30,
		},
	})
	if len(results) != 1 || results[0].Status != Pass {
		t.Fatalf("unknown cache measurement = %+v, want pass/unknown", results)
	}
	if !strings.Contains(results[0].Detail, "unknown") || strings.Contains(results[0].Detail, "0 B") {
		t.Fatalf("unknown cache detail = %q", results[0].Detail)
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
