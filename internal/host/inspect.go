package host

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mbvlabs/eregion/internal/access"
	"github.com/mbvlabs/eregion/internal/buildinfo"
	"github.com/mbvlabs/eregion/internal/controlplane"
	"github.com/mbvlabs/eregion/internal/doctor"
	"github.com/mbvlabs/eregion/internal/secrets"
)

const caddyCertificateRoot = "/var/lib/caddy/.local/share/caddy/certificates"

// ServiceUnits lists the systemd units a role must keep running.
func ServiceUnits(role Role, hostname string) []string {
	config, err := ConfigForRole(role)
	if err != nil {
		return nil
	}
	var units []string
	if config.ControlPlane {
		units = append(units, "eregion-api.service", "eregion-worker.service", "wg-quick@wg0.service")
		if hostname != "" {
			units = append(units, "caddy.service")
		}
	}
	if config.Runtime {
		units = append(units, "docker.service")
	}
	return units
}

// GatherFacts observes the node for doctor and status. Commands are read-only;
// no step is applied and no operation is queued.
func GatherFacts(ctx context.Context, cfg Config, cfgErr error, drift Plan, driftErr error, clientVersion, clientCommit string, registry *secrets.Registry) doctor.Facts {
	if registry == nil {
		registry = &secrets.Registry{}
	}
	redact := func(text string) string { return registry.Redact(text) }
	now := time.Now().UTC()
	facts := doctor.Facts{Now: now, Node: cfg.Node, Role: string(cfg.Role), Hostname: cfg.Hostname}
	if facts.Node == "" {
		facts.Node = strings.TrimSpace(os.Getenv("EREGION_NODE_NAME"))
	}
	if facts.Node == "" {
		facts.Node = "local"
	}
	if !cfg.Role.Valid() {
		if state, exists, err := ReadState(Config{Layout: DefaultLayout()}); err == nil && exists {
			facts.Role, facts.Node = string(state.Role), state.Node
		}
	}
	info := buildinfo.Current()
	facts.CLI = clientVersion
	facts.CLIC = clientCommit
	facts.NodeVersion, facts.NodeCommit = info.Version, info.Commit

	if driftErr != nil {
		facts.DriftError = redact(safeError(driftErr))
	} else {
		for _, entry := range drift.Pending() {
			if entry.Error != "" && facts.DriftError == "" {
				facts.DriftError = entry.ID + ": " + entry.Error
				continue
			}
			facts.PendingSteps = append(facts.PendingSteps, entry.ID)
		}
	}

	facts.SSHDDropIn, facts.SSHDDropInKnown = readSSHDDropIn()
	facts.PermitRootLogin, facts.PasswordAuthentication, facts.SSHDKnown = sshdEffective(ctx)
	facts.Firewall = ufwFacts(ctx)
	facts.WireGuardActive, facts.WireGuardKnown = wireGuardActive(ctx)
	facts.CertNotAfter, facts.CertKnown, facts.CertError = certificateExpiry(caddyCertificateRoot, cfg.Hostname)
	facts.DiskTotal, facts.DiskFree, facts.DiskKnown = diskUsage(cfg.Layout.DataDir)
	facts.DiskPath = nearestExisting(cfg.Layout.DataDir)
	facts.UptimeSeconds = uptimeSeconds()
	facts.SchemaCurrent, facts.SchemaRequired, facts.RiverCurrent, facts.RiverRequired, facts.SchemaKnown, facts.SchemaError = schemaVersions(ctx, cfg.DatabaseURL, redact)
	facts.Services = serviceStates(ctx, ServiceUnits(cfg.Role, cfg.Hostname))
	facts.ServicesKnown = len(facts.Services) > 0

	if cfgErr != nil {
		facts.ConfigError = redact(safeError(cfgErr))
	}
	return facts
}

// NodeStatus is `server status`'s node-side observation.
type NodeStatus struct {
	Node          string                     `json:"node"`
	Role          string                     `json:"role"`
	CLI           string                     `json:"cli_version,omitempty"`
	NodeVersion   string                     `json:"node_version"`
	NodeCommit    string                     `json:"node_commit,omitempty"`
	Schema        controlplane.MigrationPlan `json:"schema"`
	SchemaError   string                     `json:"schema_error,omitempty"`
	Services      map[string]bool            `json:"services"`
	UptimeSeconds int64                      `json:"uptime_seconds"`
	Disk          DiskStatus                 `json:"disk"`
}

// DiskStatus is one filesystem's observation.
type DiskStatus struct {
	Path       string  `json:"path"`
	TotalBytes uint64  `json:"total_bytes"`
	FreeBytes  uint64  `json:"free_bytes"`
	FreePct    float64 `json:"free_pct"`
}

// StatusFromFacts turns the shared observation into `server status`.
func StatusFromFacts(facts doctor.Facts) NodeStatus {
	status := NodeStatus{
		Node: facts.Node, Role: facts.Role, CLI: facts.CLI,
		NodeVersion: facts.NodeVersion, NodeCommit: facts.NodeCommit,
		Schema: controlplane.MigrationPlan{
			Current: facts.SchemaCurrent, Required: facts.SchemaRequired,
			RiverCurrent: facts.RiverCurrent, RiverRequired: facts.RiverRequired,
		},
		SchemaError: facts.SchemaError, Services: facts.Services,
		UptimeSeconds: facts.UptimeSeconds,
		Disk: DiskStatus{
			Path: facts.DiskPath, TotalBytes: facts.DiskTotal, FreeBytes: facts.DiskFree,
			FreePct: doctor.FreePct(facts.DiskTotal, facts.DiskFree),
		},
	}
	return status
}

func sshdDropInPath() string { return "/etc/ssh/sshd_config.d/00-eregion.conf" }

