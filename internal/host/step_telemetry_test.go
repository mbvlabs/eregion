package host

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/access"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/telemetry"
)

type telemetryExec struct {
	scripts       []string
	contexts      []context.Context
	output        string
	serviceOutput string
	err           error
	failContains  string
}

func (e *telemetryExec) Output(_ context.Context, script string) (string, error) {
	if strings.Contains(script, "unit:present") {
		return e.serviceOutput, nil
	}
	return e.output, nil
}
func (e *telemetryExec) Run(ctx context.Context, _ string, script string, _ map[string]string, _ engine.Reporter) error {
	e.scripts = append(e.scripts, script)
	e.contexts = append(e.contexts, ctx)
	if e.failContains != "" && strings.Contains(script, e.failContains) {
		if e.err != nil {
			return e.err
		}
		return contract.NewFault(contract.Unavailable, "fixture command failed", "Retry the fixture.")
	}
	return e.err
}

type telemetryCallbacks struct {
	validated int
	restarted int
	stopped   int
}

func testTelemetryStep(t *testing.T) (*telemetryStep, *telemetryExec, *telemetryCallbacks) {
	t.Helper()
	dir := t.TempDir()
	exec := &telemetryExec{
		output:        "enabled:ok\nactive:ok\nfirewall:ok",
		serviceOutput: "unit:present\nenabled:yes\nactive:yes",
	}
	callbacks := &telemetryCallbacks{}
	cfg := DefaultConfig(RoleMain, "main")
	cfg.Hostname = "main.example.com"
	cfg.Layout.SystemdDir = filepath.Join(dir, "systemd")
	step := &telemetryStep{
		cfg: cfg, exec: exec,
		dir:         dir + "/telemetry",
		targetsPath: dir + "/telemetry/targets.json",
		caddyPath:   dir + "/telemetry/Caddyfile",
		unitFile:    filepath.Join(cfg.Layout.SystemdDir, TelemetryService),
		lockPath:    dir + "/lock/rotate.lock",
		validate: func(context.Context) error {
			callbacks.validated++
			return nil
		},
		restart: func(context.Context) error {
			callbacks.restarted++
			return nil
		},
		stop: func(context.Context) error {
			callbacks.stopped++
			return nil
		},
	}
	return step, exec, callbacks
}

func legacyTelemetryCaddyfile(hash string) []byte {
	return []byte("# managed by eregion metrics rotate; contains a bcrypt hash, not a password\n{\n\tadmin off\n\tauto_https off\n}\nhttp://" + access.NodeAddress + ":9101 {\n\tbind " + access.NodeAddress + "\n\tbasic_auth {\n\t\t" + telemetry.Username + " " + hash + "\n\t}\n\thandle " + telemetry.GatewayDiscoveryPath + " {\n\t\troot * " + TelemetryDir + "\n\t\tfile_server\n\t}\n\thandle " + telemetry.GatewayMetricsPath + " {\n\t\treverse_proxy " + NodeExporterLoopbackAddress + " {\n\t\t\trewrite /metrics\n\t\t\theader_up -Authorization\n\t\t\theader_up -Cookie\n\t\t}\n\t}\n\thandle {\n\t\trespond 404\n\t}\n}\n")
}

func TestTelemetryTargetsArePublicHTTPSDiscovery(t *testing.T) {
	step, _, _ := testTelemetryStep(t)
	targets := step.targets()
	var entries []struct {
		Targets []string          `json:"targets"`
		Labels  map[string]string `json:"labels"`
	}
	if err := json.Unmarshal(targets, &entries); err != nil || len(entries) != 1 {
		t.Fatalf("targets = %s err=%v", targets, err)
	}
	if entries[0].Targets[0] != "main.example.com:443" ||
		entries[0].Labels["__metrics_path__"] != telemetry.MetricsPath ||
		entries[0].Labels["__scheme__"] != "https" ||
		entries[0].Labels["eregion_node"] != "main" ||
		entries[0].Labels["eregion_component"] != "node" ||
		entries[0].Labels["eregion_public"] != "true" ||
		entries[0].Labels["eregion_access"] != "public_https" {
		t.Fatalf("targets = %+v", entries[0])
	}
	for _, forbidden := range []string{access.NodeAddress, "127.0.0.1", "9101", "9100"} {
		if strings.Contains(string(targets), forbidden) {
			t.Fatalf("discovery target came from a listener or WireGuard address: %s", targets)
		}
	}
}

