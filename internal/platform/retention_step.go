package platform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/retention"
)

// RegistryImage is the local registry, pinned by digest, not only by tag. The
// digest is the multi-architecture official registry:2.8.3 image index. Host
// convergence and the retention garbage-collect step both use exactly this
// image.
const RegistryImage = "registry:2.8.3@sha256:a3d8aaa63ed8681a604f1dea0aa03f100d5895b6a58ace528858a7b332415373"

// The registry's host paths. Host convergence may use another layout; the
// retention effects target these defaults unless the payload names them.
const (
	RegistryUnit          = "eregion-registry.service"
	RegistryContainer     = "eregion-registry"
	RegistryContainerPort = 5000
	RegistryConfigPath    = "/etc/eregion/registry/config.yml"
	DefaultRegistryAddr   = "127.0.0.1:5000"
)

var (
	retentionRemoveStepPattern = regexp.MustCompile(`^platform-retention-remove-[0-9a-f]{12}-v1$`)
	retentionGCStepPattern     = regexp.MustCompile(`^platform-retention-gc-01-v([0-9]{1,9})$`)
	registryRepositoryPattern  = regexp.MustCompile(`^[a-z0-9]+([._-][a-z0-9]+)*(/[a-z0-9]+([._-][a-z0-9]+)*)*$`)
	registryDigestPattern      = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	cacheVolumePattern         = regexp.MustCompile(`^(eregion-(build|launch)-cache-[0-9a-f]{2,80}_[0-9a-f]{2,80}|pack-cache-[a-z0-9][a-z0-9_.-]*\.(build|launch))$`)
	registryUnitPattern        = regexp.MustCompile(`^[a-z0-9][a-z0-9@._-]{0,127}\.service$`)
)

// RetentionRemovePayload removes one planner item from the node. The item's
// kind is a retention.ItemKind, so the planner, run record and node effect use
// one vocabulary. Reference rules:
//
//   - release / image / dangling_image: an image ID, digest or reference
//   - registry_artifact:        <repository>@sha256:<digest>; the delete is
//     scoped to that repository, never to the bare digest
//   - buildpack_cache:          the cache volume name
//   - container:                the container name
type RetentionRemovePayload struct {
	Step        string `json:"step"`
	Kind        string `json:"kind"`
	Reference   string `json:"reference"`
	Registry    string `json:"registry,omitempty"`
	App         string `json:"app,omitempty"`
	Environment string `json:"environment,omitempty"`
}

// RetentionRemoveStepID derives the per-item step identity. One operation can
// remove several items without their journal records colliding, and the same
// kind and reference always produce the same effect key.
func RetentionRemoveStepID(kind retention.ItemKind, reference string) string {
	sum := sha256.Sum256([]byte(string(kind) + "\x00" + reference))
	return "platform-retention-remove-" + hex.EncodeToString(sum[:6]) + "-v1"
}

// RegistryArtifactReference renders the reference form of one registry
// manifest.
func RegistryArtifactReference(repository, digest string) string {
	return repository + "@" + digest
}

func parseRegistryArtifactReference(reference string) (repository, digest string, err error) {
	repository, digest, ok := strings.Cut(strings.TrimSpace(reference), "@")
	if !ok || repository == "" || digest == "" {
		return "", "", errors.New("a registry artifact reference must be repository@sha256:<digest>")
	}
	return repository, digest, nil
}

