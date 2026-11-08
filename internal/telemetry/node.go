// Package telemetry reads one bounded, current observation from the node's
// local node_exporter. It is deliberately not a telemetry server, metric store
// or history: a snapshot exists for the operator command that requested it and
// is then discarded. Durable operation history remains in PostgreSQL and never
// depends on this package.
package telemetry

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/mbvlabs/eregion/internal/contract"
)

// NodeMetricsSchema identifies the node-metrics result shape.
const NodeMetricsSchema = "eregion/node-metrics/v1"

const (
	// DefaultNodeSampleInterval is the bounded gap between the two counter
	// reads used for CPU utilization.
	DefaultNodeSampleInterval = time.Second
	// DefaultNodeStaleAfter is how old an exporter-reported timestamp may be
	// before the snapshot is called stale.
	DefaultNodeStaleAfter = 30 * time.Second
	// DefaultNodeFutureSkew is how far the exporter's timestamp may lead the
	// local collection clock before freshness is unknown. A materially future
	// timestamp is not clamped to a fresh zero-age sample.
	DefaultNodeFutureSkew = 30 * time.Second
	// DefaultNodeMaxBody bounds one node_exporter text response.
	DefaultNodeMaxBody int64 = 4 << 20
)

// linuxCPUModes is the full mode set node_exporter v1.11.1 emits for Linux
// node_cpu_seconds_total. Guest time is exported separately as
// node_cpu_guest_seconds_total (modes user/nice), so it is not part of this
// expected set; legacy guest/guest_nice series are tolerated but excluded from
// the utilization denominator to avoid double counting.
var linuxCPUModes = []string{
	"user", "nice", "system", "idle", "iowait", "irq", "softirq", "steal",
}

// Freshness states for one snapshot.
const (
	FreshnessFresh   = "fresh"
	FreshnessStale   = "stale"
	FreshnessUnknown = "unknown"
)

// ScrapeInfo describes where the current observation came from. The exporter is
// reachable only on the node's WireGuard address and is read by the operator's
// node-RPC command; there is no public endpoint or independent scraper account.
type ScrapeInfo struct {
	Endpoint       string `json:"endpoint"`
	Listen         string `json:"listen"`
	Access         string `json:"access"`
	Public         bool   `json:"public"`
	Authentication string `json:"authentication"`
	Collection     string `json:"collection"`
	Note           string `json:"note"`
}

// Freshness is the age of the exporter's own clock sample.
type Freshness struct {
	State            string   `json:"state"`
	AgeSeconds       *float64 `json:"age_seconds,omitempty"`
	ThresholdSeconds float64  `json:"threshold_seconds"`
}

// Issue names one observation that could not be produced. A nil value with an
// issue is never rendered as zero.
type Issue struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

// CPUStats is the utilization over the two bounded counter reads.
// UtilizationPercent is nil when a truthful percentage cannot be computed; the
// snapshot's Issues then name the reason.
type CPUStats struct {
	UtilizationPercent *float64 `json:"utilization_percent"`
	LogicalCPUs        int      `json:"logical_cpus"`
	WindowSeconds      float64  `json:"window_seconds"`
	TotalSeconds       float64  `json:"total_seconds"`
	IdleSeconds        float64  `json:"idle_seconds"`
}

// MemoryStats is the node's memory from node_exporter.
type MemoryStats struct {
	TotalBytes     uint64  `json:"total_bytes"`
	AvailableBytes uint64  `json:"available_bytes"`
	UsedBytes      uint64  `json:"used_bytes"`
	UsedPercent    float64 `json:"used_percent"`
}

// FilesystemStats is one real mounted filesystem.
type FilesystemStats struct {
	Device         string  `json:"device"`
	Filesystem     string  `json:"filesystem"`
	Mountpoint     string  `json:"mountpoint"`
	TotalBytes     uint64  `json:"total_bytes"`
	AvailableBytes uint64  `json:"available_bytes"`
	UsedBytes      uint64  `json:"used_bytes"`
	UsedPercent    float64 `json:"used_percent"`
}

// LoadStats is the kernel load average.
type LoadStats struct {
	Load1  float64 `json:"load1"`
	Load5  float64 `json:"load5"`
	Load15 float64 `json:"load15"`
}