func TestTelemetryCaddyfileIsLoopbackAuthenticatedAndStripsScraperCredential(t *testing.T) {
	site := string(telemetryCaddyfile("$2a$08$hash"))
	for _, want := range []string{
		"admin off",
		"auto_https off",
		"http://" + telemetry.GatewayListenAddress,
		"bind " + telemetry.GatewayHost,
		"basic_auth",
		telemetry.Username + " $2a$08$hash",
		"handle " + telemetry.GatewayDiscoveryPath,
		"root * " + TelemetryDir,
		"file_server",
		"handle " + telemetry.GatewayMetricsPath,
		"reverse_proxy " + NodeExporterLoopbackAddress,
		"rewrite /metrics",
		"header_up -Authorization",
		"header_up -Cookie",
		"dial_timeout 2s",
		"response_header_timeout 15s",
	} {
		if !strings.Contains(site, want) {
			t.Fatalf("Caddyfile missing %q:\n%s", want, site)
		}
	}
	for _, unwanted := range []string{"0.0.0.0", "10.78.0.1", telemetry.MetricsPath, "https://main.example.com"} {
		if strings.Contains(site, unwanted) {
			t.Fatalf("Caddyfile unexpectedly contains %q:\n%s", unwanted, site)
		}
	}
}

func TestTelemetryUnitHasNoWireGuardDependency(t *testing.T) {
	unit := string(telemetryUnit())
	for _, want := range []string{
		"Wants=network-online.target",
		"After=network-online.target",
		"ConditionPathExists=" + TelemetryCaddyPath,
		"User=caddy",
		"StateDirectory=eregion-telemetry",
		"ExecStart=/usr/bin/caddy run --config " + TelemetryCaddyPath + " --adapter caddyfile",
		"Restart=on-failure",
		"MemoryMax=64M",
		"WantedBy=multi-user.target",
	} {
		if !strings.Contains(unit, want) {
			t.Fatalf("unit missing %q:\n%s", want, unit)
		}
	}
	for _, unwanted := range []string{"wg-quick", "BindsTo=", "ConditionPathExists=/sys/class/net/wg0", "10.78.0.1", "caddy.service"} {
		if strings.Contains(unit, unwanted) {
			t.Fatalf("unit still depends on WireGuard (%q):\n%s", unwanted, unit)
		}
	}
}

func TestTelemetryApplyWritesLoopbackGatewayAndRemovesOnlyTheLegacyRule(t *testing.T) {
	step, exec, _ := testTelemetryStep(t)
	if _, err := step.Apply(context.Background(), engine.Runtime{}, nil); err != nil {
		t.Fatal(err)
	}
	raw, mode, exists, err := fileState(step.targetsPath)
	if err != nil || !exists || mode != 0o640 || !strings.Contains(string(raw), `"main.example.com:443"`) {
		t.Fatalf("targets = %q mode=%04o exists=%t err=%v", raw, mode, exists, err)
	}
	for _, forbidden := range []string{access.NodeAddress, "127.0.0.1:9101"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("targets contain a private listener target: %s", raw)
		}
	}
	unit, unitMode, unitExists, err := fileState(step.unitPath())
	if err != nil || !unitExists || unitMode != 0o644 || !strings.Contains(string(unit), "ExecStart=/usr/bin/caddy run") {
		t.Fatalf("unit = %q mode=%04o exists=%t err=%v", unit, unitMode, unitExists, err)
	}
	if strings.Contains(string(unit), "wg-quick") {
		t.Fatalf("unit couples to WireGuard:\n%s", unit)
	}
	if len(exec.scripts) != 1 {
		t.Fatalf("service scripts = %v", exec.scripts)
	}
	script := exec.scripts[0]
	if !strings.Contains(script, "systemctl enable "+TelemetryService) ||
		!strings.Contains(script, "ufw --force delete allow in on wg0 to "+access.NodeAddress+" port 9101 proto tcp") {
		t.Fatalf("apply did not converge the loopback gateway prerequisites:\n%s", script)
	}
	if strings.Contains(script, "ufw allow in on wg0 to "+access.NodeAddress+" port 9101 proto tcp >/dev/null") {
		t.Fatalf("apply added a WireGuard telemetry rule:\n%s", script)
	}
	if _, _, exists, err := fileState(step.caddyPath); err != nil || exists {
		t.Fatalf("apply wrote a rotation-owned Caddyfile: exists=%t err=%v", exists, err)
	}
}

