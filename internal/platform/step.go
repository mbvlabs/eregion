package platform

import (
	"bytes"
	"context"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/mbvlabs/eregion/internal/buildpacks"
	"github.com/mbvlabs/eregion/internal/containerresolver"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/output"
	"github.com/mbvlabs/eregion/internal/secrets"
)

// Family is one node step family. Node builds its default registry from these
// without importing controlplane code.
type Family struct {
	Match func(string) bool
	Build func(engine.Execution, json.RawMessage) (engine.Step, error)
}

var (
	argvStepPattern       = regexp.MustCompile(`^platform-(argv|node|pull|image|cleanup|preflight|port|network)-[0-9a-z]+(-[0-9a-z]+)*-v1$`)
	containerStepPattern  = regexp.MustCompile(`^platform-container-[0-9a-z]+(-[0-9a-z]+)*-v1$`)
	healthStepPattern     = regexp.MustCompile(`^platform-health-[0-9a-z]+(-[0-9a-z]+)*-v1$`)
	packStepPattern       = regexp.MustCompile(`^platform-pack-[0-9]{2}-v1$`)
	readyImageStepPattern = regexp.MustCompile(`^platform-ready-image-[0-9]{2}-v1$`)
	buildStopStepPattern  = regexp.MustCompile(`^platform-build-stop-[0-9]{2}-v1$`)
	containerNamePattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)
	labelKeyPattern       = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,61}[a-z0-9])?$`)
)

//go:embed scripts/git-askpass.sh
var gitAskpassScript []byte

// StepFamilies returns the platform step families for the node registry.
func StepFamilies() []Family {
	return []Family{
		{Match: argvStepPattern.MatchString, Build: buildArgvStep},
		{Match: containerStepPattern.MatchString, Build: buildContainerStep},
		{Match: releaseCommandStepPattern.MatchString, Build: buildReleaseCommandStep},
		{Match: healthStepPattern.MatchString, Build: buildHealthStep},
		{Match: packStepPattern.MatchString, Build: buildPackStep},
		{Match: readyImageStepPattern.MatchString, Build: buildReadyImageStep},
		{Match: buildStopStepPattern.MatchString, Build: buildBuildStopStep},
		{Match: deleteStepPattern.MatchString, Build: buildDeleteStep},
		{Match: retentionRemoveStepPattern.MatchString, Build: buildRetentionRemoveStep},
		{Match: retentionGCStepPattern.MatchString, Build: buildRetentionGCStep},
		{Match: resourceStepPattern.MatchString, Build: buildResourceStep},
		{Match: resourceVolumeRemoveStepPattern.MatchString, Build: buildResourceVolumeRemoveStep},
		{Match: candidateDiagnosticStepPattern.MatchString, Build: buildCandidateDiagnosticStep},
	}
}

func buildResourceStep(exec engine.Execution, raw json.RawMessage) (engine.Step, error) {
	switch {
	case strings.HasPrefix(exec.Step, "platform-resource-network-"),
		strings.HasPrefix(exec.Step, "platform-resource-pull-"),
		strings.HasPrefix(exec.Step, "platform-resource-image-"):
		return buildArgvStep(exec, raw)
	case strings.HasPrefix(exec.Step, "platform-resource-volume-"):
		return buildResourceVolumeStep(exec, raw)
	case strings.HasPrefix(exec.Step, "platform-resource-run-"):
		return buildResourceRunStep(exec, raw)
	case strings.HasPrefix(exec.Step, "platform-resource-health-"):
		return buildResourceHealthStep(exec, raw)
	case strings.HasPrefix(exec.Step, "platform-resource-destroy-"):
		return buildResourceDestroyStep(exec, raw)
	default:
		return nil, fmt.Errorf("resource step %q has no builder", exec.Step)
	}
}

func decodePayload(exec engine.Execution, raw json.RawMessage, dst any) error {
	if len(raw) == 0 {
		return fmt.Errorf("platform step payload is required")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(dst); err != nil {
		return fmt.Errorf("invalid platform step payload: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------- argv step

type ArgvPayload struct {
	Step          string            `json:"step"`
	Argv          []string          `json:"argv"`
	CheckArgv     []string          `json:"check_argv,omitempty"`
	Env           map[string]string `json:"env,omitempty"`
	SecretEnvKeys []string          `json:"secret_env_keys,omitempty"`
	AllowFailure  bool              `json:"allow_failure,omitempty"`
	// MissingOK makes a "No such container" removal result a success. It is
	// only for idempotent cleanup, never for a step that creates state.
	MissingOK bool `json:"missing_ok,omitempty"`
	// IsolatedDocker runs the command with an explicit private empty Docker
	// config, never the node user's HOME/DOCKER_CONFIG or credential helpers.
	IsolatedDocker bool `json:"isolated_docker,omitempty"`
	TimeoutSeconds int  `json:"timeout_seconds,omitempty"`
}

type ArgvStep struct {
	exec    engine.Execution
	payload ArgvPayload
}

func buildArgvStep(exec engine.Execution, raw json.RawMessage) (engine.Step, error) {
	var payload ArgvPayload
	if err := decodePayload(exec, raw, &payload); err != nil {
		return nil, err
	}
	if payload.Step != exec.Step {
		return nil, fmt.Errorf("step ID %q does not match the payload step %q", exec.Step, payload.Step)
	}
	if err := validateArgv(payload.Argv); err != nil {
		return nil, err
	}
	// Container lifecycle create/start is reserved for the typed release
	// command step; it is not a generic argv action.
	if filepath.Base(payload.Argv[0]) == "docker" && len(payload.Argv) > 1 && (payload.Argv[1] == "create" || payload.Argv[1] == "start") {
		return nil, errors.New("docker create and start are reserved for typed release commands")
	}
	if len(payload.CheckArgv) > 0 {
		if err := validateArgv(payload.CheckArgv); err != nil {
			return nil, err
		}
		if filepath.Base(payload.CheckArgv[0]) == "docker" && len(payload.CheckArgv) > 1 && (payload.CheckArgv[1] == "create" || payload.CheckArgv[1] == "start") {
			return nil, errors.New("docker create and start are reserved for typed release commands")
		}
	}
	if payload.TimeoutSeconds == 0 {
		payload.TimeoutSeconds = 120
	}
	if payload.TimeoutSeconds < 1 || payload.TimeoutSeconds > 900 {
		return nil, errors.New("platform step timeout must be 1–900 seconds")
	}
	if err := validateEnv(payload.Env, "step environment"); err != nil {
		return nil, err
	}
	for _, key := range payload.SecretEnvKeys {
		if !envKeyPattern.MatchString(key) {
			return nil, errors.New("invalid secret environment key")
		}
	}
	step := &ArgvStep{exec: exec, payload: payload}
	if exec.Step == "platform-network-01-v1" {
		if len(payload.Argv) != 10 || !slices.Equal(payload.Argv[:5], []string{"docker", "network", "create", "--driver", "bridge"}) ||
			payload.Argv[5] != "--label" || payload.Argv[7] != "--label" ||
			!strings.HasPrefix(payload.Argv[6], "io.eregion.app=") || !strings.HasPrefix(payload.Argv[8], "io.eregion.environment=") ||
			ValidateName(strings.TrimPrefix(payload.Argv[6], "io.eregion.app=")) != nil ||
			ValidateName(strings.TrimPrefix(payload.Argv[8], "io.eregion.environment=")) != nil ||
			!strings.HasPrefix(payload.Argv[9], "er-") ||
			EnvironmentID(strings.TrimPrefix(payload.Argv[9], "er-")).Validate() != nil ||
			!slices.Equal(payload.CheckArgv, []string{"docker", "network", "inspect", "--format", "{{.Name}}", payload.Argv[9]}) ||
			len(payload.Env) != 0 || len(payload.SecretEnvKeys) != 0 || payload.AllowFailure || payload.MissingOK || payload.IsolatedDocker || payload.TimeoutSeconds != 30 {
			return nil, errors.New("invalid exact deployment network ensure")
		}
		return &deploymentNetworkStep{ArgvStep: step, network: payload.Argv[9]}, nil
	}
	return step, nil
}

type deploymentNetworkStep struct {
	*ArgvStep
	network string
}

func (s *deploymentNetworkStep) SharedHostResources() []string {
	return []string{"docker-network\x00" + s.network}
}

func (s *ArgvStep) ID() string                      { return s.payload.Step }
func (s *ArgvStep) Describe() string                { return "Run a bounded node command" }
func (s *ArgvStep) CancelBoundary() engine.Boundary { return engine.BoundaryAfterApply }

func (s *ArgvStep) Inputs() ([]byte, error) { return json.Marshal(s.payload) }

func (s *ArgvStep) Check(ctx context.Context, rt engine.Runtime) (engine.CheckResult, error) {
	if len(s.payload.CheckArgv) == 0 {
		return engine.CheckResult{Complete: false, Detail: "command has no read-only completion check"}, nil
	}
	timeout := time.Duration(s.payload.TimeoutSeconds) * time.Second
	if timeout > 20*time.Second {
		timeout = 20 * time.Second
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var env []string
	cleanup := func() {}
	if s.payload.IsolatedDocker {
		var envErr error
		env, cleanup, envErr = s.commandEnv(rt)
		if envErr != nil {
			return engine.CheckResult{Complete: false, Detail: "isolated Docker configuration is unavailable"}, nil
		}
	}
	defer cleanup()
	_, _, err := s.run(probeCtx, s.payload.CheckArgv, env, rt, nil)
	if err != nil {
		return engine.CheckResult{Complete: false, Detail: "node command check did not succeed"}, nil
	}
	return engine.CheckResult{Complete: true, Detail: "node command check already succeeded"}, nil
}

func (s *ArgvStep) commandEnv(rt engine.Runtime) ([]string, func(), error) {
	env, err := stepEnv(s.payload.Env, s.payload.SecretEnvKeys, rt)
	if err != nil {
		return nil, func() {}, fmt.Errorf("%w: %s", engine.ErrPermanent, err)
	}
	if s.payload.IsolatedDocker {
		isolated, cleanup, isoErr := IsolatedDockerEnvironment()
		if isoErr != nil {
			return nil, func() {}, fmt.Errorf("%w: %s", engine.ErrPermanent, isoErr)
		}
		env = append(env, isolated...)
		return env, cleanup, nil
	}
	return env, func() {}, nil
}

func (s *ArgvStep) run(ctx context.Context, argv, env []string, rt engine.Runtime, report engine.Reporter) (string, string, error) {
	return s.runWithOutputInfo(ctx, argv, env, rt, report, nil)
}

func (s *ArgvStep) runWithOutputInfo(ctx context.Context, argv, env []string, rt engine.Runtime, report engine.Reporter, info *commandOutputInfo) (string, string, error) {
	if s.payload.IsolatedDocker {
		return runArgvWithEnvGraceCapture(ctx, argv, env, rt, s.ID(), report, 0, info)
	}
	return runArgvWithEnvGraceCapture(ctx, argv, mergeEnv(os.Environ(), env), rt, s.ID(), report, 0, info)
}

func (s *ArgvStep) Apply(ctx context.Context, rt engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	timeout := time.Duration(s.payload.TimeoutSeconds) * time.Second
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	extraEnv, cleanup, err := s.commandEnv(rt)
	if err != nil {
		return engine.ApplyResult{}, err
	}
	defer cleanup()
	var info commandOutputInfo
	_, stderr, err := s.runWithOutputInfo(runCtx, s.payload.Argv, extraEnv, rt, report, &info)
	stdout := info.stdoutSafe
	if err != nil && s.payload.MissingOK && containerAlreadyGone(stderr) {
		return engine.ApplyResult{Result: map[string]any{
			"stdout": truncate(stdout, 8192), "missing": true,
		}}, nil
	}
	if err != nil && s.payload.AllowFailure {
		return engine.ApplyResult{Result: map[string]any{
			"stdout": truncate(stdout, 8192), "stderr": truncate(stderr, 4096), "exit_ignored": true,
		}}, nil
	}
	if err != nil {
		wrapped := permanentCommandError(err)
		if s.payload.MissingOK {
			detail := firstLine(stderr)
			if detail == "" {
				detail = err.Error()
			}
			wrapped = fmt.Errorf("%w: %s", wrapped, detail)
		}
		return engine.ApplyResult{Result: StepExecutionEvidence{Stage: s.ID(), OutputAvailability: "bounded", RedactionContext: originalRedactionContext(rt), Stdout: truncate(stdout, 8192), Stderr: truncate(stderr, 4096), StdoutTruncated: info.stdoutTruncated || len(stdout) > 8192, StderrTruncated: info.stderrTruncated || len(stderr) > 4096}}, wrapped
	}
	return engine.ApplyResult{Result: map[string]any{"stdout": truncate(stdout, 8192)}}, nil
}

// containerAlreadyGone classifies Docker's idempotent-removal answer. A
// missing container is success; every other stderr line is a real failure.
func containerAlreadyGone(stderr string) bool {
	lowered := strings.ToLower(stderr)
	return strings.Contains(lowered, "no such container") || strings.Contains(lowered, "no such object")
}

func stepEnv(values map[string]string, secretKeys []string, rt engine.Runtime) ([]string, error) {
	env := make([]string, 0, len(values)+len(secretKeys))
	for key, value := range values {
		env = append(env, key+"="+value)
	}
	for _, key := range secretKeys {
		value, ok := rt.SecretValues[key]
		if !ok || value == "" {
			return nil, fmt.Errorf("secret value for %s is unavailable", key)
		}
		env = append(env, key+"="+value)
	}
	return env, nil
}

// ----------------------------------------------------------- container step

type ContainerPayload struct {
	Step          string `json:"step"`
	Name          string `json:"name"`
	Image         string `json:"image"`
	FallbackImage string `json:"fallback_image,omitempty"`
	ContainerPort int    `json:"container_port,omitempty"`
	HostPort      int    `json:"host_port,omitempty"`
	// Publish selects a loopback port binding. HostPort 0 asks Docker to
	// allocate a free port on the node; the applied result records it.
	Publish bool              `json:"publish,omitempty"`
	Bind    string            `json:"bind,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	// SecretEnvKeys names environment variables resolved from the transient
	// request secrets. Values are never part of this payload or the journal.
	SecretEnvKeys []string `json:"secret_env_keys,omitempty"`
	// Command is the process command: run through the Buildpacks launcher for
	// a Buildpacks image, as deploycrate does, otherwise as the entrypoint.
	// A bare command runs through the mounted resolver, which searches the
	// effective container PATH and cwd without changing either.
	Command []string          `json:"command,omitempty"`
	Labels  map[string]string `json:"labels,omitempty"`
	Restart string            `json:"restart,omitempty"`
	// Network is the environment's bridge network; NetworkAlias is the
	// process name other containers on it reach this one by.
	Network      string `json:"network,omitempty"`
	NetworkAlias string `json:"network_alias,omitempty"`
}

type ContainerStep struct {
	exec    engine.Execution
	payload ContainerPayload
	hash    string
}

