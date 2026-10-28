// Package doctor holds eregion doctor's checks. The node gathers Facts from
// read-only observations; every check is a pure function of them, so thresholds
// and aggregation are testable without a server.
package doctor

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// Status is a check's outcome.
type Status string

const (
	Pass Status = "pass"
	Warn Status = "warn"
	Fail Status = "fail"
	Skip Status = "skip"
)

// Categories, for --only.
const (
	CatHost     = "host"
	CatSecurity = "security"
	CatTLS      = "tls"
	CatVersions = "versions"
	CatServices = "services"
)

// Categories lists the --only values.
var Categories = []string{CatHost, CatSecurity, CatTLS, CatVersions, CatServices}

// Thresholds.
const (
	DiskWarnFreePct = 20
	DiskFailFreePct = 10
	TLSWarnDays     = 21
	TLSFailDays     = 7
)

// Result is one finding.
type Result struct {
	ID       string `json:"id"`
	Subject  string `json:"subject,omitempty"`
	Status   Status `json:"status"`
	Detail   string `json:"detail"`
	Fix      string `json:"fix,omitempty"`
	Category string `json:"category"`
	Security bool   `json:"security,omitempty"`
}

// Check is one doctor check.
type Check struct {
	ID       string
	Category string
	Security bool
	// AtInit marks a check that can pass during server init. The init gate
	// omits every other check until the host is a finished, operating node.
	AtInit bool
	Run    func(Facts) []Result
}

// FirewallFacts is the observed ufw state.
type FirewallFacts struct {
	Known               bool
	Active              bool
	DefaultDenyIncoming bool
	PublicSSH           bool
}

// Facts is every read-only observation the checks consume.
type Facts struct {
	Now time.Time
	// Init is set by the server init gate. Checks that cannot pass before the
	// first confirmed handoff are left out rather than reported as failures.
	Init        bool
	Node        string
	Role        string
	Hostname    string
	CLI         string
	CLIC        string
	NodeVersion string
	NodeCommit  string
	ConfigError string

	PendingSteps []string
	DriftError   string

	SSHDDropIn             string
	SSHDDropInKnown        bool
	PermitRootLogin        string
	PasswordAuthentication string
	SSHDKnown              bool

	Firewall        FirewallFacts
	WireGuardActive bool
	WireGuardKnown  bool

	CertNotAfter time.Time
	CertKnown    bool
	CertError    string

	DiskTotal     uint64
	DiskFree      uint64
	DiskKnown     bool
	DiskPath      string
	UptimeSeconds int64

	SchemaCurrent  int
	SchemaRequired int
	RiverCurrent   int
	RiverRequired  int
	SchemaKnown    bool
	SchemaError    string

	Services      map[string]bool
	ServicesKnown bool
}

// FreePct is the free share of a filesystem in percent.
func FreePct(total, free uint64) float64 {
	if total == 0 {
		return 0
	}
	return float64(free) * 100 / float64(total)
}

// Checks are the checks the node runs, in report order.
func Checks() []Check {
	return []Check{
		{ID: "host.intent", Category: CatHost, AtInit: true, Run: hostIntent},
		{ID: "drift.pending", Category: CatHost, AtInit: true, Run: driftPending},
		{ID: "host.disk", Category: CatHost, AtInit: true, Run: disk},
		{ID: "ssh.root_login", Category: CatSecurity, Security: true, AtInit: true, Run: sshRootLogin},
		{ID: "ssh.password_auth", Category: CatSecurity, Security: true, AtInit: true, Run: sshPasswordAuth},
		{ID: "ssh.drop_in", Category: CatSecurity, Security: true, AtInit: true, Run: sshDropIn},
		{ID: "ssh.public_closed", Category: CatSecurity, Security: true, AtInit: true, Run: sshPublicClosed},
		{ID: "net.wireguard", Category: CatSecurity, Security: true, AtInit: true, Run: wireGuard},
		{ID: "fw.active", Category: CatSecurity, Security: true, AtInit: true, Run: firewall},
		{ID: "tls.certificate", Category: CatTLS, AtInit: true, Run: certificate},
		{ID: "version.cli_node", Category: CatVersions, AtInit: true, Run: versionCLINode},
		{ID: "version.schema", Category: CatVersions, AtInit: true, Run: versionSchema},
		{ID: "version.river", Category: CatVersions, AtInit: true, Run: versionRiver},
		{ID: "services.running", Category: CatServices, AtInit: true, Run: services},
	}
}

// Filter picks checks for --only (empty is every check).
func Filter(checks []Check, only string) []Check {
	if only == "" {
		return checks
	}
	var out []Check
	for _, check := range checks {
		if check.Category == only || (only == CatSecurity && check.Security) {
			out = append(out, check)
		}
	}
	return out
}

// Run runs checks against facts. With Facts.Init, checks that cannot pass
// during server init are left out.
func Run(checks []Check, facts Facts) []Result {
	var out []Result
	for _, check := range checks {
		if facts.Init && !check.AtInit {
			continue
		}
		for _, result := range check.Run(facts) {
			result.ID, result.Category, result.Security = check.ID, check.Category, check.Security
			out = append(out, result)
		}
	}
	return out
}

