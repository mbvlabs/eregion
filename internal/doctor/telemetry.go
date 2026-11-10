package doctor

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// TelemetryPathFacts is one owned path inspected without following symlinks.
// Mode includes setuid/setgid/sticky bits (for example 2755 when setgid is
// present), so an exact 0755 comparison rejects special modes.
type TelemetryPathFacts struct {
	Path       string
	Exists     bool
	Symlink    bool
	Directory  bool
	OwnerKnown bool
	UID        uint32
	GID        uint32
	Mode       uint32
	Error      string
}

// ValidateTelemetryDirectory is the strict shared criterion for the exporter's
// parent and textfile directories: a real directory, root:root, exactly 0755,
// not a symlink. It is used by doctor and the host step check.
func ValidateTelemetryDirectory(path TelemetryPathFacts) error {
	if path.Error != "" {
		return fmt.Errorf("%s could not be read: %s", path.Path, path.Error)
	}
	if !path.Exists {
		return fmt.Errorf("%s is missing", path.Path)
	}
	if path.Symlink {
		return fmt.Errorf("%s is a symlink; refusing to follow it", path.Path)
	}
	if !path.Directory {
		return fmt.Errorf("%s is not a directory", path.Path)
	}
	if !path.OwnerKnown {
		return fmt.Errorf("%s ownership cannot be verified", path.Path)
	}
	if path.UID != 0 || path.GID != 0 {
		return fmt.Errorf("%s is owned %d:%d; want root:root", path.Path, path.UID, path.GID)
	}
	if path.Mode != 0o755 {
		return fmt.Errorf("%s has mode %04o; want 0755", path.Path, path.Mode)
	}
	return nil
}

// TelemetryFacts is the narrow local telemetry health observation. It never
// carries a metric history: the exporter read is current and discarded.
type TelemetryFacts struct {
	Known bool
	// Parent and Textfile are the two strict owned directories. They are
	// observed independently of the exporter so a configuration error is never
	// hidden by WireGuard or exporter unavailability.
	Parent          TelemetryPathFacts
	Textfile        TelemetryPathFacts
	Endpoint        string
	ExporterVersion string
	Collectors      map[string]bool
	// CollectorsAmbiguous names a collector whose success sample was duplicated,
	// timestamped or malformed; it fails closed and is never treated as success.
	CollectorsAmbiguous map[string]bool
	// TextfileScrapeError is node_textfile_scrape_error. Nil means unobserved;
	// only exactly one zero sample is a healthy textfile collector.
	TextfileScrapeError          *float64
	TextfileScrapeErrorAmbiguous bool
	EndpointError                string

	// Endpoint is the public HTTPS telemetry endpoint, separate from the
	// loopback exporter read above. State describes local configuration only;
	// the auth observation is a separate probe.
	EndpointState      string
	EndpointListen     string
	EndpointConfigured bool
	EndpointConfigErr  string
	EndpointAuthStatus int
	EndpointAuthKnown  bool
	EndpointAuthError  string
}

// telemetryLocal checks that the loopback node_exporter answers and that its
// textfile and narrow systemd collectors are enabled and successful. It is a
// health check, not a metrics scrape or a Prometheus server: no sample is
// stored and no collector is invented.
func telemetryLocal(facts Facts) []Result {
	telemetry := facts.Telemetry
	if !telemetry.Known {
		return one(Skip, "local telemetry health is not configured for this role", "")
	}
	// Directory configuration is observable independently of WireGuard. Surface
	// it first instead of hiding a real config error behind an unavailable
	// exporter; validate both the parent and the textfile child strictly.
	for _, path := range []TelemetryPathFacts{telemetry.Parent, telemetry.Textfile} {
		if err := ValidateTelemetryDirectory(path); err != nil {
			return one(Fail, "node_exporter directory is unsafe or missing: "+err.Error(), "fix ownership/mode to root:root 0755 without symlinks, then run eregion apply")
		}
	}
	if telemetry.EndpointError != "" {
		if !facts.WireGuardKnown {
			return one(Warn, "node_exporter was not observed and WireGuard state is unknown: "+telemetry.EndpointError, "check the loopback exporter, then re-run doctor")
		}
		return one(Fail, "node_exporter does not answer on "+telemetry.Endpoint+": "+telemetry.EndpointError, "check node-exporter.service and retry")
	}
	for _, collector := range []string{"*", "textfile", "systemd"} {
		if telemetry.CollectorsAmbiguous[collector] {
			name := collector
			if name == "*" {
				name = "collector success family"
			}
			return one(Fail, "node_exporter reported an ambiguous "+name+" success sample", "keep exactly one unlabelled, untimestamped node_scrape_collector_success sample per collector; an ambiguous success is not treated as healthy")
		}
	}
	for _, collector := range []string{"textfile", "systemd"} {
		success, present := telemetry.Collectors[collector]
		if !present {
			return one(Fail, "node_exporter does not report the "+collector+" collector", "run eregion apply to apply host-node-exporter-v3, then restart node-exporter.service")
		}
		if !success {
			return one(Fail, "node_exporter collector "+collector+" reported failure", "check node-exporter.service logs, the textfile directory and systemd D-Bus access")
		}
	}
	if telemetry.TextfileScrapeErrorAmbiguous {
		return one(Fail, "node_exporter reported an ambiguous node_textfile_scrape_error sample", "keep one unlabelled, untimestamped node_textfile_scrape_error sample and retry; an ambiguous textfile error is not treated as healthy")
	}
	if telemetry.TextfileScrapeError == nil {
		return one(Fail, "node_exporter did not report node_textfile_scrape_error", "apply host-node-exporter-v3 and restart node-exporter.service; collector success alone does not prove the textfile was readable")
	}
	if *telemetry.TextfileScrapeError != 0 {
		return one(Fail, "node_exporter reported node_textfile_scrape_error="+formatTelemetryValue(*telemetry.TextfileScrapeError), "inspect node_exporter logs and the textfile directory; a non-zero scrape error is a failed textfile read")
	}
	detail := "node_exporter answers with textfile and systemd collectors and node_textfile_scrape_error=0"
	if telemetry.ExporterVersion != "" {
		detail = "node_exporter " + telemetry.ExporterVersion + " answers with textfile and systemd collectors and node_textfile_scrape_error=0"
	}
	detail += "; textfile directory " + telemetry.Textfile.Path + " is root:root 0755"
	if extra := unsupportedCollectors(telemetry.Collectors); extra != "" {
		detail += " (" + extra + " also reported)"
	}
	return one(Pass, detail, "")
}