func TestTelemetryApplyPreservesTheExistingCredentialHashOnLoopbackRender(t *testing.T) {
	step, _, callbacks := testTelemetryStep(t)
	if err := os.MkdirAll(filepath.Dir(step.caddyPath), 0o755); err != nil {
		t.Fatal(err)
	}
	const hash = "$2a$08$Zkx19XLiW6VYouLHR5NmfOFU0z2GTNmpkT/5qqR7hx4IjWJPDhjvG"
	legacy := legacyTelemetryCaddyfile(hash)
	if err := os.WriteFile(step.caddyPath, legacy, 0o640); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(step.caddyPath)
	if _, err := step.Apply(context.Background(), engine.Runtime{}, nil); err != nil {
		t.Fatal(err)
	}
	after, mode, exists, err := fileState(step.caddyPath)
	if err != nil || !exists || mode != 0o640 {
		t.Fatalf("gateway config = mode %04o exists=%t err=%v", mode, exists, err)
	}
	if string(after) == string(before) {
		t.Fatal("apply did not move the gateway Caddyfile to loopback")
	}
	got, err := extractTelemetryHash(after)
	if err != nil || got != hash {
		t.Fatalf("apply changed or invented the credential hash: got=%q err=%v", got, err)
	}
	if !strings.Contains(string(after), "bind "+telemetry.GatewayHost) || strings.Contains(string(after), access.NodeAddress) {
		t.Fatalf("gateway is not loopback only:\n%s", after)
	}
	if callbacks.validated != 1 || callbacks.restarted != 1 {
		t.Fatalf("apply did not validate/restart the gateway once: %+v", callbacks)
	}
}

func TestTelemetryApplyFailsClosedOnMalformedExistingConfiguration(t *testing.T) {
	step, _, callbacks := testTelemetryStep(t)
	if err := os.MkdirAll(filepath.Dir(step.caddyPath), 0o755); err != nil {
		t.Fatal(err)
	}
	malformed := []byte("# managed by eregion\nthis is not a basic auth file\n")
	if err := os.WriteFile(step.caddyPath, malformed, 0o640); err != nil {
		t.Fatal(err)
	}
	_, err := step.Apply(context.Background(), engine.Runtime{}, nil)
	if err == nil || contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("apply = %v, want precondition_failed", err)
	}
	raw, err := os.ReadFile(step.caddyPath)
	if err != nil || string(raw) != string(malformed) {
		t.Fatalf("malformed config changed: %q err=%v", raw, err)
	}
	if callbacks.validated != 0 || callbacks.restarted != 0 || callbacks.stopped != 0 {
		t.Fatalf("malformed config activated a service: %+v", callbacks)
	}
}

func TestTelemetryApplyWaitsForAConcurrentRotationAndPreservesTheNewHash(t *testing.T) {
	step, _, _ := testTelemetryStep(t)
	oldCredential, err := telemetry.NewCredential()
	if err != nil {
		t.Fatal(err)
	}
	oldHash, err := telemetry.HashPassword(oldCredential.Password)
	if err != nil {
		t.Fatal(err)
	}
	newCredential, err := telemetry.NewCredential()
	if err != nil {
		t.Fatal(err)
	}
	newHash, err := telemetry.HashPassword(newCredential.Password)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(step.caddyPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(step.caddyPath, telemetryCaddyfile(oldHash), 0o640); err != nil {
		t.Fatal(err)
	}
	release, err := engine.Acquire(context.Background(), step.rotationLockPath(), engine.LockExclusive, 0)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, applyErr := step.Apply(context.Background(), engine.Runtime{}, nil)
		done <- applyErr
	}()
	// Apply now takes the telemetry lock before it reads or writes any managed
	// file, so wait briefly for the goroutine to reach the lock and then let a
	// simulated rotation finish while apply is waiting.
	time.Sleep(100 * time.Millisecond)
	// Simulate a rotation that finishes while apply is waiting. Apply must not
	// overwrite this newer credential with the older hash it may have seen.
	if err := os.WriteFile(step.caddyPath, telemetryCaddyfile(newHash), 0o640); err != nil {
		t.Fatal(err)
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(step.caddyPath)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := extractTelemetryHash(raw)
	if err != nil || hash != newHash {
		t.Fatalf("apply did not preserve the concurrent rotation hash: got=%q err=%v", hash, err)
	}
}

func TestTelemetryApplyErrorsDoNotLeakCredentialMaterial(t *testing.T) {
	step, _, _ := testTelemetryStep(t)
	credential, err := telemetry.NewCredential()
	if err != nil {
		t.Fatal(err)
	}
	hash, err := telemetry.HashPassword(credential.Password)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(step.caddyPath), 0o755); err != nil {
		t.Fatal(err)
	}
	malformed := []byte("# managed by eregion\nbasic_auth {\n\tmetrics " + hash + "\n\tcanary " + credential.Password + "\n}\n")
	if err := os.WriteFile(step.caddyPath, malformed, 0o640); err != nil {
		t.Fatal(err)
	}
	_, applyErr := step.Apply(context.Background(), engine.Runtime{}, nil)
	if applyErr == nil {
		t.Fatal("malformed multi-principal config was accepted")
	}
	diff, err := step.Diff(context.Background(), engine.Runtime{})
	if err != nil {
		t.Fatal(err)
	}
	inputs, err := step.Inputs()
	if err != nil {
		t.Fatal(err)
	}
	for name, text := range map[string]string{
		"apply error": applyErr.Error(),
		"diff":        strings.Join(diff, "\n"),
		"inputs":      string(inputs),
	} {
		for _, secret := range []string{hash, credential.Password} {
			if strings.Contains(text, secret) {
				t.Fatalf("%s leaked credential material:\n%s", name, text)
			}
		}
	}
}

