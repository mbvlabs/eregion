package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/output"
)

// resourceStepPattern matches the generic resource lifecycle effects. The
// token is derived from the resource identity and declaration, so two specs
// never share an effect identity.
var resourceStepPattern = regexp.MustCompile(`^platform-resource-(network|pull|image|volume|run|health|destroy)-[0-9a-f]{12}(-[0-9]+)?-v1$`)

// resourceVolumeRemoveStepPattern is the explicit data-destroy effect. It is
// separate from the create step so a journaled create can never be reinterpreted
// as a deletion.
var resourceVolumeRemoveStepPattern = regexp.MustCompile(`^platform-resource-volume-remove-[0-9a-f]{12}-[0-9]+-v1$`)

// ResourceVolumePayload creates one resource-scoped named volume. A foreign
// volume with the same name is never adopted.
type ResourceVolumePayload struct {
	Step        string `json:"step"`
	Volume      string `json:"volume"`
	App         string `json:"app"`
	Environment string `json:"environment"`
	Resource    string `json:"resource"`
	ResourceID  string `json:"resource_id"`
}

type ResourceVolumeStep struct {
	exec    engine.Execution
	payload ResourceVolumePayload
}

func buildResourceVolumeStep(exec engine.Execution, raw json.RawMessage) (engine.Step, error) {
	var payload ResourceVolumePayload
	if err := decodePayload(exec, raw, &payload); err != nil {
		return nil, err
	}
	if payload.Step != exec.Step {
		return nil, fmt.Errorf("step ID %q does not match the payload step %q", exec.Step, payload.Step)
	}
	if !ResourceVolumeNameValid(payload.Volume) {
		return nil, errors.New("resource volume names must be er-res-<resource id>-<mount>")
	}
	if err := ValidateName(payload.App); err != nil {
		return nil, err
	}
	if err := ValidateName(payload.Environment); err != nil {
		return nil, err
	}
	if err := ValidateResourceName(payload.Resource); err != nil {
		return nil, err
	}
	id := ResourceID(payload.ResourceID)
	if err := id.Validate(); err != nil {
		return nil, err
	}
	return &ResourceVolumeStep{exec: exec, payload: payload}, nil
}

func (s *ResourceVolumeStep) ID() string                      { return s.payload.Step }
func (s *ResourceVolumeStep) Describe() string                { return "Ensure resource volume " + s.payload.Volume }
func (s *ResourceVolumeStep) CancelBoundary() engine.Boundary { return engine.BoundaryAfterApply }
func (s *ResourceVolumeStep) Inputs() ([]byte, error)         { return json.Marshal(s.payload) }

func (s *ResourceVolumeStep) labels() map[string]string {
	return ResourceOwnershipLabels(s.payload.App, s.payload.Environment, s.payload.Resource, ResourceID(s.payload.ResourceID))
}

func (s *ResourceVolumeStep) inspect(ctx context.Context) (map[string]string, bool, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	stdout, stderr, err := runArgv(probeCtx, []string{"docker", "volume", "inspect", "--format", "{{json .Labels}}", s.payload.Volume}, nil, engine.Runtime{}, s.ID(), nil)
	if err != nil {
		if alreadyGoneString(stderr) {
			return nil, false, nil
		}
		return nil, false, permanentCommandError(fmt.Errorf("inspect resource volume: %w: %s", err, firstLine(stderr)))
	}
	labels := map[string]string{}
	if text := strings.TrimSpace(stdout); text != "" {
		if err := json.Unmarshal([]byte(text), &labels); err != nil {
			return nil, false, fmt.Errorf("%w: the node returned unreadable volume labels", engine.ErrPermanent)
		}
	}
	return labels, true, nil
}