// NodeSnapshot is one current per-node observation. Missing observations are
// null/empty and named in Issues; they are never fabricated as zero or healthy.
type NodeSnapshot struct {
	Schema          string            `json:"schema"`
	Node            string            `json:"node"`
	Hostname        string            `json:"hostname,omitempty"`
	OS              string            `json:"os,omitempty"`
	Kernel          string            `json:"kernel,omitempty"`
	Machine         string            `json:"machine,omitempty"`
	ExporterVersion string            `json:"exporter_version,omitempty"`
	FetchedAt       time.Time         `json:"fetched_at"`
	ObservedAt      *time.Time        `json:"observed_at,omitempty"`
	Freshness       Freshness         `json:"freshness"`
	Scrape          ScrapeInfo        `json:"scrape"`
	CPU             *CPUStats         `json:"cpu"`
	Memory          *MemoryStats      `json:"memory"`
	Filesystems     []FilesystemStats `json:"filesystems"`
	Load            *LoadStats        `json:"load"`
	UptimeSeconds   *float64          `json:"uptime_seconds,omitempty"`
	Issues          []Issue           `json:"issues,omitempty"`
}

// Validate refuses a snapshot that is not safe to present as a node
// observation. It does not turn an unavailable field into a value.
func (s NodeSnapshot) Validate() error {
	if s.Schema != NodeMetricsSchema {
		return fmt.Errorf("unexpected node-metrics schema %q", s.Schema)
	}
	if strings.TrimSpace(s.Node) == "" {
		return fmt.Errorf("node identity is missing")
	}
	if s.FetchedAt.IsZero() {
		return fmt.Errorf("collection time is missing")
	}
	if strings.TrimSpace(s.Scrape.Endpoint) == "" || strings.TrimSpace(s.Scrape.Access) == "" {
		return fmt.Errorf("scrape details are missing")
	}
	if s.Scrape.Public {
		return fmt.Errorf("node_exporter scrape is marked public")
	}
	switch s.Freshness.State {
	case FreshnessFresh, FreshnessStale, FreshnessUnknown:
	default:
		return fmt.Errorf("unexpected freshness state %q", s.Freshness.State)
	}
	if s.Freshness.AgeSeconds != nil && (*s.Freshness.AgeSeconds < 0 || math.IsNaN(*s.Freshness.AgeSeconds)) {
		return fmt.Errorf("invalid freshness age")
	}
	if s.CPU != nil && s.CPU.UtilizationPercent != nil {
		if math.IsNaN(*s.CPU.UtilizationPercent) || *s.CPU.UtilizationPercent < 0 || *s.CPU.UtilizationPercent > 100 {
			return fmt.Errorf("invalid CPU utilization")
		}
	}
	if s.Memory != nil && (s.Memory.UsedPercent < 0 || s.Memory.UsedPercent > 100) {
		return fmt.Errorf("invalid memory utilization")
	}
	for _, filesystem := range s.Filesystems {
		if filesystem.Mountpoint == "" || filesystem.TotalBytes == 0 {
			return fmt.Errorf("invalid filesystem observation")
		}
	}
	return nil
}

// NodeExporterConfig configures one bounded local collection.
type NodeExporterConfig struct {
	Endpoint       string
	Scrape         ScrapeInfo
	Client         *http.Client
	Now            func() time.Time
	SampleInterval time.Duration
	StaleAfter     time.Duration
	MaxBody        int64
}