func buildContainerStep(exec engine.Execution, raw json.RawMessage) (engine.Step, error) {
	var payload ContainerPayload
	if err := decodePayload(exec, raw, &payload); err != nil {
		return nil, err
	}
	if payload.Step != exec.Step {
		return nil, fmt.Errorf("step ID %q does not match the payload step %q", exec.Step, payload.Step)
	}
	if !containerNamePattern.MatchString(payload.Name) {
		return nil, errors.New("invalid container name")
	}
	if err := ValidateImage(payload.Image); err != nil {
		return nil, err
	}
	if payload.FallbackImage != "" {
		if err := ValidateImage(payload.FallbackImage); err != nil {
			return nil, err
		}
	}
	if payload.ContainerPort < 0 || payload.ContainerPort > 65535 || payload.HostPort < 0 || payload.HostPort > 65535 {
		return nil, errors.New("container and host ports must be 0–65535")
	}
	if !payload.Publish && (payload.ContainerPort > 0 || payload.HostPort > 0) {
		// Older payloads expressed publication by declaring a port.
		payload.Publish = true
	}
	if payload.Publish {
		if payload.ContainerPort < 1 {
			return nil, errors.New("a published container needs a container port")
		}
		if payload.Bind == "" {
			payload.Bind = "127.0.0.1"
		}
		if net.ParseIP(payload.Bind) == nil || !net.ParseIP(payload.Bind).IsLoopback() {
			return nil, errors.New("container publish bind must be a loopback literal IP")
		}
	} else if payload.ContainerPort != 0 || payload.HostPort != 0 {
		return nil, errors.New("an unpublished container cannot declare ports")
	}
	if payload.Restart == "" {
		payload.Restart = "no"
	}
	if payload.Restart != "no" && payload.Restart != "unless-stopped" {
		return nil, errors.New("unsupported container restart policy")
	}
	if err := validateEnv(payload.Env, "container environment"); err != nil {
		return nil, err
	}
	for _, key := range payload.SecretEnvKeys {
		if !envKeyPattern.MatchString(key) {
			return nil, errors.New("invalid container secret environment key")
		}
	}
	if len(payload.Command) > 129 {
		return nil, errors.New("container command has too many arguments")
	}
	for _, arg := range payload.Command {
		if len(arg) > 4096 || strings.ContainsAny(arg, "\x00\r\n") {
			return nil, errors.New("container command argument is too long or contains a control character")
		}
	}
	for key, value := range payload.Labels {
		if err := validateLabelKey(key); err != nil {
			return nil, err
		}
		if len(value) > 128 || strings.ContainsAny(value, "\x00\r\n") {
			return nil, fmt.Errorf("container label %q has an invalid value", key)
		}
	}
	hash, err := containerConfigHash(payload)
	if err != nil {
		return nil, err
	}
	return &ContainerStep{exec: exec, payload: payload, hash: hash}, nil
}

func containerConfigHash(payload ContainerPayload) (string, error) {
	raw, err := json.Marshal(struct {
		Image         string            `json:"image"`
		ContainerPort int               `json:"container_port"`
		HostPort      int               `json:"host_port"`
		Publish       bool              `json:"publish"`
		Bind          string            `json:"bind"`
		Env           map[string]string `json:"env"`
		SecretEnvKeys []string          `json:"secret_env_keys"`
		Command       []string          `json:"command"`
		Labels        map[string]string `json:"labels"`
		Restart       string            `json:"restart"`
	}{payload.Image, payload.ContainerPort, payload.HostPort, payload.Publish, payload.Bind, payload.Env, payload.SecretEnvKeys, payload.Command, payload.Labels, payload.Restart})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func (s *ContainerStep) ID() string { return s.payload.Step }
func (s *ContainerStep) Describe() string {
	return "Ensure container " + s.payload.Name + " runs " + s.payload.Image
}
func (s *ContainerStep) CancelBoundary() engine.Boundary { return engine.BoundaryAfterApply }

func (s *ContainerStep) Inputs() ([]byte, error) {
	return json.Marshal(struct {
		ContainerPayload
		ConfigHash string `json:"config_hash"`
	}{s.payload, s.hash})
}

func (s *ContainerStep) inspect(ctx context.Context) (running bool, image, config string, exists bool, err error) {
	args := []string{"docker", "inspect", "--type=container", "--format",
		"{{.State.Running}}|{{.Config.Image}}|{{index .Config.Labels \"io.eregion.config\"}}", s.payload.Name}
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	stdout, stderr, runErr := runArgv(probeCtx, args, nil, engine.Runtime{}, s.ID(), nil)
	if runErr != nil {
		if strings.Contains(strings.ToLower(stderr), "no such") || strings.Contains(strings.ToLower(stderr), "not found") {
			return false, "", "", false, nil
		}
		return false, "", "", false, runErr
	}
	parts := strings.Split(strings.TrimSpace(stdout), "|")
	if len(parts) != 3 {
		return false, "", "", true, nil
	}
	return parts[0] == "true", parts[1], parts[2], true, nil
}

func (s *ContainerStep) Check(ctx context.Context, rt engine.Runtime) (engine.CheckResult, error) {
	running, image, config, exists, err := s.inspect(ctx)
	if err != nil {
		return engine.CheckResult{}, err
	}
	if !exists {
		return engine.CheckResult{Complete: false, Detail: "container " + s.payload.Name + " is absent"}, nil
	}
	if running && image == s.payload.Image && config == s.hash {
		return engine.CheckResult{Complete: true, Detail: "container " + s.payload.Name + " already runs the desired image and configuration"}, nil
	}
	return engine.CheckResult{Complete: false, Detail: "container " + s.payload.Name + " exists with other state; it will be replaced"}, nil
}

func (s *ContainerStep) Apply(ctx context.Context, rt engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	args, env, err := s.runArgs(ctx, s.payload.Image, rt)
	if err != nil {
		return engine.ApplyResult{}, err
	}
	var previous *ExecutionEvidence
	if _, _, _, exists, _ := s.inspect(ctx); exists {
		evidence := captureExecutionEvidence(ctx, s.payload.Name, s.payload.Labels, rt)
		previous = &evidence
	}
	removeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	_, _, _ = runArgv(removeCtx, []string{"docker", "rm", "-f", s.payload.Name}, nil, rt, s.ID(), nil)
	cancel()
	runCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	var startInfo commandOutputInfo
	_, stderr, err := runArgvWithEnvGraceCapture(runCtx, args, mergeEnv(os.Environ(), env), rt, s.ID(), report, 0, &startInfo)
	cancel()
	if err != nil {
		current := captureExecutionEvidence(ctx, s.payload.Name, s.payload.Labels, rt)
		failure := engine.ApplyResult{Result: StepExecutionEvidence{Stage: "container-start", OutputAvailability: "bounded", RedactionContext: originalRedactionContext(rt), Stderr: truncate(stderr, 4096), StderrTruncated: startInfo.stderrTruncated || len(stderr) > 4096, Previous: previous, Container: &current}}
		if s.payload.FallbackImage != "" {
			fallbackArgs, fallbackEnv, fallbackEnvErr := s.runArgs(ctx, s.payload.FallbackImage, rt)
			if fallbackEnvErr != nil {
				return failure, fallbackEnvErr
			}
			fallbackCtx, fallbackCancel := context.WithTimeout(ctx, 2*time.Minute)
			_, _, fallbackErr := runArgv(fallbackCtx, fallbackArgs, fallbackEnv, rt, s.ID(), report)
			fallbackCancel()
			if fallbackErr == nil {
				return failure, fmt.Errorf("%w: starting %s failed (%s); the previous image was restored", engine.ErrPermanent, s.payload.Image, firstLine(stderr))
			}
		}
		return failure, fmt.Errorf("%w: docker run failed: %s", engine.ErrPermanent, firstLine(stderr))
	}
	hostPort := s.payload.HostPort
	if s.payload.Publish && hostPort == 0 {
		portCtx, portCancel := context.WithTimeout(ctx, 20*time.Second)
		var portInfo commandOutputInfo
		output, portStderr, portErr := runArgvWithEnvGraceCapture(portCtx, []string{"docker", "port", s.payload.Name, fmt.Sprintf("%d/tcp", s.payload.ContainerPort)}, os.Environ(), rt, s.ID(), nil, 0, &portInfo)
		portCancel()
		if portErr != nil {
			evidence := captureExecutionEvidence(ctx, s.payload.Name, s.payload.Labels, rt)
			cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			_, _, _ = runArgv(cleanupCtx, []string{"docker", "rm", "-f", s.payload.Name}, nil, rt, s.ID(), nil)
			cleanupCancel()
			return engine.ApplyResult{Result: StepExecutionEvidence{Stage: "container-port", OutputAvailability: "bounded", RedactionContext: originalRedactionContext(rt), Stderr: truncate(portStderr, 4096), StderrTruncated: portInfo.stderrTruncated || len(portStderr) > 4096, Previous: previous, Container: &evidence}}, fmt.Errorf("%w: reading the dynamic loopback port failed: %s", engine.ErrPermanent, firstLine(portStderr))
		}
		hostPort, err = ParsePublishedPort(output, s.payload.ContainerPort)
		if err != nil {
			evidence := captureExecutionEvidence(ctx, s.payload.Name, s.payload.Labels, rt)
			cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			_, _, _ = runArgv(cleanupCtx, []string{"docker", "rm", "-f", s.payload.Name}, nil, rt, s.ID(), nil)
			cleanupCancel()
			return engine.ApplyResult{Result: StepExecutionEvidence{Stage: "container-port", OutputAvailability: "bounded", RedactionContext: originalRedactionContext(rt), Stdout: truncate(portInfo.stdoutSafe, 8192), StdoutTruncated: portInfo.stdoutTruncated || len(portInfo.stdoutSafe) > 8192, Previous: previous, Container: &evidence}}, fmt.Errorf("%w: %s", engine.ErrPermanent, err)
		}
	}
	if report != nil {
		report(engine.Event{Kind: "log", Step: s.ID(), Message: "container " + s.payload.Name + " runs " + s.payload.Image})
	}
	result := map[string]any{
		"container": s.payload.Name,
		"image":     s.payload.Image,
	}
	if previous != nil {
		result["previous_container_evidence"] = previous
		result["redaction_context"] = originalRedactionContext(rt)
	}
	if s.payload.Publish {
		result["host_port"] = hostPort
		result["container_port"] = s.payload.ContainerPort
	}
	return engine.ApplyResult{Result: result}, nil
}

// PublishArgument renders Docker's loopback publish argument. A zero host port
// asks the node's Docker daemon to allocate a free port.
func PublishArgument(bind string, hostPort, containerPort int) string {
	if bind == "" {
		bind = "127.0.0.1"
	}
	if hostPort == 0 {
		return fmt.Sprintf("%s::%d", bind, containerPort)
	}
	return fmt.Sprintf("%s:%d:%d", bind, hostPort, containerPort)
}

// ParsePublishedPort parses `docker port <container> <port>/tcp` output. Docker
// may print one binding per family; the loopback IPv4 binding is preferred.
func ParsePublishedPort(output string, containerPort int) (int, error) {
	fallback := 0
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		host, portText, err := net.SplitHostPort(line)
		if err != nil {
			continue
		}
		port, err := strconv.Atoi(portText)
		if err != nil || port < 1 || port > 65535 {
			continue
		}
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() && ip.To4() != nil {
			return port, nil
		}
		if fallback == 0 {
			fallback = port
		}
	}
	if fallback == 0 {
		return 0, fmt.Errorf("Docker did not publish a host port for container port %d", containerPort)
	}
	return fallback, nil
}

func (s *ContainerStep) runArgs(ctx context.Context, image string, rt engine.Runtime) ([]string, []string, error) {
	prepared, err := prepareImageCommand(ctx, image, s.payload.Command, rt, s.ID())
	if err != nil {
		return nil, nil, err
	}
	args := []string{"docker", "run", "-d", "--name", s.payload.Name, "--pull", "never", "--restart", s.payload.Restart}
	if s.payload.Network != "" {
		args = append(args, "--network", s.payload.Network)
		if s.payload.NetworkAlias != "" {
			args = append(args, "--network-alias", s.payload.NetworkAlias)
		}
	}
	if s.payload.Publish {
		args = append(args, "-p", PublishArgument(s.payload.Bind, s.payload.HostPort, s.payload.ContainerPort))
	}
	args = append(args, "--label", "io.eregion.config="+s.hash)
	keys := sortedEnvKeys(s.payload.Labels)
	for _, key := range keys {
		args = append(args, "--label", key+"="+s.payload.Labels[key])
	}
	env := make([]string, 0, len(s.payload.Env)+len(s.payload.SecretEnvKeys)+1)
	for _, key := range sortedEnvKeys(s.payload.Env) {
		// The key alone tells Docker to read the value from the client
		// environment, so configuration values do not appear in the process
		// argument list on the node.
		args = append(args, "-e", key)
		env = append(env, key+"="+s.payload.Env[key])
	}
	for _, key := range s.payload.SecretEnvKeys {
		value, ok := rt.SecretValues[key]
		if !ok || value == "" {
			return nil, nil, fmt.Errorf("%w: secret value for %s is unavailable", engine.ErrPermanent, key)
		}
		args = append(args, "-e", key)
		env = append(env, key+"="+value)
	}
	for _, mount := range prepared.Mounts {
		args = append(args, "--mount", mount)
	}
	args = append(args, prepared.Arguments...)
	return args, env, nil
}

// buildpacksLabel marks an image built by Cloud Native Buildpacks.
const buildpacksLabel = "io.buildpacks.lifecycle.metadata"

// resolverExecutable is replaceable by tests. It must name the running static
// Linux eregion binary that will be bind-mounted read-only into the container.
var resolverExecutable = os.Executable

type preparedImageCommand struct {
	Arguments []string
	Mounts    []string
}

// prepareImageCommand keeps the image's normal command semantics. A Buildpacks
// command runs through the CNB launcher so it observes the completed launch
// environment. A bare command on either image kind runs through the same
// eregion binary mounted read-only at a fixed path; the hidden resolver searches
// the effective PATH in order, including relative and empty entries, then the
// working directory, and execs the target without a shell. Explicit paths and a
// commandless container keep their existing behavior and receive no mount.
func prepareImageCommand(
	ctx context.Context,
	image string,
	command []string,
	rt engine.Runtime,
	step string,
) (preparedImageCommand, error) {
	if len(command) == 0 {
		return preparedImageCommand{Arguments: []string{image}}, nil
	}
	buildpacks, err := buildpacksImage(ctx, image, rt, step)
	if err != nil {
		return preparedImageCommand{}, err
	}
	if command[0] == "" || strings.Contains(command[0], "/") {
		return preparedImageCommand{Arguments: processCommandArgs(image, command, buildpacks)}, nil
	}
	source, err := containerResolverSource()
	if err != nil {
		return preparedImageCommand{}, err
	}
	resolverArgs := make([]string, 0, len(command)+2)
	resolverArgs = append(resolverArgs, containerresolver.TargetPath, containerresolver.Invocation)
	resolverArgs = append(resolverArgs, command...)
	return preparedImageCommand{
		Arguments: processCommandArgs(image, resolverArgs, buildpacks),
		Mounts: []string{fmt.Sprintf(
			"type=bind,source=%s,destination=%s,readonly=true", source, containerresolver.TargetPath)},
	}, nil
}

// buildpacksImage reports whether image was built by Cloud Native Buildpacks.
// The inspect response is one label value, independent of PATH or image size.
func buildpacksImage(ctx context.Context, image string, rt engine.Runtime, step string) (bool, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	stdout, stderr, err := runArgv(probeCtx, []string{
		"docker", "inspect", "--type=image", "--format",
		"{{index .Config.Labels \"" + buildpacksLabel + "\"}}", image,
	}, nil, rt, step, nil)
	if err != nil {
		return false, classifyImageInspectError(ctx, err, stderr)
	}
	value := strings.TrimSpace(stdout)
	return value != "" && value != "<no value>", nil
}

