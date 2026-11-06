package platform

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
)

type ResourceID string

const resourceIDPattern = `^res_[0-9a-f]{32}$`

var (
	resourceIDRegexp      = regexp.MustCompile(resourceIDPattern)
	resourceNamePattern   = regexp.MustCompile(`^[a-z]([a-z0-9-]{0,38}[a-z0-9])?$`)
	resourceMountPattern  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,19}$`)
	resourceVolumePattern = regexp.MustCompile(`^er-res-[0-9a-f]{32}-[a-z][a-z0-9-]{0,19}$`)
	imageIDPattern        = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	resourceNetworkRegexp = regexp.MustCompile(`^er-env_[0-9a-f]{32}$`)
)

// ResourceState is the observed lifecycle of the one container behind a
// declared resource. A declaration is not proof of readiness.
type ResourceState string

const (
	ResourcePending      ResourceState = "pending"
	ResourceProvisioning ResourceState = "provisioning"
	ResourceReady        ResourceState = "ready"
	ResourceDegraded     ResourceState = "degraded"
	ResourceDestroying   ResourceState = "destroying"
	ResourceDestroyed    ResourceState = "destroyed"
)

func (s ResourceState) Valid() bool {
	switch s {
	case ResourcePending, ResourceProvisioning, ResourceReady, ResourceDegraded, ResourceDestroying, ResourceDestroyed:
		return true
	}
	return false
}

// ResourceMount declares one resource-scoped named volume. Name is a logical
// suffix; the Docker volume is derived from the stable resource identity so a
// destroy never guesses which data belongs to another resource. Host bind
// mounts are not representable.
type ResourceMount struct {
	Name        string `json:"name"`
	Destination string `json:"destination"`
	ReadOnly    bool   `json:"read_only,omitempty"`
}

// ResourceHealthSpec is an explicit exec probe inside the resource container.
// The probe is generic: eregion does not assume an engine-specific client.
type ResourceHealthSpec struct {
	Type           string   `json:"type"`
	Command        []string `json:"command"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
	IntervalMS     int      `json:"interval_ms,omitempty"`
}

// ResourceSpec is the complete generic image declaration. SecretEnv maps a
// container environment variable to an environment-scoped vault secret NAME.
// It never carries a secret value.
type ResourceSpec struct {
	Image string `json:"image"`
	// Port is the container's private listening port. It is recorded for the
	// application's private hostname/port mapping and is never published on a
	// host interface.
	Port      int                `json:"port,omitempty"`
	Env       map[string]string  `json:"env,omitempty"`
	SecretEnv map[string]string  `json:"secret_env,omitempty"`
	Mounts    []ResourceMount    `json:"mounts,omitempty"`
	Health    ResourceHealthSpec `json:"health"`
	// Command optionally replaces the image command with an explicit argv.
	Command []string `json:"command,omitempty"`
}

// Resource is the durable declaration plus the last observation of its
// container. Spec contains secret names only. ImageID is the immutable image
// identity recorded by a successful apply.
type Resource struct {
	ID          ResourceID    `json:"id"`
	App         string        `json:"app"`
	Environment string        `json:"environment"`
	Name        string        `json:"name"`
	Node        string        `json:"node"`
	Spec        ResourceSpec  `json:"spec"`
	State       ResourceState `json:"state"`
	Image       string        `json:"image,omitempty"`
	ImageID     string        `json:"image_id,omitempty"`
	// ImageDigest is the repository-qualified digest recorded with ImageID when
	// the image exposes one; it does not replace the immutable local image ID.
	ImageDigest string `json:"image_digest,omitempty"`
	// ContainerName, NetworkName and volumes are the last applied private
	// endpoint facts. Volumes is cumulative: a mount removed by a later apply
	// stays recorded so an explicit --delete-data can remove only this
	// resource's data, including volumes created before an interrupted apply.
	// None of these fields contains a host-published port.
	ContainerName string                 `json:"container_name,omitempty"`
	NetworkName   string                 `json:"network_name,omitempty"`
	Volumes       []ResourceVolumeStatus `json:"volumes,omitempty"`
	HealthState   HealthState            `json:"health_state"`
	HealthDetail  string                 `json:"health_detail,omitempty"`
	HealthChecked *time.Time             `json:"health_checked_at,omitempty"`
	OperationID   contract.OperationID   `json:"operation_id,omitempty"`
	ErrorCode     contract.Code          `json:"error_code,omitempty"`
	Error         string                 `json:"error,omitempty"`
	CreatedAt     time.Time              `json:"created_at"`
	UpdatedAt     time.Time              `json:"updated_at"`
	DestroyedAt   *time.Time             `json:"destroyed_at,omitempty"`
}