func (s *ResourceVolumeStep) Check(ctx context.Context, _ engine.Runtime) (engine.CheckResult, error) {
	labels, exists, err := s.inspect(ctx)
	if err != nil {
		return engine.CheckResult{}, err
	}
	if !exists {
		return engine.CheckResult{Complete: false, Detail: "resource volume is absent"}, nil
	}
	if !labelsOwnedBy(labels, s.labels()) {
		return engine.CheckResult{}, fmt.Errorf("%w: volume %q belongs to another owner", engine.ErrPermanent, s.payload.Volume)
	}
	return engine.CheckResult{Complete: true, Detail: "resource volume already exists with the expected owner"}, nil
}

func (s *ResourceVolumeStep) Apply(ctx context.Context, _ engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	runCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	args := []string{"docker", "volume", "create"}
	labels := s.labels()
	keys := sortedEnvKeys(labels)
	for _, key := range keys {
		args = append(args, "--label", key+"="+labels[key])
	}
	args = append(args, s.payload.Volume)
	_, stderr, err := runArgv(runCtx, args, nil, engine.Runtime{}, s.ID(), report)
	if err != nil {
		// A concurrent create is acceptable only when the existing volume has
		// our exact ownership labels. A foreign volume is never adopted.
		existing, exists, inspectErr := s.inspect(ctx)
		if inspectErr == nil && exists && labelsOwnedBy(existing, labels) {
			return engine.ApplyResult{Result: map[string]any{"volume": s.payload.Volume, "changed": false}}, nil
		}
		return engine.ApplyResult{}, permanentCommandError(fmt.Errorf("create resource volume: %w: %s", err, firstLine(stderr)))
	}
	// Docker treats create of an existing volume as success and leaves its
	// labels untouched. Re-inspect so a same-name foreign volume created in a
	// race is never silently adopted as ours.
	existing, exists, inspectErr := s.inspect(ctx)
	if inspectErr != nil {
		return engine.ApplyResult{}, inspectErr
	}
	if !exists || !labelsOwnedBy(existing, labels) {
		return engine.ApplyResult{}, fmt.Errorf("%w: volume %q exists without this resource's ownership labels", engine.ErrPermanent, s.payload.Volume)
	}
	return engine.ApplyResult{Result: map[string]any{"volume": s.payload.Volume, "changed": true}}, nil
}

// ResourceVolumeRemovePayload destroys one named volume only after verifying
// the exact resource ownership labels. It is used only by an explicit
// delete-data destroy, never by ordinary detach or application deletion.
type ResourceVolumeRemovePayload struct {
	Step        string `json:"step"`
	Volume      string `json:"volume"`
	App         string `json:"app"`
	Environment string `json:"environment"`
	Resource    string `json:"resource"`
	ResourceID  string `json:"resource_id"`
}

type ResourceVolumeRemoveStep struct {
	exec    engine.Execution
	payload ResourceVolumeRemovePayload
}

func buildResourceVolumeRemoveStep(exec engine.Execution, raw json.RawMessage) (engine.Step, error) {
	var payload ResourceVolumeRemovePayload
	if err := decodePayload(exec, raw, &payload); err != nil {
		return nil, err
	}
	if payload.Step != exec.Step {
		return nil, fmt.Errorf("step ID %q does not match the payload step %q", exec.Step, payload.Step)
	}
	if !ResourceVolumeNameValid(payload.Volume) {
		return nil, errors.New("resource volume names must be er-res-<resource id>-<mount>")
	}
	if err := ValidateName(payload.App); err != nil {
		return nil, err
	}
	if err := ValidateName(payload.Environment); err != nil {
		return nil, err
	}
	if err := ValidateResourceName(payload.Resource); err != nil {
		return nil, err
	}
	if err := ResourceID(payload.ResourceID).Validate(); err != nil {
		return nil, err
	}
	return &ResourceVolumeRemoveStep{exec: exec, payload: payload}, nil
}

func (s *ResourceVolumeRemoveStep) ID() string { return s.payload.Step }
func (s *ResourceVolumeRemoveStep) Describe() string {
	return "Remove resource volume " + s.payload.Volume + " and its data"
}
func (s *ResourceVolumeRemoveStep) CancelBoundary() engine.Boundary {
	return engine.BoundaryAfterApply
}
func (s *ResourceVolumeRemoveStep) Inputs() ([]byte, error) { return json.Marshal(s.payload) }