// classifyImageInspectError keeps daemon and transport failures retryable,
// while a missing image, permission error, template incompatibility or invalid
// reference is terminal for this attempt.
func classifyImageInspectError(ctx context.Context, err error, stderr string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, exec.ErrNotFound) {
		return fmt.Errorf("%w: inspect image configuration: %w", engine.ErrPermanent, err)
	}
	message := strings.ToLower(stderr + " " + err.Error())
	switch {
	case strings.Contains(message, "no such image"),
		strings.Contains(message, "image not found"),
		strings.Contains(message, "no such object"),
		strings.Contains(message, "manifest unknown"),
		strings.Contains(message, "permission denied"),
		strings.Contains(message, "template parsing"),
		strings.Contains(message, "template:"),
		strings.Contains(message, "invalid reference"):
		return fmt.Errorf("%w: inspect image configuration: %s", engine.ErrPermanent, firstLine(stderr))
	default:
		return fmt.Errorf("inspect image configuration: %w: %s", err, firstLine(stderr))
	}
}
func containerResolverSource() (string, error) {
	executable, err := resolverExecutable()
	if err != nil {
		return "", fmt.Errorf("%w: locate the eregion resolver binary: %v", engine.ErrPermanent, err)
	}
	source, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return "", fmt.Errorf("%w: resolve the eregion resolver binary: %v", engine.ErrPermanent, err)
	}
	if !filepath.IsAbs(source) || strings.ContainsAny(source, ",\x00\r\n") {
		return "", fmt.Errorf("%w: the eregion resolver binary path cannot be bind-mounted safely", engine.ErrPermanent)
	}
	info, err := os.Stat(source)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("%w: the eregion resolver binary is not an executable regular file", engine.ErrPermanent)
	}
	return source, nil
}

// processCommandArgs is the image and command part of docker run. A
// Buildpacks image runs the command through its launcher, which sets up the
// buildpack environment; a plain image runs it as the entrypoint. With no
// command the image's own entrypoint and command run. A bare command has
// already been replaced by the mounted resolver invocation; explicit paths and
// commandless containers pass through unchanged.
func processCommandArgs(image string, command []string, buildpacks bool) []string {
	switch {
	case len(command) == 0:
		return []string{image}
	case buildpacks:
		return append([]string{"--entrypoint", "/cnb/lifecycle/launcher", image, "--"}, command...)
	default:
		return append([]string{"--entrypoint", command[0], image}, command[1:]...)
	}
}

// -------------------------------------------------------------- health step

type HealthPayload struct {
	Step           string `json:"step"`
	Type           string `json:"type"`
	Target         string `json:"target"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty"`
	IntervalMS     int    `json:"interval_ms,omitempty"`
}

type HealthStep struct {
	exec    engine.Execution
	payload HealthPayload
}

func buildHealthStep(exec engine.Execution, raw json.RawMessage) (engine.Step, error) {
	var payload HealthPayload
	if err := decodePayload(exec, raw, &payload); err != nil {
		return nil, err
	}
	if payload.Step != exec.Step {
		return nil, fmt.Errorf("step ID %q does not match the payload step %q", exec.Step, payload.Step)
	}
	if payload.Type != "http" && payload.Type != "tcp" {
		return nil, errors.New("health type must be http or tcp")
	}
	if payload.Target == "" || strings.ContainsAny(payload.Target, " \t\r\n\x00") {
		return nil, errors.New("invalid health target")
	}
	if err := validateLoopbackTarget(payload.Type, payload.Target); err != nil {
		return nil, err
	}
	if payload.TimeoutSeconds == 0 {
		payload.TimeoutSeconds = 30
	}
	if payload.TimeoutSeconds < 1 || payload.TimeoutSeconds > 300 {
		return nil, errors.New("health timeout must be 1–300 seconds")
	}
	if payload.IntervalMS == 0 {
		payload.IntervalMS = 500
	}
	if payload.IntervalMS < 100 || payload.IntervalMS > 5000 {
		return nil, errors.New("health interval must be 100–5000 ms")
	}
	return &HealthStep{exec: exec, payload: payload}, nil
}

func validateLoopbackTarget(kind, target string) error {
	if kind == "tcp" {
		host, _, err := net.SplitHostPort(target)
		if err != nil {
			return fmt.Errorf("invalid TCP health address: %w", err)
		}
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return errors.New("TCP health checks must use a loopback address")
		}
		return nil
	}
	index := strings.Index(target, "://")
	if index < 0 || target[:index] != "http" {
		return errors.New("HTTP health checks must use an http:// loopback URL")
	}
	rest := target[index+3:]
	slash := strings.IndexByte(rest, '/')
	host := rest
	if slash >= 0 {
		host = rest[:slash]
	}
	if colon := strings.LastIndexByte(host, ':'); colon >= 0 {
		host = host[:colon]
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil || !ip.IsLoopback() {
		return errors.New("HTTP health checks must use a loopback address")
	}
	return nil
}

func (s *HealthStep) ID() string                      { return s.payload.Step }
func (s *HealthStep) Describe() string                { return "Observe application health at " + s.payload.Target }
func (s *HealthStep) CancelBoundary() engine.Boundary { return engine.BoundaryAfterApply }
func (s *HealthStep) Inputs() ([]byte, error)         { return json.Marshal(s.payload) }

func (s *HealthStep) probe(ctx context.Context) (bool, string, int) {
	timeout := 3 * time.Second
	switch s.payload.Type {
	case "tcp":
		conn, err := net.DialTimeout("tcp", s.payload.Target, timeout)
		if err != nil {
			return false, "tcp connect failed", 0
		}
		_ = conn.Close()
		return true, "tcp connect succeeded", 0
	default:
		client := &http.Client{
			Timeout: timeout,
			// Loopback health checks must never be routed through an
			// environment proxy or follow a redirect off the node.
			Transport: &http.Transport{Proxy: nil},
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		}
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, s.payload.Target, nil)
		if err != nil {
			return false, "health request is invalid", 0
		}
		response, err := client.Do(request)
		if err != nil {
			return false, "health request failed", 0
		}
		defer response.Body.Close()
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		if response.StatusCode >= 200 && response.StatusCode < 400 {
			return true, "http status " + strconv.Itoa(response.StatusCode), response.StatusCode
		}
		return false, "http status " + strconv.Itoa(response.StatusCode), response.StatusCode
	}
}

func (s *HealthStep) Check(ctx context.Context, rt engine.Runtime) (engine.CheckResult, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	ok, detail, _ := s.probe(probeCtx)
	return engine.CheckResult{Complete: ok, Detail: detail}, nil
}

func (s *HealthStep) Apply(ctx context.Context, rt engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	deadline := time.Now().Add(time.Duration(s.payload.TimeoutSeconds) * time.Second)
	interval := time.Duration(s.payload.IntervalMS) * time.Millisecond
	checks := 0
	last := "no probe completed"
	for {
		checks++
		probeCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		ok, detail, status := s.probe(probeCtx)
		cancel()
		last = detail
		if ok {
			if report != nil {
				report(engine.Event{Kind: "log", Step: s.ID(), Message: fmt.Sprintf("health passed after %d check(s): %s", checks, detail)})
			}
			return engine.ApplyResult{Result: map[string]any{"healthy": true, "checks": checks, "last_status": status}}, nil
		}
		if time.Now().Add(interval).After(deadline) {
			return engine.ApplyResult{}, fmt.Errorf("%w: health check at %s did not pass within %ds: %s", engine.ErrPermanent, s.payload.Target, s.payload.TimeoutSeconds, last)
		}
		select {
		case <-ctx.Done():
			return engine.ApplyResult{}, engine.ErrInterrupted
		case <-time.After(interval):
		}
	}
}

// ---------------------------------------------------------------- pack step

const (
	// A pack build gets a dedicated Docker network so every lifecycle phase
	// container is attributable to this operation, step and attempt. The
	// private registry container is temporarily attached to it so a remote
	// publish never needs host networking.
	buildNetworkOwner          = "pack"
	buildNetworkOwnerLabel     = "io.eregion.build.owner"
	buildNetworkOperationLabel = "io.eregion.build.operation"
	buildNetworkStepLabel      = "io.eregion.build.step"
	buildNetworkAttemptLabel   = "io.eregion.build.attempt"
	buildNetworkCreateTimeout  = 30 * time.Second
	buildNetworkCleanupTimeout = 30 * time.Second
	buildNetworkDetailFormat   = "{{json .}}"
	// packBuildKillGrace lets pack remove its own lifecycle containers and
	// volumes after cancellation before it is killed. The owned-network
	// cleanup remains the backstop when pack cannot clean up in time.
	packBuildKillGrace = 5 * time.Second
	// packPublishPlatformAPIMinor is the first Platform API that makes pack's
	// --insecure-registry reach the lifecycle. Pack silently omits the flag
	// below it, so a successful build would otherwise fail to reach our
	// loopback HTTP registry.
	packPublishPlatformAPIMinor = 13
	// packSupportedPlatformAPIMinor is the latest Platform API pack 0.40.9
	// offers; the builder must advertise one of 0.13–0.15 for a supported
	// insecure publish.
	packSupportedPlatformAPIMinor = 15
	packCompletionSchema          = 1
)

type PackPayload struct {
	Step    string `json:"step"`
	Image   string `json:"image"`
	Path    string `json:"path,omitempty"`
	GitURL  string `json:"git_url,omitempty"`
	GitRef  string `json:"git_ref,omitempty"`
	Builder string `json:"builder"`
	// Registry is the node's actual configured loopback registry endpoint. It
	// is never defaulted in the read path: a missing or unreadable node
	// configuration fails the build instead of publishing somewhere else.
	Registry string `json:"registry"`
	// GitContextPath is the Buildpacks context inside the git checkout. It is
	// meaningless for a directory source and therefore valid only with GitURL.
	GitContextPath string                `json:"git_context_path,omitempty"`
	Env            map[string]string     `json:"env,omitempty"`
	GoTargets      []buildpacks.GoTarget `json:"go_targets,omitempty"`
	Buildpacks     *buildpacks.Settings  `json:"buildpacks,omitempty"`
	// GitCredentialSecretKey names the transient secret value used by the
	// embedded askpass helper. The value never enters the payload.
	GitCredentialSecretKey string `json:"git_credential_secret_key,omitempty"`
	TimeoutSeconds         int    `json:"timeout_seconds,omitempty"`
	// Cache and LaunchCache name the explicit Buildpacks cache volumes.
	// BuildpackCacheVolume and BuildpackLaunchCacheVolume derive the
	// deterministic per-environment names; an empty value keeps the builder's
	// own default (no named eregion cache to attribute or prune).
	Cache       string `json:"cache,omitempty"`
	LaunchCache string `json:"launch_cache,omitempty"`
}

// packCompletion is the durable evidence written after a published build has
// been pulled and verified but before the local completion tag exists. Check
// can therefore recover the exact commit and canonical registry reference
// after a crash without trusting a pre-existing local tag.
type packCompletion struct {
	Schema      int    `json:"schema"`
	OperationID string `json:"operation_id"`
	Step        string `json:"step"`
	Attempt     uint32 `json:"attempt"`
	InputsHash  string `json:"inputs_hash"`
	Image       string `json:"image"`
	Registry    string `json:"registry"`
	Platform    string `json:"platform"`
	Reference   string `json:"reference"`
	Digest      string `json:"digest"`
	ImageID     string `json:"image_id"`
	Commit      string `json:"commit,omitempty"`
}

func (c packCompletion) result() map[string]any {
	fields := map[string]any{
		"image":     c.Image,
		"image_id":  c.ImageID,
		"reference": c.Reference,
		"digest":    c.Digest,
	}
	if c.Commit != "" {
		fields["commit"] = c.Commit
	}
	return fields
}

type PackStep struct {
	exec    engine.Execution
	payload PackPayload
}

func buildPackStep(exec engine.Execution, raw json.RawMessage) (engine.Step, error) {
	var payload PackPayload
	if err := decodePayload(exec, raw, &payload); err != nil {
		return nil, err
	}
	if payload.Step != exec.Step {
		return nil, fmt.Errorf("step ID %q does not match the payload step %q", exec.Step, payload.Step)
	}
	if err := ValidateImage(payload.Image); err != nil {
		return nil, err
	}
	if strings.TrimSpace(payload.Registry) == "" {
		return nil, errors.New("pack payload requires the node's configured loopback registry endpoint")
	}
	registry, err := normalizeRegistryAddress(payload.Registry)
	if err != nil {
		return nil, fmt.Errorf("pack payload registry: %w", err)
	}
	payload.Registry = registry
	if payload.Builder != "" {
		if err := ValidateImage(payload.Builder); err != nil {
			return nil, err
		}
	}
	if (payload.Path == "") == (payload.GitURL == "") {
		return nil, errors.New("pack source must have exactly one of path or git_url")
	}
	if payload.Path != "" && !filepath.IsAbs(payload.Path) {
		return nil, errors.New("pack source path must be absolute")
	}
	if payload.GitContextPath != "" {
		if payload.GitURL == "" {
			return nil, errors.New("a git context path requires a git source")
		}
		if err := validateContextPath(payload.GitContextPath); err != nil {
			return nil, err
		}
	}
	if payload.GitURL != "" {
		if err := ValidateGitSourceURL(payload.GitURL); err != nil {
			return nil, err
		}
		switch {
		case payload.GitRef == "":
		case shaPattern.MatchString(payload.GitRef):
		case validGitRef(payload.GitRef):
		default:
			return nil, errors.New("invalid git ref")
		}
	}
	if err := validateEnv(payload.Env, "build environment"); err != nil {
		return nil, err
	}
	payload.GoTargets, err = buildpacks.NormalizeTargets(payload.GoTargets)
	if err != nil {
		return nil, err
	}
	payload.Buildpacks, err = buildpacks.NormalizeSettings(payload.Buildpacks)
	if err != nil {
		return nil, err
	}
	if payload.GitCredentialSecretKey != "" {
		if payload.GitURL == "" {
			return nil, errors.New("a git credential requires a git source")
		}
		if !envKeyPattern.MatchString(payload.GitCredentialSecretKey) {
			return nil, errors.New("invalid git credential secret key")
		}
	}
	if payload.TimeoutSeconds == 0 {
		payload.TimeoutSeconds = 600
	}
	if payload.TimeoutSeconds < 10 || payload.TimeoutSeconds > 1800 {
		return nil, errors.New("build timeout must be 10–1800 seconds")
	}
	if payload.Cache != "" && !cacheVolumePattern.MatchString(payload.Cache) {
		return nil, errors.New("a build cache must be an eregion build cache volume name")
	}
	if payload.LaunchCache != "" && !cacheVolumePattern.MatchString(payload.LaunchCache) {
		return nil, errors.New("a launch cache must be an eregion launch cache volume name")
	}
	step := &PackStep{exec: exec, payload: payload}
	if exec.Step == "platform-pack-02-v1" {
		return &sharedPackStep{PackStep: step}, nil
	}
	return step, nil
}

type sharedPackStep struct{ *PackStep }

func (s *sharedPackStep) SharedHostResources() []string {
	resources := []string{"docker-image\x00" + s.payload.Image}
	if s.payload.Cache != "" {
		resources = append(resources, "docker-volume\x00"+s.payload.Cache)
	}
	if s.payload.LaunchCache != "" {
		resources = append(resources, "docker-volume\x00"+s.payload.LaunchCache)
	}
	return resources
}

func (s *PackStep) ID() string { return s.payload.Step }
func (s *PackStep) Describe() string {
	return "Build " + s.payload.Image + " with Cloud Native Buildpacks"
}
func (s *PackStep) CancelBoundary() engine.Boundary { return engine.BoundaryAfterApply }
func (s *PackStep) Inputs() ([]byte, error)         { return json.Marshal(s.payload) }