// Summary counts results by status.
func Summary(results []Result) map[Status]int {
	out := map[Status]int{Pass: 0, Warn: 0, Fail: 0, Skip: 0}
	for _, result := range results {
		out[result.Status]++
	}
	return out
}

// Report is the structured doctor result.
type Report struct {
	Results []Result       `json:"results"`
	Summary map[Status]int `json:"summary"`
}

// BuildReport computes the summary for results.
func BuildReport(results []Result) Report {
	return Report{Results: results, Summary: Summary(results)}
}

// SummaryLine is the compact success line shown by the init result card.
func SummaryLine(summary map[Status]int) string {
	parts := []string{fmt.Sprintf("%d ok", summary[Pass])}
	if summary[Warn] > 0 {
		parts = append(parts, fmt.Sprintf("%d warn", summary[Warn]))
	}
	if summary[Skip] > 0 {
		parts = append(parts, fmt.Sprintf("%d skip", summary[Skip]))
	}
	return strings.Join(parts, " · ")
}

// Message renders problems first, then the status counts.
func Message(results []Result) string {
	var lines []string
	for _, want := range []Status{Fail, Warn} {
		for _, result := range results {
			if result.Status != want {
				continue
			}
			name := result.ID
			if result.Subject != "" {
				name += " " + result.Subject
			}
			line := fmt.Sprintf("%-4s %-32s %s", strings.ToUpper(string(result.Status)), name, result.Detail)
			if result.Fix != "" {
				line += "\n     fix: " + result.Fix
			}
			lines = append(lines, line)
		}
	}
	summary := Summary(results)
	lines = append(lines, fmt.Sprintf("%d pass · %d warn · %d fail · %d skip", summary[Pass], summary[Warn], summary[Fail], summary[Skip]))
	return strings.Join(lines, "\n")
}

// Failed returns the failing results.
func Failed(results []Result) []Result {
	var out []Result
	for _, result := range results {
		if result.Status == Fail {
			out = append(out, result)
		}
	}
	return out
}

// ValidOnly reports whether only is a known --only value.
func ValidOnly(only string) bool {
	if only == "" {
		return true
	}
	for _, category := range Categories {
		if category == only {
			return true
		}
	}
	return false
}

func one(status Status, detail, fix string) []Result {
	return []Result{{Status: status, Detail: detail, Fix: fix}}
}

func hostIntent(facts Facts) []Result {
	if facts.ConfigError != "" {
		return one(Fail, facts.ConfigError, "run server init again to record the node's desired host state")
	}
	return one(Pass, "the node's host intent is recorded", "")
}

func driftPending(facts Facts) []Result {
	if facts.DriftError != "" {
		return one(Fail, "cannot evaluate host drift: "+facts.DriftError, "run eregion plan, then fix the reported error")
	}
	if len(facts.PendingSteps) > 0 {
		sorted := append([]string(nil), facts.PendingSteps...)
		sort.Strings(sorted)
		return one(Fail, fmt.Sprintf("%d host step(s) pending: %s", len(sorted), strings.Join(sorted, ", ")), "run eregion apply")
	}
	return one(Pass, "no host step is pending", "")
}

func disk(facts Facts) []Result {
	if !facts.DiskKnown {
		return one(Warn, "cannot read the data filesystem", "")
	}
	pct := FreePct(facts.DiskTotal, facts.DiskFree)
	detail := fmt.Sprintf("%.0f%% free on %s", pct, facts.DiskPath)
	switch {
	case pct < DiskFailFreePct:
		return one(Fail, detail+" (fails under 10%)", "free space or grow the volume")
	case pct < DiskWarnFreePct:
		return one(Warn, detail, "free space or grow the volume before it reaches 10%")
	}
	return one(Pass, detail, "")
}

func sshRootLogin(facts Facts) []Result {
	if !facts.SSHDKnown {
		return one(Warn, "cannot read the effective sshd configuration", "run eregion apply")
	}
	if facts.PermitRootLogin == "no" {
		return one(Pass, "root SSH login is off", "")
	}
	return one(Fail, "sshd PermitRootLogin is "+orUnknown(facts.PermitRootLogin), "run eregion apply")
}

func sshPasswordAuth(facts Facts) []Result {
	if !facts.SSHDKnown {
		return one(Warn, "cannot read the effective sshd configuration", "run eregion apply")
	}
	if facts.PasswordAuthentication == "no" {
		return one(Pass, "SSH password authentication is off", "")
	}
	return one(Fail, "sshd PasswordAuthentication is "+orUnknown(facts.PasswordAuthentication), "run eregion apply")
}

func sshDropIn(facts Facts) []Result {
	if !facts.SSHDDropInKnown {
		return one(Fail, "the eregion sshd drop-in is missing", "run eregion apply")
	}
	for _, required := range []string{"PermitRootLogin no", "PasswordAuthentication no"} {
		if !strings.Contains(facts.SSHDDropIn, required) {
			return one(Fail, "the eregion sshd drop-in lacks "+required, "run eregion apply")
		}
	}
	return one(Pass, "the eregion sshd drop-in is installed", "")
}