// CollectNodeExporter reads the node's local node_exporter twice with a bounded
// gap, computes CPU utilization from the counter delta and returns one snapshot.
// It never stores the samples or contacts an external backend.
func CollectNodeExporter(ctx context.Context, cfg NodeExporterConfig) (NodeSnapshot, error) {
	endpoint, err := validateNodeExporterURL(cfg.Endpoint)
	if err != nil {
		return NodeSnapshot{}, err
	}
	interval := cfg.SampleInterval
	if interval == 0 {
		interval = DefaultNodeSampleInterval
	}
	if interval < 100*time.Millisecond || interval > 30*time.Second {
		return NodeSnapshot{}, contract.NewFault(contract.ValidationFailed, "invalid node metrics sample interval", "Use 100 milliseconds–30 seconds.")
	}
	maxBody := cfg.MaxBody
	if maxBody == 0 {
		maxBody = DefaultNodeMaxBody
	}
	if maxBody < 4096 || maxBody > 32<<20 {
		return NodeSnapshot{}, contract.NewFault(contract.ValidationFailed, "invalid node metrics response bound", "Use 4 KiB–32 MiB.")
	}
	staleAfter := cfg.StaleAfter
	if staleAfter == 0 {
		staleAfter = DefaultNodeStaleAfter
	}
	if staleAfter < time.Second || staleAfter > 24*time.Hour {
		return NodeSnapshot{}, contract.NewFault(contract.ValidationFailed, "invalid node metrics freshness threshold", "Use 1 second–24 hours.")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	client := cfg.Client
	if client == nil {
		client = defaultNodeExporterClient(5 * time.Second)
	}

	firstAt := now().UTC()
	first, err := fetchNodeExporter(ctx, client, endpoint, maxBody)
	if err != nil {
		return NodeSnapshot{}, err
	}
	timer := time.NewTimer(interval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return NodeSnapshot{}, ctx.Err()
	case <-timer.C:
	}
	second, err := fetchNodeExporter(ctx, client, endpoint, maxBody)
	if err != nil {
		return NodeSnapshot{}, err
	}
	// Completion time is recorded only after the second response is fully read.
	secondAt := now().UTC()
	snapshot, err := ParseNodeExporter(first, second, firstAt, secondAt)
	if err != nil {
		return NodeSnapshot{}, err
	}
	snapshot.Scrape = cfg.Scrape
	snapshot.Freshness.ThresholdSeconds = staleAfter.Seconds()
	if snapshot.ObservedAt != nil {
		age := secondAt.Sub(*snapshot.ObservedAt)
		if age < 0 {
			age = 0
		}
		seconds := age.Seconds()
		snapshot.Freshness.AgeSeconds = &seconds
		state := FreshnessFresh
		if age > staleAfter {
			state = FreshnessStale
		}
		snapshot.Freshness.State = state
	}
	return snapshot, nil
}

func defaultNodeExporterClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			// The endpoint is a local WireGuard address. An ambient HTTP proxy
			// must never receive this read.
			Proxy: nil,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func validateNodeExporterURL(value string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme != "http" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Path != "/metrics" {
		return "", contract.NewFault(contract.ValidationFailed, "invalid node_exporter endpoint", "Use the node's WireGuard HTTP endpoint ending in /metrics.")
	}
	return parsed.String(), nil
}

func fetchNodeExporter(ctx context.Context, client *http.Client, endpoint string, maxBody int64) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", contract.NewFault(contract.Internal, "could not construct the node_exporter read", "Check the node's exporter endpoint.")
	}
	request.Header.Set("Accept", "text/plain")
	response, err := client.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", contract.NewFault(contract.Unavailable, "node_exporter is unreachable on the WireGuard address", "Check node-exporter.service and that wg0 is up; the exporter is not public.")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return "", contract.NewFault(contract.Unavailable, "node_exporter returned an error", fmt.Sprintf("node_exporter answered HTTP %d; check node-exporter.service.", response.StatusCode))
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxBody+1))
	if err != nil {
		return "", contract.NewFault(contract.Unavailable, "node_exporter response could not be read", "Check node-exporter.service and retry.")
	}
	if int64(len(raw)) > maxBody {
		return "", contract.NewFault(contract.Unavailable, "node_exporter response exceeded its bound", "The response was larger than the documented limit; do not render it as a healthy observation.")
	}
	return string(raw), nil
}

// ParseNodeExporter turns two bounded text responses into one snapshot. firstAt
// and secondAt are the local collection times; the freshness timestamp is the
// second read's own node_time_seconds only. A missing second timestamp stays
// unknown instead of borrowing the first read's clock.
func ParseNodeExporter(first, second string, firstAt, secondAt time.Time) (NodeSnapshot, error) {
	firstScrape, err := parseNodeExporterScrape(first)
	if err != nil {
		return NodeSnapshot{}, err
	}
	secondScrape, err := parseNodeExporterScrape(second)
	if err != nil {
		return NodeSnapshot{}, err
	}
	snapshot := NodeSnapshot{Schema: NodeMetricsSchema, FetchedAt: secondAt.UTC(), Freshness: Freshness{State: FreshnessUnknown}, Filesystems: []FilesystemStats{}}
	identity := secondScrape.identity
	if identity.nodename == "" {
		identity = firstScrape.identity
	}
	snapshot.Hostname = identity.nodename
	snapshot.Node = identity.nodename
	snapshot.OS = identity.sysname
	snapshot.Kernel = identity.release
	snapshot.Machine = identity.machine
	snapshot.ExporterVersion = secondScrape.exporterVersion
	if snapshot.ExporterVersion == "" {
		snapshot.ExporterVersion = firstScrape.exporterVersion
	}

	observed := secondScrape.nodeTime
	snapshot.Freshness.ThresholdSeconds = DefaultNodeStaleAfter.Seconds()
	if !observed.IsZero() {
		observed = observed.UTC()
		age := secondAt.Sub(observed)
		if age < -DefaultNodeFutureSkew {
			// A materially future exporter clock is not evidence that the
			// sample is fresh now. Do not expose a future observed_at or let
			// CollectNodeExporter recompute a clamped zero age.
			snapshot.Issues = append(snapshot.Issues, Issue{
				Field: "timestamp", Reason: "node_time_seconds is materially in the future; freshness is unknown",
			})
		} else {
			snapshot.ObservedAt = &observed
			if age < 0 {
				age = 0
			}
			seconds := age.Seconds()
			state := FreshnessFresh
			if age > DefaultNodeStaleAfter {
				state = FreshnessStale
			}
			snapshot.Freshness = Freshness{State: state, AgeSeconds: &seconds, ThresholdSeconds: DefaultNodeStaleAfter.Seconds()}
		}
	} else {
		snapshot.Issues = append(snapshot.Issues, Issue{Field: "timestamp", Reason: "node_time_seconds is missing; freshness is unknown"})
	}

	if identity.nodename == "" {
		snapshot.Node = "local"
		snapshot.Issues = append(snapshot.Issues, Issue{Field: "identity", Reason: "node_uname_info is missing; the host identity is unavailable"})
	}
	if snapshot.ExporterVersion == "" {
		snapshot.Issues = append(snapshot.Issues, Issue{Field: "exporter_version", Reason: "node_exporter_build_info is missing; the exporter version is unknown"})
	}

	snapshot.CPU, snapshot.Memory, snapshot.Load, snapshot.UptimeSeconds = secondScrape.values(firstScrape, firstAt, secondAt, &snapshot.Issues)
	snapshot.Filesystems = secondScrape.filesystems(&snapshot.Issues)
	return snapshot, nil
}