func (s *PackStep) Check(ctx context.Context, rt engine.Runtime) (engine.CheckResult, error) {
	completion, exists, err := loadPackCompletion(rt.Root, s.exec.OperationID, s.payload.Step)
	if err != nil {
		return engine.CheckResult{}, err
	}
	if !exists {
		return engine.CheckResult{Detail: "no durable registry-backed build completion exists"}, nil
	}
	if !IsImageDigestReference(completion.Reference) || !registryDigestPattern.MatchString(completion.Digest) || !imageIDPattern.MatchString(completion.ImageID) || !strings.HasSuffix(completion.Reference, "@"+completion.Digest) {
		return engine.CheckResult{Detail: "the durable build completion has no valid immutable registry identity"}, nil
	}
	inputs, err := s.Inputs()
	if err != nil {
		return engine.CheckResult{}, err
	}
	expectedHash := engine.InputsHash(s.ID(), inputs)
	expectedPlatform, platformErr := buildpacks.TargetPlatform(runtime.GOARCH)
	if platformErr != nil {
		return engine.CheckResult{}, platformErr
	}
	if completion.InputsHash != expectedHash || completion.Image != s.payload.Image || completion.Registry != s.payload.Registry || completion.OperationID != string(s.exec.OperationID) || completion.Step != s.payload.Step || completion.Platform != expectedPlatform {
		return engine.CheckResult{Detail: "the durable build completion belongs to other inputs or another operation"}, nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	stdout, stderr, err := runArgvClean(probeCtx, []string{"docker", "image", "inspect", "--format", "{{.Id}}|{{json .RepoDigests}}", s.payload.Image}, nil, rt, s.ID(), nil)
	if err != nil {
		if imageMissingError(stderr) {
			return engine.CheckResult{Detail: "the local completion tag is absent"}, nil
		}
		return engine.CheckResult{}, permanentCommandError(err)
	}
	imageID, repoDigests, err := parsePackImageInspection(stdout)
	if err != nil {
		return engine.CheckResult{}, err
	}
	if imageID != completion.ImageID {
		return engine.CheckResult{Detail: "the local completion tag does not match the durable image config digest"}, nil
	}
	if !containsString(repoDigests, completion.Reference) {
		return engine.CheckResult{Detail: "the local completion tag does not carry the canonical registry digest"}, nil
	}
	return engine.CheckResult{
		Complete:       true,
		AppliedAttempt: completion.Attempt,
		Result:         completion.result(),
		Detail:         "recovered a verified registry-backed build completion (" + completion.Digest + ")",
	}, nil
}

func (s *PackStep) Apply(ctx context.Context, rt engine.Runtime, report engine.Reporter) (result engine.ApplyResult, applyErr error) {
	work := filepath.Join(rt.Root, "builds", string(s.exec.OperationID)+"-"+s.payload.Step)
	if err := os.RemoveAll(work); err != nil {
		return engine.ApplyResult{}, err
	}
	if err := os.MkdirAll(work, 0o700); err != nil {
		return engine.ApplyResult{}, err
	}
	defer os.RemoveAll(work)
	isolation, err := buildIsolation(work)
	if err != nil {
		return engine.ApplyResult{}, err
	}
	buildCtx, stopBuild := context.WithCancel(ctx)
	defer stopBuild()
	go watchBuildCancel(buildCtx, buildCancelPath(rt.Root, string(s.exec.OperationID), s.payload.Step), stopBuild)

	source := s.payload.Path
	commit := ""
	if s.payload.GitURL != "" {
		if err := CheckGitSourceResolution(ctx, s.payload.GitURL); err != nil {
			return engine.ApplyResult{}, fmt.Errorf("%w: %s", engine.ErrPermanent, err)
		}
		gitEnv, envErr := gitBuildEnvironment(isolation, work, s.payload.GitCredentialSecretKey, rt)
		if envErr != nil {
			return engine.ApplyResult{}, fmt.Errorf("%w: %s", engine.ErrPermanent, envErr)
		}
		source = filepath.Join(work, "src")
		args := []string{"git", "clone"}
		if shaPattern.MatchString(s.payload.GitRef) {
			// A commit SHA is immutable but is not a branch/tag; fetch the
			// repository without checkout and detach at the exact commit.
			args = append(args, "--no-checkout", "--filter=blob:none", s.payload.GitURL, source)
		} else {
			args = append(args, "--depth", "1")
			if s.payload.GitRef != "" {
				args = append(args, "--branch", s.payload.GitRef)
			}
			args = append(args, s.payload.GitURL, source)
		}
		cloneCtx, cancel := context.WithTimeout(buildCtx, 5*time.Minute)
		_, cloneStderr, err := runArgvClean(cloneCtx, args, gitEnv, rt, s.ID(), report)
		cancel()
		if err != nil {
			if buildCtx.Err() != nil {
				return engine.ApplyResult{}, engine.ErrInterrupted
			}
			return engine.ApplyResult{}, fmt.Errorf("%w: git clone failed: %s", engine.ErrPermanent, firstLine(cloneStderr))
		}
		if shaPattern.MatchString(s.payload.GitRef) {
			checkoutCtx, cancel := context.WithTimeout(buildCtx, 2*time.Minute)
			_, checkoutStderr, err := runArgvClean(checkoutCtx, []string{"git", "-C", source, "checkout", "--detach", s.payload.GitRef}, gitEnv, rt, s.ID(), report)
			cancel()
			if err != nil {
				if buildCtx.Err() != nil {
					return engine.ApplyResult{}, engine.ErrInterrupted
				}
				return engine.ApplyResult{}, fmt.Errorf("%w: git checkout failed: %s", engine.ErrPermanent, firstLine(checkoutStderr))
			}
		}
		resolveCtx, cancel := context.WithTimeout(buildCtx, 30*time.Second)
		stdout, resolveStderr, resolveErr := runArgvClean(resolveCtx, []string{"git", "-C", source, "rev-parse", "HEAD"}, gitEnv, rt, s.ID(), nil)
		cancel()
		if resolveErr != nil {
			if buildCtx.Err() != nil {
				return engine.ApplyResult{}, engine.ErrInterrupted
			}
			return engine.ApplyResult{}, fmt.Errorf("%w: git ref resolution failed: %s", engine.ErrPermanent, firstLine(resolveStderr))
		}
		commit = strings.TrimSpace(stdout)
		if !shaPattern.MatchString(commit) {
			return engine.ApplyResult{}, fmt.Errorf("%w: git ref did not resolve to an exact commit", engine.ErrPermanent)
		}
		if shaPattern.MatchString(s.payload.GitRef) && commit != s.payload.GitRef {
			return engine.ApplyResult{}, fmt.Errorf("%w: checked-out commit %s does not match the resolved commit %s", engine.ErrPermanent, commit, s.payload.GitRef)
		}
	}
	if s.payload.GitContextPath != "" {
		resolved, err := resolveGitContextPath(source, s.payload.GitContextPath)
		if err != nil {
			return engine.ApplyResult{}, fmt.Errorf("%w: %s", engine.ErrPermanent, err)
		}
		source = resolved
	}
	info, err := os.Stat(source) //nolint:gosec // G304: resolved source root or a directory under the node root.
	if err != nil || !info.IsDir() {
		return engine.ApplyResult{}, fmt.Errorf("%w: build source %s is not a readable directory on the node", engine.ErrPermanent, source)
	}
	builder, runImage := s.payload.Builder, ""
	if builder == "" {
		profile, err := buildpacks.ProfileForArchitecture(runtime.GOARCH)
		if err != nil {
			return engine.ApplyResult{}, fmt.Errorf("%w: %s", engine.ErrPermanent, err)
		}
		builder, runImage = profile.Builder, profile.RunImage
	}
	platform, err := buildpacks.TargetPlatform(runtime.GOARCH)
	if err != nil {
		return engine.ApplyResult{}, fmt.Errorf("%w: %s", engine.ErrPermanent, err)
	}
	if err := verifyBuilderPlatformAPI(buildCtx, rt, isolation, builder, platform, s.ID(), report); err != nil {
		if buildCtx.Err() != nil {
			return engine.ApplyResult{}, engine.ErrInterrupted
		}
		return engine.ApplyResult{}, fmt.Errorf("%w: %s", engine.ErrPermanent, err)
	}
	registry, err := inspectRegistryContainer(buildCtx, rt, isolation, s.payload.Registry, s.ID(), report)
	if err != nil {
		if buildCtx.Err() != nil {
			return engine.ApplyResult{}, engine.ErrInterrupted
		}
		return engine.ApplyResult{}, err
	}
	if err := removeStaleBuildNetworks(buildCtx, rt, isolation, registry.ID, s.exec, report); err != nil {
		if buildCtx.Err() != nil {
			return engine.ApplyResult{}, engine.ErrInterrupted
		}
		return engine.ApplyResult{}, err
	}
	networkCtx, cancelNetwork := context.WithTimeout(buildCtx, buildNetworkCreateTimeout)
	network, err := createBuildNetwork(networkCtx, rt, isolation, s.exec.OperationID, s.payload.Step, s.exec.Attempt)
	cancelNetwork()
	if err != nil {
		cleanupErr := cleanupAmbiguousBuildNetwork(ctx, rt, isolation, network, s.exec.OperationID, s.payload.Step, s.exec.Attempt, registry.ID)
		if cleanupErr != nil && report != nil {
			report(engine.Event{Kind: "warning", Step: s.ID(), Message: cleanupErr.Error()})
		}
		if buildCtx.Err() != nil {
			return engine.ApplyResult{}, engine.ErrInterrupted
		}
		if cleanupErr != nil {
			return engine.ApplyResult{}, fmt.Errorf("%w; cleaning the build network after an ambiguous create failed: %v", err, cleanupErr)
		}
		return engine.ApplyResult{}, err
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), buildNetworkCleanupTimeout)
		defer cancel()
		cleanupErr := removeBuildNetwork(cleanupCtx, rt, isolation, network, s.exec.OperationID, s.payload.Step, s.exec.Attempt, registry.ID)
		if cleanupErr == nil {
			return
		}
		if report != nil {
			report(engine.Event{Kind: "warning", Step: s.ID(), Message: cleanupErr.Error()})
		}
		if applyErr != nil {
			applyErr = fmt.Errorf("%w; cleaning the build network failed: %v", applyErr, cleanupErr)
		}
	}()
	transientEndpoint, err := attachRegistryToBuildNetwork(buildCtx, rt, isolation, network, registry, s.ID(), report)
	if err != nil {
		if buildCtx.Err() != nil {
			return engine.ApplyResult{}, engine.ErrInterrupted
		}
		return engine.ApplyResult{}, err
	}
	publishReference, repository, tag, err := packPublishReference(s.payload.Image, transientEndpoint)
	if err != nil {
		return engine.ApplyResult{}, fmt.Errorf("%w: %s", engine.ErrPermanent, err)
	}
	args := packBuildArgs(s.payload, source, network, builder, runImage, publishReference, platform, transientEndpoint)
	buildRunCtx, cancel := context.WithTimeout(buildCtx, time.Duration(s.payload.TimeoutSeconds)*time.Second)
	defer cancel()
	_, stderr, err := runArgvCleanPack(buildRunCtx, args, isolation, rt, s.ID(), report)
	if err != nil {
		if buildCtx.Err() != nil {
			return engine.ApplyResult{}, engine.ErrInterrupted
		}
		if errors.Is(err, exec.ErrNotFound) || strings.Contains(strings.ToLower(err.Error()), "executable file not found") {
			return engine.ApplyResult{}, fmt.Errorf("%w: pack is not installed on this node; install Buildpacks or use an image release", engine.ErrPermanent)
		}
		return engine.ApplyResult{}, fmt.Errorf("%w: pack build failed: %s", engine.ErrPermanent, commandDetail(err, stderr))
	}
	digest, err := resolvePublishedManifestDigest(buildCtx, s.payload.Registry, repository, tag)
	if err != nil {
		if buildCtx.Err() != nil {
			return engine.ApplyResult{}, engine.ErrInterrupted
		}
		return engine.ApplyResult{}, err
	}
	canonical := s.payload.Registry + "/" + repository + "@" + digest
	imageID, err := pullAndVerifyPublishedImage(buildCtx, rt, isolation, canonical, runtime.GOARCH, s.ID(), report)
	if err != nil {
		if buildCtx.Err() != nil {
			return engine.ApplyResult{}, engine.ErrInterrupted
		}
		return engine.ApplyResult{}, err
	}
	// The registry attachment and every lifecycle container belong to the
	// transient build. Clean them before the local completion tag so a failure
	// here cannot leave a tag that looks like a finished artifact.
	cleanupCtx, cancelCleanup := context.WithTimeout(context.WithoutCancel(ctx), buildNetworkCleanupTimeout)
	cleanupErr := removeBuildNetwork(cleanupCtx, rt, isolation, network, s.exec.OperationID, s.payload.Step, s.exec.Attempt, registry.ID)
	cancelCleanup()
	if cleanupErr != nil {
		return engine.ApplyResult{}, fmt.Errorf("cleaning the build network before completion failed: %w", cleanupErr)
	}
	if buildCtx.Err() != nil {
		return engine.ApplyResult{}, engine.ErrInterrupted
	}
	inputs, err := s.Inputs()
	if err != nil {
		return engine.ApplyResult{}, err
	}
	completion := packCompletion{
		Schema: packCompletionSchema, OperationID: string(s.exec.OperationID), Step: s.payload.Step,
		Attempt: s.exec.Attempt, InputsHash: engine.InputsHash(s.ID(), inputs),
		Image: s.payload.Image, Registry: s.payload.Registry, Platform: platform,
		Reference: canonical, Digest: digest, ImageID: imageID, Commit: commit,
	}
	if err := writePackCompletion(rt.Root, completion); err != nil {
		return engine.ApplyResult{}, err
	}
	taggedID, err := tagVerifiedPublishedImage(buildCtx, rt, isolation, s.payload.Image, canonical, s.ID(), report)
	if err != nil {
		if buildCtx.Err() != nil {
			return engine.ApplyResult{}, engine.ErrInterrupted
		}
		return engine.ApplyResult{}, err
	}
	if taggedID != imageID {
		return engine.ApplyResult{}, fmt.Errorf("%w: the local completion tag resolved to %s, not the verified registry image %s", engine.ErrPermanent, taggedID, imageID)
	}
	return engine.ApplyResult{Result: completion.result()}, nil
}

// packBuildArgs renders one Buildpacks build. Explicit volume caches keep the
// build and launch caches attributable to one environment; pack otherwise
// creates a randomly keyed launch volume on every build. The dedicated network
// makes every lifecycle container this build creates identifiable after a
// cancellation. Publish sends the finished app image to the private registry
// and selects the build node's exact target platform for both the pinned
// default builder and an explicit builder.
func packBuildArgs(payload PackPayload, source, network, builder, runImage, publishReference, platform, insecureRegistry string) []string {
	args := []string{"pack", "build", publishReference, "--publish", "--path", source, "--builder", builder}
	if runImage != "" {
		args = append(args, "--run-image", runImage)
	}
	if platform != "" {
		args = append(args, "--platform", platform)
	}
	if insecureRegistry != "" {
		args = append(args, "--insecure-registry", insecureRegistry)
	}
	if payload.Buildpacks != nil {
		for _, ref := range payload.Buildpacks.Order {
			args = append(args, "--buildpack", ref)
		}
	}
	if network != "" {
		args = append(args, "--network", network)
	}
	if payload.Cache != "" {
		args = append(args, "--cache", "type=build;format=volume;name="+payload.Cache)
	}
	if payload.LaunchCache != "" {
		// pack 0.40 parses one cache type per flag occurrence; two flags set
		// both the build and launch caches reliably.
		args = append(args, "--cache", "type=launch;format=volume;name="+payload.LaunchCache)
	}
	env := make(map[string]string, len(payload.Env))
	for key, value := range payload.Env {
		env[key] = value
	}
	for key, value := range buildpacks.Environment(payload.Buildpacks, payload.GoTargets) {
		env[key] = value
	}
	for _, key := range sortedEnvKeys(env) {
		args = append(args, "--env", key+"="+env[key])
	}
	return args
}

