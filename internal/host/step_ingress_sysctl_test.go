package host

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/engine"
)

func TestIngressSysctlEnablesTCPMigrationNowAndAtBoot(t *testing.T) {
	dir := t.TempDir()
	step := &ingressSysctlStep{confPath: filepath.Join(dir, "60-eregion-ingress.conf"), procPath: filepath.Join(dir, "tcp_migrate_req")}
	if err := os.WriteFile(step.procPath, []byte("0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	check, err := step.Check(ctx, engine.Runtime{})
	if err != nil || check.Complete {
		t.Fatalf("fresh host: check=%+v err=%v", check, err)
	}
	diff, err := step.Diff(ctx, engine.Runtime{})
	if err != nil || !strings.Contains(strings.Join(diff, "\n"), "tcp_migrate_req = 1") {
		t.Fatalf("diff=%v err=%v", diff, err)
	}
	if _, err := step.Apply(ctx, engine.Runtime{}, nil); err != nil {
		t.Fatal(err)
	}
	conf, err := os.ReadFile(step.confPath)
	if err != nil || !strings.Contains(string(conf), "\nnet.ipv4.tcp_migrate_req = 1\n") {
		t.Fatalf("boot config %q err=%v", conf, err)
	}
	if check, err := step.Check(ctx, engine.Runtime{}); err != nil || !check.Complete {
		t.Fatalf("after apply: check=%+v err=%v", check, err)
	}

	// A kernel value reset (for example by another sysctl file) is drift even
	// when the boot file is intact.
	if err := os.WriteFile(step.procPath, []byte("0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if check, err := step.Check(ctx, engine.Runtime{}); err != nil || check.Complete {
		t.Fatalf("kernel drift: check=%+v err=%v", check, err)
	}
}

func TestIngressSysctlRunsOnlyWithAMainHostname(t *testing.T) {
	ids := func(cfg Config) []string {
		steps, err := (&Runner{Config: cfg}).steps(Credentials{})
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, step := range steps {
			out = append(out, step.ID())
		}
		return out
	}
	cfg := Config{Role: RoleMain, Hostname: "main.example.com"}
	got := strings.Join(ids(cfg), " ")
	if !strings.Contains(got, "host-ingress-sysctl-v1 host-ingress-v2") {
		t.Fatalf("main node steps: %s", got)
	}
	cfg.Hostname = ""
	got = strings.Join(ids(cfg), " ")
	if strings.Contains(got, "host-ingress-sysctl") {
		t.Fatalf("hostname-less node still gets ingress sysctl: %s", got)
	}
	if !strings.Contains(got, "host-ingress-v2") {
		t.Fatalf("hostname-less control plane lacks the pinned Caddy prerequisite: %s", got)
	}
	steps := ids(cfg)
	ingress, telemetry := -1, -1
	for i, id := range steps {
		switch id {
		case "host-ingress-v2":
			ingress = i
		case "host-telemetry-v1":
			telemetry = i
		}
	}
	if ingress < 0 || telemetry < 0 || ingress > telemetry {
		t.Fatalf("Caddy prerequisite must precede telemetry: %v", steps)
	}
}

func TestIngressWithoutHostnameInstallsCaddyWithoutAPublicSite(t *testing.T) {
	step := &ingressStep{cfg: Config{Arch: "amd64"}}
	if file := step.caddyfile(); file != nil {
		t.Fatalf("hostname-less Caddyfile must not be written:\n%s", file)
	}
	if !strings.Contains(step.Describe(), "dedicated telemetry") {
		t.Fatalf("describe = %q", step.Describe())
	}
	diff, err := step.Diff(context.Background(), engine.Runtime{})
	if err != nil || !strings.Contains(strings.Join(diff, "\n"), "refuse an existing /etc/caddy/Caddyfile") {
		t.Fatalf("diff = %v err=%v", diff, err)
	}
}