type nodeExporterScrape struct {
	identity        unameInfo
	exporterVersion string
	nodeTime        time.Time
	bootTime        time.Time
	cpu             map[cpuMode]float64
	memoryTotal     *float64
	memoryAvailable *float64
	load1           *float64
	load5           *float64
	load15          *float64
	filesystemSize  map[filesystemKey]float64
	filesystemAvail map[filesystemKey]float64
	found           bool
}

type unameInfo struct {
	sysname  string
	nodename string
	release  string
	machine  string
}

type cpuMode struct {
	cpu  string
	mode string
}

type filesystemKey struct {
	device     string
	fstype     string
	mountpoint string
}

func parseNodeExporterScrape(text string) (nodeExporterScrape, error) {
	scrape := nodeExporterScrape{
		cpu:             map[cpuMode]float64{},
		filesystemSize:  map[filesystemKey]float64{},
		filesystemAvail: map[filesystemKey]float64{},
	}
	seenOnce := map[string]bool{}
	once := func(name string) bool {
		if seenOnce[name] {
			return false
		}
		seenOnce[name] = true
		return true
	}
	scanner := bufio.NewScanner(strings.NewReader(text))
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, labels, value, timestamped, ok := parsePrometheusLine(line)
		if !ok {
			if metric := nodeExporterMetricName(line); relevantNodeExporterMetric(metric) {
				return nodeExporterScrape{}, malformedNodeExporterSample(metric)
			}
			continue
		}
		if timestamped && relevantNodeExporterMetric(name) {
			return nodeExporterScrape{}, timestampedNodeExporterSample(name)
		}
		switch name {
		case "node_cpu_seconds_total":
			cpu := sanitizeNodeExporterLabel(labels["cpu"])
			mode := sanitizeNodeExporterLabel(labels["mode"])
			if cpu == "" || mode == "" || value < 0 {
				return nodeExporterScrape{}, malformedNodeExporterSample(name)
			}
			key := cpuMode{cpu: cpu, mode: mode}
			if _, exists := scrape.cpu[key]; exists {
				return nodeExporterScrape{}, duplicateNodeExporterSeries(name)
			}
			scrape.cpu[key] = value
		case "node_memory_MemTotal_bytes":
			if !once(name) {
				return nodeExporterScrape{}, duplicateNodeExporterSeries(name)
			}
			scrape.memoryTotal = floatPointer(value)
		case "node_memory_MemAvailable_bytes":
			if !once(name) {
				return nodeExporterScrape{}, duplicateNodeExporterSeries(name)
			}
			scrape.memoryAvailable = floatPointer(value)
		case "node_load1":
			if !once(name) {
				return nodeExporterScrape{}, duplicateNodeExporterSeries(name)
			}
			scrape.load1 = floatPointer(value)
		case "node_load5":
			if !once(name) {
				return nodeExporterScrape{}, duplicateNodeExporterSeries(name)
			}
			scrape.load5 = floatPointer(value)
		case "node_load15":
			if !once(name) {
				return nodeExporterScrape{}, duplicateNodeExporterSeries(name)
			}
			scrape.load15 = floatPointer(value)
		case "node_time_seconds":
			if value <= 0 {
				return nodeExporterScrape{}, malformedNodeExporterSample(name)
			}
			if !once(name) {
				return nodeExporterScrape{}, duplicateNodeExporterSeries(name)
			}
			scrape.nodeTime = unixFloat(value)
		case "node_boot_time_seconds":
			if value <= 0 {
				return nodeExporterScrape{}, malformedNodeExporterSample(name)
			}
			if !once(name) {
				return nodeExporterScrape{}, duplicateNodeExporterSeries(name)
			}
			scrape.bootTime = unixFloat(value)
		case "node_filesystem_size_bytes":
			key, usable := filesystemKeyFor(labels)
			if !usable {
				return nodeExporterScrape{}, malformedNodeExporterSample(name)
			}
			if skipFilesystem(key) {
				continue
			}
			if _, exists := scrape.filesystemSize[key]; exists {
				return nodeExporterScrape{}, duplicateNodeExporterSeries(name)
			}
			scrape.filesystemSize[key] = value
		case "node_filesystem_avail_bytes":
			key, usable := filesystemKeyFor(labels)
			if !usable {
				return nodeExporterScrape{}, malformedNodeExporterSample(name)
			}
			if skipFilesystem(key) {
				continue
			}
			if _, exists := scrape.filesystemAvail[key]; exists {
				return nodeExporterScrape{}, duplicateNodeExporterSeries(name)
			}
			scrape.filesystemAvail[key] = value
		case "node_uname_info":
			if !once(name) {
				return nodeExporterScrape{}, duplicateNodeExporterSeries(name)
			}
			scrape.identity = unameInfo{
				sysname: sanitizeNodeExporterLabel(labels["sysname"]), nodename: sanitizeNodeExporterLabel(labels["nodename"]),
				release: sanitizeNodeExporterLabel(labels["release"]), machine: sanitizeNodeExporterLabel(labels["machine"]),
			}
		case "node_exporter_build_info":
			if !once(name) {
				return nodeExporterScrape{}, duplicateNodeExporterSeries(name)
			}
			scrape.exporterVersion = sanitizeNodeExporterLabel(labels["version"])
		default:
			continue
		}
		scrape.found = true
	}
	if err := scanner.Err(); err != nil {
		return nodeExporterScrape{}, contract.NewFault(contract.Unavailable, "node_exporter response is not readable", "The response contained an overlong or unreadable line; retry and check the exporter.")
	}
	if !scrape.found {
		return nodeExporterScrape{}, contract.NewFault(contract.Unavailable, "node_exporter returned no recognized host metrics", "Check that the configured endpoint is node_exporter, not another HTTP service.")
	}
	return scrape, nil
}