func (s *ResourceVolumeRemoveStep) labels() map[string]string {
	return ResourceOwnershipLabels(s.payload.App, s.payload.Environment, s.payload.Resource, ResourceID(s.payload.ResourceID))
}

func (s *ResourceVolumeRemoveStep) inspect(ctx context.Context) (map[string]string, bool, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	stdout, stderr, err := runArgv(probeCtx, []string{"docker", "volume", "inspect", "--format", "{{json .Labels}}", s.payload.Volume}, nil, engine.Runtime{}, s.ID(), nil)
	if err != nil {
		if alreadyGoneString(stderr) {
			return nil, false, nil
		}
		return nil, false, permanentCommandError(fmt.Errorf("inspect resource volume: %w: %s", err, firstLine(stderr)))
	}
	labels := map[string]string{}
	if text := strings.TrimSpace(stdout); text != "" {
		if err := json.Unmarshal([]byte(text), &labels); err != nil {
			return nil, false, fmt.Errorf("%w: the node returned unreadable volume labels", engine.ErrPermanent)
		}
	}
	return labels, true, nil
}

func (s *ResourceVolumeRemoveStep) Check(ctx context.Context, _ engine.Runtime) (engine.CheckResult, error) {
	labels, exists, err := s.inspect(ctx)
	if err != nil {
		return engine.CheckResult{}, err
	}
	if !exists {
		return engine.CheckResult{Complete: true, Detail: "resource volume and data are already absent"}, nil
	}
	if !labelsOwnedBy(labels, s.labels()) {
		return engine.CheckResult{}, fmt.Errorf("%w: volume %q belongs to another owner; refusing to delete data", engine.ErrPermanent, s.payload.Volume)
	}
	return engine.CheckResult{Complete: false, Detail: "resource volume and data still exist"}, nil
}

func (s *ResourceVolumeRemoveStep) Apply(ctx context.Context, rt engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	runCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	labels, exists, err := s.inspect(runCtx)
	if err != nil {
		return engine.ApplyResult{}, err
	}
	if !exists {
		return engine.ApplyResult{Result: map[string]any{"volume": s.payload.Volume, "removed": false, "data_deleted": true}}, nil
	}
	if !labelsOwnedBy(labels, s.labels()) {
		return engine.ApplyResult{}, fmt.Errorf("%w: volume %q belongs to another owner; refusing to delete data", engine.ErrPermanent, s.payload.Volume)
	}
	_, stderr, err := runArgv(runCtx, []string{"docker", "volume", "rm", s.payload.Volume}, nil, rt, s.ID(), report)
	if err != nil && !alreadyGoneString(stderr) {
		return engine.ApplyResult{}, permanentCommandError(fmt.Errorf("remove resource volume: %w: %s", err, firstLine(stderr)))
	}
	return engine.ApplyResult{Result: map[string]any{"volume": s.payload.Volume, "removed": true, "data_deleted": true}}, nil
}

// ResourceRunPayload starts one resource container on the environment's
// private bridge network. It deliberately has no publish option.
type ResourceRunPayload struct {
	Step        string            `json:"step"`
	Name        string            `json:"name"`
	ImageID     string            `json:"image_id"`
	Network     string            `json:"network"`
	Alias       string            `json:"alias"`
	Env         map[string]string `json:"env,omitempty"`
	SecretKeys  []string          `json:"secret_env_keys,omitempty"`
	Command     []string          `json:"command,omitempty"`
	Mounts      []ResourceVolume  `json:"mounts,omitempty"`
	ConfigHash  string            `json:"config_hash"`
	App         string            `json:"app"`
	Environment string            `json:"environment"`
	Resource    string            `json:"resource"`
	ResourceID  string            `json:"resource_id"`
	Restart     string            `json:"restart"`
}