// buildNetworkName is deterministic for one operation, pack step and attempt.
// A retry gets a fresh attempt and therefore a fresh network instead of
// colliding with resources left by the earlier attempt.
func buildNetworkName(operationID contract.OperationID, step string, attempt uint32) string {
	sum := sha256.Sum256([]byte(string(operationID) + "\x00" + step + "\x00" + strconv.FormatUint(uint64(attempt), 10)))
	return "eregion-build-" + hex.EncodeToString(sum[:8])
}

// buildNetworkLabels are written when the network is created and verified
// before anything attached to it is removed.
func buildNetworkLabels(operationID contract.OperationID, step string, attempt uint32) map[string]string {
	return map[string]string{
		buildNetworkOwnerLabel:     buildNetworkOwner,
		buildNetworkOperationLabel: string(operationID),
		buildNetworkStepLabel:      step,
		buildNetworkAttemptLabel:   strconv.FormatUint(uint64(attempt), 10),
	}
}

func createBuildNetwork(ctx context.Context, rt engine.Runtime, isolation []string, operationID contract.OperationID, step string, attempt uint32) (string, error) {
	name := buildNetworkName(operationID, step, attempt)
	labels := buildNetworkLabels(operationID, step, attempt)
	args := []string{"docker", "network", "create"}
	for _, key := range sortedEnvKeys(labels) {
		args = append(args, "--label", key+"="+labels[key])
	}
	args = append(args, name)
	_, stderr, err := runArgvClean(ctx, args, isolation, rt, step, nil)
	if err != nil {
		return name, fmt.Errorf("creating the build network %s failed: %s", name, commandDetail(err, stderr))
	}
	return name, nil
}

// cleanupAmbiguousBuildNetwork handles a docker network create that returned an
// error after the request may already have reached the daemon. The build
// context can be cancelled by then, so the ownership check and removal use a
// fresh bounded context. The deterministic network is removed only after
// removeBuildNetwork verifies every ownership label; a foreign or pre-existing
// network is refused and left untouched.
func cleanupAmbiguousBuildNetwork(ctx context.Context, rt engine.Runtime, isolation []string, network string, operationID contract.OperationID, step string, attempt uint32, registryID string) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), buildNetworkCleanupTimeout)
	defer cancel()
	return removeBuildNetwork(cleanupCtx, rt, isolation, network, operationID, step, attempt, registryID)
}

type buildNetworkContainer struct {
	Name        string `json:"Name"`
	IPv4Address string `json:"IPv4Address"`
}

type buildNetworkInspection struct {
	Name       string                           `json:"Name"`
	Labels     map[string]string                `json:"Labels"`
	Containers map[string]buildNetworkContainer `json:"Containers"`
}

// inspectBuildNetwork reads the exact name, ownership labels and attached
// containers in one bounded Docker call. A missing network is reported as
// clean rather than as an error.
func inspectBuildNetwork(ctx context.Context, rt engine.Runtime, isolation []string, name, step string) (buildNetworkInspection, bool, error) {
	stdout, stderr, err := runArgvClean(ctx, []string{"docker", "network", "inspect", "--format", buildNetworkDetailFormat, name}, isolation, rt, step, nil)
	if err != nil {
		if buildNetworkGone(stderr) {
			return buildNetworkInspection{}, false, nil
		}
		return buildNetworkInspection{}, false, fmt.Errorf("inspecting build network %s failed: %s", name, commandDetail(err, stderr))
	}
	var inspection buildNetworkInspection
	if err := json.Unmarshal([]byte(strings.TrimSpace(stdout)), &inspection); err != nil {
		return buildNetworkInspection{}, false, fmt.Errorf("build network %s did not report its details: %v", name, err)
	}
	if inspection.Labels == nil {
		inspection.Labels = map[string]string{}
	}
	if inspection.Containers == nil {
		inspection.Containers = map[string]buildNetworkContainer{}
	}
	return inspection, true, nil
}

// removeBuildNetwork removes only containers attached to the network created
// for this exact build attempt, detaches the validated private registry, then
// removes the network itself. It verifies the ownership labels first so a name
// collision or a foreign network is never cleaned. The shared registry
// container is never stopped, removed or restarted here. A missing network is
// already clean.
func removeBuildNetwork(ctx context.Context, rt engine.Runtime, isolation []string, name string, operationID contract.OperationID, step string, attempt uint32, registryID string) error {
	expected := buildNetworkLabels(operationID, step, attempt)
	inspection, exists, err := inspectBuildNetwork(ctx, rt, isolation, name, step)
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	for key, want := range expected {
		if inspection.Labels[key] != want {
			return fmt.Errorf("refusing to clean build network %s: label %s does not have the expected ownership value", name, key)
		}
	}
	removeContainers := func() error {
		listed, listStderr, listErr := runArgvClean(ctx, []string{"docker", "ps", "-aq", "--filter", "network=" + name, "--filter", "label=author=pack"}, isolation, rt, step, nil)
		if listErr != nil {
			return fmt.Errorf("listing containers on build network %s failed: %s", name, commandDetail(listErr, listStderr))
		}
		ids := strings.Fields(listed)
		if len(ids) == 0 {
			return nil
		}
		args := append([]string{"docker", "rm", "-f"}, ids...)
		_, removeStderr, removeErr := runArgvClean(ctx, args, isolation, rt, step, nil)
		if removeErr != nil && !containerAlreadyGone(removeStderr) {
			return fmt.Errorf("removing containers on build network %s failed: %s", name, commandDetail(removeErr, removeStderr))
		}
		return nil
	}
	detachRegistry := func() error {
		if registryID == "" {
			return nil
		}
		if _, attached := inspection.Containers[registryID]; !attached {
			return nil
		}
		_, disconnectStderr, disconnectErr := runArgvClean(ctx, []string{"docker", "network", "disconnect", name, registryID}, isolation, rt, step, nil)
		if disconnectErr != nil && !buildNetworkNotConnected(disconnectStderr) && !buildNetworkGone(disconnectStderr) {
			return fmt.Errorf("detaching the registry container from build network %s failed: %s", name, commandDetail(disconnectErr, disconnectStderr))
		}
		return nil
	}
	for pass := 0; pass < 2; pass++ {
		if err := removeContainers(); err != nil {
			return err
		}
		if err := detachRegistry(); err != nil {
			return err
		}
		_, stderr, err := runArgvClean(ctx, []string{"docker", "network", "rm", name}, isolation, rt, step, nil)
		if err == nil || buildNetworkGone(stderr) {
			return nil
		}
		if pass == 1 || !buildNetworkInUse(stderr) {
			return fmt.Errorf("removing build network %s failed: %s", name, commandDetail(err, stderr))
		}
	}
	return nil
}

// removeStaleBuildNetworks cleans only this or earlier attempts of this exact effect.
// The engine holds its effect lock throughout this call. Other effects may be
// live or resumable even when no process currently holds the host lock.
func removeStaleBuildNetworks(ctx context.Context, rt engine.Runtime, isolation []string, registryID string, execution engine.Execution, report engine.Reporter) error {
	step := execution.Step
	listed, stderr, err := runArgvClean(ctx, []string{"docker", "network", "ls", "-q", "--filter", "label=" + buildNetworkOwnerLabel + "=" + buildNetworkOwner, "--filter", "label=" + buildNetworkOperationLabel + "=" + string(execution.OperationID), "--filter", "label=" + buildNetworkStepLabel + "=" + execution.Step}, isolation, rt, step, nil)
	if err != nil {
		return fmt.Errorf("listing stale build networks failed: %s", commandDetail(err, stderr))
	}
	for _, id := range strings.Fields(listed) {
		inspection, exists, err := inspectBuildNetwork(ctx, rt, isolation, id, step)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		if inspection.Labels[buildNetworkOwnerLabel] != buildNetworkOwner {
			return fmt.Errorf("refusing to clean build network %s: it does not carry the pack ownership label", id)
		}
		operationID := contract.OperationID(inspection.Labels[buildNetworkOperationLabel])
		if err := operationID.Validate(); err != nil {
			return fmt.Errorf("refusing to clean build network %s: its operation label is invalid", id)
		}
		networkStep := inspection.Labels[buildNetworkStepLabel]
		if !packStepPattern.MatchString(networkStep) {
			return fmt.Errorf("refusing to clean build network %s: its step label is invalid", id)
		}
		attemptValue := inspection.Labels[buildNetworkAttemptLabel]
		attempt, parseErr := strconv.ParseUint(attemptValue, 10, 32)
		if parseErr != nil || attempt == 0 {
			return fmt.Errorf("refusing to clean build network %s: its attempt label is invalid", id)
		}
		if expectedName := buildNetworkName(operationID, networkStep, uint32(attempt)); inspection.Name != expectedName {
			return fmt.Errorf("refusing to clean build network %s: it is not the deterministic network %s", inspection.Name, expectedName)
		}
		if operationID != execution.OperationID || networkStep != execution.Step || uint32(attempt) > execution.Attempt {
			continue
		}
		if err := removeBuildNetwork(ctx, rt, isolation, inspection.Name, operationID, networkStep, uint32(attempt), registryID); err != nil {
			return err
		}
		if report != nil {
			report(engine.Event{Kind: "log", Step: step, Message: "removed a stale build network left by an interrupted attempt"})
		}
	}
	return nil
}

// sweepCompletedBuildNetworks runs only under the retention effect's exclusive
// host fence. Failed, resumable, unreadable and locked effects remain untouched.
func sweepCompletedBuildNetworks(ctx context.Context, rt engine.Runtime, step string, report engine.Reporter) error {
	isolation, cleanup, err := IsolatedDockerEnvironment()
	if err != nil {
		return err
	}
	defer cleanup()
	out, _, err := runArgvClean(ctx, []string{"docker", "inspect", "--type=container", "--format", "{{.Id}}|{{.Name}}|{{.Config.Image}}", RegistryContainer}, isolation, rt, step, nil)
	if err != nil {
		return err
	}
	parts := strings.Split(strings.TrimSpace(out), "|")
	if len(parts) != 3 || len(parts[0]) != 64 || !shaPattern.MatchString(parts[0]) || strings.TrimPrefix(parts[1], "/") != RegistryContainer || parts[2] != RegistryImage {
		return errors.New("private registry identity is not verifiable")
	}
	registryID := parts[0]
	listed, _, err := runArgvClean(ctx, []string{"docker", "network", "ls", "-q", "--filter", "label=" + buildNetworkOwnerLabel + "=" + buildNetworkOwner}, isolation, rt, step, nil)
	if err != nil {
		return err
	}
	for _, id := range strings.Fields(listed) {
		inspection, exists, err := inspectBuildNetwork(ctx, rt, isolation, id, step)
		if err != nil {
			return err
		}
		if !exists || inspection.Labels[buildNetworkOwnerLabel] != buildNetworkOwner {
			continue
		}
		op := contract.OperationID(inspection.Labels[buildNetworkOperationLabel])
		networkStep := inspection.Labels[buildNetworkStepLabel]
		attempt, err := strconv.ParseUint(inspection.Labels[buildNetworkAttemptLabel], 10, 32)
		if op.Validate() != nil || networkStep != "platform-pack-02-v1" || err != nil || attempt == 0 ||
			strconv.FormatUint(attempt, 10) != inspection.Labels[buildNetworkAttemptLabel] ||
			inspection.Name != buildNetworkName(op, networkStep, uint32(attempt)) {
			continue
		}
		execution := engine.Execution{OperationID: op, Step: networkStep, Attempt: uint32(attempt)}
		release, err := engine.TryEffectLock(ctx, rt.Root, execution)
		if errors.Is(err, engine.ErrLockHeld) {
			continue
		}
		if err != nil {
			return err
		}
		err = sweepCompletedBuildNetwork(ctx, rt, isolation, registryID, execution)
		release()
		if err != nil {
			return err
		}
	}
	return nil
}

func sweepCompletedBuildNetwork(ctx context.Context, rt engine.Runtime, isolation []string, registryID string, execution engine.Execution) error {
	journal, err := engine.LoadJournal(engine.JournalPath(rt.Root))
	if err != nil {
		return err
	}
	task, ok := journal.Tasks[execution.EffectKey()]
	if !ok || task.Status != engine.TaskCompleted || task.OperationID != string(execution.OperationID) || task.Step != execution.Step ||
		task.AppliedAttempt < execution.Attempt || task.Attempt < execution.Attempt || task.CompletedAt == "" || len(task.InputsHash) != 64 || !shaPattern.MatchString(task.InputsHash) {
		return nil
	}
	record, ok, err := engine.LoadEffectRecord((&engine.Engine{StateDir: rt.Root}).EffectRecordPath(execution))
	if err != nil {
		return err
	}
	if !ok || record.OperationID != task.OperationID || record.Step != task.Step || record.InputsHash != task.InputsHash ||
		record.AppliedAttempt != task.AppliedAttempt || record.AppliedAttempt < execution.Attempt || record.CompletedAt == "" {
		return nil
	}
	name := buildNetworkName(execution.OperationID, execution.Step, execution.Attempt)
	return removeBuildNetwork(ctx, rt, isolation, name, execution.OperationID, execution.Step, execution.Attempt, registryID)
}

func buildNetworkGone(stderr string) bool {
	lowered := strings.ToLower(stderr)
	return strings.Contains(lowered, "no such network") ||
		(strings.Contains(lowered, "network") && strings.Contains(lowered, "not found"))
}

func buildNetworkInUse(stderr string) bool {
	lowered := strings.ToLower(stderr)
	return strings.Contains(lowered, "active endpoint") || strings.Contains(lowered, "connected to network")
}

func buildNetworkNotConnected(stderr string) bool {
	lowered := strings.ToLower(stderr)
	return strings.Contains(lowered, "not connected") || strings.Contains(lowered, "is not connected to network")
}

type registryContainer struct {
	ID    string
	Image string
}

type registryPortBinding struct {
	HostIP   string `json:"HostIp"`
	HostPort string `json:"HostPort"`
}