func floatPointer(value float64) *float64 { return &value }

func nodeExporterMetricName(line string) string {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return ""
	}
	if brace := strings.IndexByte(line, '{'); brace >= 0 {
		return strings.TrimSpace(line[:brace])
	}
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return ""
	}
	return fields[0]
}

func relevantNodeExporterMetric(name string) bool {
	switch name {
	case "node_cpu_seconds_total",
		"node_memory_MemTotal_bytes", "node_memory_MemAvailable_bytes",
		"node_load1", "node_load5", "node_load15",
		"node_time_seconds", "node_boot_time_seconds",
		"node_filesystem_size_bytes", "node_filesystem_avail_bytes",
		"node_uname_info", "node_exporter_build_info":
		return true
	}
	return false
}

func malformedNodeExporterSample(name string) error {
	return contract.NewFault(contract.Unavailable,
		"node_exporter returned a malformed sample for "+name,
		"Check the exporter version and retry; malformed relevant telemetry is reported, never silently treated as missing or healthy.")
}

func timestampedNodeExporterSample(name string) error {
	return contract.NewFault(contract.Unavailable,
		"node_exporter returned a timestamped sample for "+name,
		"Timestamped samples are not accepted because this snapshot does not support their timestamp semantics; remove the timestamp from the exporter response.")
}

func duplicateNodeExporterSeries(name string) error {
	return contract.NewFault(contract.Unavailable,
		"node_exporter returned a duplicate consumed series for "+name,
		"Keep one series per CPU/mode, filesystem and singleton metric; a duplicate is rejected instead of overwriting an earlier sample.")
}

func unixFloat(value float64) time.Time {
	if value <= 0 || math.IsNaN(value) || math.IsInf(value, 0) {
		return time.Time{}
	}
	seconds := int64(value)
	nanos := int64(math.Round((value - math.Floor(value)) * 1e9))
	return time.Unix(seconds, nanos).UTC()
}