type ResourceRunStep struct {
	exec    engine.Execution
	payload ResourceRunPayload
}

func buildResourceRunStep(exec engine.Execution, raw json.RawMessage) (engine.Step, error) {
	var payload ResourceRunPayload
	if err := decodePayload(exec, raw, &payload); err != nil {
		return nil, err
	}
	if payload.Step != exec.Step {
		return nil, fmt.Errorf("step ID %q does not match the payload step %q", exec.Step, payload.Step)
	}
	if !containerNamePattern.MatchString(payload.Name) {
		return nil, errors.New("invalid resource container name")
	}
	if !ResourceImageIDValid(payload.ImageID) {
		return nil, errors.New("a resource container must use an immutable image ID")
	}
	if !ResourceNetworkValid(payload.Network) {
		return nil, errors.New("invalid environment network")
	}
	if err := ValidateResourceName(payload.Alias); err != nil {
		return nil, err
	}
	if err := ValidateName(payload.App); err != nil {
		return nil, err
	}
	if err := ValidateName(payload.Environment); err != nil {
		return nil, err
	}
	if err := ValidateResourceName(payload.Resource); err != nil {
		return nil, err
	}
	if err := ResourceID(payload.ResourceID).Validate(); err != nil {
		return nil, err
	}
	if err := validateEnv(payload.Env, "resource environment"); err != nil {
		return nil, err
	}
	for _, key := range payload.SecretKeys {
		if !envKeyPattern.MatchString(key) {
			return nil, errors.New("invalid secret environment key")
		}
		if _, plain := payload.Env[key]; plain {
			return nil, errors.New("a key cannot be both plain configuration and a secret reference")
		}
	}
	if len(payload.SecretKeys) > 64 {
		return nil, errors.New("too many secret environment keys")
	}
	if payload.ConfigHash == "" || len(payload.ConfigHash) > 128 {
		return nil, errors.New("invalid resource config hash")
	}
	if payload.Restart == "" {
		payload.Restart = "unless-stopped"
	}
	if payload.Restart != "unless-stopped" {
		return nil, errors.New("resource containers must restart unless-stopped")
	}
	seenDestinations := map[string]bool{}
	for _, mount := range payload.Mounts {
		if !ResourceVolumeNameValid(mount.Name) {
			return nil, errors.New("invalid resource volume name")
		}
		if _, err := cleanResourceDestination(mount.Destination); err != nil {
			return nil, err
		}
		if seenDestinations[mount.Destination] {
			return nil, errors.New("two resource mounts use the same destination")
		}
		seenDestinations[mount.Destination] = true
	}
	return &ResourceRunStep{exec: exec, payload: payload}, nil
}

func (s *ResourceRunStep) ID() string { return s.payload.Step }
func (s *ResourceRunStep) Describe() string {
	return "Ensure resource container " + s.payload.Name + " runs its immutable image"
}
func (s *ResourceRunStep) CancelBoundary() engine.Boundary { return engine.BoundaryAfterApply }
func (s *ResourceRunStep) Inputs() ([]byte, error)         { return json.Marshal(s.payload) }

func (s *ResourceRunStep) labels() map[string]string {
	return ResourceOwnershipLabels(s.payload.App, s.payload.Environment, s.payload.Resource, ResourceID(s.payload.ResourceID))
}

type resourceContainerObservation struct {
	ID      string
	Image   string
	Running bool
	Labels  map[string]string
}