// inspectRegistryContainer validates the shared private registry by exact
// container ID, pinned image reference, running state and the actual loopback
// publication. A missing or foreign container is never adopted for a build.
func inspectRegistryContainer(ctx context.Context, rt engine.Runtime, isolation []string, endpoint, step string, report engine.Reporter) (registryContainer, error) {
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil {
		return registryContainer{}, fmt.Errorf("%w: configured registry endpoint %q is invalid: %v", engine.ErrPermanent, endpoint, err)
	}
	stdout, stderr, err := runArgvClean(ctx, []string{
		"docker", "inspect", "--type=container", "--format",
		"{{.Id}}|{{.Name}}|{{.Config.Image}}|{{.State.Running}}|{{json .HostConfig.PortBindings}}",
		RegistryContainer,
	}, isolation, rt, step, nil)
	if err != nil {
		if imageMissingError(stderr) || strings.Contains(strings.ToLower(stderr), "no such container") {
			return registryContainer{}, fmt.Errorf("%w: the private registry container %s is not available; apply the registry host step before building", engine.ErrPermanent, RegistryContainer)
		}
		return registryContainer{}, fmt.Errorf("inspecting the private registry container failed: %s", commandDetail(err, stderr))
	}
	parts := strings.SplitN(strings.TrimSpace(stdout), "|", 5)
	if len(parts) != 5 {
		return registryContainer{}, fmt.Errorf("%w: the private registry container returned an unreadable inspection", engine.ErrPermanent)
	}
	id, name, image, running := parts[0], strings.TrimPrefix(parts[1], "/"), parts[2], parts[3]
	if !shaPattern.MatchString(id) || len(id) != 64 {
		return registryContainer{}, fmt.Errorf("%w: the private registry container ID is not a full immutable ID", engine.ErrPermanent)
	}
	if name != RegistryContainer {
		return registryContainer{}, fmt.Errorf("%w: refusing the registry container named %q", engine.ErrPermanent, name)
	}
	if image != RegistryImage {
		return registryContainer{}, fmt.Errorf("%w: the private registry container does not run the pinned image %s", engine.ErrPermanent, RegistryImage)
	}
	if strings.ToLower(strings.TrimSpace(running)) != "true" {
		return registryContainer{}, fmt.Errorf("%w: the private registry container is not running", engine.ErrPermanent)
	}
	var bindings map[string][]registryPortBinding
	if err := json.Unmarshal([]byte(parts[4]), &bindings); err != nil {
		return registryContainer{}, fmt.Errorf("%w: the private registry container did not report readable port bindings", engine.ErrPermanent)
	}
	published := false
	for _, binding := range bindings["5000/tcp"] {
		if binding.HostIP == host && binding.HostPort == port {
			published = true
			break
		}
	}
	if !published {
		return registryContainer{}, fmt.Errorf("%w: the private registry container is not published on the configured loopback endpoint %s", engine.ErrPermanent, endpoint)
	}
	if report != nil {
		report(engine.Event{Kind: "log", Step: step, Message: "validated private registry container " + id + " on " + endpoint})
	}
	return registryContainer{ID: id, Image: image}, nil
}

// attachRegistryToBuildNetwork attaches the already-validated registry
// container to the owned build network and returns its assigned bridge
// endpoint. The container always listens on RegistryContainerPort; the
// configured loopback endpoint's host port is only the host publication and
// must not leak into the bridge address. An ambiguous connect error is
// resolved by reading the network endpoint rather than assuming success or
// failure.
func attachRegistryToBuildNetwork(ctx context.Context, rt engine.Runtime, isolation []string, network string, registry registryContainer, step string, report engine.Reporter) (string, error) {
	_, stderr, err := runArgvClean(ctx, []string{"docker", "network", "connect", network, registry.ID}, isolation, rt, step, nil)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		inspectCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), buildNetworkCreateTimeout)
		inspection, exists, inspectErr := inspectBuildNetwork(inspectCtx, rt, isolation, network, step)
		cancel()
		if inspectErr != nil || !exists {
			return "", fmt.Errorf("attaching the private registry to build network %s failed: %s", network, commandDetail(err, stderr))
		}
		if _, attached := inspection.Containers[registry.ID]; !attached {
			return "", fmt.Errorf("attaching the private registry to build network %s failed: %s", network, commandDetail(err, stderr))
		}
	}
	inspection, exists, err := inspectBuildNetwork(ctx, rt, isolation, network, step)
	if err != nil {
		return "", err
	}
	if !exists {
		return "", fmt.Errorf("build network %s disappeared before the registry address was read", network)
	}
	container, attached := inspection.Containers[registry.ID]
	if !attached {
		return "", fmt.Errorf("the validated registry container is not attached to build network %s", network)
	}
	address := strings.TrimSpace(container.IPv4Address)
	if slash := strings.IndexByte(address, '/'); slash >= 0 {
		address = address[:slash]
	}
	ip := net.ParseIP(address)
	if ip == nil || ip.To4() == nil {
		return "", fmt.Errorf("build network %s did not assign the registry container an IPv4 address", network)
	}
	endpoint := net.JoinHostPort(ip.String(), strconv.Itoa(RegistryContainerPort))
	if report != nil {
		report(engine.Event{Kind: "log", Step: step, Message: "private registry is reachable for this build at " + endpoint})
	}
	return endpoint, nil
}

func imageMissingError(stderr string) bool {
	lowered := strings.ToLower(stderr)
	return strings.Contains(lowered, "no such image") || strings.Contains(lowered, "no such object") ||
		strings.Contains(lowered, "no such container") || strings.Contains(lowered, "not found")
}

func parsePackImageInspection(stdout string) (string, []string, error) {
	parts := strings.SplitN(strings.TrimSpace(stdout), "|", 2)
	if len(parts) != 2 || !imageIDPattern.MatchString(parts[0]) {
		return "", nil, fmt.Errorf("%w: Docker returned an unreadable image identity", engine.ErrPermanent)
	}
	var digests []string
	if err := json.Unmarshal([]byte(parts[1]), &digests); err != nil {
		return "", nil, fmt.Errorf("%w: Docker returned an unreadable repository digest list", engine.ErrPermanent)
	}
	return parts[0], digests, nil
}

// packPublishReference places the same repository path under the registry's
// transient bridge address. Only the transport endpoint changes.
func packPublishReference(localImage, transientEndpoint string) (reference, repository, tag string, err error) {
	slash := strings.LastIndexByte(localImage, '/')
	colon := strings.LastIndexByte(localImage, ':')
	if colon <= slash || colon == len(localImage)-1 {
		return "", "", "", fmt.Errorf("pack image %q must include a tag when publishing", localImage)
	}
	if strings.Contains(localImage, "@") {
		return "", "", "", fmt.Errorf("pack image %q must be a local tag, not a digest", localImage)
	}
	repository, tag = localImage[:colon], localImage[colon+1:]
	if repository == "" {
		return "", "", "", fmt.Errorf("pack image %q has no repository path", localImage)
	}
	if _, _, splitErr := net.SplitHostPort(transientEndpoint); splitErr != nil {
		return "", "", "", fmt.Errorf("transient registry endpoint %q is invalid: %w", transientEndpoint, splitErr)
	}
	reference = transientEndpoint + "/" + repository + ":" + tag
	if err := ValidateImage(reference); err != nil {
		return "", "", "", err
	}
	return reference, repository, tag, nil
}

// resolvePublishedManifestDigest reads the immutable manifest digest through
// the node's canonical loopback endpoint, never through the transient bridge
// address. The registry's Docker-Content-Digest is authoritative; a missing
// header falls back to the exact manifest bytes.
func resolvePublishedManifestDigest(ctx context.Context, endpoint, repository, tag string) (string, error) {
	address, err := normalizeRegistryAddress(endpoint)
	if err != nil {
		return "", err
	}
	manifestURL := (&url.URL{
		Scheme: "http",
		Host:   address,
		Path:   "/v2/" + repository + "/manifests/" + tag,
	}).String()
	request, err := http.NewRequestWithContext(ctx, http.MethodHead, manifestURL, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Accept", registryManifestAccept)
	response, err := retentionRegistryHTTPClient.Do(request)
	if err != nil {
		return "", contract.NewFault(contract.Unavailable, "the private registry is not reachable through its loopback endpoint", err.Error())
	}
	closeRegistryResponse(response)
	digest := strings.TrimSpace(response.Header.Get("Docker-Content-Digest"))
	switch response.StatusCode {
	case http.StatusOK:
		if isIndexMediaType(response.Header.Get("Content-Type")) {
			return "", fmt.Errorf("%w: the published tag is an image index; the bounded single-platform publish contract refuses it", engine.ErrPermanent)
		}
		if registryDigestPattern.MatchString(digest) {
			return digest, nil
		}
		if digest != "" {
			return "", fmt.Errorf("%w: the private registry returned an invalid manifest digest %q", engine.ErrPermanent, digest)
		}
		return digestPublishedManifest(ctx, manifestURL)
	case http.StatusNotFound:
		return "", fmt.Errorf("%w: the private registry has no manifest for %s:%s after the publish", engine.ErrPermanent, repository, tag)
	default:
		return "", contract.NewFault(contract.Unavailable, "the private registry answered the manifest lookup with "+response.Status, "Retry the build after the registry is reachable.")
	}
}

func isIndexMediaType(contentType string) bool {
	contentType = strings.ToLower(strings.TrimSpace(contentType))
	return strings.HasPrefix(contentType, "application/vnd.oci.image.index.v1+json") ||
		strings.HasPrefix(contentType, "application/vnd.docker.distribution.manifest.list.v2+json")
}

func digestPublishedManifest(ctx context.Context, manifestURL string) (string, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, manifestURL, nil)
	if err != nil {
		return "", err
	}
	request.Header.Set("Accept", registryManifestAccept)
	response, err := retentionRegistryHTTPClient.Do(request)
	if err != nil {
		return "", contract.NewFault(contract.Unavailable, "the private registry is not reachable through its loopback endpoint", err.Error())
	}
	defer closeRegistryResponse(response)
	if response.StatusCode != http.StatusOK {
		return "", contract.NewFault(contract.Unavailable, "the private registry answered the manifest read with "+response.Status, "Retry the build after the registry is reachable.")
	}
	if isIndexMediaType(response.Header.Get("Content-Type")) {
		return "", fmt.Errorf("%w: the published tag is an image index; the bounded single-platform publish contract refuses it", engine.ErrPermanent)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 4<<20+1))
	if err != nil {
		return "", err
	}
	if len(body) > 4<<20 {
		return "", fmt.Errorf("%w: the published manifest is larger than the bounded read", engine.ErrPermanent)
	}
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func closeRegistryResponse(response *http.Response) {
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	_ = response.Body.Close()
}

// pullAndVerifyPublishedImage materializes the application image through the
// canonical loopback reference. Docker never pulls the transient bridge
// address, so the daemon needs no insecure-registry configuration.
func pullAndVerifyPublishedImage(ctx context.Context, rt engine.Runtime, isolation []string, canonical, expectedArchitecture, step string, report engine.Reporter) (string, error) {
	if !IsImageDigestReference(canonical) {
		return "", fmt.Errorf("%w: published image reference %q is not immutable", engine.ErrPermanent, canonical)
	}
	platform := "linux/" + expectedArchitecture
	pullCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	_, stderr, err := runArgvClean(pullCtx, []string{"docker", "pull", "--platform", platform, canonical}, isolation, rt, step, report)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("pulling the published image %s failed: %s", canonical, commandDetail(err, stderr))
	}
	stdout, inspectStderr, err := runArgvClean(pullCtx, []string{"docker", "image", "inspect", "--format", "{{.Id}}|{{.Os}}|{{.Architecture}}|{{json .RepoDigests}}", canonical}, isolation, rt, step, nil)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("inspecting the published image %s failed: %s", canonical, commandDetail(err, inspectStderr))
	}
	parts := strings.SplitN(strings.TrimSpace(stdout), "|", 4)
	if len(parts) != 4 || !imageIDPattern.MatchString(parts[0]) {
		return "", fmt.Errorf("%w: the published image did not report an immutable config digest", engine.ErrPermanent)
	}
	if parts[1] != "linux" || parts[2] != expectedArchitecture {
		return "", fmt.Errorf("%w: the published image platform is %s/%s, not linux/%s", engine.ErrPermanent, parts[1], parts[2], expectedArchitecture)
	}
	var digests []string
	if err := json.Unmarshal([]byte(parts[3]), &digests); err != nil || !containsString(digests, canonical) {
		return "", fmt.Errorf("%w: the published image does not carry its canonical repository digest %s", engine.ErrPermanent, canonical)
	}
	if report != nil {
		report(engine.Event{Kind: "log", Step: step, Message: "materialized canonical image " + canonical + " as " + parts[0]})
	}
	return parts[0], nil
}

func tagVerifiedPublishedImage(ctx context.Context, rt engine.Runtime, isolation []string, localImage, canonical, step string, report engine.Reporter) (string, error) {
	if _, stderr, err := runArgvClean(ctx, []string{"docker", "tag", canonical, localImage}, isolation, rt, step, report); err != nil {
		return "", fmt.Errorf("tagging the verified registry image as %s failed: %s", localImage, commandDetail(err, stderr))
	}
	stdout, stderr, err := runArgvClean(ctx, []string{"docker", "image", "inspect", "--format", "{{.Id}}|{{json .RepoDigests}}", localImage}, isolation, rt, step, nil)
	if err != nil {
		return "", fmt.Errorf("inspecting the local completion tag %s failed: %s", localImage, commandDetail(err, stderr))
	}
	imageID, digests, err := parsePackImageInspection(stdout)
	if err != nil {
		return "", err
	}
	if !containsString(digests, canonical) {
		return "", fmt.Errorf("%w: the local completion tag does not carry the canonical registry digest", engine.ErrPermanent)
	}
	return imageID, nil
}

type builderMetadata struct {
	Lifecycle struct {
		APIs struct {
			Platform struct {
				Deprecated []string `json:"deprecated"`
				Supported  []string `json:"supported"`
			} `json:"platform"`
		} `json:"apis"`
	} `json:"lifecycle"`
}

func platformAPIMinor(version string) (int, bool) {
	parts := strings.Split(strings.TrimPrefix(strings.TrimSpace(version), "v"), ".")
	if len(parts) < 2 {
		return 0, false
	}
	major, majorErr := strconv.Atoi(parts[0])
	minor, minorErr := strconv.Atoi(parts[1])
	if majorErr != nil || minorErr != nil || major != 0 {
		return 0, false
	}
	return minor, true
}

// verifyBuilderPlatformAPI refuses a builder whose negotiated Platform API
// cannot carry --insecure-registry into the lifecycle. Pack silently omits the
// flag below 0.13, and a successful-looking build would then fail to reach the
// loopback HTTP registry.
func verifyBuilderPlatformAPI(ctx context.Context, rt engine.Runtime, isolation []string, builder, platform, step string, report engine.Reporter) error {
	format := `{{index .Config.Labels "io.buildpacks.builder.metadata"}}`
	inspectBuilder := func() (string, string, error) {
		return runArgvClean(ctx, []string{"docker", "image", "inspect", "--format", format, builder}, isolation, rt, step, nil)
	}
	stdout, stderr, err := inspectBuilder()
	if err != nil {
		if !imageMissingError(stderr) {
			return fmt.Errorf("inspecting builder %s failed: %s", builder, commandDetail(err, stderr))
		}
		pullCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		_, pullStderr, pullErr := runArgvClean(pullCtx, []string{"docker", "pull", "--platform", platform, builder}, isolation, rt, step, report)
		cancel()
		if pullErr != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return fmt.Errorf("pulling builder %s failed: %s", builder, commandDetail(pullErr, pullStderr))
		}
		stdout, stderr, err = inspectBuilder()
		if err != nil {
			return fmt.Errorf("inspecting builder %s after pull failed: %s", builder, commandDetail(err, stderr))
		}
	}
	raw := strings.TrimSpace(stdout)
	if raw == "" || raw == "<no value>" || raw == "null" {
		return fmt.Errorf("builder %s does not report io.buildpacks.builder.metadata; its Platform API cannot be verified for insecure registry publishing", builder)
	}
	var metadata builderMetadata
	if err := json.Unmarshal([]byte(raw), &metadata); err != nil {
		return fmt.Errorf("builder %s has unreadable Buildpacks metadata: %v", builder, err)
	}
	versions := append(append([]string{}, metadata.Lifecycle.APIs.Platform.Deprecated...), metadata.Lifecycle.APIs.Platform.Supported...)
	selected := 0
	for _, version := range versions {
		minor, ok := platformAPIMinor(version)
		if !ok || minor < packPublishPlatformAPIMinor || minor > packSupportedPlatformAPIMinor {
			continue
		}
		if minor > selected {
			selected = minor
		}
	}
	if selected == 0 {
		return fmt.Errorf("builder %s does not advertise a Platform API from 0.%d to 0.%d; --insecure-registry would be ignored (supported: %s)",
			builder, packPublishPlatformAPIMinor, packSupportedPlatformAPIMinor, strings.Join(versions, ", "))
	}
	if report != nil {
		report(engine.Event{Kind: "log", Step: step, Message: fmt.Sprintf("builder %s supports Platform API 0.%d for the published build", builder, selected)})
	}
	return nil
}

