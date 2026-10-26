package platform

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
)

// ObservationState is the value reported when a fact is not observable in this
// build. A domain binding is not proof of a DNS record, a route, a certificate
// or a public response.
const ObservationUnknown = "unknown"

// ContainerObservation is one managed container as Docker reports it on the
// node. Ownership comes from labels, never from the container name.
type ContainerObservation struct {
	App         string `json:"app,omitempty"`
	Environment string `json:"environment,omitempty"`
	Process     string `json:"process,omitempty"`
	Kind        string `json:"kind,omitempty"`
	Replica     string `json:"replica,omitempty"`
	ReleaseID   string `json:"release_id,omitempty"`
	Deployment  string `json:"deployment_id,omitempty"`
	Name        string `json:"name,omitempty"`
	ContainerID string `json:"container_id,omitempty"`
	State       string `json:"state"`
	Status      string `json:"status,omitempty"`
	HostPort    int    `json:"host_port,omitempty"`
}

// Workloads is a point-in-time read of managed containers selected by labels.
type Workloads struct {
	App         string                 `json:"app,omitempty"`
	Environment string                 `json:"environment,omitempty"`
	ObservedAt  time.Time              `json:"observed_at"`
	Containers  []ContainerObservation `json:"containers"`
}

// ReplicaObservation joins one desired replica with the container Docker shows
// for it. A missing container is an explicit state, not an empty row.
type ReplicaObservation struct {
	Replica     string `json:"replica"`
	Container   string `json:"container,omitempty"`
	ContainerID string `json:"container_id,omitempty"`
	State       string `json:"state"`
	Status      string `json:"status,omitempty"`
	HostPort    int    `json:"host_port,omitempty"`
	Serving     bool   `json:"serving"`
}

// ProcessObservation is one desired process and its replicas.
type ProcessObservation struct {
	Name            string               `json:"name"`
	Kind            string               `json:"kind"`
	DesiredReplicas int                  `json:"desired_replicas"`
	Replicas        []ReplicaObservation `json:"replicas"`
}

// DomainObservation is one row from the `domains` inventory collection plus
// the ready flag derived from the same four observations. It keeps the binding,
// route, DNS, certificate and public response separate.
type DomainObservation struct {
	DomainBinding
	Ready bool `json:"ready"`
}

// EnvironmentObservation is the status read model. It carries the environment
// record (desired release, current revision, processes, deployability), the
// node's container observation, the domain bindings and the last operations.
type EnvironmentObservation struct {
	Environment     Environment          `json:"environment"`
	ServingRelease  *Release             `json:"serving_release,omitempty"`
	Processes       []ProcessObservation `json:"processes"`
	OtherContainers []ReplicaObservation `json:"other_containers,omitempty"`
	Domains         []DomainObservation  `json:"domains"`
	Operations      []contract.Operation `json:"operations,omitempty"`
	WorkloadError   string               `json:"workload_error,omitempty"`
	DomainsError    string               `json:"domains_error,omitempty"`
	OperationsError string               `json:"operations_error,omitempty"`
}

// AppObservation is `app status`: the application and one observation per
// environment.
type AppObservation struct {
	App             App                      `json:"app"`
	Environments    []EnvironmentObservation `json:"environments"`
	Operations      []contract.Operation     `json:"operations,omitempty"`
	OperationsError string                   `json:"operations_error,omitempty"`
}