func readSSHDDropIn() (string, bool) {
	raw, err := os.ReadFile(sshdDropInPath()) //nolint:gosec // G304: fixed system configuration path.
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(raw)), true
}

func sshdEffective(ctx context.Context) (rootLogin, passwordAuth string, known bool) {
	output, err := readCommand(ctx, "sshd", "-T")
	if err != nil {
		return "", "", false
	}
	values := map[string]string{}
	for _, line := range strings.Split(output, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), " ")
		if ok {
			values[strings.ToLower(key)] = strings.ToLower(strings.TrimSpace(value))
		}
	}
	if len(values) == 0 {
		return "", "", false
	}
	return values["permitrootlogin"], values["passwordauthentication"], true
}

func ufwFacts(ctx context.Context) doctor.FirewallFacts {
	output, err := readCommand(ctx, "ufw", "status", "verbose")
	if err != nil {
		return doctor.FirewallFacts{}
	}
	return ParseUFWStatus(output)
}

// ParseUFWStatus reads `ufw status verbose` output. It is deliberately plain
// text parsing: ufw has no stable machine format across the supported hosts.
func ParseUFWStatus(text string) doctor.FirewallFacts {
	facts := doctor.FirewallFacts{Known: true}
	port := sshPort()
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "Status: active":
			facts.Active = true
		case strings.HasPrefix(line, "Default:") && strings.Contains(line, "deny (incoming)"):
			facts.DefaultDenyIncoming = true
		case strings.Contains(line, port+"/tcp") && strings.Contains(line, "ALLOW IN"):
			// A rule scoped to the WireGuard interface (or another interface)
			// is not a public listener.
			if strings.Contains(line, " on ") {
				continue
			}
			facts.PublicSSH = true
		}
	}
	return facts
}

func sshPort() string {
	port := "22"
	raw, err := os.ReadFile(filepath.Join(access.StateDir, "ssh-port")) //nolint:gosec // G304: node access state.
	if err == nil {
		if value := strings.TrimSpace(string(raw)); value != "" {
			port = value
		}
	}
	return port
}

func wireGuardActive(ctx context.Context) (bool, bool) {
	out, err := readCommand(ctx, "wg", "show", "wg0")
	if err != nil {
		// wg exits nonzero when the interface does not exist, which is a
		// definite "not active" rather than an unobservable state.
		if strings.Contains(err.Error(), "exit status") {
			return false, true
		}
		return false, false
	}
	return strings.TrimSpace(out) != "", true
}

func serviceStates(ctx context.Context, units []string) map[string]bool {
	states := map[string]bool{}
	for _, unit := range units {
		_, err := readCommand(ctx, "systemctl", "is-active", "--quiet", unit)
		states[unit] = err == nil
	}
	return states
}

func schemaVersions(ctx context.Context, dsn string, redact func(string) string) (current, required, riverCurrent, riverRequired int, known bool, detail string) {
	if strings.TrimSpace(dsn) == "" {
		return 0, 0, 0, 0, false, "the node has no control database URL"
	}
	pool, err := controlplane.Open(ctx, dsn)
	if err != nil {
		return 0, 0, 0, 0, false, redact(safeError(err))
	}
	defer pool.Close()
	plan, err := controlplane.PlanMigrations(ctx, pool)
	if err != nil {
		return 0, 0, 0, 0, false, redact(safeError(err))
	}
	return plan.Current, plan.Required, plan.RiverCurrent, plan.RiverRequired, true, ""
}

func certificateExpiry(root, hostname string) (time.Time, bool, string) {
	if strings.TrimSpace(hostname) == "" {
		return time.Time{}, false, ""
	}
	var newest time.Time
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil //nolint:nilerr // An unreadable subtree is not a fatal observation.
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".crt") {
			return nil
		}
		raw, readErr := os.ReadFile(path) //nolint:gosec // G304: fixed Caddy storage root.
		if readErr != nil {
			return nil //nolint:nilerr // Keep observing other certificates.
		}
		for block, rest := pem.Decode(raw); block != nil; block, rest = pem.Decode(rest) {
			if block.Type != "CERTIFICATE" {
				continue
			}
			certificate, parseErr := x509.ParseCertificate(block.Bytes)
			if parseErr != nil {
				continue
			}
			if certificate.VerifyHostname(hostname) == nil && certificate.NotAfter.After(newest) {
				newest = certificate.NotAfter
			}
		}
		return nil
	})
	if err != nil {
		return time.Time{}, false, err.Error()
	}
	if newest.IsZero() {
		return time.Time{}, false, "no Caddy certificate for " + hostname
	}
	return newest, true, ""
}

func diskUsage(path string) (total, free uint64, known bool) {
	path = nearestExisting(path)
	if path == "" {
		return 0, 0, false
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, 0, false
	}
	blockSize := uint64(stat.Bsize) //nolint:gosec // G115: a filesystem block size is positive.
	return stat.Blocks * blockSize, stat.Bavail * blockSize, true
}

func nearestExisting(path string) string {
	for {
		if path == "" {
			return ""
		}
		if _, err := os.Stat(path); err == nil {
			return path
		}
		parent := filepath.Dir(path)
		if parent == path {
			return ""
		}
		path = parent
	}
}

func uptimeSeconds() int64 {
	raw, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(raw))
	if len(fields) == 0 {
		return 0
	}
	seconds, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || seconds < 0 {
		return 0
	}
	return int64(seconds)
}

func readCommand(ctx context.Context, name string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, name, args...) //nolint:gosec // G204: fixed local observation commands.
	output, err := command.CombinedOutput()
	text := strings.TrimSpace(string(output))
	if err != nil {
		return text, fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return text, nil
}