func packCompletionPath(root string, operationID contract.OperationID, step string) string {
	return filepath.Join(root, "builds", "completions", string(operationID)+"-"+step+".json")
}

func loadPackCompletion(root string, operationID contract.OperationID, step string) (packCompletion, bool, error) {
	raw, err := os.ReadFile(packCompletionPath(root, operationID, step)) //nolint:gosec // G304: fixed path beneath the node state directory.
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return packCompletion{}, false, nil
		}
		return packCompletion{}, false, err
	}
	var completion packCompletion
	if err := json.Unmarshal(raw, &completion); err != nil || completion.Schema != packCompletionSchema {
		// A corrupt marker is not durable evidence. Refuse to trust a
		// pre-existing local tag and let a new build replace the marker.
		return packCompletion{}, false, nil
	}
	return completion, true, nil
}

func writePackCompletion(root string, completion packCompletion) error {
	dir := filepath.Dir(packCompletionPath(root, contract.OperationID(completion.OperationID), completion.Step))
	if err := engine.EnsureDir(dir); err != nil {
		return err
	}
	raw, err := json.Marshal(completion)
	if err != nil {
		return err
	}
	return engine.WriteFileAtomic(packCompletionPath(root, contract.OperationID(completion.OperationID), completion.Step), append(raw, '\n'), 0o600)
}

// resolveGitContextPath resolves the revision's Buildpacks context inside the
// checkout. Symlinks are resolved before the containment check, so a context
// path cannot read outside the repository even when a directory inside it
// points elsewhere. Every error names the configured path.
func resolveGitContextPath(cloneRoot, contextPath string) (string, error) {
	if contextPath == "" {
		// The repository root resolves like ".", so every context path is
		// returned as the same real path.
		contextPath = "."
	}
	if filepath.IsAbs(contextPath) {
		return "", fmt.Errorf("git context path %q must be relative to the repository", contextPath)
	}
	root, err := filepath.EvalSymlinks(cloneRoot)
	if err != nil {
		return "", fmt.Errorf("git context path %q: the checkout is unreadable", contextPath)
	}
	resolved, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(contextPath)))
	if err != nil {
		return "", fmt.Errorf("git context path %q is not present in the repository", contextPath)
	}
	if resolved != root && !strings.HasPrefix(resolved, root+string(filepath.Separator)) {
		return "", fmt.Errorf("git context path %q resolves outside the repository", contextPath)
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("git context path %q is not a directory", contextPath)
	}
	return resolved, nil
}

// watchBuildCancel stops a running build when the build-stop step writes the
// cancel marker for the same operation and pack step.
func watchBuildCancel(ctx context.Context, path string, cancel context.CancelFunc) {
	ticker := time.NewTicker(500 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(path); err == nil { //nolint:gosec // G304: fixed path beneath the node state directory.
			cancel()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// gitBuildEnvironment adds the embedded askpass helper and, when the revision
// names a read credential, the transient secret value. Only git child processes
// receive it; pack itself runs with the isolated environment alone.
func gitBuildEnvironment(isolation []string, work, secretKey string, rt engine.Runtime) ([]string, error) {
	value := ""
	if secretKey != "" {
		resolved, ok := rt.SecretValues[secretKey]
		if !ok || resolved == "" {
			return nil, errors.New("git read credential is unavailable")
		}
		value = resolved
	}
	return gitCredentialEnvironment(isolation, work, value)
}

// gitCredentialEnvironment writes the askpass helper and returns a child
// environment. The credential itself is only an environment value: it cannot
// appear in argv, the step payload, the journal, or an event.
func gitCredentialEnvironment(isolation []string, work, credential string) ([]string, error) {
	askpass := filepath.Join(work, "git-askpass.sh")
	if err := os.WriteFile(askpass, gitAskpassScript, 0o700); err != nil {
		return nil, err
	}
	env := append([]string{}, isolation...)
	env = append(env, "GIT_ASKPASS="+askpass, "GIT_TERMINAL_PROMPT=0")
	if credential == "" {
		return env, nil
	}
	env = append(env, "EREGION_GIT_USERNAME=x-access-token", "EREGION_GIT_PASSWORD="+credential)
	return env, nil
}

// buildCancelPath is where a build-stop step marks the running pack process. It
// sits beside the build work directory so removing that directory cannot erase a
// cancellation that arrived first.
func buildCancelPath(root, operationID, step string) string {
	return filepath.Join(root, "builds", operationID+"-"+step+".cancel")
}

// ------------------------------------------------------- ready image step

// ReadyImagePayload re-materializes one ready registry-backed release on its
// application node. The recorded image is an immutable manifest reference and
// ImageID is the exact config digest the release already recorded; a pull that
// resolves to any other config digest is refused rather than accepted.
type ReadyImagePayload struct {
	Step    string `json:"step"`
	Image   string `json:"image"`
	ImageID string `json:"image_id"`
}

type ReadyImageStep struct {
	exec    engine.Execution
	payload ReadyImagePayload
}

func buildReadyImageStep(exec engine.Execution, raw json.RawMessage) (engine.Step, error) {
	var payload ReadyImagePayload
	if err := decodePayload(exec, raw, &payload); err != nil {
		return nil, err
	}
	if payload.Step != exec.Step {
		return nil, fmt.Errorf("step ID %q does not match the payload step %q", exec.Step, payload.Step)
	}
	if !IsImageDigestReference(payload.Image) {
		return nil, errors.New("a ready release image must be an immutable registry digest reference")
	}
	if !imageIDPattern.MatchString(payload.ImageID) {
		return nil, errors.New("a ready release needs its recorded immutable image config digest")
	}
	return &ReadyImageStep{exec: exec, payload: payload}, nil
}

func (s *ReadyImageStep) ID() string { return s.payload.Step }
func (s *ReadyImageStep) Describe() string {
	return "Materialize ready release image " + s.payload.Image
}
func (s *ReadyImageStep) CancelBoundary() engine.Boundary { return engine.BoundaryAfterApply }
func (s *ReadyImageStep) Inputs() ([]byte, error)         { return json.Marshal(s.payload) }

func (s *ReadyImageStep) Check(ctx context.Context, rt engine.Runtime) (engine.CheckResult, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	env, cleanup, envErr := IsolatedDockerEnvironment()
	if envErr != nil {
		return engine.CheckResult{}, permanentCommandError(envErr)
	}
	defer cleanup()
	stdout, stderr, err := runArgvClean(probeCtx, []string{"docker", "image", "inspect", "--format", "{{.Id}}", s.payload.ImageID}, env, rt, s.ID(), nil)
	if err != nil {
		if imageMissingError(stderr) {
			return engine.CheckResult{Detail: "the recorded release image is absent on this node"}, nil
		}
		return engine.CheckResult{}, permanentCommandError(err)
	}
	if id := strings.TrimSpace(stdout); id != s.payload.ImageID {
		return engine.CheckResult{}, fmt.Errorf("%w: the recorded release image resolved to %s, not %s", engine.ErrPermanent, id, s.payload.ImageID)
	}
	return engine.CheckResult{Complete: true, Detail: "the recorded release image is already present"}, nil
}

func (s *ReadyImageStep) Apply(ctx context.Context, rt engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	pullCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	env, cleanup, envErr := IsolatedDockerEnvironment()
	if envErr != nil {
		return engine.ApplyResult{}, permanentCommandError(envErr)
	}
	defer cleanup()
	_, stderr, err := runArgvClean(pullCtx, []string{"docker", "pull", "--platform", "linux/" + runtime.GOARCH, s.payload.Image}, env, rt, s.ID(), report)
	if err != nil {
		if ctx.Err() != nil {
			return engine.ApplyResult{}, ctx.Err()
		}
		return engine.ApplyResult{}, fmt.Errorf("pulling the recorded release image %s failed: %s", s.payload.Image, commandDetail(err, stderr))
	}
	stdout, inspectStderr, err := runArgvClean(pullCtx, []string{"docker", "image", "inspect", "--format", "{{.Id}}|{{json .RepoDigests}}", s.payload.Image}, env, rt, s.ID(), nil)
	if err != nil {
		return engine.ApplyResult{}, fmt.Errorf("inspecting the pulled release image %s failed: %s", s.payload.Image, commandDetail(err, inspectStderr))
	}
	imageID, digests, err := parsePackImageInspection(stdout)
	if err != nil {
		return engine.ApplyResult{}, err
	}
	if imageID != s.payload.ImageID {
		return engine.ApplyResult{}, fmt.Errorf("%w: pulled image config digest %s does not match the recorded release image_id %s", engine.ErrPermanent, imageID, s.payload.ImageID)
	}
	if !containsString(digests, s.payload.Image) {
		return engine.ApplyResult{}, fmt.Errorf("%w: the pulled image does not carry its canonical repository digest", engine.ErrPermanent)
	}
	return engine.ApplyResult{Result: map[string]any{"image": s.payload.Image, "image_id": imageID}}, nil
}

type BuildStopPayload struct {
	Step        string `json:"step"`
	OperationID string `json:"operation_id"`
	PackStep    string `json:"pack_step"`
}

type BuildStopStep struct {
	exec    engine.Execution
	payload BuildStopPayload
}

func buildBuildStopStep(exec engine.Execution, raw json.RawMessage) (engine.Step, error) {
	var payload BuildStopPayload
	if err := decodePayload(exec, raw, &payload); err != nil {
		return nil, err
	}
	if payload.Step != exec.Step {
		return nil, fmt.Errorf("step ID %q does not match the payload step %q", exec.Step, payload.Step)
	}
	if err := contract.OperationID(payload.OperationID).Validate(); err != nil {
		return nil, errors.New("invalid build operation ID")
	}
	if !packStepPattern.MatchString(payload.PackStep) {
		return nil, errors.New("invalid build pack step")
	}
	return &BuildStopStep{exec: exec, payload: payload}, nil
}

// Only writes an operation-private cancellation marker. Waiting for the global
// host lock would prevent the marker reaching pack until pack had already ended.
func (s *BuildStopStep) IndependentOfHostLock() bool { return true }

func (s *BuildStopStep) ID() string                      { return s.payload.Step }
func (s *BuildStopStep) Describe() string                { return "Stop a running Buildpacks process" }
func (s *BuildStopStep) CancelBoundary() engine.Boundary { return engine.BoundaryAfterApply }
func (s *BuildStopStep) Inputs() ([]byte, error)         { return json.Marshal(s.payload) }

func (s *BuildStopStep) marker(rt engine.Runtime) string {
	return buildCancelPath(rt.Root, s.payload.OperationID, s.payload.PackStep)
}

func (s *BuildStopStep) Check(_ context.Context, rt engine.Runtime) (engine.CheckResult, error) {
	if _, err := os.Stat(s.marker(rt)); err == nil {
		return engine.CheckResult{Complete: true, Detail: "build cancellation is already recorded"}, nil
	}
	return engine.CheckResult{Detail: "no build cancellation marker exists"}, nil
}

func (s *BuildStopStep) Apply(_ context.Context, rt engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	path := s.marker(rt)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return engine.ApplyResult{}, err
	}
	if err := os.WriteFile(path, []byte("cancelled\n"), 0o600); err != nil {
		return engine.ApplyResult{}, err
	}
	if report != nil {
		report(engine.Event{Kind: "log", Step: s.ID(), Message: "build cancellation requested"})
	}
	return engine.ApplyResult{Result: map[string]any{"cancelled": true, "operation_id": s.payload.OperationID}}, nil
}

// buildIsolation creates an operation-private HOME, Docker config, cache, and
// temp tree. A build never inherits ambient credentials such as DATABASE_URL and
// never reads the host user's Docker auth or cache.
func buildIsolation(work string) ([]string, error) {
	dirs := map[string]string{
		"home": filepath.Join(work, "home"), "docker": filepath.Join(work, "docker"),
		"cache": filepath.Join(work, "cache"), "tmp": filepath.Join(work, "tmp"), "pack": filepath.Join(work, "pack"),
	}
	for _, dir := range dirs {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, err
		}
	}
	path := os.Getenv("PATH")
	if path == "" {
		path = "/usr/local/bin:/usr/bin:/bin"
	}
	return []string{
		"PATH=" + path,
		"HOME=" + dirs["home"],
		"DOCKER_CONFIG=" + dirs["docker"],
		"XDG_CACHE_HOME=" + dirs["cache"],
		"TMPDIR=" + dirs["tmp"],
		"PACK_HOME=" + dirs["pack"],
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_TERMINAL_PROMPT=0",
		// Never let an allowed forge redirect a clone to an internal service.
		// Git reads these as configuration without a writable gitconfig.
		"GIT_CONFIG_COUNT=3",
		"GIT_CONFIG_KEY_0=http.followRedirects",
		"GIT_CONFIG_VALUE_0=false",
		"GIT_CONFIG_KEY_1=protocol.allow",
		"GIT_CONFIG_VALUE_1=never",
		"GIT_CONFIG_KEY_2=protocol.https",
		"GIT_CONFIG_VALUE_2=always",
		"GIT_ALLOW_PROTOCOL=https",
	}, nil
}

// -------------------------------------------------------------- command run

// validateLabelKey accepts both Docker label forms: a reverse-DNS key such as
// "io.eregion.app" and a namespaced key such as "com.example/component".
func validateLabelKey(key string) error {
	if key == "" || len(key) > 255 || strings.ContainsAny(key, "\x00\r\n ") {
		return fmt.Errorf("container label %q is invalid", key)
	}
	prefix, suffix, qualified := strings.Cut(key, "/")
	if !qualified {
		prefix, suffix, qualified = strings.Cut(key, ".")
	}
	if !qualified || !labelKeyPattern.MatchString(prefix) || !labelKeyPattern.MatchString(suffix) {
		return fmt.Errorf("container label %q must have a DNS-style prefix and name", key)
	}
	return nil
}

// maxPlatformArgs bounds one node command: up to 64 configuration keys and
// 128 secrets as -e pairs, 128 process arguments, and the fixed options.
const maxPlatformArgs = 1024

// platformArgumentLimit is one node command argument's byte limit.
const platformArgumentLimit = 4096

func validateArgv(argv []string) error {
	// Each configuration key and secret is one -e argument besides the image,
	// options and process arguments, so a real environment needs room.
	if len(argv) == 0 || len(argv) > maxPlatformArgs {
		return fmt.Errorf("a platform command needs 1–%d arguments", maxPlatformArgs)
	}
	for _, arg := range argv {
		if len(arg) > platformArgumentLimit || strings.ContainsAny(arg, "\x00\r\n") {
			return errors.New("platform command argument is empty, too long, or contains a control character")
		}
	}
	program := filepath.Base(argv[0])
	switch program {
	case "docker":
		if len(argv) < 2 {
			return errors.New("docker command is missing a subcommand")
		}
		switch argv[1] {
		case "version", "pull", "run", "create", "start", "stop", "rm", "inspect", "port", "ps", "network", "restart", "exec", "tag":
		case "image":
			if len(argv) < 3 || (argv[2] != "inspect" && argv[2] != "ls" && argv[2] != "rm") {
				return errors.New("unsupported docker image command")
			}
		case "volume":
			if len(argv) < 3 || (argv[2] != "create" && argv[2] != "ls" && argv[2] != "inspect" && argv[2] != "rm") {
				return errors.New("unsupported docker volume command")
			}
		default:
			return errors.New("unsupported docker command")
		}
		execCommand := argv[1] == "exec"
		for index := 1; index < len(argv); index++ {
			arg := argv[index]
			switch {
			case arg == "--privileged", strings.HasPrefix(arg, "--cap-add"), strings.HasPrefix(arg, "--device"),
				strings.HasPrefix(arg, "--security-opt"), strings.HasPrefix(arg, "--network=host"),
				strings.HasPrefix(arg, "--pid=host"), arg == "-v", strings.HasPrefix(arg, "--volume"),
				strings.HasPrefix(arg, "--env-file"):
				return errors.New("privileged or host-mutating docker option is not allowed")
			case arg == "--network" && index+1 < len(argv) && argv[index+1] == "host":
				return errors.New("host networking is not allowed")
			case execCommand && (arg == "-e" || arg == "--env" || strings.HasPrefix(arg, "--env=")):
				return errors.New("environment values are not allowed in a docker exec argv")
			case arg == "--mount":
				if index+1 >= len(argv) {
					return errors.New("docker --mount needs a value")
				}
				if err := validateMountOption(argv[index+1]); err != nil {
					return err
				}
				index++
			case strings.HasPrefix(arg, "--mount="):
				return errors.New("use --mount with a separate named-volume value")
			}
		}
	case "pack":
		// `pack version` is the read-only availability probe used by node
		// checks. Builds are the only mutating pack operation.
		if len(argv) == 2 && argv[1] == "version" {
			break
		}
		if len(argv) < 2 || argv[1] != "build" {
			return errors.New("only pack build and pack version are supported")
		}
	case "git":
		subcommand := 1
		if len(argv) > 3 && argv[1] == "-C" {
			subcommand = 3
		}
		if len(argv) <= subcommand {
			return errors.New("git subcommand is missing")
		}
		switch argv[subcommand] {
		case "clone", "checkout", "rev-parse":
		default:
			return errors.New("only git clone, checkout, and rev-parse are supported")
		}
	default:
		return fmt.Errorf("unsupported platform command %q", program)
	}
	return nil
}

func validateMountOption(value string) error {
	if strings.Contains(value, "type=bind") {
		return validateResolverMountOption(value)
	}
	return validateVolumeMountOption(value)
}

// validateResolverMountOption allows exactly the fixed read-only bind mount of
// the node's eregion binary. It is a second line of defense: the step itself
// computes the source from os.Executable, never from user input.
func validateResolverMountOption(value string) error {
	parts := strings.Split(value, ",")
	fields := map[string]string{}
	for _, part := range parts {
		key, text, ok := strings.Cut(part, "=")
		if !ok || key == "" || text == "" {
			return errors.New("the resolver mount needs key=value fields")
		}
		if _, duplicate := fields[key]; duplicate {
			return errors.New("the resolver mount repeats a field")
		}
		fields[key] = text
	}
	for key := range fields {
		switch key {
		case "type", "source", "destination", "readonly":
		default:
			return errors.New("the resolver mount supports only type, source, destination and readonly")
		}
	}
	if fields["type"] != "bind" {
		return errors.New("the resolver mount must be a bind mount")
	}
	source := fields["source"]
	if !filepath.IsAbs(source) || strings.ContainsAny(source, ",\x00\r\n") {
		return errors.New("the resolver mount source must be an absolute safe path")
	}
	if fields["destination"] != containerresolver.TargetPath {
		return errors.New("the resolver mount destination is reserved")
	}
	if fields["readonly"] != "true" {
		return errors.New("the resolver mount must be read-only")
	}
	expected, err := containerResolverSource()
	if err != nil || source != expected {
		return errors.New("the resolver mount source is not this node's eregion binary")
	}
	return nil
}

func validateVolumeMountOption(value string) error {
	parts := strings.Split(value, ",")
	seen := map[string]string{}
	for _, part := range parts {
		key, text, ok := strings.Cut(part, "=")
		if !ok || key == "" || text == "" {
			return errors.New("a resource volume mount needs key=value fields")
		}
		if _, duplicate := seen[key]; duplicate {
			return errors.New("a resource volume mount repeats a field")
		}
		seen[key] = text
	}
	allowed := map[string]bool{"type": true, "source": true, "destination": true, "readonly": true}
	for key := range seen {
		if !allowed[key] {
			return errors.New("a resource volume mount supports only type, source, destination and readonly")
		}
	}
	if seen["type"] != "volume" {
		return errors.New("only named Docker volumes may be mounted")
	}
	if !ResourceVolumeNameValid(seen["source"]) {
		return errors.New("resource mount source must be an eregion resource volume name")
	}
	if _, err := cleanResourceDestination(seen["destination"]); err != nil {
		return errors.New("invalid resource mount destination")
	}
	if readonly, ok := seen["readonly"]; ok && readonly != "true" && readonly != "false" {
		return errors.New("resource mount readonly must be true or false")
	}
	return nil
}

func permanentCommandError(err error) error {
	message := strings.ToLower(err.Error())
	if errors.Is(err, exec.ErrNotFound) || strings.Contains(message, "executable file not found") ||
		strings.Contains(message, "permission denied") || strings.Contains(message, "operation not permitted") ||
		strings.Contains(message, "cannot connect to the docker daemon") {
		return fmt.Errorf("%w: %s", engine.ErrPermanent, firstLine(err.Error()))
	}
	return err
}

func runArgv(ctx context.Context, argv []string, extraEnv []string, rt engine.Runtime, step string, report engine.Reporter) (string, string, error) {
	return runArgvWithEnv(ctx, argv, mergeEnv(os.Environ(), extraEnv), rt, step, report)
}

// runArgvClean replaces the process environment entirely. Builds use it so an
// ambient DATABASE_URL or registry credential cannot leak into a child process.
func runArgvClean(ctx context.Context, argv []string, env []string, rt engine.Runtime, step string, report engine.Reporter) (string, string, error) {
	return runArgvWithEnv(ctx, argv, env, rt, step, report)
}

// runArgvCleanPack gives pack a short grace period after cancellation so it can
// remove the lifecycle containers and volumes it created. The build network
// cleanup is the backstop when pack cannot finish in time.
func runArgvCleanPack(ctx context.Context, argv []string, env []string, rt engine.Runtime, step string, report engine.Reporter) (string, string, error) {
	return runArgvWithEnvGrace(ctx, argv, env, rt, step, report, packBuildKillGrace)
}

func runArgvWithEnv(ctx context.Context, argv []string, env []string, rt engine.Runtime, step string, report engine.Reporter) (string, string, error) {
	return runArgvWithEnvGrace(ctx, argv, env, rt, step, report, 0)
}

func runArgvWithEnvGrace(ctx context.Context, argv []string, env []string, rt engine.Runtime, step string, report engine.Reporter, grace time.Duration) (string, string, error) {
	return runArgvWithEnvGraceCapture(ctx, argv, env, rt, step, report, grace, nil)
}

type commandOutputInfo struct {
	stdoutTruncated bool
	stderrTruncated bool
	stdoutSafe      string
}

func runArgvWithEnvGraceCapture(ctx context.Context, argv []string, env []string, rt engine.Runtime, step string, report engine.Reporter, grace time.Duration, info *commandOutputInfo) (string, string, error) {
	if err := validateArgv(argv); err != nil {
		return "", "", fmt.Errorf("%w: %s", engine.ErrPermanent, err)
	}
	command := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // G204: argv is validated and no shell is used.
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if grace > 0 {
		done := make(chan struct{})
		var once sync.Once
		command.Cancel = func() error {
			once.Do(func() {
				if command.Process == nil {
					return
				}
				pid := command.Process.Pid
				_ = syscall.Kill(-pid, syscall.SIGTERM)
				go func() {
					timer := time.NewTimer(grace)
					defer timer.Stop()
					select {
					case <-timer.C:
						_ = syscall.Kill(-pid, syscall.SIGKILL)
					case <-done:
					}
				}()
			})
			return nil
		}
		defer close(done)
	}
	if len(env) > 0 {
		command.Env = env
	}
	registry := rt.Secrets
	if registry == nil {
		registry = &secrets.Registry{}
	}
	capture := &commandCapture{registry: registry, step: step, report: report}
	streamSafe := rt.Secrets == nil || rt.SecretValues != nil
	if rt.Secrets.RequiresWholeTextRedaction() {
		streamSafe = false
	}
	stdout := &lineSink{capture: capture, stream: "stdout", streaming: streamSafe}
	stderr := &lineSink{capture: capture, stream: "stderr", streaming: streamSafe}
	command.Stdout, command.Stderr = stdout, stderr
	err := command.Run()
	stdout.Flush()
	stderr.Flush()
	capture.finish()
	if info != nil {
		info.stdoutTruncated = stdout.discarded > 0 || len(stdout.clean) > 65536
		info.stderrTruncated = stderr.discarded > 0 || len(stderr.clean) > 65536
		info.stdoutSafe = diagnosticPrefix(stdout.clean, 65536)
	}
	if ctx.Err() != nil {
		if command.Process != nil {
			_ = syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		}
		return stdout.String(), stderr.String(), ctx.Err()
	}
	return stdout.String(), stderr.String(), err
}

func mergeEnv(base, extra []string) []string {
	replaced := make(map[string]struct{}, len(extra))
	for _, entry := range extra {
		if index := strings.IndexByte(entry, '='); index > 0 {
			replaced[entry[:index]] = struct{}{}
		}
	}
	out := make([]string, 0, len(base)+len(extra))
	for _, entry := range base {
		if index := strings.IndexByte(entry, '='); index > 0 {
			if _, ok := replaced[entry[:index]]; ok {
				continue
			}
		}
		out = append(out, entry)
	}
	return append(out, extra...)
}

const commandLogHalf = 1 << 20

// One lock orders both streams and serializes the reporter for this invocation.
type commandCapture struct {
	mu                           sync.Mutex
	registry                     *secrets.Registry
	step                         string
	report                       engine.Reporter
	prefix                       int
	tailing                      bool
	tail                         []string
	tailBytes                    int
	omittedRecords, omittedBytes uint64
}

func (c *commandCapture) record(message string) {
	if c.report == nil {
		return
	}
	message = c.registry.RedactPreservingLength(message)
	if !c.tailing && c.prefix+len(message) <= commandLogHalf {
		c.prefix += len(message)
		c.report(engine.Event{Kind: "log", Step: c.step, Message: message})
		return
	}
	c.tailing = true
	c.tail = append(c.tail, message)
	c.tailBytes += len(message)
	for c.tailBytes > 2*commandLogHalf-c.prefix {
		c.evict()
	}
}

func (c *commandCapture) evict() {
	first := c.tail[0]
	c.tail[0] = ""
	c.tail = c.tail[1:]
	c.tailBytes -= len(first)
	c.omittedRecords++
	c.omittedBytes += uint64(len(first))
}

func (c *commandCapture) finish() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.report == nil {
		return
	}
	if c.omittedRecords > 0 {
		// Reserve enough for the notice even as its counters grow during eviction.
		for c.tailBytes > 2*commandLogHalf-c.prefix-128 {
			c.evict()
		}
		notice := fmt.Sprintf("command output omitted: %d records, %d rendered bytes", c.omittedRecords, c.omittedBytes)
		c.report(engine.Event{Kind: "log", Step: c.step, Message: c.registry.RedactPreservingLength(notice)})
	}
	for _, message := range c.tail {
		c.report(engine.Event{Kind: "log", Step: c.step, Message: message})
	}
	c.tail = nil
	c.report = nil
}