func filesystemKeyFor(labels map[string]string) (filesystemKey, bool) {
	key := filesystemKey{
		device:     sanitizeNodeExporterLabel(labels["device"]),
		fstype:     sanitizeNodeExporterLabel(labels["fstype"]),
		mountpoint: sanitizeNodeExporterLabel(labels["mountpoint"]),
	}
	if key.mountpoint == "" || key.fstype == "" {
		return filesystemKey{}, false
	}
	return key, true
}

func sanitizeNodeExporterLabel(value string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			return ' '
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r < 0xa0):
			return -1
		case unicode.Is(unicode.Cf, r):
			return -1
		default:
			return r
		}
	}, value)
}

func skipFilesystem(key filesystemKey) bool {
	switch key.fstype {
	case "tmpfs", "devtmpfs", "squashfs", "overlay", "ramfs", "proc", "sysfs", "cgroup", "cgroup2", "nsfs", "autofs", "iso9660", "udf":
		return true
	}
	for _, prefix := range []string{"/run", "/snap", "/var/lib/docker", "/proc", "/sys", "/dev"} {
		if key.mountpoint == prefix || strings.HasPrefix(key.mountpoint, prefix+"/") {
			return true
		}
	}
	return false
}

func (s nodeExporterScrape) values(first nodeExporterScrape, firstAt, secondAt time.Time, issues *[]Issue) (*CPUStats, *MemoryStats, *LoadStats, *float64) {
	cpu := s.cpuStats(first, firstAt, secondAt, issues)
	memory := s.memoryStats(issues)
	load := s.loadStats(issues)
	uptime := s.uptime(issues)
	return cpu, memory, load, uptime
}

func (s nodeExporterScrape) cpuStats(first nodeExporterScrape, firstAt, secondAt time.Time, issues *[]Issue) *CPUStats {
	if len(first.cpu) == 0 || len(s.cpu) == 0 {
		addIssue(issues, "cpu", "node_cpu_seconds_total is missing; CPU utilization is unavailable")
		return nil
	}
	var totalDelta, idleDelta float64
	seenCPUs := map[string]bool{}
	for key := range first.cpu {
		if _, ok := s.cpu[key]; !ok {
			addIssue(issues, "cpu", "the CPU counter set changed between samples; CPU utilization is unavailable")
			return nil
		}
	}
	for key := range s.cpu {
		seenCPUs[key.cpu] = true
		if _, ok := first.cpu[key]; !ok {
			addIssue(issues, "cpu", "the CPU counter set changed between samples; CPU utilization is unavailable")
			return nil
		}
	}
	// Every observed CPU must have the full Linux counter mode set in both
	// reads. A partial family (for example idle-only) would otherwise look
	// numerical while silently fabricating a zero-utilization sample.
	for cpu := range seenCPUs {
		for _, mode := range linuxCPUModes {
			key := cpuMode{cpu: cpu, mode: mode}
			if _, ok := first.cpu[key]; !ok {
				addIssue(issues, "cpu", "node_cpu_seconds_total has no "+mode+"-mode series for cpu "+cpu+"; CPU utilization is unavailable")
				return nil
			}
			if _, ok := s.cpu[key]; !ok {
				addIssue(issues, "cpu", "node_cpu_seconds_total has no "+mode+"-mode series for cpu "+cpu+"; CPU utilization is unavailable")
				return nil
			}
		}
	}
	for key, secondValue := range s.cpu {
		firstValue := first.cpu[key]
		if secondValue < firstValue {
			addIssue(issues, "cpu", "a CPU counter reset between samples; CPU utilization is unavailable")
			return nil
		}
		// Linux counts guest time inside user/nice, so summing guest and
		// guest_nice would double-count it. Every other mode contributes to
		// the CPU-seconds denominator.
		if key.mode != "guest" && key.mode != "guest_nice" {
			totalDelta += secondValue - firstValue
		}
		if key.mode == "idle" {
			idleDelta += secondValue - firstValue
		}
	}
	if idleDelta < 0 {
		addIssue(issues, "cpu", "node_cpu_seconds_total has no usable idle-mode delta; CPU utilization is unavailable")
		return nil
	}
	if totalDelta <= 0 {
		addIssue(issues, "cpu", "the CPU counters did not advance between samples; CPU utilization is unavailable")
		return nil
	}
	utilization := (1 - idleDelta/totalDelta) * 100
	if utilization < -0.5 || utilization > 100.5 {
		addIssue(issues, "cpu", "the CPU counters moved inconsistently between samples; CPU utilization is unavailable")
		return nil
	}
	utilization = math.Max(0, math.Min(100, utilization))
	window := secondAt.Sub(firstAt).Seconds()
	if !first.nodeTime.IsZero() && !s.nodeTime.IsZero() && s.nodeTime.After(first.nodeTime) {
		window = s.nodeTime.Sub(first.nodeTime).Seconds()
	}
	return &CPUStats{
		UtilizationPercent: &utilization, LogicalCPUs: len(seenCPUs),
		WindowSeconds: window, TotalSeconds: totalDelta, IdleSeconds: idleDelta,
	}
}