// BuildEnvironmentObservation joins durable desired state with a Docker read
// and the row-6 domain inventory.
func BuildEnvironmentObservation(env Environment, serving *Release, workloads []ContainerObservation, domains []DomainBinding, operations []contract.Operation) EnvironmentObservation {
	observation := EnvironmentObservation{
		Environment: env, ServingRelease: serving, Operations: operations,
		Domains: EnvironmentDomains(env, domains), Processes: []ProcessObservation{}, OtherContainers: []ReplicaObservation{},
	}
	desired := make(map[string]ProcessSpec, len(env.Processes))
	for _, process := range env.Processes {
		desired[process.Name] = process
	}
	byProcess := map[string]map[string]ContainerObservation{}
	for _, container := range workloads {
		if container.App != "" && env.App != "" && container.App != env.App {
			continue
		}
		if container.Environment != "" && container.Environment != env.Name {
			continue
		}
		process := container.Process
		if process == "" {
			process = "unlabelled"
		}
		if byProcess[process] == nil {
			byProcess[process] = map[string]ContainerObservation{}
		}
		byProcess[process][container.Replica] = container
	}
	names := make([]string, 0, len(desired))
	for name := range desired {
		names = append(names, name)
	}
	sort.Strings(names)
	seen := map[string]bool{}
	for _, name := range names {
		process := desired[name]
		seen[name] = true
		observation.Processes = append(observation.Processes, buildProcessObservation(env, process, byProcess[name]))
	}
	extra := make([]string, 0)
	for name := range byProcess {
		if !seen[name] {
			extra = append(extra, name)
		}
	}
	sort.Strings(extra)
	for _, name := range extra {
		for _, replica := range sortedReplicas(byProcess[name]) {
			observation.OtherContainers = append(observation.OtherContainers, containerReplica(env, byProcess[name][replica]))
		}
	}
	return observation
}

func buildProcessObservation(env Environment, process ProcessSpec, containers map[string]ContainerObservation) ProcessObservation {
	out := ProcessObservation{Name: process.Name, Kind: process.Kind, DesiredReplicas: process.Replicas, Replicas: []ReplicaObservation{}}
	desired := process.Replicas
	if desired < 1 {
		desired = 1
	}
	seen := map[string]bool{}
	for replica := 1; replica <= desired; replica++ {
		key := strconv.Itoa(replica)
		seen[key] = true
		if container, ok := containers[key]; ok {
			out.Replicas = append(out.Replicas, containerReplica(env, container))
			continue
		}
		out.Replicas = append(out.Replicas, ReplicaObservation{Replica: key, State: "missing"})
	}
	for _, replica := range sortedReplicas(containers) {
		if seen[replica] {
			continue
		}
		out.Replicas = append(out.Replicas, containerReplica(env, containers[replica]))
	}
	return out
}