func inspectResourceContainer(ctx context.Context, step, name string) (resourceContainerObservation, bool, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	stdout, stderr, err := runArgv(probeCtx, []string{
		"docker", "inspect", "--type=container", "--format",
		`{{json .Config.Labels}}|{{.State.Running}}|{{.Image}}|{{.Id}}`, name,
	}, nil, engine.Runtime{}, step, nil)
	if err != nil {
		if containerAlreadyGone(stderr) {
			return resourceContainerObservation{}, false, nil
		}
		return resourceContainerObservation{}, false, permanentCommandError(fmt.Errorf("inspect resource container: %w: %s", err, firstLine(stderr)))
	}
	parts := strings.SplitN(strings.TrimSpace(stdout), "|", 4)
	if len(parts) != 4 || parts[3] == "" {
		return resourceContainerObservation{}, false, fmt.Errorf("%w: the node returned an unreadable container inspection", engine.ErrPermanent)
	}
	observation := resourceContainerObservation{ID: parts[3], Image: parts[2], Running: parts[1] == "true", Labels: map[string]string{}}
	if strings.TrimSpace(parts[0]) != "" && parts[0] != "<no value>" {
		if err := json.Unmarshal([]byte(parts[0]), &observation.Labels); err != nil {
			return resourceContainerObservation{}, false, fmt.Errorf("%w: the node returned unreadable container labels", engine.ErrPermanent)
		}
	}
	return observation, true, nil
}

func (s *ResourceRunStep) Check(ctx context.Context, _ engine.Runtime) (engine.CheckResult, error) {
	observation, exists, err := inspectResourceContainer(ctx, s.ID(), s.payload.Name)
	if err != nil {
		return engine.CheckResult{}, err
	}
	if !exists {
		return engine.CheckResult{Complete: false, Detail: "resource container is absent"}, nil
	}
	if !labelsOwnedBy(observation.Labels, s.labels()) {
		return engine.CheckResult{}, fmt.Errorf("%w: container %q belongs to another owner", engine.ErrPermanent, s.payload.Name)
	}
	if resourceContainerCurrent(observation, s.payload, s.labels()) {
		return engine.CheckResult{Complete: true, Detail: "resource container already runs the immutable image and configuration"}, nil
	}
	return engine.CheckResult{Complete: false, Detail: "resource container exists with other image or configuration"}, nil
}

// resourceContainerCurrent is the pure no-restart decision: a healthy running
// container with the exact immutable image, ownership labels and non-secret
// configuration hash is already converged. Apply is not called in that case.
func resourceContainerCurrent(observation resourceContainerObservation, payload ResourceRunPayload, labels map[string]string) bool {
	return observation.Running &&
		observation.Image == payload.ImageID &&
		observation.Labels["io.eregion.config"] == payload.ConfigHash &&
		labelsOwnedBy(observation.Labels, labels)
}

func (s *ResourceRunStep) Apply(ctx context.Context, rt engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	removeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	removed := false
	if observation, exists, err := inspectResourceContainer(removeCtx, s.ID(), s.payload.Name); err != nil {
		cancel()
		return engine.ApplyResult{}, err
	} else if exists {
		if !labelsOwnedBy(observation.Labels, s.labels()) {
			cancel()
			return engine.ApplyResult{}, fmt.Errorf("%w: container %q belongs to another owner", engine.ErrPermanent, s.payload.Name)
		}
		_, stderr, removeErr := runArgv(removeCtx, []string{"docker", "rm", "-f", s.payload.Name}, nil, rt, s.ID(), report)
		if removeErr != nil && !containerAlreadyGone(stderr) {
			cancel()
			return engine.ApplyResult{}, permanentCommandError(fmt.Errorf("remove previous resource container: %w: %s", removeErr, firstLine(stderr)))
		}
		removed = true
	}
	cancel()

	args, env, err := s.runArgs(rt)
	if err != nil {
		return engine.ApplyResult{}, fmt.Errorf("%w: %s", engine.ErrPermanent, err)
	}
	runCtx, runCancel := context.WithTimeout(ctx, 2*time.Minute)
	defer runCancel()
	stdout, stderr, err := runArgv(runCtx, args, env, rt, s.ID(), report)
	if err != nil {
		return engine.ApplyResult{}, permanentCommandError(fmt.Errorf("start resource container: %w: %s", err, redactedResourceDetail(rt, stderr)))
	}
	containerID := strings.TrimSpace(stdout)
	if containerID == "" {
		return engine.ApplyResult{}, fmt.Errorf("%w: docker run returned no container ID", engine.ErrPermanent)
	}
	if report != nil {
		report(engine.Event{Kind: "log", Step: s.ID(), Message: "resource container " + s.payload.Name + " runs " + s.payload.ImageID})
	}
	return engine.ApplyResult{Result: map[string]any{
		"container": s.payload.Name, "container_id": containerID, "image_id": s.payload.ImageID,
		"changed": true, "replaced": removed,
	}}, nil
}