func sshPublicClosed(facts Facts) []Result {
	if !facts.Firewall.Known {
		return one(Warn, "cannot read the firewall rules", "run eregion apply")
	}
	if facts.Firewall.PublicSSH {
		return one(Fail, "public SSH is allowed in the firewall", "run eregion node unlock-ssh only for break-glass, then re-lock")
	}
	return one(Pass, "public SSH is closed; SSH is reachable over WireGuard", "")
}

func wireGuard(facts Facts) []Result {
	if !facts.WireGuardKnown {
		return one(Warn, "cannot observe the WireGuard interface", "run eregion apply")
	}
	if facts.WireGuardActive {
		return one(Pass, "wg0 is up", "")
	}
	return one(Fail, "wg0 is not up: the operator channel is unavailable", "inspect the provider console, then run eregion apply")
}

func firewall(facts Facts) []Result {
	if !facts.Firewall.Known {
		return one(Warn, "cannot read the firewall state", "run eregion apply")
	}
	if !facts.Firewall.Active {
		return one(Fail, "ufw is not active", "run eregion apply")
	}
	if !facts.Firewall.DefaultDenyIncoming {
		return one(Fail, "ufw does not deny incoming traffic by default", "run eregion apply")
	}
	return one(Pass, "ufw is active and denies incoming traffic by default", "")
}

func certificate(facts Facts) []Result {
	if strings.TrimSpace(facts.Hostname) == "" {
		return one(Skip, "no main hostname is configured", "")
	}
	if !facts.CertKnown {
		detail := "cannot read the Caddy certificate for " + facts.Hostname
		if facts.CertError != "" {
			detail += ": " + facts.CertError
		}
		return one(Warn, detail, "check DNS and the Caddy logs; the certificate may still be pending")
	}
	days := facts.CertNotAfter.Sub(facts.Now).Hours() / 24
	detail := fmt.Sprintf("the certificate expires %s (%.0f days)", facts.CertNotAfter.UTC().Format(time.RFC3339), math.Floor(days))
	switch {
	case days < TLSFailDays:
		return one(Fail, detail, "check DNS and Caddy's ACME renewal")
	case days < TLSWarnDays:
		return one(Warn, detail+"; ACME renewal starts 30 days ahead, so it is failing", "check the Caddy logs")
	}
	return one(Pass, detail, "")
}

func versionCLINode(facts Facts) []Result {
	if facts.NodeVersion == "" {
		return one(Warn, "the node build is unknown", "")
	}
	if facts.CLI == "" {
		return one(Pass, "the node runs eregion "+facts.NodeVersion, "")
	}
	if facts.CLI != facts.NodeVersion || (facts.CLI == "dev" && facts.CLIC != facts.NodeCommit) {
		return one(Fail, fmt.Sprintf("the CLI is %s (%s), the node is %s (%s)", facts.CLI, facts.CLIC, facts.NodeVersion, facts.NodeCommit), "run the same build on both sides, or re-run with --auto-upgrade-nodes")
	}
	return one(Pass, "the CLI and the node run the same build", "")
}

func versionSchema(facts Facts) []Result {
	if !facts.SchemaKnown {
		detail := "cannot read the control database schema"
		if facts.SchemaError != "" {
			detail += ": " + facts.SchemaError
		}
		return one(Fail, detail, "check EREGION_DATABASE_URL on the node, then run eregion apply")
	}
	detail := fmt.Sprintf("schema %d of %d", facts.SchemaCurrent, facts.SchemaRequired)
	if facts.SchemaCurrent != facts.SchemaRequired {
		return one(Fail, detail+"; migrations are pending", "run eregion apply")
	}
	return one(Pass, detail, "")
}

func versionRiver(facts Facts) []Result {
	if !facts.SchemaKnown {
		return one(Warn, "cannot read the River schema", "fix the control database connection first")
	}
	detail := fmt.Sprintf("River schema %d of %d", facts.RiverCurrent, facts.RiverRequired)
	if facts.RiverCurrent != facts.RiverRequired {
		return one(Fail, detail+"; migrations are pending", "run eregion apply")
	}
	return one(Pass, detail, "")
}

func services(facts Facts) []Result {
	if !facts.ServicesKnown {
		return one(Skip, "no services are configured for this role", "")
	}
	units := make([]string, 0, len(facts.Services))
	for unit := range facts.Services {
		units = append(units, unit)
	}
	sort.Strings(units)
	results := make([]Result, 0, len(units))
	for _, unit := range units {
		if facts.Services[unit] {
			results = append(results, Result{Subject: unit, Status: Pass, Detail: "active"})
			continue
		}
		results = append(results, Result{Subject: unit, Status: Fail, Detail: "not active", Fix: "systemctl status " + unit + "; then run eregion apply"})
	}
	return results
}

func orUnknown(value string) string {
	if strings.TrimSpace(value) == "" {
		return "unknown"
	}
	return value
}