func (s nodeExporterScrape) memoryStats(issues *[]Issue) *MemoryStats {
	if s.memoryTotal == nil || s.memoryAvailable == nil {
		addIssue(issues, "memory", "node_memory_MemTotal_bytes or node_memory_MemAvailable_bytes is missing; memory is unavailable")
		return nil
	}
	total, available := *s.memoryTotal, *s.memoryAvailable
	if total <= 0 || available < 0 || available > total {
		addIssue(issues, "memory", "the memory counters are inconsistent; memory is unavailable")
		return nil
	}
	used := total - available
	return &MemoryStats{
		TotalBytes: uint64(total), AvailableBytes: uint64(available), UsedBytes: uint64(used),
		UsedPercent: used * 100 / total,
	}
}

func (s nodeExporterScrape) loadStats(issues *[]Issue) *LoadStats {
	if s.load1 == nil || s.load5 == nil || s.load15 == nil {
		addIssue(issues, "load", "node_load1, node_load5 or node_load15 is missing; load is unavailable")
		return nil
	}
	if *s.load1 < 0 || *s.load5 < 0 || *s.load15 < 0 {
		addIssue(issues, "load", "a load average is negative; load is unavailable")
		return nil
	}
	return &LoadStats{Load1: *s.load1, Load5: *s.load5, Load15: *s.load15}
}

func (s nodeExporterScrape) uptime(issues *[]Issue) *float64 {
	if s.nodeTime.IsZero() || s.bootTime.IsZero() {
		addIssue(issues, "uptime", "node_time_seconds or node_boot_time_seconds is missing; uptime is unavailable")
		return nil
	}
	seconds := s.nodeTime.Sub(s.bootTime).Seconds()
	if seconds < 0 {
		addIssue(issues, "uptime", "the boot time is after the exporter time; uptime is unavailable")
		return nil
	}
	return &seconds
}

func (s nodeExporterScrape) filesystems(issues *[]Issue) []FilesystemStats {
	result := make([]FilesystemStats, 0, len(s.filesystemSize))
	seen := map[string]bool{}
	for key, total := range s.filesystemSize {
		available, ok := s.filesystemAvail[key]
		if !ok {
			addIssue(issues, "filesystem", key.mountpoint+" has no node_filesystem_avail_bytes; it is omitted")
			continue
		}
		if total <= 0 || available < 0 || available > total {
			addIssue(issues, "filesystem", key.mountpoint+" has inconsistent size counters; it is omitted")
			continue
		}
		if seen[key.mountpoint] {
			continue
		}
		seen[key.mountpoint] = true
		used := total - available
		result = append(result, FilesystemStats{
			Device: key.device, Filesystem: key.fstype, Mountpoint: key.mountpoint,
			TotalBytes: uint64(total), AvailableBytes: uint64(available), UsedBytes: uint64(used),
			UsedPercent: used * 100 / total,
		})
	}
	if len(result) == 0 {
		addIssue(issues, "filesystem", "no filesystem size/availability pair was found; filesystems are unavailable")
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Mountpoint == result[j].Mountpoint {
			return result[i].Device < result[j].Device
		}
		return result[i].Mountpoint < result[j].Mountpoint
	})
	if len(result) > 64 {
		addIssue(issues, "filesystem", "more than 64 filesystems were reported; only the first 64 are shown")
		result = result[:64]
	}
	return result
}

func addIssue(issues *[]Issue, field, reason string) {
	for _, existing := range *issues {
		if existing.Field == field && existing.Reason == reason {
			return
		}
	}
	*issues = append(*issues, Issue{Field: field, Reason: reason})
}

