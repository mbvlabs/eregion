package upgrade

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
)

func TestApplyHealthCheckRunsBeforeJournalSuccess(t *testing.T) {
	f := newFixture(t)
	called := false
	f.manager.HealthProbe = func(_ context.Context, path string) (string, error) {
		called = true
		if got := readFile(t, path); got != string(f.newBytes) {
			t.Fatalf("probe saw %q, want the new binary", got)
		}
		return "version probe: 1.1.0", nil
	}
	plan, err := f.manager.Prepare(f.manifest)
	if err != nil {
		t.Fatal(err)
	}
	applied, err := f.manager.Apply(context.Background(), f.manifest, plan.Confirmation)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !called || applied.HealthCheck != "version probe: 1.1.0" {
		t.Fatalf("health probe not recorded: called=%t result=%+v", called, applied)
	}
	journal, err := loadJournal(journalPath(f.state, TargetOperator))
	if err != nil || journal == nil || journal.Status != StatusInstalled || journal.HealthCheck != "version probe: 1.1.0" {
		t.Fatalf("journal = %+v, err = %v", journal, err)
	}
}

func TestApplyHealthCheckFailureRestoresBackupAutomatically(t *testing.T) {
	f := newFixture(t)
	f.manager.HealthProbe = func(context.Context, string) (string, error) {
		return "", contract.NewFault(contract.PreconditionFailed, "version probe reported a different version", "")
	}
	plan, err := f.manager.Prepare(f.manifest)
	if err != nil {
		t.Fatal(err)
	}
	applied, err := f.manager.Apply(context.Background(), f.manifest, plan.Confirmation)
	if err == nil {
		t.Fatal("a failed health probe activated the release")
	}
	if contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("unexpected code: %v", contract.Classify(err).Code)
	}
	if got := readFile(t, f.binary); got != string(f.oldBytes) {
		t.Fatalf("binary = %q, want the restored previous binary", got)
	}
	journal, loadErr := loadJournal(journalPath(f.state, TargetOperator))
	if loadErr != nil || journal == nil || journal.Status != StatusRolledBack || !strings.Contains(journal.Error, "health check failed") {
		t.Fatalf("journal = %+v, err = %v", journal, loadErr)
	}
	if applied.Journal.OperationID == "" {
		t.Fatal("failed apply did not report its journal")
	}
}

func TestApplyHealthCheckTimeoutRestoresBackup(t *testing.T) {
	f := newFixture(t)
	f.manager.HealthTimeout = 10 * time.Millisecond
	f.manager.HealthProbe = func(ctx context.Context, _ string) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	}
	plan, err := f.manager.Prepare(f.manifest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.manager.Apply(context.Background(), f.manifest, plan.Confirmation); err == nil {
		t.Fatal("a timed-out health probe activated the release")
	}
	if got := readFile(t, f.binary); got != string(f.oldBytes) {
		t.Fatalf("binary = %q, want the restored previous binary", got)
	}
}

func TestHealthProbeChecksOperatorVersionAndReadiness(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the probe fixture is a POSIX shell script")
	}
	script := `#!/bin/sh
case "$1" in
version)
  if [ "$2" = "--json" ]; then
    echo '{"ok":true,"data":{"version":"1.1.0"}}'
  fi
  ;;
system)
  echo '{"ok":true,"data":{"current":3,"required":3}}'
  ;;
*)
  exit 2
  ;;
esac
`
	path := writeProbeScript(t, script)
	manifest := Manifest{Version: "1.1.0", SchemaVersion: 3}
	probe := NewHealthProbe(ProbeConfig{Target: TargetOperator, Manifest: manifest, DatabaseURL: "postgres://test.invalid/db", Timeout: 5 * time.Second})
	detail, err := probe(context.Background(), path)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !strings.Contains(detail, "version probe: 1.1.0") || !strings.Contains(detail, "readiness probe: schema 3 current") {
		t.Fatalf("detail = %q", detail)
	}
}