func TestTelemetryApplyRejectsMalformedManagedConfigBeforeAnyMutation(t *testing.T) {
	const hash = "$2a$08$Zkx19XLiW6VYouLHR5NmfOFU0z2GTNmpkT/5qqR7hx4IjWJPDhjvG"
	valid := string(telemetryCaddyfile(hash))
	for name, malformed := range map[string]string{
		"unknown site directive": strings.Replace(valid, "\tbasic_auth {", "\ttls internal\n\tbasic_auth {", 1),
		"duplicate transport":    strings.Replace(valid, "\t\t\ttransport http {", "\t\t\ttransport http {\n\t\t\t\tdial_timeout 99s\n\t\t\t}\n\t\t\ttransport http {", 1),
	} {
		t.Run(name, func(t *testing.T) {
			step, exec, callbacks := testTelemetryStep(t)
			payload := []byte(malformed)
			previousTargets := []byte("{\"old\":\"targets\"}\n")
			previousUnit := []byte("[Unit]\nDescription=old telemetry unit\n")
			if err := os.MkdirAll(filepath.Dir(step.caddyPath), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Dir(step.unitPath()), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(step.caddyPath, payload, 0o640); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(step.targetsPath, previousTargets, 0o640); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(step.unitPath(), previousUnit, 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := step.Apply(context.Background(), engine.Runtime{}, nil)
			if err == nil || contract.Classify(err).Code != contract.PreconditionFailed {
				t.Fatalf("apply = %v, want precondition_failed", err)
			}
			if strings.Contains(err.Error(), hash) || strings.Contains(err.Error(), "tls internal") ||
				strings.Contains(err.Error(), "dial_timeout 99s") {
				t.Fatalf("apply error echoed config or credential material: %v", err)
			}
			for file, want := range map[string]struct {
				path string
				data []byte
			}{
				"caddy":   {step.caddyPath, payload},
				"targets": {step.targetsPath, previousTargets},
				"unit":    {step.unitPath(), previousUnit},
			} {
				raw, _, exists, err := fileState(want.path)
				if err != nil || !exists || string(raw) != string(want.data) {
					t.Fatalf("%s changed before validation: %q err=%v", file, raw, err)
				}
			}
			if len(exec.scripts) != 0 || callbacks.validated != 0 || callbacks.restarted != 0 || callbacks.stopped != 0 {
				t.Fatalf("malformed config mutated service state: scripts=%v callbacks=%+v", exec.scripts, callbacks)
			}
		})
	}
}

func TestTelemetryApplyRestoresAllManagedStateWhenActivationFails(t *testing.T) {
	step, exec, callbacks := testTelemetryStep(t)
	const hash = "$2a$08$Zkx19XLiW6VYouLHR5NmfOFU0z2GTNmpkT/5qqR7hx4IjWJPDhjvG"
	previousTargets := []byte("{\"old\":\"targets\"}\n")
	previousUnit := []byte("[Unit]\nDescription=old telemetry unit\n")
	if err := os.MkdirAll(filepath.Dir(step.caddyPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(step.unitPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(step.caddyPath, legacyTelemetryCaddyfile(hash), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(step.caddyPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(step.targetsPath, previousTargets, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(step.targetsPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(step.unitPath(), previousUnit, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(step.unitPath(), 0o640); err != nil {
		t.Fatal(err)
	}
	callbacks.restarted = 0
	step.validate = func(context.Context) error {
		callbacks.validated++
		return contract.NewFault(contract.PreconditionFailed, "validation failed", "")
	}
	_, err := step.Apply(context.Background(), engine.Runtime{}, nil)
	if err == nil {
		t.Fatal("a failed gateway validation reported success")
	}
	for name, want := range map[string]struct {
		path string
		data []byte
		mode os.FileMode
	}{
		"caddy":   {step.caddyPath, legacyTelemetryCaddyfile(hash), 0o600},
		"targets": {step.targetsPath, previousTargets, 0o600},
		"unit":    {step.unitPath(), previousUnit, 0o640},
	} {
		raw, mode, exists, err := fileState(want.path)
		if err != nil || !exists || mode != want.mode || string(raw) != string(want.data) {
			t.Fatalf("%s was not restored: data=%q mode=%04o exists=%t err=%v", name, raw, mode, exists, err)
		}
	}
	if callbacks.validated != 1 || callbacks.restarted != 1 {
		t.Fatalf("validation/recovery callbacks = %+v err=%v scripts=%v", callbacks, err, exec.scripts)
	}
	if len(exec.scripts) == 0 || !strings.Contains(exec.scripts[len(exec.scripts)-1], "systemctl enable "+TelemetryService) {
		t.Fatalf("previous enabled service state was not restored: %v", exec.scripts)
	}
}

func TestTelemetryApplyRollbackRemovesFilesThatDidNotExistBefore(t *testing.T) {
	step, exec, _ := testTelemetryStep(t)
	exec.serviceOutput = "unit:absent"
	exec.failContains = "systemctl enable"
	_, err := step.Apply(context.Background(), engine.Runtime{}, nil)
	if err == nil {
		t.Fatal("a failed prerequisite activation reported success")
	}
	for name, path := range map[string]string{
		"caddy":   step.caddyPath,
		"targets": step.targetsPath,
		"unit":    step.unitPath(),
	} {
		if _, _, exists, err := fileState(path); err != nil || exists {
			t.Fatalf("%s remained after rollback: exists=%t err=%v", name, exists, err)
		}
	}
}

func TestTelemetryApplyRollbackRestoresDisabledInactivePriorService(t *testing.T) {
	step, exec, callbacks := testTelemetryStep(t)
	const hash = "$2a$08$Zkx19XLiW6VYouLHR5NmfOFU0z2GTNmpkT/5qqR7hx4IjWJPDhjvG"
	exec.serviceOutput = "unit:present\nenabled:no\nactive:no"
	if err := os.MkdirAll(filepath.Dir(step.caddyPath), 0o755); err != nil {
		t.Fatal(err)
	}
	previous := legacyTelemetryCaddyfile(hash)
	if err := os.WriteFile(step.caddyPath, previous, 0o640); err != nil {
		t.Fatal(err)
	}
	callbacks.restarted = 0
	step.restart = func(context.Context) error {
		return contract.NewFault(contract.Unavailable, "activation failed", "")
	}
	_, err := step.Apply(context.Background(), engine.Runtime{}, nil)
	if err == nil {
		t.Fatal("a failed gateway activation reported success")
	}
	raw, err := os.ReadFile(step.caddyPath)
	if err != nil || string(raw) != string(previous) {
		t.Fatalf("previous gateway configuration was not restored: %q err=%v", raw, err)
	}
	if callbacks.stopped != 1 {
		t.Fatalf("prior stopped service was not restored: %+v", callbacks)
	}
	if len(exec.scripts) == 0 || !strings.Contains(exec.scripts[len(exec.scripts)-1], "systemctl disable "+TelemetryService) {
		t.Fatalf("prior disabled service state was not restored: %v", exec.scripts)
	}
}

func TestTelemetryActivationRollbackUsesLiveRecoveryContext(t *testing.T) {
	for name, tc := range map[string]struct {
		firstErr    error
		recoveryErr error
		cancel      bool
		wantCode    contract.Code
		wantError   error
	}{
		"cancelled": {
			firstErr: context.Canceled, cancel: true,
			wantCode: contract.Cancelled, wantError: context.Canceled,
		},
		"deadline": {
			firstErr: context.DeadlineExceeded,
			wantCode: contract.Unavailable, wantError: context.DeadlineExceeded,
		},
		"cancelled with failed recovery": {
			firstErr: context.Canceled, cancel: true,
			recoveryErr: contract.NewFault(contract.Unavailable, "recovery failed", ""),
			wantCode:    contract.Cancelled, wantError: context.Canceled,
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "Caddyfile")
			previous := []byte("# managed by eregion\nprevious\n")
			if err := os.WriteFile(path, previous, 0o600); err != nil {
				t.Fatal(err)
			}
			var recoveryCtx context.Context
			var recoveryLive, recoveryDeadline bool
			var recoveryValue any
			var cancel context.CancelFunc
			restarts := 0
			ctx := context.WithValue(context.Background(), testContextKey("recovery"), "preserved")
			if tc.cancel {
				ctx, cancel = context.WithCancel(ctx)
			}
			err := activateTelemetryCaddy(ctx, path, previous, 0o600, true, []byte("# managed by eregion\nnew\n"),
				func(context.Context) error {
					if cancel != nil {
						// The activation context is cancelled while the request
						// is still inside activation.
						cancel()
					}
					return tc.firstErr
				},
				func(recovery context.Context) error {
					restarts++
					recoveryCtx = recovery
					recoveryLive = recovery.Err() == nil
					_, recoveryDeadline = recovery.Deadline()
					recoveryValue = recovery.Value(testContextKey("recovery"))
					return tc.recoveryErr
				},
				func(context.Context) error { return nil },
			)
			if !errors.Is(err, tc.wantError) || contract.Classify(err).Code != tc.wantCode {
				t.Fatalf("activation error = %v class=%s, want %s", err, contract.Classify(err).Code, tc.wantCode)
			}
			if recoveryCtx == nil || !recoveryLive {
				t.Fatalf("recovery ran with a cancelled context: %+v", recoveryCtx)
			}
			if !recoveryDeadline {
				t.Fatal("recovery context has no finite deadline")
			}
			if recoveryValue != "preserved" {
				t.Fatal("recovery context lost the original values")
			}
			if restarts != 1 {
				t.Fatalf("prior service was not reactivated exactly once: %d", restarts)
			}
			raw, err := os.ReadFile(path)
			if err != nil || string(raw) != string(previous) {
				t.Fatalf("previous gateway config was not restored: %q err=%v", raw, err)
			}
		})
	}
}

func TestTelemetryEndpointInfoMarksAnOldGatewayRenderAsNeedsApply(t *testing.T) {
	const hash = "$2a$08$Zkx19XLiW6VYouLHR5NmfOFU0z2GTNmpkT/5qqR7hx4IjWJPDhjvG"
	old := legacyTelemetryCaddyfile(hash)
	info, err := telemetryEndpointInfo("main", "main.example.com", old, true)
	if err != nil {
		t.Fatal(err)
	}
	if info.State != telemetry.EndpointStateNeedsApply || !info.Credential || info.MetricsURL != "https://main.example.com/metrics/node" {
		t.Fatalf("old gateway endpoint = %+v", info)
	}
	if err := info.Validate(); err != nil {
		t.Fatalf("needs_apply endpoint did not validate: %v", err)
	}
	current := telemetryCaddyfile(hash)
	info, err = telemetryEndpointInfo("main", "main.example.com", current, true)
	if err != nil || info.State != telemetry.EndpointStateConfigured {
		t.Fatalf("current gateway endpoint = %+v err=%v", info, err)
	}
	info, err = telemetryEndpointInfo("main", "main.example.com", nil, false)
	if err != nil || info.State != telemetry.EndpointStatePendingCredential {
		t.Fatalf("unconfigured gateway endpoint = %+v err=%v", info, err)
	}
	malformed := []byte(strings.Replace(string(telemetryCaddyfile(hash)), "\tbasic_auth {", "\ttls internal\n\tbasic_auth {", 1))
	if _, err := telemetryEndpointInfo("main", "main.example.com", malformed, true); err == nil {
		t.Fatal("describe accepted a malformed managed configuration")
	}
}

func TestTelemetryRotationResultUsesPublicHTTPSURLs(t *testing.T) {
	rotation, err := buildTelemetryRotation("main", "main.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if rotation.Schema != telemetry.RotationSchema || rotation.Hostname != "main.example.com" ||
		rotation.MetricsURL != "https://main.example.com/metrics/node" ||
		rotation.Discovery != "https://main.example.com/metrics/targets.json" ||
		strings.Contains(rotation.MetricsURL, "10.78.0.1") || strings.Contains(rotation.MetricsURL, "127.0.0.1") {
		t.Fatalf("rotation = %+v", rotation)
	}
	if _, err := buildTelemetryRotation("main", ""); err == nil {
		t.Fatal("rotation built a result without a public hostname")
	}
}

func TestTelemetryFirewallCommandsOnlyAddressTheExactLegacyRule(t *testing.T) {
	rule := legacyTelemetryFirewallRule()
	if rule != "ufw allow in on wg0 to "+access.NodeAddress+" port 9101 proto tcp" {
		t.Fatalf("rule = %q", rule)
	}
	check := firewallCheckScript()
	if !strings.Contains(check, "ufw show added") || !strings.Contains(check, "grep -Fqx '"+rule+"'") {
		t.Fatalf("check script does not observe the exact ufw rule:\n%s", check)
	}
	remove := firewallRemovalScript()
	if !strings.HasPrefix(strings.TrimSpace(remove), "set -euo pipefail") {
		t.Fatalf("removal script hides a failed delete:\n%s", remove)
	}
	if !strings.Contains(remove, "grep -Fqx '"+rule+"'") ||
		!strings.Contains(remove, "ufw --force delete allow in on wg0 to "+access.NodeAddress+" port 9101 proto tcp") {
		t.Fatalf("removal script does not address the exact owned rule:\n%s", remove)
	}
	for _, forbidden := range []string{"ufw delete 9101", "ufw reset", "wg0 to any", "port 9100"} {
		if strings.Contains(remove, forbidden) {
			t.Fatalf("removal script could affect a foreign rule (%q):\n%s", forbidden, remove)
		}
	}
}

func TestTelemetryCheckSurfacesAFirewallReadFailure(t *testing.T) {
	step, exec, _ := testTelemetryStep(t)
	step.cfg.Hostname = ""
	exec.output = "firewall:error"
	check, err := step.Check(context.Background(), engine.Runtime{})
	if err != nil || check.Complete || !strings.Contains(check.Detail, "could not be read") {
		t.Fatalf("check = %+v err=%v", check, err)
	}
}

func TestTelemetryMissingHostnameIsUnconfiguredWithoutAWGFallback(t *testing.T) {
	step, exec, _ := testTelemetryStep(t)
	step.cfg.Hostname = ""
	if diff, err := step.Diff(context.Background(), engine.Runtime{}); err != nil || !strings.Contains(strings.Join(diff, "\n"), "no WireGuard fallback") {
		t.Fatalf("diff = %v err=%v", diff, err)
	}
	if _, err := step.Apply(context.Background(), engine.Runtime{}, nil); err != nil {
		t.Fatal(err)
	}
	if _, _, exists, err := fileState(step.targetsPath); err != nil || exists {
		t.Fatalf("hostname-less apply wrote discovery: exists=%t err=%v", exists, err)
	}
	if len(exec.scripts) != 1 || !strings.Contains(exec.scripts[0], "ufw --force delete") {
		t.Fatalf("hostname-less apply scripts = %v", exec.scripts)
	}
	script := exec.scripts[0]
	for _, want := range []string{
		"systemctl disable " + TelemetryService,
		"systemctl stop " + TelemetryService,
		"systemctl is-enabled --quiet " + TelemetryService,
		"systemctl is-active --quiet " + TelemetryService,
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("hostname-less apply does not verify %q:\n%s", want, script)
		}
	}
}

func TestTelemetryMissingHostnameCheckRejectsEnabledOrActiveLegacyService(t *testing.T) {
	for name, serviceOutput := range map[string]string{
		"stopped but enabled": "unit:present\nenabled:yes\nactive:no",
		"disabled but active": "unit:present\nenabled:no\nactive:yes",
	} {
		t.Run(name, func(t *testing.T) {
			step, exec, _ := testTelemetryStep(t)
			step.cfg.Hostname = ""
			exec.output = "firewall:ok"
			exec.serviceOutput = serviceOutput
			check, err := step.Check(context.Background(), engine.Runtime{})
			if err != nil || check.Complete {
				t.Fatalf("check = %+v err=%v, want incomplete", check, err)
			}
			if !strings.Contains(check.Detail, "enabled or active") {
				t.Fatalf("check detail = %q", check.Detail)
			}
		})
	}
}

func TestTelemetryMissingHostnameApplySurfacesDisableFailure(t *testing.T) {
	step, exec, _ := testTelemetryStep(t)
	step.cfg.Hostname = ""
	exec.failContains = "systemctl disable"
	_, err := step.Apply(context.Background(), engine.Runtime{}, nil)
	if err == nil || !strings.Contains(err.Error(), "fixture command failed") {
		t.Fatalf("apply = %v, want the disable failure", err)
	}
	if _, _, exists, _ := fileState(step.targetsPath); exists {
		t.Fatal("failed hostname-less apply wrote discovery")
	}
	if _, _, exists, _ := fileState(step.unitPath()); exists {
		t.Fatal("failed hostname-less apply wrote a unit")
	}
}

func TestTelemetryCaddyfileValidatesWithPinnedCaddy(t *testing.T) {
	bin := os.Getenv("EREGION_CADDY_BIN")
	if bin == "" {
		t.Skip("set EREGION_CADDY_BIN to a pinned Caddy binary to validate the generated Caddyfile")
	}
	// A real bcrypt hash from Caddy's own basic_auth example. The hash format,
	// bind, admin and handler syntax are what this test validates.
	const hash = "$2a$14$Zkx19XLiW6VYouLHR5NmfOFU0z2GTNmpkT/5qqR7hx4IjWJPDhjvG"
	path := filepath.Join(t.TempDir(), "telemetry.Caddyfile")
	if err := os.WriteFile(path, telemetryCaddyfile(hash), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "validate", "--config", path, "--adapter", "caddyfile")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("telemetry Caddyfile rejected by %s: %v\n%s", bin, err, output)
	}
}

func TestManagedTelemetryCaddyfileAcceptsOnlySupportedCompleteLayouts(t *testing.T) {
	const hash = "$2a$08$Zkx19XLiW6VYouLHR5NmfOFU0z2GTNmpkT/5qqR7hx4IjWJPDhjvG"
	for name, raw := range map[string][]byte{
		"current loopback": telemetryCaddyfile(hash),
		"legacy WireGuard": legacyTelemetryCaddyfile(hash),
	} {
		t.Run(name, func(t *testing.T) {
			got, err := extractTelemetryHash(raw)
			if err != nil || got != hash {
				t.Fatalf("extract = %q err=%v", got, err)
			}
		})
	}
}

func TestExtractTelemetryHashRejectsUnsafeShapesWithoutEchoingMaterial(t *testing.T) {
	const hash = "$2a$08$Zkx19XLiW6VYouLHR5NmfOFU0z2GTNmpkT/5qqR7hx4IjWJPDhjvG"
	valid := string(telemetryCaddyfile(hash))
	cases := map[string]string{
		"no header":               "http://127.0.0.1:9101 {\n\tbasic_auth {\n\t\tmetrics " + hash + "\n\t}\n}\n",
		"missing outer structure": "# managed by eregion\nbasic_auth {\n\tmetrics " + hash + "\n}\n",
		"wrong user":              "# managed by eregion\nbasic_auth {\n\tadmin " + hash + "\n}\n",
		"plaintext":               "# managed by eregion\nbasic_auth {\n\tmetrics not-a-hash\n}\n",
		"two principals":          "# managed by eregion\nbasic_auth {\n\tmetrics " + hash + "\n\tother " + hash + "\n}\n",
		"two blocks":              "# managed by eregion\nbasic_auth {\n\tmetrics " + hash + "\n}\nbasic_auth {\n\tmetrics " + hash + "\n}\n",
		"unclosed":                "# managed by eregion\nbasic_auth {\n\tmetrics " + hash + "\n",
		"unbalanced site":         strings.TrimSuffix(valid, "}\n"),
		"unknown site directive":  strings.Replace(valid, "\tbasic_auth {", "\ttls internal\n\tbasic_auth {", 1),
		"unsupported listener":    strings.Replace(valid, "http://127.0.0.1:9101", "http://0.0.0.0:9101", 1),
		"duplicate discovery":     strings.Replace(valid, "\thandle /node {", "\thandle /targets.json {\n\t\troot * "+TelemetryDir+"\n\t\tfile_server\n\t}\n\thandle /node {", 1),
		"duplicate transport":     strings.Replace(valid, "\t\t\ttransport http {", "\t\t\ttransport http {\n\t\t\t\tdial_timeout 99s\n\t\t\t}\n\t\t\ttransport http {", 1),
		"duplicate rewrite":       strings.Replace(valid, "\t\t\trewrite /metrics", "\t\t\trewrite /metrics\n\t\t\trewrite /metrics", 1),
		"duplicate auth header":   strings.Replace(valid, "\t\t\theader_up -Authorization", "\t\t\theader_up -Authorization\n\t\t\theader_up -Authorization", 1),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := extractTelemetryHash([]byte(raw))
			if err == nil {
				t.Fatal("unsafe managed configuration was accepted")
			}
			if strings.Contains(err.Error(), hash) || strings.Contains(err.Error(), "not-a-hash") {
				t.Fatalf("error echoed credential material: %v", err)
			}
		})
	}
}