func buildRetentionRemoveStep(exec engine.Execution, raw json.RawMessage) (engine.Step, error) {
	var payload RetentionRemovePayload
	if err := decodePayload(exec, raw, &payload); err != nil {
		return nil, err
	}
	if payload.Step != exec.Step {
		return nil, fmt.Errorf("step ID %q does not match the payload step %q", exec.Step, payload.Step)
	}
	kind := retention.ItemKind(payload.Kind)
	switch kind {
	case retention.ItemRelease, retention.ItemImage, retention.ItemDanglingImage:
		if err := ValidateImage(payload.Reference); err != nil {
			return nil, err
		}
	case retention.ItemRegistryArtifact:
		repository, digest, err := parseRegistryArtifactReference(payload.Reference)
		if err != nil {
			return nil, err
		}
		if !registryRepositoryPattern.MatchString(repository) {
			return nil, fmt.Errorf("invalid registry repository %q", repository)
		}
		if !registryDigestPattern.MatchString(digest) {
			return nil, fmt.Errorf("invalid registry digest %q", digest)
		}
	case retention.ItemBuildpackCache:
		if !cacheVolumePattern.MatchString(payload.Reference) {
			return nil, errors.New("only an eregion build/launch cache or legacy pack cache volume can be pruned")
		}
	case retention.ItemContainer:
		if !containerNamePattern.MatchString(payload.Reference) {
			return nil, errors.New("invalid container name")
		}
	default:
		return nil, fmt.Errorf("unknown retention item kind %q", payload.Kind)
	}
	if payload.App != "" {
		if err := ValidateName(payload.App); err != nil {
			return nil, err
		}
	}
	if payload.Environment != "" {
		if err := ValidateName(payload.Environment); err != nil {
			return nil, err
		}
	}
	if kind == retention.ItemRegistryArtifact {
		address, err := normalizeRegistryAddress(payload.Registry)
		if err != nil {
			return nil, err
		}
		payload.Registry = address
	} else {
		payload.Registry = ""
	}
	if expected := RetentionRemoveStepID(kind, payload.Reference); payload.Step != expected {
		// The step ID is derived from the item, so a mismatched ID would make
		// the same reference look like two different effects.
		return nil, fmt.Errorf("step ID %q does not match the item identity %q", payload.Step, expected)
	}
	return &RetentionRemoveStep{exec: exec, payload: payload}, nil
}

// normalizeRegistryAddress validates a loopback registry endpoint. A retention
// effect never reaches a remote registry.
func normalizeRegistryAddress(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = DefaultRegistryAddr
	}
	host, port, err := net.SplitHostPort(value)
	if err != nil {
		return "", fmt.Errorf("invalid registry address %q: %w", value, err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return "", errors.New("the registry address must be a loopback literal IP")
	}
	if port == "" || strings.HasPrefix(port, "0") {
		return "", errors.New("invalid registry port")
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return "", errors.New("invalid registry port")
	}
	return value, nil
}

// RetentionRemoveStep performs one idempotent removal. Already-gone state is
// success: retention runs may be retried after a partial prune.
type RetentionRemoveStep struct {
	exec    engine.Execution
	payload RetentionRemovePayload
}

func (s *RetentionRemoveStep) ID() string { return s.payload.Step }
func (s *RetentionRemoveStep) Describe() string {
	return "Remove retention item " + s.payload.Kind + " " + s.payload.Reference
}
func (s *RetentionRemoveStep) CancelBoundary() engine.Boundary { return engine.BoundaryAfterApply }
func (s *RetentionRemoveStep) Inputs() ([]byte, error)         { return json.Marshal(s.payload) }

func (s *RetentionRemoveStep) dockerCheckArgs() []string {
	switch retention.ItemKind(s.payload.Kind) {
	case retention.ItemRelease, retention.ItemImage, retention.ItemDanglingImage:
		return []string{"docker", "image", "inspect", "--format", "{{.Id}}", s.payload.Reference}
	case retention.ItemBuildpackCache:
		return []string{"docker", "volume", "inspect", "--format", "{{.Name}}", s.payload.Reference}
	}
	return nil
}

// retentionContainerInspectFormat reads the immutable ID and ownership labels
// in one Docker call. Container removal never trusts a name alone.
const retentionContainerInspectFormat = `{{.Id}}|{{index .Config.Labels "io.eregion.app"}}|{{index .Config.Labels "io.eregion.environment"}}|{{.Name}}`

type retentionContainer struct {
	ID          string
	App         string
	Environment string
	Name        string
}

func (s *RetentionRemoveStep) inspectContainer(ctx context.Context, rt engine.Runtime) (retentionContainer, bool, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	stdout, stderr, err := runArgv(probeCtx, []string{
		"docker", "inspect", "--type=container", "--format", retentionContainerInspectFormat, s.payload.Reference,
	}, nil, rt, s.ID(), nil)
	if err != nil {
		if alreadyGoneString(stderr) {
			return retentionContainer{}, false, nil
		}
		return retentionContainer{}, false, retentionCommandError("inspect container "+s.payload.Reference, err, stderr)
	}
	parts := strings.SplitN(strings.TrimSpace(stdout), "|", 4)
	if len(parts) != 4 || parts[0] == "" {
		return retentionContainer{}, false, fmt.Errorf("%w: the node returned an unreadable container inspection", engine.ErrPermanent)
	}
	value := func(part string) string {
		if part == "<no value>" {
			return ""
		}
		return part
	}
	return retentionContainer{
		ID: parts[0], App: value(parts[1]), Environment: value(parts[2]),
		Name: strings.TrimPrefix(parts[3], "/"),
	}, true, nil
}