func (s *ResourceRunStep) runArgs(rt engine.Runtime) ([]string, []string, error) {
	args := []string{
		"docker", "run", "-d", "--name", s.payload.Name, "--pull", "never",
		"--restart", s.payload.Restart, "--network", s.payload.Network,
		"--network-alias", s.payload.Alias,
	}
	for _, mount := range s.payload.Mounts {
		args = append(args, "--mount", resourceMountArgument(mount))
	}
	args = append(args, "--label", "io.eregion.config="+s.payload.ConfigHash)
	labels := s.labels()
	for _, key := range sortedEnvKeys(labels) {
		args = append(args, "--label", key+"="+labels[key])
	}
	env := make([]string, 0, len(s.payload.Env)+len(s.payload.SecretKeys))
	for _, key := range sortedEnvKeys(s.payload.Env) {
		// Docker reads the value from the node process environment, so a plain
		// configuration value never appears in the node argv or journal.
		args = append(args, "-e", key)
		env = append(env, key+"="+s.payload.Env[key])
	}
	for _, key := range s.payload.SecretKeys {
		value, ok := rt.SecretValues[key]
		if !ok || value == "" {
			return nil, nil, errors.New("secret value for " + key + " is unavailable")
		}
		args = append(args, "-e", key)
		env = append(env, key+"="+value)
	}
	args = append(args, s.payload.ImageID)
	args = append(args, s.payload.Command...)
	return args, env, nil
}

func resourceMountArgument(mount ResourceVolume) string {
	value := "type=volume,source=" + mount.Name + ",destination=" + mount.Destination
	if mount.ReadOnly {
		value += ",readonly=true"
	}
	return value
}