// splitPrometheusSample separates the metric name/label set from the sample
// value. Whitespace inside a label set is valid, so the first whitespace is not
// necessarily the separator; braces must be balanced and any quote escaping
// inside label values is respected.
func splitPrometheusSample(line string) (string, string, bool) {
	line = strings.TrimSpace(line)
	if line == "" {
		return "", "", false
	}
	braceOpen, braceClosed, quoted, escaped := false, false, false, false
	headEnd := -1
	for index := 0; index < len(line); index++ {
		ch := line[index]
		if quoted {
			switch {
			case escaped:
				escaped = false
			case ch == '\\':
				escaped = true
			case ch == '"':
				quoted = false
			}
			continue
		}
		switch ch {
		case '"':
			quoted = true
		case '{':
			if braceOpen || braceClosed {
				return "", "", false
			}
			braceOpen = true
		case '}':
			if !braceOpen {
				return "", "", false
			}
			braceOpen = false
			braceClosed = true
			headEnd = index + 1
			if headEnd >= len(line) || (line[headEnd] != ' ' && line[headEnd] != '\t') {
				return "", "", false
			}
		case ' ', '\t':
			if !braceOpen && !braceClosed {
				headEnd = index
			}
		}
		if headEnd >= 0 {
			break
		}
	}
	if quoted || braceOpen || headEnd <= 0 {
		return "", "", false
	}
	head := strings.TrimSpace(line[:headEnd])
	rest := strings.TrimSpace(line[headEnd:])
	if head == "" || rest == "" {
		return "", "", false
	}
	return head, rest, true
}

// parsePrometheusLine parses one bounded Prometheus text sample. Metric names
// and label values come from the node's own exporter; this is still treated as
// untrusted input. Unknown or malformed lines are skipped by the caller unless
// they name a consumed family.
func parsePrometheusLine(line string) (string, map[string]string, float64, bool, bool) {
	head, rest, ok := splitPrometheusSample(line)
	if !ok {
		return "", nil, 0, false, false
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 || len(fields) > 2 {
		return "", nil, 0, false, false
	}
	value, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
		return "", nil, 0, false, false
	}
	timestamped := len(fields) == 2
	if timestamped {
		if _, err := strconv.ParseInt(fields[1], 10, 64); err != nil {
			return "", nil, 0, false, false
		}
	}
	name := head
	labels := map[string]string{}
	if brace := strings.IndexByte(head, '{'); brace >= 0 {
		if !strings.HasSuffix(head, "}") {
			return "", nil, 0, false, false
		}
		name = strings.TrimSpace(head[:brace])
		parsed, ok := parsePrometheusLabels(head[brace+1 : len(head)-1])
		if !ok {
			return "", nil, 0, false, false
		}
		labels = parsed
	}
	if name == "" {
		return "", nil, 0, false, false
	}
	return name, labels, value, timestamped, true
}

func parsePrometheusLabels(text string) (map[string]string, bool) {
	labels := map[string]string{}
	index := 0
	skipSpace := func() {
		for index < len(text) && (text[index] == ' ' || text[index] == '\t') {
			index++
		}
	}
	for {
		skipSpace()
		if index >= len(text) {
			return labels, true
		}
		if len(labels) > 0 {
			if text[index] != ',' {
				return nil, false
			}
			index++
			skipSpace()
			if index >= len(text) {
				// Prometheus accepts one trailing comma after the final label.
				return labels, true
			}
		} else if text[index] == ',' {
			return nil, false
		}
		keyStart := index
		for index < len(text) && validLabelNameByte(text[index]) {
			index++
		}
		key := text[keyStart:index]
		if !validLabelName(key) {
			return nil, false
		}
		skipSpace()
		if index >= len(text) || text[index] != '=' {
			return nil, false
		}
		index++
		skipSpace()
		if index >= len(text) || text[index] != '"' {
			return nil, false
		}
		index++
		var value strings.Builder
		closed := false
		for index < len(text) {
			ch := text[index]
			if ch == '\\' {
				if index+1 >= len(text) {
					return nil, false
				}
				switch text[index+1] {
				case 'n':
					value.WriteByte('\n')
				case '\\', '"':
					value.WriteByte(text[index+1])
				default:
					return nil, false
				}
				index += 2
				continue
			}
			if ch == '"' {
				index++
				closed = true
				break
			}
			if ch == '\n' || ch == '\r' {
				return nil, false
			}
			value.WriteByte(ch)
			index++
		}
		if !closed || key == "" {
			return nil, false
		}
		if _, exists := labels[key]; exists {
			return nil, false
		}
		labels[key] = value.String()
	}
}

func validLabelNameByte(ch byte) bool {
	return ch == '_' || (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9')
}

func validLabelName(name string) bool {
	if name == "" || (name[0] >= '0' && name[0] <= '9') {
		return false
	}
	for index := 0; index < len(name); index++ {
		if !validLabelNameByte(name[index]) {
			return false
		}
	}
	return true
}