// telemetryEndpoint checks the public HTTPS authenticated endpoint. It probes
// discovery without a credential: a healthy endpoint answers 401, and a 200 is
// an exposure rather than a pass. A missing public hostname or gateway
// credential is a warning until configuration exists.
func telemetryEndpoint(facts Facts) []Result {
	telemetry := facts.Telemetry
	if !telemetry.Known {
		return one(Skip, "local telemetry is not configured for this role", "")
	}
	if telemetry.EndpointConfigErr != "" {
		return one(Fail, "the telemetry gateway configuration is malformed: "+telemetry.EndpointConfigErr, "repair or replace the managed telemetry gateway configuration, then re-run doctor")
	}
	// An observed unauthenticated 200 is a security failure regardless of the
	// local configuration; do not downgrade it to a missing-credential warning.
	if telemetry.EndpointAuthKnown && telemetry.EndpointAuthStatus == 200 {
		return one(Fail, "the telemetry endpoint served discovery without authentication", "remove the unauthenticated route and run eregion metrics rotate; an unauthenticated telemetry endpoint is never healthy")
	}
	if telemetry.EndpointState == "unconfigured" {
		return one(Warn, "the public telemetry endpoint is unconfigured: the node has no public hostname", "configure the main node's public hostname; there is no WireGuard fallback target")
	}
	if telemetry.EndpointState == "needs_apply" {
		return one(Warn, "the telemetry gateway credential exists but its configuration is not the current loopback render", "run eregion apply to migrate the gateway, then re-run doctor")
	}
	if !telemetry.EndpointConfigured {
		return one(Warn, "the authenticated telemetry endpoint has no credential; external scrapers cannot connect", "run eregion metrics rotate --confirm <node>/metrics")
	}
	if telemetry.EndpointAuthError != "" {
		return one(Fail, "the telemetry endpoint does not answer on "+telemetry.EndpointListen+": "+telemetry.EndpointAuthError, "check public ingress, TLS and eregion-telemetry.service, then run eregion apply")
	}
	if !telemetry.EndpointAuthKnown {
		return one(Fail, "the telemetry endpoint's authentication was not observed", "check public ingress and eregion-telemetry.service, then retry")
	}
	if telemetry.EndpointAuthStatus != 401 {
		return one(Fail, fmt.Sprintf("the telemetry endpoint answered HTTP %d without a credential; want 401", telemetry.EndpointAuthStatus), "check the public telemetry route and run eregion metrics rotate")
	}
	return one(Pass, "the authenticated telemetry endpoint answers 401 without a credential on the public HTTPS path", "")
}

func formatTelemetryValue(value float64) string {
	return strconv.FormatFloat(value, 'g', -1, 64)
}

func unsupportedCollectors(collectors map[string]bool) string {
	var extra []string
	for name := range collectors {
		if name != "textfile" && name != "systemd" {
			extra = append(extra, name)
		}
	}
	if len(extra) == 0 {
		return ""
	}
	sort.Strings(extra)
	return strings.Join(extra, ", ")
}
