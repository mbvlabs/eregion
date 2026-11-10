package host

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mbvlabs/eregion/internal/access"
	"github.com/mbvlabs/eregion/internal/buildinfo"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/controlplane"
	"github.com/mbvlabs/eregion/internal/doctor"
	"github.com/mbvlabs/eregion/internal/secrets"
	"github.com/mbvlabs/eregion/internal/telemetry"
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
		units = append(units, "eregion-api.service", "eregion-worker.service", "wg-quick@wg0.service", "node-exporter.service")
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
	facts.SchemaCurrent, facts.SchemaRequired, facts.RiverCurrent, facts.RiverRequired, facts.SchemaKnown, facts.SchemaError, facts.SchemaFix = schemaVersions(ctx, cfg.DatabaseURL, redact)
	facts.Services = serviceStates(ctx, ServiceUnits(cfg.Role, cfg.Hostname))
	facts.ServicesKnown = len(facts.Services) > 0
	facts.Telemetry = telemetryFacts(ctx, Role(facts.Role), NodeExporterTextfileDir, NodeExporterMetricsURL())
	facts.Retention, facts.BuildCache = doctor.GatherRetentionFacts(ctx, facts.Role, cfg.DatabaseURL)

	if cfgErr != nil {
		facts.ConfigError = redact(safeError(cfgErr))
	}
	return facts
}

// telemetryFacts gathers the current local exporter and authenticated-endpoint
// health used by doctor. It runs on the node itself and reads the loopback
// exporter plus one unauthenticated endpoint probe; it does not persist
// samples, start a scraper or require the control database.
func telemetryFacts(ctx context.Context, role Role, textfileDir, endpoint string) doctor.TelemetryFacts {
	roleConfig, err := ConfigForRole(role)
	if err != nil || !roleConfig.ControlPlane {
		return doctor.TelemetryFacts{}
	}
	facts := doctor.TelemetryFacts{Known: true, Endpoint: endpoint, EndpointListen: TelemetryListenAddress()}
	info, infoErr := TelemetryEndpointInfo()
	if infoErr != nil {
		facts.EndpointConfigErr = contract.Classify(infoErr).Message
	} else {
		facts.EndpointState = info.State
		facts.EndpointConfigured = info.Credential
		if info.State == telemetry.EndpointStateConfigured && strings.TrimSpace(info.Hostname) != "" {
			probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
			// Probe the public URL through loopback Caddy with the real TLS
			// server name. This observes the route, certificate and 401
			// challenge without claiming that public DNS resolves here.
			endpointProbe := telemetry.ProbeEndpoint(probeCtx, info.DiscoveryURL, publicTelemetryHTTPClient(info.Hostname))
			cancel()
			facts.EndpointAuthKnown = endpointProbe.Reachable
			facts.EndpointAuthStatus = endpointProbe.Status
			facts.EndpointAuthError = endpointProbe.Error
		}
	}
	parent := filepath.Dir(textfileDir)
	facts.Parent = inspectNodeExporterDir(parent).doctorFacts()
	facts.Textfile = inspectNodeExporterDir(textfileDir).doctorFacts()
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	probe, err := telemetry.ProbeLocal(probeCtx, endpoint)
	if err != nil {
		facts.EndpointError = contract.Classify(err).Message
		return facts
	}
	facts.ExporterVersion = probe.ExporterVersion
	facts.Collectors = probe.Collectors
	facts.CollectorsAmbiguous = probe.CollectorsAmbiguous
	facts.TextfileScrapeError = probe.TextfileScrapeError
	facts.TextfileScrapeErrorAmbiguous = probe.TextfileScrapeErrorAmbiguous
	return facts
}

// publicTelemetryHTTPClient probes the public telemetry discovery URL through
// local Caddy. It verifies the certificate presented for the real hostname
// against the system trust store and does not follow redirects.
func publicTelemetryHTTPClient(hostname string) *http.Client {
	return &http.Client{
		Timeout: 5 * time.Second,
		Transport: &http.Transport{
			Proxy: nil,
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				dialer := &net.Dialer{Timeout: 3 * time.Second}
				return dialer.DialContext(ctx, "tcp", "127.0.0.1:443")
			},
			TLSClientConfig: &tls.Config{MinVersion: tls.VersionTLS12, ServerName: hostname},
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
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
	SchemaFix     string                     `json:"schema_fix,omitempty"`
	Services      map[string]bool            `json:"services"`
	UptimeSeconds int64                      `json:"uptime_seconds"`
	Disk          DiskStatus                 `json:"disk"`
	// Workspace is read from PostgreSQL. Configured=false is the explicit
	// legacy/unconfigured state; WorkspaceError means the authoritative read
	// failed and the unconfigured state must not be inferred.
	Workspace      controlplane.WorkspaceIdentity `json:"workspace"`
	WorkspaceError string                         `json:"workspace_error,omitempty"`
}

// ReadWorkspaceStatus returns the authoritative workspace identity for status.
// It never creates a row, so a read on an alpha.6 installation remains
// unconfigured and changes nothing.
func ReadWorkspaceStatus(ctx context.Context, dsn string) (controlplane.WorkspaceIdentity, error) {
	if strings.TrimSpace(dsn) == "" {
		return controlplane.WorkspaceIdentity{}, contract.NewFault(contract.InputRequired,
			"the node has no control database URL", "Set EREGION_DATABASE_URL in the node's protected service environment, then run eregion apply.")
	}
	pool, err := controlplane.Open(ctx, dsn)
	if err != nil {
		return controlplane.WorkspaceIdentity{}, err
	}
	defer pool.Close()
	return controlplane.ReadWorkspace(ctx, pool)
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
		SchemaError: facts.SchemaError, SchemaFix: facts.SchemaFix, Services: facts.Services,
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

func schemaVersions(ctx context.Context, dsn string, redact func(string) string) (current, required, riverCurrent, riverRequired int, known bool, detail, fix string) {
	if strings.TrimSpace(dsn) == "" {
		return 0, 0, 0, 0, false, "the node has no control database URL", "Set EREGION_DATABASE_URL in the node's protected service environment, then run eregion apply."
	}
	pool, err := controlplane.Open(ctx, dsn)
	if err != nil {
		failure := controlplane.ClassifyDatabaseFailure(dsn, err)
		return 0, 0, 0, 0, false, redact(failure.Summary()), redact(failure.Fix)
	}
	defer pool.Close()
	plan, err := controlplane.PlanMigrations(ctx, pool)
	if err != nil {
		failure := controlplane.ClassifyDatabaseFailure(dsn, err)
		return 0, 0, 0, 0, false, redact(failure.Summary()), redact(failure.Fix)
	}
	return plan.Current, plan.Required, plan.RiverCurrent, plan.RiverRequired, true, "", ""
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