type lineSink struct {
	capture       *commandCapture
	stream        string
	buf           bytes.Buffer
	discarded     uint64
	clean         string
	private       string
	streaming     bool
	pending       []byte
	lineDiscarded uint64
}

func (s *lineSink) Write(p []byte) (int, error) {
	s.capture.mu.Lock()
	defer s.capture.mu.Unlock()
	n := len(p)
	room := 2*commandLogHalf - s.buf.Len()
	if room > len(p) {
		room = len(p)
	}
	_, _ = s.buf.Write(p[:room])
	s.discarded += uint64(len(p) - room)
	if s.streaming && s.capture.report != nil {
		for len(p) > 0 {
			index := bytes.IndexByte(p, '\n')
			segment := p
			if index >= 0 {
				segment = p[:index]
			}
			if s.lineDiscarded > 0 || len(s.pending)+len(segment) > 65536 {
				s.lineDiscarded += uint64(len(s.pending) + len(segment))
				s.pending = nil
			} else {
				s.pending = append(s.pending, segment...)
			}
			if index < 0 {
				break
			}
			s.flushStreamingLine()
			p = p[index+1:]
		}
	}
	return n, nil
}

func (s *lineSink) flushStreamingLine() {
	if s.lineDiscarded > 0 {
		s.capture.record(fmt.Sprintf("%s: oversized line discarded (%d bytes)", s.stream, s.lineDiscarded))
		s.lineDiscarded = 0
		s.pending = nil
		return
	}
	text := s.capture.registry.Redact(string(s.pending))
	text = s.capture.registry.Redact(output.SafeText(text))
	s.emit(text)
	s.pending = nil
}

func (s *lineSink) Flush() {
	s.capture.mu.Lock()
	defer s.capture.mu.Unlock()
	raw := s.buf.String()
	if s.stream == "stdout" {
		s.private = raw[:min(len(raw), 65536)]
	}
	if s.discarded > 0 {
		// Redaction after sanitization can reconstruct a secret beyond an
		// arbitrary raw cutoff. Do not publish a buffered partial stream.
		raw = ""
	}
	clean := s.capture.registry.RedactPreservingLength(raw)
	clean = strings.ReplaceAll(clean, "\r\n", "\n")
	clean = s.capture.registry.Redact(output.SafeText(clean))
	s.clean = clean
	// Chunk the sanitized stream, rather than emitting one database event per
	// tiny line. Newlines remain in the message and the existing 4 KiB frame
	// bound and 2 MiB total event budget still apply.
	if s.streaming {
		s.flushStreamingLine()
	} else {
		s.emit(clean)
	}
	if s.discarded > 0 {
		s.capture.record(fmt.Sprintf("%s: command output truncated; at least %d raw bytes omitted", s.stream, s.discarded))
	}
	s.buf.Reset()
}

func (s *lineSink) emit(line string) {
	if s.capture.report == nil || strings.TrimSpace(line) == "" {
		return
	}
	message := line
	prefix := s.stream + ": "
	for len(message) > 0 {
		end := min(len(message), 4096-len(prefix))
		for end < len(message) && !utf8.RuneStart(message[end]) {
			end--
		}
		s.capture.record(prefix + message[:end])
		message = message[end:]
	}
}

func (s *lineSink) String() string {
	s.capture.mu.Lock()
	defer s.capture.mu.Unlock()
	if s.stream == "stdout" {
		return s.private
	}
	return diagnosticPrefix(s.clean, 65536)
}

func firstLine(value string) string {
	value = strings.TrimSpace(value)
	if index := strings.IndexByte(value, '\n'); index >= 0 {
		value = value[:index]
	}
	return truncate(value, 500)
}

// commandDetail prefers the child's stderr but never reports an empty detail
// when the command failed before writing one.
func commandDetail(err error, stderr string) string {
	if detail := firstLine(stderr); detail != "" {
		return detail
	}
	if err != nil {
		return firstLine(err.Error())
	}
	return "unknown error"
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