func TestHealthProbeRejectsVersionMismatchAndPendingSchema(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the probe fixture is a POSIX shell script")
	}
	manifest := Manifest{Version: "1.1.0", SchemaVersion: 3}
	for name, script := range map[string]string{
		"version mismatch": `#!/bin/sh
if [ "$1" = "version" ]; then echo '{"ok":true,"data":{"version":"9.9.9"}}'; exit 0; fi
exit 0
`,
		"pending schema": `#!/bin/sh
if [ "$1" = "version" ]; then echo '{"ok":true,"data":{"version":"1.1.0"}}'; exit 0; fi
if [ "$1" = "system" ]; then echo '{"ok":true,"data":{"current":2,"required":3}}'; exit 0; fi
exit 0
`,
		"manifest schema mismatch": `#!/bin/sh
if [ "$1" = "version" ]; then echo '{"ok":true,"data":{"version":"1.1.0"}}'; exit 0; fi
if [ "$1" = "system" ]; then echo '{"ok":true,"data":{"current":4,"required":4}}'; exit 0; fi
exit 0
`,
	} {
		path := writeProbeScript(t, script)
		probe := NewHealthProbe(ProbeConfig{Target: TargetOperator, Manifest: manifest, DatabaseURL: "postgres://test.invalid/db", Timeout: 5 * time.Second})
		if _, err := probe(context.Background(), path); err == nil {
			t.Fatalf("%s was accepted", name)
		}
	}
}

func TestHealthProbeNodeUsesTheEregionVersion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the probe fixture is a POSIX shell script")
	}
	// A node runs the same eregion binary, so it answers the same enveloped
	// version --json. A development build or a different release must never
	// satisfy a signed artifact.
	envelope := func(version string) string {
		return "#!/bin/sh\nif [ \"$1\" = \"version\" ] && [ \"$2\" = \"--json\" ]; then echo '{\"ok\":true,\"data\":{\"version\":\"" + version + "\"}}'; exit 0; fi\nexit 1\n"
	}
	probe := NewHealthProbe(ProbeConfig{Target: TargetNode, Manifest: Manifest{Version: "1.1.0"}, Timeout: 5 * time.Second})
	if detail, err := probe(context.Background(), writeProbeScript(t, envelope("1.1.0"))); err != nil || !strings.Contains(detail, "version probe: 1.1.0") {
		t.Fatalf("detail = %q, err = %v", detail, err)
	}
	for _, wrong := range []string{"dev", "1.0.0", "9.9.9"} {
		if _, err := probe(context.Background(), writeProbeScript(t, envelope(wrong))); err == nil {
			t.Fatalf("node version %q was accepted for a 1.1.0 manifest", wrong)
		}
	}
	if _, err := probe(context.Background(), writeProbeScript(t, "#!/bin/sh\necho 'eregion-node 1.1.0'\n")); err == nil {
		t.Fatal("a separate node binary passed the eregion version probe")
	}
}

func writeProbeScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "binary")
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestInterruptedHealthCheckIsNeverTreatedAsInstalled(t *testing.T) {
	for name, replaced := range map[string]bool{"before rename": false, "after rename": true} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			plan, err := f.manager.Prepare(f.manifest)
			if err != nil {
				t.Fatal(err)
			}
			if err := f.manager.recordBackup(plan); err != nil {
				t.Fatal(err)
			}
			if replaced {
				if err := os.WriteFile(f.binary, f.newBytes, 0o755); err != nil {
					t.Fatal(err)
				}
			}
			newSHA, _, _, err := fileSHA256(f.binary, MaxArtifactBytes)
			if err != nil {
				t.Fatal(err)
			}
			journal := Journal{
				Schema: JournalSchema, Target: TargetOperator, Status: StatusVerifying,
				BinaryPath: f.binary, FromVersion: "1.0.0", FromSHA256: plan.CurrentSHA256,
				ToVersion: "1.1.0", ToSHA256: newSHA, BackupPath: plan.BackupPath,
				BackupSHA256: plan.CurrentSHA256,
			}
			if err := saveJournal(journalPath(f.state, TargetOperator), journal); err != nil {
				t.Fatal(err)
			}
			recovery, err := f.manager.Recover(context.Background())
			if err != nil {
				t.Fatalf("recover: %v", err)
			}
			if recovery.Action != "rolled_back" {
				t.Fatalf("recovery action = %q", recovery.Action)
			}
			if got := readFile(t, f.binary); got != string(f.oldBytes) {
				t.Fatalf("binary = %q, want the previous binary", got)
			}
			saved, err := loadJournal(journalPath(f.state, TargetOperator))
			if err != nil || saved == nil || saved.Status != StatusRolledBack {
				t.Fatalf("journal = %+v, err = %v", saved, err)
			}
		})
	}
}