// ResourceHealthPayload probes the running resource container with an explicit
// exec command. It is generic and does not assume an engine client.
type ResourceHealthPayload struct {
	Step           string   `json:"step"`
	Container      string   `json:"container"`
	Command        []string `json:"command"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
	IntervalMS     int      `json:"interval_ms,omitempty"`
}

type ResourceHealthStep struct {
	exec    engine.Execution
	payload ResourceHealthPayload
}

func buildResourceHealthStep(exec engine.Execution, raw json.RawMessage) (engine.Step, error) {
	var payload ResourceHealthPayload
	if err := decodePayload(exec, raw, &payload); err != nil {
		return nil, err
	}
	if payload.Step != exec.Step {
		return nil, fmt.Errorf("step ID %q does not match the payload step %q", exec.Step, payload.Step)
	}
	if !containerNamePattern.MatchString(payload.Container) {
		return nil, errors.New("invalid resource container name")
	}
	if len(payload.Command) == 0 || len(payload.Command) > 32 {
		return nil, errors.New("a resource exec health check needs 1–32 argv entries")
	}
	for _, arg := range payload.Command {
		if arg == "" || len(arg) > 1024 || strings.ContainsAny(arg, "\x00\r\n") {
			return nil, errors.New("invalid resource health argument")
		}
	}
	if strings.HasPrefix(payload.Command[0], "-") {
		return nil, errors.New("a resource health command cannot start with a dash")
	}
	if payload.TimeoutSeconds == 0 {
		payload.TimeoutSeconds = 30
	}
	if payload.TimeoutSeconds < 1 || payload.TimeoutSeconds > 300 {
		return nil, errors.New("resource health timeout must be 1–300 seconds")
	}
	if payload.IntervalMS == 0 {
		payload.IntervalMS = 500
	}
	if payload.IntervalMS < 100 || payload.IntervalMS > 5000 {
		return nil, errors.New("resource health interval must be 100–5000 milliseconds")
	}
	return &ResourceHealthStep{exec: exec, payload: payload}, nil
}

func (s *ResourceHealthStep) ID() string { return s.payload.Step }
func (s *ResourceHealthStep) Describe() string {
	return "Probe resource " + s.payload.Container + " with its exec health check"
}
func (s *ResourceHealthStep) CancelBoundary() engine.Boundary { return engine.BoundaryAfterApply }
func (s *ResourceHealthStep) Inputs() ([]byte, error)         { return json.Marshal(s.payload) }

func (s *ResourceHealthStep) probe(ctx context.Context, rt engine.Runtime) (bool, string, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	argv := append([]string{"docker", "exec", s.payload.Container}, s.payload.Command...)
	_, stderr, err := runArgv(probeCtx, argv, nil, rt, s.ID(), nil)
	if err == nil {
		return true, "", nil
	}
	if probeCtx.Err() == context.DeadlineExceeded {
		return false, "health probe exceeded its 20-second per-attempt bound", nil
	}
	text := firstLine(stderr)
	if rt.Secrets != nil {
		text = rt.Secrets.Redact(text)
	}
	text = output.SafeText(text)
	if len(text) > 200 {
		text = text[:200]
	}
	detail := "health command exited unsuccessfully"
	if text != "" {
		detail = "health command reported: " + text
	}
	if containerAlreadyGone(stderr) {
		return false, "resource container is absent", nil
	}
	return false, detail, nil
}

func (s *ResourceHealthStep) Check(ctx context.Context, rt engine.Runtime) (engine.CheckResult, error) {
	healthy, detail, err := s.probe(ctx, rt)
	if err != nil {
		return engine.CheckResult{}, err
	}
	if healthy {
		return engine.CheckResult{Complete: true, Detail: "resource exec health check already succeeds"}, nil
	}
	return engine.CheckResult{Complete: false, Detail: "resource health is not yet healthy: " + detail}, nil
}

func (s *ResourceHealthStep) Apply(ctx context.Context, rt engine.Runtime, _ engine.Reporter) (engine.ApplyResult, error) {
	deadline := time.Now().Add(time.Duration(s.payload.TimeoutSeconds) * time.Second)
	interval := time.Duration(s.payload.IntervalMS) * time.Millisecond
	last := "health check did not run"
	for {
		healthy, detail, err := s.probe(ctx, rt)
		if err != nil {
			return engine.ApplyResult{}, err
		}
		if healthy {
			return engine.ApplyResult{Result: map[string]any{"healthy": true, "detail": "resource exec health check succeeded"}}, nil
		}
		if detail != "" {
			last = detail
		}
		if !time.Now().Before(deadline) {
			return engine.ApplyResult{}, fmt.Errorf("%w: resource health check did not succeed before the deadline: %s", engine.ErrPermanent, last)
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return engine.ApplyResult{}, ctx.Err()
		case <-timer.C:
		}
	}
}

// ResourceDestroyPayload removes one owned container. The named volumes are
// intentionally not touched; data deletion is a separate, unreviewed decision.
type ResourceDestroyPayload struct {
	Step        string `json:"step"`
	Container   string `json:"container"`
	App         string `json:"app"`
	Environment string `json:"environment"`
	Resource    string `json:"resource"`
	ResourceID  string `json:"resource_id"`
}

type ResourceDestroyStep struct {
	exec    engine.Execution
	payload ResourceDestroyPayload
}

func buildResourceDestroyStep(exec engine.Execution, raw json.RawMessage) (engine.Step, error) {
	var payload ResourceDestroyPayload
	if err := decodePayload(exec, raw, &payload); err != nil {
		return nil, err
	}
	if payload.Step != exec.Step {
		return nil, fmt.Errorf("step ID %q does not match the payload step %q", exec.Step, payload.Step)
	}
	if !containerNamePattern.MatchString(payload.Container) {
		return nil, errors.New("invalid resource container name")
	}
	if err := ValidateName(payload.App); err != nil {
		return nil, err
	}
	if err := ValidateName(payload.Environment); err != nil {
		return nil, err
	}
	if err := ValidateResourceName(payload.Resource); err != nil {
		return nil, err
	}
	if err := ResourceID(payload.ResourceID).Validate(); err != nil {
		return nil, err
	}
	return &ResourceDestroyStep{exec: exec, payload: payload}, nil
}

func (s *ResourceDestroyStep) ID() string { return s.payload.Step }
func (s *ResourceDestroyStep) Describe() string {
	return "Remove resource container " + s.payload.Container + " and retain its named volumes"
}
func (s *ResourceDestroyStep) CancelBoundary() engine.Boundary { return engine.BoundaryAfterApply }
func (s *ResourceDestroyStep) Inputs() ([]byte, error)         { return json.Marshal(s.payload) }

func (s *ResourceDestroyStep) labels() map[string]string {
	return ResourceOwnershipLabels(s.payload.App, s.payload.Environment, s.payload.Resource, ResourceID(s.payload.ResourceID))
}

func (s *ResourceDestroyStep) Check(ctx context.Context, _ engine.Runtime) (engine.CheckResult, error) {
	observation, exists, err := inspectResourceContainer(ctx, s.ID(), s.payload.Container)
	if err != nil {
		return engine.CheckResult{}, err
	}
	if !exists {
		return engine.CheckResult{Complete: true, Detail: "resource container is already absent"}, nil
	}
	if !labelsOwnedBy(observation.Labels, s.labels()) {
		return engine.CheckResult{}, fmt.Errorf("%w: container %q belongs to another owner", engine.ErrPermanent, s.payload.Container)
	}
	return engine.CheckResult{Complete: false, Detail: "resource container still exists"}, nil
}

func (s *ResourceDestroyStep) Apply(ctx context.Context, rt engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	runCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	observation, exists, err := inspectResourceContainer(runCtx, s.ID(), s.payload.Container)
	if err != nil {
		return engine.ApplyResult{}, err
	}
	if !exists {
		return engine.ApplyResult{Result: map[string]any{"removed": false, "data_retained": true}}, nil
	}
	if !labelsOwnedBy(observation.Labels, s.labels()) {
		return engine.ApplyResult{}, fmt.Errorf("%w: container %q belongs to another owner", engine.ErrPermanent, s.payload.Container)
	}
	_, stderr, err := runArgv(runCtx, []string{"docker", "rm", "-f", s.payload.Container}, nil, rt, s.ID(), report)
	if err != nil && !containerAlreadyGone(stderr) {
		return engine.ApplyResult{}, permanentCommandError(fmt.Errorf("remove resource container: %w: %s", err, firstLine(stderr)))
	}
	return engine.ApplyResult{Result: map[string]any{"removed": true, "data_retained": true}}, nil
}

func labelsOwnedBy(labels, expected map[string]string) bool {
	for key, value := range expected {
		if labels[key] != value {
			return false
		}
	}
	return true
}

func redactedResourceDetail(rt engine.Runtime, text string) string {
	detail := firstLine(text)
	if rt.Secrets != nil {
		detail = rt.Secrets.Redact(detail)
	}
	detail = output.SafeText(detail)
	if len(detail) > 500 {
		detail = detail[:500]
	}
	return detail
}

func sortedResourceVolumes(volumes []ResourceVolume) []ResourceVolume {
	out := append([]ResourceVolume(nil), volumes...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

var _ engine.Step = (*ResourceVolumeStep)(nil)
var _ engine.Step = (*ResourceVolumeRemoveStep)(nil)
var _ engine.Step = (*ResourceRunStep)(nil)
var _ engine.Step = (*ResourceHealthStep)(nil)
var _ engine.Step = (*ResourceDestroyStep)(nil)