// ResourceVolumeStatus is one owned named-volume identity. Destination and
// ReadOnly record the latest declaration facts for that identity. It is
// non-secret.
type ResourceVolumeStatus struct {
	Name        string `json:"name"`
	Destination string `json:"destination"`
	ReadOnly    bool   `json:"read_only,omitempty"`
}

// PlannedResourceVolumes projects a plan's derived volume identities into the
// durable resource status shape.
func PlannedResourceVolumes(plan ResourcePlan) []ResourceVolumeStatus {
	volumes := make([]ResourceVolumeStatus, 0, len(plan.Volumes))
	for _, volume := range plan.Volumes {
		volumes = append(volumes, ResourceVolumeStatus{
			Name: volume.Name, Destination: volume.Destination, ReadOnly: volume.ReadOnly,
		})
	}
	return volumes
}

// MergeResourceVolumes returns the cumulative owned-volume identities for one
// resource. An existing identity is never dropped: when a declaration removes a
// mount, the Docker volume remains this resource's data and a later explicit
// --delete-data must still find it. A planned entry replaces the recorded
// destination/mode for the same derived identity because that is the current
// mount. The result is sorted by volume name so every update is deterministic.
func MergeResourceVolumes(existing, planned []ResourceVolumeStatus) []ResourceVolumeStatus {
	byName := make(map[string]ResourceVolumeStatus, len(existing)+len(planned))
	for _, volume := range existing {
		if volume.Name == "" {
			continue
		}
		byName[volume.Name] = volume
	}
	for _, volume := range planned {
		if volume.Name == "" {
			continue
		}
		byName[volume.Name] = volume
	}
	out := make([]ResourceVolumeStatus, 0, len(byName))
	for _, volume := range byName {
		out = append(out, volume)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ResourceVolumesForDestroy returns every named volume an explicit
// --delete-data destroy may own: identities already recorded plus the current
// declaration's deterministic identities. The union recovers a volume whose
// create succeeded before an interrupted apply recorded it, while every removal
// still verifies the exact resource ownership labels before deleting data.
func ResourceVolumesForDestroy(owned []ResourceVolumeStatus, plan ResourcePlan) []ResourceVolumeStatus {
	return MergeResourceVolumes(owned, PlannedResourceVolumes(plan))
}

// ResourceApplyInput creates a resource identity or applies a new image/spec to
// the same environment-owned identity. It never publishes a host port.
type ResourceApplyInput struct {
	App         string `json:"app,omitempty"`
	Environment string `json:"environment,omitempty"`
	Name        string `json:"name,omitempty"`
	ResourceSpec
}

func (in *ResourceApplyInput) Target() Target {
	return Target{Kind: TargetResource, App: in.App, Environment: in.Environment, Resource: in.Name}
}
func (in *ResourceApplyInput) Preview() any { return in }

// ResourceDestroyInput removes the running container for one exact resource and
// retains its named volumes and row by default. DeleteData additionally removes
// only the resource's owned named volumes; it is a separate explicit decision.
type ResourceDestroyInput struct {
	App         string `json:"app,omitempty"`
	Environment string `json:"environment,omitempty"`
	Name        string `json:"name,omitempty"`
	// DeleteData removes the resource's named volumes after the container is
	// gone. The default is false: scope destroy is separate from data destroy.
	DeleteData bool `json:"delete_data,omitempty"`
	// Confirm names app/environment/resource (MASTER D12). It is validated
	// against the canonical target by the control plane.
	Confirm string `json:"confirm,omitempty"`
}

func (in *ResourceDestroyInput) Target() Target {
	return Target{Kind: TargetResource, App: in.App, Environment: in.Environment, Resource: in.Name}
}
func (in *ResourceDestroyInput) Preview() any { return in }

// ResourceVolume is the derived Docker volume identity for one declaration.
type ResourceVolume struct {
	Name        string `json:"name"`
	Destination string `json:"destination"`
	ReadOnly    bool   `json:"read_only,omitempty"`
}

// ResourcePlan is the deterministic private runtime identity for one resource
// declaration. It contains no secret value.
type ResourcePlan struct {
	ID          ResourceID
	App         string
	Environment string
	Name        string
	Node        string
	Spec        ResourceSpec
	SpecDigest  string
	StepToken   string
	Container   string
	Network     string
	Alias       string
	Volumes     []ResourceVolume
	Labels      map[string]string
}

// NewResourceID returns a new durable resource identity.
func NewResourceID() (ResourceID, error) {
	value, err := newID("res_")
	return ResourceID(value), err
}

func (id ResourceID) Validate() error {
	if !resourceIDRegexp.MatchString(string(id)) {
		return contract.NewFault(contract.ValidationFailed, "invalid resource ID", "Use the resource ID returned by resource inventory.")
	}
	return nil
}

// ValidateResourceName validates a lowercase resource name. It is the stable
// declaration identity inside one environment and the private DNS alias on the
// environment network.
func ValidateResourceName(name string) error {
	if !resourceNamePattern.MatchString(name) {
		return contract.NewFault(contract.ValidationFailed, "invalid resource name", "Use 1–40 lowercase letters, digits and inner hyphens; start with a letter and end with a letter or digit.")
	}
	return nil
}

// NormalizeResourceSpec validates and canonicalizes one generic resource
// declaration. It rejects secret-value fields by strict decoding at the action
// boundary; this function only ever accepts secret NAMES.
func NormalizeResourceSpec(spec ResourceSpec) (ResourceSpec, error) {
	if err := ValidateImage(spec.Image); err != nil {
		return spec, err
	}
	spec.Image = strings.TrimSpace(spec.Image)
	if spec.Port != 0 {
		if err := validatePort(spec.Port, 1); err != nil {
			return spec, err
		}
	}
	if spec.Env == nil {
		spec.Env = map[string]string{}
	}
	if err := validateEnv(spec.Env, "resource environment"); err != nil {
		return spec, err
	}
	if spec.SecretEnv == nil {
		spec.SecretEnv = map[string]string{}
	}
	if len(spec.SecretEnv) > 64 {
		return spec, invalid("resource secret environment", "Use at most 64 secret environment mappings.")
	}
	for key, name := range spec.SecretEnv {
		if !envKeyPattern.MatchString(key) {
			return spec, invalid("resource secret environment key", "Use 1–128 ASCII letters, digits, and underscores; do not start with a digit.")
		}
		if _, plain := spec.Env[key]; plain {
			return spec, invalid("resource secret environment", "A key cannot be both plain configuration and a secret reference.")
		}
		if !secretNamePattern.MatchString(name) {
			return spec, invalid("resource secret name", "Reference an uppercase vault secret name such as DATABASE_PASSWORD.")
		}
	}
	if len(spec.Mounts) > 16 {
		return spec, invalid("resource mounts", "Use at most 16 named-volume mounts.")
	}
	seenMounts := map[string]bool{}
	seenDestinations := map[string]bool{}
	for index := range spec.Mounts {
		mount := &spec.Mounts[index]
		mount.Name = strings.TrimSpace(mount.Name)
		if !resourceMountPattern.MatchString(mount.Name) {
			return spec, invalid("resource mount name", "Use 1–20 lowercase letters, digits and inner hyphens for a mount name.")
		}
		if seenMounts[mount.Name] {
			return spec, invalid("resource mount name", "Use each mount name at most once.")
		}
		seenMounts[mount.Name] = true
		destination, err := cleanResourceDestination(mount.Destination)
		if err != nil {
			return spec, err
		}
		if seenDestinations[destination] {
			return spec, invalid("resource mount destination", "Use each container destination at most once.")
		}
		seenDestinations[destination] = true
		mount.Destination = destination
	}
	if spec.Health.Type == "" {
		spec.Health.Type = "exec"
	}
	if spec.Health.Type != "exec" {
		return spec, invalid("resource health type", `Use health.type "exec" with a command that exits 0 when the resource is ready.`)
	}
	if len(spec.Health.Command) == 0 || len(spec.Health.Command) > 32 {
		return spec, invalid("resource health command", "Provide 1–32 argv entries for the exec health check.")
	}
	for _, arg := range spec.Health.Command {
		if arg == "" || len(arg) > 1024 || strings.ContainsAny(arg, "\x00\r\n") {
			return spec, invalid("resource health command", "Health argv entries must be non-empty, at most 1024 bytes and free of control characters.")
		}
	}
	if strings.HasPrefix(spec.Health.Command[0], "-") {
		return spec, invalid("resource health command", "The health command cannot start with a dash.")
	}
	if spec.Health.TimeoutSeconds == 0 {
		spec.Health.TimeoutSeconds = 30
	}
	if spec.Health.TimeoutSeconds < 1 || spec.Health.TimeoutSeconds > 300 {
		return spec, invalid("resource health timeout", "Use a timeout from 1 to 300 seconds.")
	}
	if spec.Health.IntervalMS == 0 {
		spec.Health.IntervalMS = 500
	}
	if spec.Health.IntervalMS < 100 || spec.Health.IntervalMS > 5000 {
		return spec, invalid("resource health interval", "Use an interval from 100 to 5000 milliseconds.")
	}
	if len(spec.Command) > 32 {
		return spec, invalid("resource command", "Use at most 32 argv entries.")
	}
	for _, arg := range spec.Command {
		if len(arg) > 4096 || strings.ContainsAny(arg, "\x00\r\n") {
			return spec, invalid("resource command", "Command argv entries must be at most 4096 bytes and free of control characters.")
		}
	}
	return spec, nil
}

func cleanResourceDestination(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" || !strings.HasPrefix(value, "/") || strings.ContainsAny(value, "\x00\r\n") {
		return "", invalid("resource mount destination", "Use an absolute container path such as /var/lib/data.")
	}
	if path.Clean(value) != value {
		return "", invalid("resource mount destination", "Use a clean absolute path without dot segments.")
	}
	switch value {
	case "/", "/proc", "/sys", "/dev", "/boot", "/etc", "/run", "/var/run", "/var/lib/docker", "/var/lib/eregion":
		return "", invalid("resource mount destination", "That destination would replace a host or runtime-managed path; choose a dedicated data directory.")
	}
	for _, prefix := range []string{"/proc/", "/sys/", "/dev/", "/boot/", "/var/lib/docker/", "/var/lib/eregion/"} {
		if strings.HasPrefix(value, prefix) {
			return "", invalid("resource mount destination", "That destination is below a host or runtime-managed path; choose a dedicated data directory.")
		}
	}
	return value, nil
}

// PlanResource derives the collision-safe private runtime identity. The
// environment network is shared with the environment's application processes;
// the resource name is its stable alias on that network.
func PlanResource(environmentID EnvironmentID, node string, id ResourceID, app, environment, name string, spec ResourceSpec) (ResourcePlan, error) {
	if err := environmentID.Validate(); err != nil {
		return ResourcePlan{}, err
	}
	if err := id.Validate(); err != nil {
		return ResourcePlan{}, err
	}
	if err := ValidateName(app); err != nil {
		return ResourcePlan{}, err
	}
	if err := ValidateName(environment); err != nil {
		return ResourcePlan{}, err
	}
	if err := ValidateResourceName(name); err != nil {
		return ResourcePlan{}, err
	}
	if err := ValidateName(node); err != nil {
		return ResourcePlan{}, err
	}
	spec, err := NormalizeResourceSpec(spec)
	if err != nil {
		return ResourcePlan{}, err
	}
	rawDigest, err := json.Marshal(spec)
	if err != nil {
		return ResourcePlan{}, err
	}
	digest := sha256.Sum256(rawDigest)
	suffix := strings.TrimPrefix(string(id), "res_")
	plan := ResourcePlan{
		ID: id, App: app, Environment: environment, Name: name, Node: node, Spec: spec,
		SpecDigest: hex.EncodeToString(digest[:]),
		Container:  "er-res-" + suffix,
		Network:    EnvironmentNetwork(environmentID),
		Alias:      name,
		Labels:     ResourceOwnershipLabels(app, environment, name, id),
	}
	token := sha256.Sum256([]byte(string(id) + "\x00" + plan.SpecDigest))
	plan.StepToken = hex.EncodeToString(token[:6])
	seen := map[string]bool{}
	for _, mount := range spec.Mounts {
		volume := "er-res-" + suffix + "-" + mount.Name
		if seen[volume] {
			return ResourcePlan{}, invalid("resource mount name", "Two mounts derived the same Docker volume name.")
		}
		seen[volume] = true
		plan.Volumes = append(plan.Volumes, ResourceVolume{Name: volume, Destination: mount.Destination, ReadOnly: mount.ReadOnly})
	}
	return plan, nil
}

func ResourceNetworkStepID(plan ResourcePlan) string {
	return "platform-resource-network-" + plan.StepToken + "-v1"
}

func ResourcePullStepID(plan ResourcePlan) string {
	return "platform-resource-pull-" + plan.StepToken + "-v1"
}

func ResourceImageStepID(plan ResourcePlan) string {
	return "platform-resource-image-" + plan.StepToken + "-v1"
}

func ResourceVolumeStepID(plan ResourcePlan, index int) string {
	return "platform-resource-volume-" + plan.StepToken + "-" + strconv.Itoa(index+1) + "-v1"
}

func ResourceRunStepID(plan ResourcePlan) string {
	return "platform-resource-run-" + plan.StepToken + "-v1"
}

func ResourceHealthStepID(plan ResourcePlan) string {
	return "platform-resource-health-" + plan.StepToken + "-v1"
}

// ResourceDestroyStepID names the container removal effect. It is derived from
// the resource identity, not its current spec, so a destroy cannot be confused
// with a different resource that reused a name in another environment.
func ResourceDestroyStepID(id ResourceID) string {
	sum := sha256.Sum256([]byte(string(id)))
	return "platform-resource-destroy-" + hex.EncodeToString(sum[:6]) + "-v1"
}

// ResourceVolumeRemoveStepID names one explicit data-destroy effect. Its token
// and index keep each named volume a distinct durable node effect.
func ResourceVolumeRemoveStepID(plan ResourcePlan, index int) string {
	sum := sha256.Sum256([]byte(string(plan.ID) + "\x00data\x00" + strconv.Itoa(index)))
	return "platform-resource-volume-remove-" + hex.EncodeToString(sum[:6]) + "-" + strconv.Itoa(index+1) + "-v1"
}

// ResourceContainerConfigHash is the stable non-secret detector for whether a
// resource container still matches its declaration. Secret NAMES and resolved
// revision identities are included, never values; a rotated secret revision
// therefore replaces the container on the next explicit apply.
func ResourceContainerConfigHash(plan ResourcePlan, imageID, secretRevisionFingerprint string, secretEnvKeys []string) string {
	secretKeys := append([]string(nil), secretEnvKeys...)
	sort.Strings(secretKeys)
	value := struct {
		Image             string            `json:"image"`
		Env               map[string]string `json:"env"`
		SecretKeys        []string          `json:"secret_keys"`
		SecretFingerprint string            `json:"secret_fingerprint"`
		Command           []string          `json:"command"`
		Network           string            `json:"network"`
		Alias             string            `json:"alias"`
		Volumes           []ResourceVolume  `json:"volumes"`
		SpecDigest        string            `json:"spec_digest"`
	}{
		Image: imageID, Env: plan.Spec.Env, SecretKeys: secretKeys,
		SecretFingerprint: secretRevisionFingerprint,
		Command:           plan.Spec.Command, Network: plan.Network, Alias: plan.Alias,
		Volumes: plan.Volumes, SpecDigest: plan.SpecDigest,
	}
	raw, _ := json.Marshal(value)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// SortedResourceSecretEnvKeys returns the image environment keys that carry
// secret values. Only key names travel in a node payload.
func SortedResourceSecretEnvKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// ParseResourceImageInspect parses
// "<image id>|<comma-separated repo digests>" from docker image inspect.
func ParseResourceImageInspect(stdout string) (imageID string, repoDigests []string, err error) {
	line := strings.TrimSpace(stdout)
	if line == "" {
		return "", nil, errors.New("docker returned no image identity")
	}
	id, digests, _ := strings.Cut(line, "|")
	id = strings.TrimSpace(id)
	if !imageIDPattern.MatchString(id) {
		return "", nil, fmt.Errorf("docker returned an invalid immutable image ID %q", id)
	}
	for _, digest := range strings.Split(digests, ",") {
		digest = strings.TrimSpace(digest)
		if digest != "" && digest != "<no value>" {
			repoDigests = append(repoDigests, digest)
		}
	}
	return id, repoDigests, nil
}

// ResourceImageDigest chooses a repository-qualified digest from the image
// inspection, preferring the one whose repository matches the requested image
// reference. Empty means Docker reported no repository digest; the immutable
// image ID still identifies the local content.
func ResourceImageDigest(image string, repoDigests []string) string {
	digests := append([]string(nil), repoDigests...)
	sort.Strings(digests)
	if len(digests) == 0 {
		return ""
	}
	repository := image
	if index := strings.IndexByte(repository, '@'); index >= 0 {
		repository = repository[:index]
	}
	if index := strings.LastIndexByte(repository, ':'); index > strings.LastIndexByte(repository, '/') {
		repository = repository[:index]
	}
	for _, digest := range digests {
		if strings.HasPrefix(digest, repository+"@") {
			return digest
		}
	}
	return digests[0]
}

// ResourceVolumeOwnershipOwned reports whether one Docker volume's labels name
// exactly this resource. A foreign volume is never adopted.
func ResourceVolumeOwned(plan ResourcePlan, labels map[string]string) bool {
	for _, key := range []string{"io.eregion.resource-id", "io.eregion.app", "io.eregion.environment", "io.eregion.resource"} {
		if labels[key] != plan.Labels[key] {
			return false
		}
	}
	return true
}

// ResourceContainerOwned reports whether one Docker container's labels name
// exactly this resource. Names alone never establish ownership.
func ResourceContainerOwned(plan ResourcePlan, labels map[string]string) bool {
	for _, key := range []string{"io.eregion.resource-id", "io.eregion.app", "io.eregion.environment", "io.eregion.resource", "io.eregion.managed"} {
		if labels[key] != plan.Labels[key] {
			return false
		}
	}
	return true
}

// ResourceVolumeNameValid reports whether a Docker volume name is one of the
// resource-scoped identities eregion creates.
func ResourceVolumeNameValid(name string) bool { return resourceVolumePattern.MatchString(name) }

// ResourceNetworkValid reports whether a network is the per-environment
// private bridge network identity.
func ResourceNetworkValid(name string) bool { return resourceNetworkRegexp.MatchString(name) }

// ResourceImageIDValid reports whether a resolved image identity is immutable.
func ResourceImageIDValid(id string) bool { return imageIDPattern.MatchString(id) }

// ResourceOwnershipLabels are the labels that establish ownership for one
// resource container and its volumes. Names alone never establish ownership.
func ResourceOwnershipLabels(app, environment, name string, id ResourceID) map[string]string {
	return map[string]string{
		"io.eregion.app":         app,
		"io.eregion.environment": environment,
		"io.eregion.resource":    name,
		"io.eregion.resource-id": string(id),
		"io.eregion.managed":     "true",
		"io.eregion.kind":        "resource",
	}
}