func sortedReplicas(containers map[string]ContainerObservation) []string {
	keys := make([]string, 0, len(containers))
	for key := range containers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func containerReplica(env Environment, container ContainerObservation) ReplicaObservation {
	state := strings.TrimSpace(container.State)
	if state == "" {
		state = ObservationUnknown
	}
	return ReplicaObservation{
		Replica: container.Replica, Container: container.Name, ContainerID: container.ContainerID,
		State: state, Status: container.Status, HostPort: container.HostPort,
		Serving: container.Deployment != "" && container.Deployment == string(env.ServingDeploymentID),
	}
}

// EnvironmentDomains selects the inventory rows for one environment and marks
// each with the same ready rule the domain list uses.
func EnvironmentDomains(env Environment, domains []DomainBinding) []DomainObservation {
	out := make([]DomainObservation, 0, len(domains))
	for _, binding := range domains {
		if binding.App != "" && binding.App != env.App {
			continue
		}
		if binding.Environment != "" && binding.Environment != env.Name {
			continue
		}
		out = append(out, DomainObservation{DomainBinding: binding, Ready: binding.Ready()})
	}
	return out
}

// LogQuery is one bounded container-log read. DeploymentID selects the serving
// formation's containers; Process optionally narrows it further.
type LogQuery struct {
	App          string
	Environment  string
	DeploymentID DeploymentID
	Process      string
	Follow       bool
	Since        time.Duration
	Tail         int
}

// Validate checks the shape of a log query. The node repeats the checks before
// it runs Docker.
func (q LogQuery) Validate() error {
	if err := ValidateName(q.App); err != nil {
		return err
	}
	if err := ValidateName(q.Environment); err != nil {
		return err
	}
	if q.DeploymentID != "" {
		if err := q.DeploymentID.Validate(); err != nil {
			return err
		}
	}
	if q.Process != "" && !processNamePattern.MatchString(q.Process) {
		return invalid("process", "Use a lowercase process name of 1–63 letters, digits, underscores, or hyphens.")
	}
	if q.Since <= 0 || q.Since > 24*time.Hour {
		return invalid("since", "Use a positive lookback of at most 24h.")
	}
	if q.Tail < 1 || q.Tail > 10000 {
		return invalid("tail", "Use a tail of 1–10000 lines per container.")
	}
	return nil
}

// LogLine is one line of container output attributed to a process replica.
type LogLine struct {
	App         string `json:"app,omitempty"`
	Environment string `json:"environment,omitempty"`
	Process     string `json:"process,omitempty"`
	Replica     string `json:"replica,omitempty"`
	Container   string `json:"container,omitempty"`
	Stream      string `json:"stream,omitempty"`
	Message     string `json:"message"`
}

// LogResult is one bounded read (or the summary of a followed read).
type LogResult struct {
	Lines     []LogLine `json:"lines,omitempty"`
	Count     int       `json:"count"`
	Follow    bool      `json:"follow,omitempty"`
	Truncated bool      `json:"truncated,omitempty"`
}

// FormatLogLine prefixes a line with process/replica so output from several
// containers is attributable without a telemetry backend.
func FormatLogLine(line LogLine) string {
	prefix := strings.TrimSpace(line.Process)
	if prefix == "" {
		prefix = strings.TrimSpace(line.Container)
	}
	if prefix == "" {
		prefix = "container"
	}
	if replica := strings.TrimSpace(line.Replica); replica != "" {
		prefix += "/" + replica
	}
	return prefix + " | " + strings.TrimRight(line.Message, "\r\n")
}

// ParseDockerPS decodes the JSON-per-line output of `docker ps --format
// '{{json .}}'` into ownership-labelled containers.
func ParseDockerPS(raw []byte) ([]ContainerObservation, error) {
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 64*1024), 1<<20)
	out := []ContainerObservation{}
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var value struct {
			ID     string `json:"ID"`
			Names  string `json:"Names"`
			State  string `json:"State"`
			Status string `json:"Status"`
			Ports  string `json:"Ports"`
			Labels string `json:"Labels"`
		}
		if err := json.Unmarshal([]byte(line), &value); err != nil {
			return nil, fmt.Errorf("invalid docker ps line: %w", err)
		}
		labels := parseDockerLabels(value.Labels)
		out = append(out, ContainerObservation{
			App: labels["io.eregion.app"], Environment: labels["io.eregion.environment"],
			Process: labels["io.eregion.process"], Kind: labels["io.eregion.process.kind"],
			Replica: labels["io.eregion.process.replica"], ReleaseID: labels["io.eregion.release"],
			Deployment: labels["io.eregion.deployment"], Name: value.Names, ContainerID: value.ID,
			State: value.State, Status: value.Status, HostPort: publishedHostPort(value.Ports),
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read docker ps output: %w", err)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Environment != out[j].Environment {
			return out[i].Environment < out[j].Environment
		}
		if out[i].Process != out[j].Process {
			return out[i].Process < out[j].Process
		}
		if out[i].Replica != out[j].Replica {
			return out[i].Replica < out[j].Replica
		}
		return out[i].Name < out[j].Name
	})
	return out, nil
}

func parseDockerLabels(value string) map[string]string {
	labels := map[string]string{}
	for _, item := range strings.Split(value, ",") {
		key, label, ok := strings.Cut(item, "=")
		if !ok {
			continue
		}
		labels[strings.TrimSpace(key)] = strings.TrimSpace(label)
	}
	return labels
}

// publishedHostPort reads the first loopback host port from a docker ps Ports
// value such as "127.0.0.1:32768->8080/tcp".
func publishedHostPort(ports string) int {
	for _, item := range strings.Split(ports, ",") {
		item = strings.TrimSpace(item)
		left, right, ok := strings.Cut(item, "->")
		if !ok || !strings.HasSuffix(right, "/tcp") {
			continue
		}
		if index := strings.LastIndex(left, ":"); index >= 0 {
			port, err := strconv.Atoi(left[index+1:])
			if err == nil && port > 0 && port <= 65535 {
				return port
			}
		}
	}
	return 0
}