// verifyContainer refuses to remove anything but an owned workload container.
// The label check runs at execution time, so a recycled name cannot delete an
// infrastructure container.
func (s *RetentionRemoveStep) verifyContainer(container retentionContainer) error {
	name := container.Name
	if name == "" {
		name = s.payload.Reference
	}
	if name == "eregion-registry" || s.payload.Reference == "eregion-registry" {
		return fmt.Errorf("%w: %s is an eregion infrastructure container and is never removed by retention", engine.ErrPermanent, name)
	}
	if container.App == "" {
		return fmt.Errorf("%w: container %s has no io.eregion.app ownership label", engine.ErrPermanent, name)
	}
	if s.payload.App != "" && container.App != s.payload.App {
		return fmt.Errorf("%w: container %s belongs to app %q, not %q", engine.ErrPermanent, name, container.App, s.payload.App)
	}
	if s.payload.Environment != "" && container.Environment != s.payload.Environment {
		return fmt.Errorf("%w: container %s belongs to environment %q, not %q", engine.ErrPermanent, name, container.Environment, s.payload.Environment)
	}
	return nil
}

func (s *RetentionRemoveStep) Check(ctx context.Context, rt engine.Runtime) (engine.CheckResult, error) {
	kind := retention.ItemKind(s.payload.Kind)
	if kind == retention.ItemRegistryArtifact {
		exists, err := s.registryManifestExists(ctx)
		if err != nil {
			return engine.CheckResult{}, err
		}
		if exists {
			return engine.CheckResult{Detail: "registry manifest " + s.payload.Reference + " still exists"}, nil
		}
		return engine.CheckResult{Complete: true, Detail: "registry manifest " + s.payload.Reference + " is already gone"}, nil
	}
	if kind == retention.ItemContainer {
		container, exists, err := s.inspectContainer(ctx, rt)
		if err != nil {
			return engine.CheckResult{}, err
		}
		if !exists {
			return engine.CheckResult{Complete: true, Detail: "container " + s.payload.Reference + " is already gone"}, nil
		}
		if err := s.verifyContainer(container); err != nil {
			return engine.CheckResult{}, err
		}
		return engine.CheckResult{Detail: "container " + container.Name + " (" + container.ID + ") still exists"}, nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	_, stderr, err := runArgv(probeCtx, s.dockerCheckArgs(), nil, rt, s.ID(), nil)
	if err != nil {
		if alreadyGoneString(stderr) {
			return engine.CheckResult{Complete: true, Detail: s.payload.Kind + " " + s.payload.Reference + " is already gone"}, nil
		}
		return engine.CheckResult{}, retentionCommandError("inspect "+s.payload.Reference, err, stderr)
	}
	return engine.CheckResult{Detail: s.payload.Kind + " " + s.payload.Reference + " still exists"}, nil
}

// retentionDockerTimeout bounds one Docker mutation. Removing an image or a
// cache volume is local and short; a stuck daemon must not hold the operation.
const retentionDockerTimeout = 2 * time.Minute

func (s *RetentionRemoveStep) skipResult(reason string) engine.ApplyResult {
	if strings.TrimSpace(reason) == "" {
		reason = "the node refused the removal with a deterministic conflict"
	}
	return engine.ApplyResult{Result: map[string]any{
		"kind": s.payload.Kind, "reference": s.payload.Reference, "skipped": true, "reason": reason,
	}}
}

func (s *RetentionRemoveStep) Apply(ctx context.Context, rt engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	kind := retention.ItemKind(s.payload.Kind)
	if kind == retention.ItemRegistryArtifact {
		return s.applyRegistryDelete(ctx, report)
	}
	switch kind {
	case retention.ItemRelease, retention.ItemImage, retention.ItemDanglingImage:
		return s.applyImageRemoval(ctx, rt, report)
	}
	runCtx, cancel := context.WithTimeout(ctx, retentionDockerTimeout)
	defer cancel()
	var argv []string
	switch kind {
	case retention.ItemContainer:
		container, exists, err := s.inspectContainer(ctx, rt)
		if err != nil {
			return engine.ApplyResult{}, err
		}
		if !exists {
			return engine.ApplyResult{Result: map[string]any{
				"kind": s.payload.Kind, "reference": s.payload.Reference, "already_gone": true,
			}}, nil
		}
		if err := s.verifyContainer(container); err != nil {
			return engine.ApplyResult{}, err
		}
		// Remove the immutable ID, not the name: the name may have been
		// recycled between inventory and execution.
		argv = []string{"docker", "rm", "--force", container.ID}
	case retention.ItemBuildpackCache:
		argv = []string{"docker", "volume", "rm", s.payload.Reference}
	default:
		return engine.ApplyResult{}, fmt.Errorf("%w: unknown retention item kind %q", engine.ErrPermanent, s.payload.Kind)
	}
	_, stderr, err := runArgv(runCtx, argv, nil, rt, s.ID(), report)
	if err != nil {
		if alreadyGoneString(stderr) {
			return engine.ApplyResult{Result: map[string]any{
				"kind": s.payload.Kind, "reference": s.payload.Reference, "already_gone": true,
			}}, nil
		}
		if reason := retentionDockerConflict(stderr); reason != "" {
			return s.skipResult(reason), nil
		}
		return engine.ApplyResult{}, retentionCommandError("remove "+s.payload.Kind+" "+s.payload.Reference, err, stderr)
	}
	return engine.ApplyResult{Result: map[string]any{
		"kind": s.payload.Kind, "reference": s.payload.Reference, "removed": true,
	}}, nil
}

// applyImageRemoval never forces an image removal. Docker refuses to remove an
// image ID while more than one repository tag points at it, so each current tag
// is verified against the immutable image ID and removed by name; removing the
// last tag removes the image. A tag that no longer resolves to the planned
// image is left alone and reported as a skip.
func (s *RetentionRemoveStep) applyImageRemoval(ctx context.Context, rt engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	runCtx, cancel := context.WithTimeout(ctx, retentionDockerTimeout)
	defer cancel()
	target := normalizeImageID(s.payload.Reference)
	tags, exists, err := s.imageRepoTags(runCtx, rt)
	if err != nil {
		return engine.ApplyResult{}, err
	}
	if !exists {
		return engine.ApplyResult{Result: map[string]any{
			"kind": s.payload.Kind, "reference": s.payload.Reference, "already_gone": true,
		}}, nil
	}

	if len(tags) == 0 {
		_, stderr, err := runArgv(runCtx, []string{"docker", "image", "rm", s.payload.Reference}, nil, rt, s.ID(), report)
		if err != nil {
			if alreadyGoneString(stderr) {
				return engine.ApplyResult{Result: map[string]any{
					"kind": s.payload.Kind, "reference": s.payload.Reference, "already_gone": true,
				}}, nil
			}
			if reason := retentionDockerConflict(stderr); reason != "" {
				return s.skipResult(reason), nil
			}
			return engine.ApplyResult{}, retentionCommandError("remove "+s.payload.Kind+" "+s.payload.Reference, err, stderr)
		}
		return engine.ApplyResult{Result: map[string]any{
			"kind": s.payload.Kind, "reference": s.payload.Reference, "removed": true,
		}}, nil
	}

	removedTags := make([]string, 0, len(tags))
	movedTags := make([]string, 0, len(tags))
	for _, tag := range tags {
		currentID, found, err := s.imageIDForReference(runCtx, rt, tag)
		if err != nil {
			return engine.ApplyResult{}, err
		}
		if !found {
			// The tag disappeared after the observation; nothing to remove.
			continue
		}
		if normalizeImageID(currentID) != target {
			movedTags = append(movedTags, tag)
			continue
		}
		_, stderr, err := runArgv(runCtx, []string{"docker", "image", "rm", tag}, nil, rt, s.ID(), report)
		if err != nil {
			if alreadyGoneString(stderr) {
				continue
			}
			if reason := retentionDockerConflict(stderr); reason != "" {
				return s.skipResult(reason), nil
			}
			return engine.ApplyResult{}, retentionCommandError("remove image tag "+tag, err, stderr)
		}
		removedTags = append(removedTags, tag)
	}
	if len(movedTags) > 0 {
		return s.skipResult("image tag(s) no longer resolve to " + target + ": " + strings.Join(movedTags, ", ")), nil
	}
	if len(removedTags) == 0 {
		return s.skipResult("no current tag resolves to image " + target), nil
	}
	// Untagging the last repository tag normally deletes the image, but a
	// digest-only reference or a reference added concurrently can keep it
	// alive. Report that honestly instead of claiming the bytes were freed.
	remainingTags, stillPresent, err := s.imageRepoTags(runCtx, rt)
	if err != nil {
		return engine.ApplyResult{}, err
	}
	if stillPresent {
		reason := "image still referenced by a digest-only local reference"
		if len(remainingTags) > 0 {
			reason = "image still referenced by tag(s) " + strings.Join(remainingTags, ", ")
		}
		return s.skipResult(reason), nil
	}
	return engine.ApplyResult{Result: map[string]any{
		"kind": s.payload.Kind, "reference": s.payload.Reference, "removed": true, "tags": removedTags,
	}}, nil
}

// imageRepoTags reads the current repository tags of one local image. A missing
// image is not an error: retention runs are retried after partial removal.
func (s *RetentionRemoveStep) imageRepoTags(ctx context.Context, rt engine.Runtime) ([]string, bool, error) {
	stdout, stderr, err := runArgv(ctx, []string{
		"docker", "image", "inspect", "--format", "{{json .RepoTags}}", s.payload.Reference,
	}, nil, rt, s.ID(), nil)
	if err != nil {
		if alreadyGoneString(stderr) {
			return nil, false, nil
		}
		return nil, false, retentionCommandError("inspect image tags "+s.payload.Reference, err, stderr)
	}
	raw := strings.TrimSpace(stdout)
	if raw == "" || raw == "null" {
		return nil, true, nil
	}
	var tags []string
	if err := json.Unmarshal([]byte(raw), &tags); err != nil {
		return nil, false, fmt.Errorf("%w: the node returned an unreadable image tag observation", engine.ErrPermanent)
	}
	sort.Strings(tags)
	unique := tags[:0]
	for _, tag := range tags {
		tag = strings.TrimSpace(tag)
		if tag == "" || (len(unique) > 0 && unique[len(unique)-1] == tag) {
			continue
		}
		unique = append(unique, tag)
	}
	return unique, true, nil
}

func (s *RetentionRemoveStep) imageIDForReference(ctx context.Context, rt engine.Runtime, reference string) (string, bool, error) {
	stdout, stderr, err := runArgv(ctx, []string{
		"docker", "image", "inspect", "--format", "{{.Id}}", reference,
	}, nil, rt, s.ID(), nil)
	if err != nil {
		if alreadyGoneString(stderr) {
			return "", false, nil
		}
		return "", false, retentionCommandError("inspect image "+reference, err, stderr)
	}
	imageID := strings.TrimSpace(stdout)
	if imageID == "" {
		return "", false, fmt.Errorf("%w: the node returned an empty image identity for %s", engine.ErrPermanent, reference)
	}
	return imageID, true, nil
}

func normalizeImageID(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

// retentionDockerConflict recognizes Docker's deterministic conflict refusals.
// They are per-item skips, not operation failures: retrying cannot make an
// image used by a container, or an image still referenced elsewhere, removable.
func retentionDockerConflict(text string) string {
	lowered := strings.ToLower(text)
	for _, phrase := range []string{
		"image is referenced in multiple repositories",
		"image is being used by",
		"is being used by stopped container",
		"container is using",
		"conflict:",
		"409 conflict",
		"volume is in use",
		"image has dependent child images",
	} {
		if strings.Contains(lowered, phrase) {
			return firstLine(text)
		}
	}
	return ""
}

// retentionCommandError keeps daemon unavailability, timeouts and other
// uncertain Docker failures retryable. Only a missing executable or an
// invalid command is permanent.
func retentionCommandError(action string, err error, stderr string) error {
	detail := firstLine(stderr)
	if detail == "" {
		detail = firstLine(err.Error())
	}
	message := strings.ToLower(err.Error() + " " + stderr)
	if errors.Is(err, exec.ErrNotFound) || strings.Contains(message, "executable file not found") {
		return fmt.Errorf("%w: %s: %s", engine.ErrPermanent, action, detail)
	}
	return fmt.Errorf("%s: %w: %s", action, err, detail)
}

func alreadyGoneString(text string) bool {
	lowered := strings.ToLower(text)
	return strings.Contains(lowered, "no such image") || strings.Contains(lowered, "no such container") ||
		strings.Contains(lowered, "no such object") || strings.Contains(lowered, "no such volume") ||
		strings.Contains(lowered, "not found")
}

// ------------------------------------------------------------ registry delete

var retentionRegistryHTTPClient = &http.Client{
	Timeout: 30 * time.Second,
	// The registry is a loopback service; an ambient HTTP proxy must never see
	// a delete request.
	Transport: &http.Transport{Proxy: nil},
	CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

var registryManifestAccept = strings.Join([]string{
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
	"application/vnd.docker.distribution.manifest.v2+json",
}, ", ")

func (s *RetentionRemoveStep) registryURL() (string, error) {
	repository, digest, err := parseRegistryArtifactReference(s.payload.Reference)
	if err != nil {
		return "", err
	}
	return (&url.URL{
		Scheme: "http",
		Host:   s.payload.Registry,
		Path:   "/v2/" + repository + "/manifests/" + digest,
	}).String(), nil
}

func (s *RetentionRemoveStep) registryManifestExists(ctx context.Context) (bool, error) {
	endpoint, err := s.registryURL()
	if err != nil {
		return false, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodHead, endpoint, nil)
	if err != nil {
		return false, err
	}
	request.Header.Set("Accept", registryManifestAccept)
	response, err := retentionRegistryHTTPClient.Do(request)
	if err != nil {
		return false, contract.NewFault(contract.Unavailable, "the local registry is not reachable", err.Error())
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		_ = response.Body.Close()
	}()
	switch response.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusNotFound:
		return false, nil
	case http.StatusMethodNotAllowed:
		// A registry without manifest reads cannot answer this check; report
		// presence so Apply issues the delete and its own status is decisive.
		return true, nil
	default:
		return false, contract.NewFault(contract.Unavailable, "the local registry answered the manifest check with "+response.Status, "")
	}
}

func (s *RetentionRemoveStep) applyRegistryDelete(ctx context.Context, report engine.Reporter) (engine.ApplyResult, error) {
	endpoint, err := s.registryURL()
	if err != nil {
		return engine.ApplyResult{}, fmt.Errorf("%w: %s", engine.ErrPermanent, err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return engine.ApplyResult{}, fmt.Errorf("%w: %s", engine.ErrPermanent, err)
	}
	request.Header.Set("Accept", registryManifestAccept)
	response, err := retentionRegistryHTTPClient.Do(request)
	if err != nil {
		return engine.ApplyResult{}, contract.NewFault(contract.Unavailable, "the local registry is not reachable", err.Error())
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		_ = response.Body.Close()
	}()
	switch {
	case response.StatusCode == http.StatusAccepted || response.StatusCode == http.StatusOK:
		if report != nil {
			report(engine.Event{Kind: "log", Step: s.ID(), Message: "deleted registry manifest " + s.payload.Reference})
		}
		return engine.ApplyResult{Result: map[string]any{
			"kind": s.payload.Kind, "reference": s.payload.Reference, "removed": true,
		}}, nil
	case response.StatusCode == http.StatusNotFound:
		return engine.ApplyResult{Result: map[string]any{
			"kind": s.payload.Kind, "reference": s.payload.Reference, "already_gone": true,
		}}, nil
	case response.StatusCode == http.StatusMethodNotAllowed:
		return engine.ApplyResult{}, fmt.Errorf("%w: the registry refuses deletes; apply host-registry-v2 to enable REGISTRY_STORAGE_DELETE_ENABLED", engine.ErrPermanent)
	case response.StatusCode == http.StatusConflict:
		return s.skipResult("the registry refused the manifest delete with " + response.Status), nil
	case response.StatusCode >= 500:
		return engine.ApplyResult{}, contract.NewFault(contract.Unavailable, "the local registry failed the manifest delete", response.Status)
	default:
		return engine.ApplyResult{}, fmt.Errorf("%w: the registry refused the manifest delete: %s", engine.ErrPermanent, response.Status)
	}
}

// ------------------------------------------------------------ garbage collect

// RetentionGCPayload is the registry garbage-collection effect. Empty paths
// mean the production layout. Generation distinguishes one GC pass from an
// earlier completed pass in the same retention run.
type RetentionGCPayload struct {
	Step        string `json:"step"`
	Generation  int    `json:"generation,omitempty"`
	RegistryDir string `json:"registry_dir,omitempty"`
	ConfigPath  string `json:"config_path,omitempty"`
	Unit        string `json:"unit,omitempty"`
	Image       string `json:"image,omitempty"`
}

// RetentionGCStepID returns the durable effect identity of one GC pass. Zero
// means the historical fixed ID (v1); new passes use v2, v3, … so a completed
// earlier pass cannot satisfy a later one, while every ID still satisfies the
// operation-step grammar ending in -v<number>.
func RetentionGCStepID(generation int) string {
	if generation <= 0 {
		return "platform-retention-gc-01-v1"
	}
	return fmt.Sprintf("platform-retention-gc-01-v%d", generation+1)
}

func buildRetentionGCStep(exec engine.Execution, raw json.RawMessage) (engine.Step, error) {
	var payload RetentionGCPayload
	if err := decodePayload(exec, raw, &payload); err != nil {
		return nil, err
	}
	if payload.Step != exec.Step {
		return nil, fmt.Errorf("step ID %q does not match the payload step %q", exec.Step, payload.Step)
	}
	matches := retentionGCStepPattern.FindStringSubmatch(payload.Step)
	if matches == nil {
		return nil, errors.New("invalid registry garbage-collect step")
	}
	version, err := strconv.Atoi(matches[1])
	if err != nil || version < 1 {
		return nil, errors.New("invalid registry garbage-collect step version")
	}
	stepGeneration := 0
	if version > 1 {
		stepGeneration = version - 1
	}
	switch {
	case payload.Generation < 0:
		return nil, errors.New("invalid registry garbage-collect generation")
	case payload.Generation != stepGeneration:
		return nil, errors.New("the registry garbage-collect step does not match its generation")
	}
	payload.Generation = stepGeneration
	payload.RegistryDir = defaultString(payload.RegistryDir, DefaultRegistryDir)
	payload.ConfigPath = defaultString(payload.ConfigPath, RegistryConfigPath)
	payload.Unit = defaultString(payload.Unit, RegistryUnit)
	payload.Image = defaultString(payload.Image, RegistryImage)
	for name, value := range map[string]string{
		"registry directory": payload.RegistryDir, "registry configuration": payload.ConfigPath,
	} {
		if !filepath.IsAbs(value) || strings.ContainsAny(value, "\x00\r\n") {
			return nil, fmt.Errorf("the %s must be an absolute path without control characters", name)
		}
	}
	if !registryUnitPattern.MatchString(payload.Unit) {
		return nil, errors.New("invalid registry systemd unit")
	}
	if err := ValidateImage(payload.Image); err != nil {
		return nil, err
	}
	if !strings.Contains(payload.Image, "@sha256:") {
		return nil, errors.New("the registry garbage-collect image must be pinned by digest")
	}
	return &RetentionGCStep{exec: exec, payload: payload}, nil
}

func defaultString(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return strings.TrimSpace(value)
}

// shellQuote renders one argument for the embedded bash scripts.
func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'\''`) + "'"
}

// RetentionGCStep stops the registry, garbage-collects its unreferenced blobs
// in a one-off container, and restarts it. It never passes --delete-untagged:
// eregion references releases by digest, so an untagged manifest can still be
// the live artifact of a release. Only manifests explicitly deleted through the
// registry API become unreferenced, and plain garbage collection removes their
// blobs.
type RetentionGCStep struct {
	exec    engine.Execution
	payload RetentionGCPayload
}

func (s *RetentionGCStep) ID() string                      { return s.payload.Step }
func (s *RetentionGCStep) Describe() string                { return "Garbage-collect the local OCI registry" }
func (s *RetentionGCStep) CancelBoundary() engine.Boundary { return engine.BoundaryAfterApply }
func (s *RetentionGCStep) Inputs() ([]byte, error)         { return json.Marshal(s.payload) }

func (s *RetentionGCStep) Check(context.Context, engine.Runtime) (engine.CheckResult, error) {
	return engine.CheckResult{Detail: "registry garbage collection runs only when a prune requests it"}, nil
}

// script renders the GC effect. A failed collection restarts the registry, but
// a stale collector is handled fail-closed: the registry is stopped, the old
// collector is removed and verified gone (both the container and any running
// registry garbage-collect process) before the restore trap is armed. If that
// cannot be verified, the script exits with the registry still stopped.
func (s *RetentionGCStep) script() string {
	unit := shellQuote(s.payload.Unit)
	return `set -euo pipefail
unit=` + unit + `
collector=eregion-registry-gc

# Stop the serving registry first: no writer may touch the storage while a
# stale collector is being removed.
if ! systemctl stop "$unit"; then
  echo "failed to stop $unit; registry garbage collection was not started" >&2
  exit 1
fi
if systemctl is-active --quiet "$unit"; then
  echo "$unit is still active after stop; registry garbage collection was not started" >&2
  exit 1
fi

# A killed attempt may have left the collector behind. Remove it and prove it
# is gone before arming the restore trap; otherwise leave the registry stopped
# and say how to recover.
stale_collector() {
  /usr/bin/docker container inspect "$collector" >/dev/null 2>&1
}
if stale_collector; then
  if ! /usr/bin/docker rm --force "$collector" >/dev/null 2>&1; then
    echo "stale registry garbage-collector $collector could not be removed; $unit is left stopped. Remove it with 'docker rm -f $collector', verify no registry garbage-collect process is running, start $unit, then retry." >&2
    exit 1
  fi
fi
if stale_collector; then
  echo "registry garbage-collector $collector is still present after removal; $unit is left stopped. Remove it with 'docker rm -f $collector', verify no registry garbage-collect process is running, start $unit, then retry." >&2
  exit 1
fi
if ! running=$(/usr/bin/docker ps --quiet --no-trunc); then
  echo "could not list containers to check for a stale registry garbage-collector; $unit is left stopped. Verify no registry garbage-collect process is running, start $unit, then retry." >&2
  exit 1
fi
if [ -n "$running" ]; then
  if ! commands=$(/usr/bin/docker inspect --type=container --format '{{.Config.Entrypoint}} {{.Config.Cmd}}' $running); then
    echo "could not inspect running containers to check for a stale registry garbage-collector; $unit is left stopped. Verify no registry garbage-collect process is running, start $unit, then retry." >&2
    exit 1
  fi
  if printf '%s\n' "$commands" | grep -q 'garbage-collect'; then
    echo "a registry garbage-collect process is still running; $unit is left stopped. Wait for it to exit or stop it, verify no container is running 'garbage-collect', start $unit, then retry." >&2
    exit 1
  fi
fi

# The orphan is gone and nothing is writing. Create our own collector; only
# then arm the trap, so a failed collection still restarts the registry.
if ! /usr/bin/docker create --name "$collector" --network none \
  --volume ` + shellQuote(s.payload.RegistryDir) + `:/var/lib/registry \
  --volume ` + shellQuote(s.payload.ConfigPath) + `:/etc/docker/registry/config.yml:ro \
  ` + shellQuote(s.payload.Image) + ` /bin/registry garbage-collect /etc/docker/registry/config.yml >/dev/null; then
  echo "could not create the registry garbage-collector; $unit is stopped. Fix the collector error and retry, or start $unit manually." >&2
  exit 1
fi
restore_registry() {
  status=$?
  if ! systemctl start "$unit"; then
    echo "failed to restart $unit after garbage collection" >&2
    status=1
  fi
  exit "$status"
}
trap restore_registry EXIT
/usr/bin/docker start --attach "$collector"
if ! /usr/bin/docker rm --force "$collector" >/dev/null 2>&1; then
  echo "warning: could not remove the finished registry garbage-collector $collector; a retry will remove it" >&2
fi
systemctl start "$unit"
systemctl is-active --quiet "$unit"`
}

func (s *RetentionGCStep) Apply(ctx context.Context, rt engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	if err := (engine.Shell{Secrets: rt.Secrets}).Run(ctx, s.ID(), s.script(), nil, report); err != nil {
		return engine.ApplyResult{}, err
	}
	return engine.ApplyResult{Result: map[string]any{"unit": s.payload.Unit, "collected": true}}, nil
}
