package platform

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/engine"
)

var releaseCommandStepPattern = regexp.MustCompile(`^platform-command-[0-9]{2}-v1$`)

// ReleaseCommandPayload runs a release's one-off command to completion in a
// throwaway container of the release image on the environment network. A bare
// command runs through the same read-only mounted eregion resolver as a process
// command, after the Buildpacks launcher for a Buildpacks image, so it resolves
// against the effective launch-time PATH and cwd without changing PATH.
type ReleaseCommandPayload struct {
	Step    string            `json:"step"`
	Name    string            `json:"name"`
	Image   string            `json:"image"`
	Network string            `json:"network"`
	Command []string          `json:"command"`
	Labels  map[string]string `json:"labels,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
	// SecretEnvKeys names environment variables resolved from the transient
	// request secrets. Values are never part of this payload or the journal.
	SecretEnvKeys  []string `json:"secret_env_keys,omitempty"`
	TimeoutSeconds int      `json:"timeout_seconds"`
}

type ReleaseCommandStep struct {
	exec    engine.Execution
	payload ReleaseCommandPayload
}

func buildReleaseCommandStep(exec engine.Execution, raw json.RawMessage) (engine.Step, error) {
	var payload ReleaseCommandPayload
	if err := decodePayload(exec, raw, &payload); err != nil {
		return nil, err
	}
	if payload.Step != exec.Step {
		return nil, fmt.Errorf("step ID %q does not match the payload step %q", exec.Step, payload.Step)
	}
	if !containerNamePattern.MatchString(payload.Name) {
		return nil, errors.New("invalid release command container name")
	}
	if !containerNamePattern.MatchString(payload.Network) {
		return nil, errors.New("invalid release command network")
	}
	if err := ValidateImage(payload.Image); err != nil {
		return nil, err
	}
	if len(payload.Command) == 0 || len(payload.Command) > 129 {
		return nil, errors.New("release command needs 1–129 arguments")
	}
	for _, arg := range payload.Command {
		if len(arg) > 4096 || strings.ContainsAny(arg, "\x00\r\n") {
			return nil, errors.New("release command argument is too long or contains a control character")
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
	if payload.TimeoutSeconds == 0 {
		payload.TimeoutSeconds = 120
	}
	if payload.TimeoutSeconds < 1 || payload.TimeoutSeconds > 600 {
		return nil, errors.New("release command timeout must be 1–600 seconds")
	}
	if err := validateEnv(payload.Env, "release command environment"); err != nil {
		return nil, err
	}
	for _, key := range payload.SecretEnvKeys {
		if !envKeyPattern.MatchString(key) {
			return nil, errors.New("invalid release command secret environment key")
		}
	}
	return &ReleaseCommandStep{exec: exec, payload: payload}, nil
}

func (s *ReleaseCommandStep) ID() string { return s.payload.Step }
func (s *ReleaseCommandStep) Describe() string {
	return "Run the release command in a one-off container"
}
func (s *ReleaseCommandStep) CancelBoundary() engine.Boundary { return engine.BoundaryAfterApply }
func (s *ReleaseCommandStep) Inputs() ([]byte, error)         { return json.Marshal(s.payload) }

// The command uses a local image, attaches to an environment network and may
// remove its named container on cancellation. Keep those identities fenced
// while allowing unrelated Pack effects to share the host fence.
func (s *ReleaseCommandStep) SharedHostResources() []string {
	return []string{
		"docker-container\x00" + s.payload.Name,
		"docker-image\x00" + s.payload.Image,
		"docker-network\x00" + s.payload.Network,
	}
}

// Check never proves completion: whether the command ran is release-scoped
// state the control plane records, not something Docker can show afterwards.
func (s *ReleaseCommandStep) Check(context.Context, engine.Runtime) (engine.CheckResult, error) {
	return engine.CheckResult{Complete: false, Detail: "command has no read-only completion check"}, nil
}

func (s *ReleaseCommandStep) Apply(ctx context.Context, rt engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	args, env, err := s.runArgs(ctx, rt)
	if err != nil {
		return engine.ApplyResult{}, err
	}
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(s.payload.TimeoutSeconds)*time.Second)
	defer cancel()
	var createInfo commandOutputInfo
	created, createStderr, err := runArgvWithEnvGraceCapture(runCtx, args, mergeEnv(os.Environ(), env), rt, s.ID(), report, 0, &createInfo)
	if err != nil {
		if runCtx.Err() != nil {
			return engine.ApplyResult{Result: StepExecutionEvidence{Stage: "release-command-create", OutputAvailability: "bounded", RedactionContext: originalRedactionContext(rt), Stderr: truncate(createStderr, 4096), StderrTruncated: createInfo.stderrTruncated || len(createStderr) > 4096}}, runCtx.Err()
		}
		return engine.ApplyResult{Result: StepExecutionEvidence{Stage: "release-command-create", OutputAvailability: "bounded", RedactionContext: originalRedactionContext(rt), Stderr: truncate(createStderr, 4096), StderrTruncated: createInfo.stderrTruncated || len(createStderr) > 4096}}, fmt.Errorf("%w: release command container could not be created: %s", engine.ErrPermanent, firstLine(createStderr))
	}
	id := strings.TrimSpace(created)
	if !evidenceContainerID.MatchString(id) {
		// Without the immutable Docker ID there is no authority to remove a
		// same-name container or to claim that the command ran.
		return engine.ApplyResult{Result: StepExecutionEvidence{Stage: "release-command-create", OutputAvailability: "bounded", RedactionContext: originalRedactionContext(rt)}}, fmt.Errorf("%w: docker did not return a valid release command container ID", engine.ErrPermanent)
	}
	// The ID returned by docker create identifies exactly this invocation even
	// if capture fails or the name is subsequently reused.
	defer func() {
		removeCtx, removeCancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer removeCancel()
		_, stderr, removeErr := runArgv(removeCtx, []string{"docker", "rm", "-f", id}, nil, rt, s.ID(), nil)
		if removeErr != nil && report != nil {
			report(engine.Event{Kind: "warning", Step: s.ID(), Message: "release command cleanup failed: " + firstLine(stderr)})
		}
	}()
	var info commandOutputInfo
	_, stderr, err := runArgvWithEnvGraceCapture(runCtx, []string{"docker", "start", "-a", id}, os.Environ(), rt, s.ID(), report, 0, &info)
	stdout := info.stdoutSafe
	evidence := captureExecutionEvidence(ctx, id, s.payload.Labels, rt)
	result := StepExecutionEvidence{Stage: "release-command", OutputAvailability: "bounded", RedactionContext: originalRedactionContext(rt), Stdout: truncate(stdout, 8192), Stderr: truncate(stderr, 4096), StdoutTruncated: info.stdoutTruncated || len(stdout) > 8192, StderrTruncated: info.stderrTruncated || len(stderr) > 4096, Container: &evidence}
	if err == nil {
		return engine.ApplyResult{Result: result}, nil
	}
	if runCtx.Err() != nil {
		if ctx.Err() != nil {
			return engine.ApplyResult{Result: result}, ctx.Err()
		}
		return engine.ApplyResult{Result: result}, fmt.Errorf("%w: release command timed out after %ds; cleanup was attempted", engine.ErrPermanent, s.payload.TimeoutSeconds)
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		// Docker exits 125 when it cannot start the container and the
		// command's own status otherwise. Either way this run has ended and
		// retrying inside the same deployment would only repeat it.
		if exitErr.ExitCode() == 125 {
			return engine.ApplyResult{Result: result}, fmt.Errorf("%w: release command container did not start: %s", engine.ErrPermanent, firstLine(stderr))
		}
		return engine.ApplyResult{Result: result}, fmt.Errorf("%w: release command failed with exit status %d", engine.ErrPermanent, exitErr.ExitCode())
	}
	return engine.ApplyResult{Result: result}, permanentCommandError(err)
}

func (s *ReleaseCommandStep) runArgs(ctx context.Context, rt engine.Runtime) ([]string, []string, error) {
	prepared, err := prepareImageCommand(ctx, s.payload.Image, s.payload.Command, rt, s.ID())
	if err != nil {
		return nil, nil, err
	}
	args := []string{"docker", "create", "--name", s.payload.Name, "--pull", "never", "--network", s.payload.Network}
	for _, key := range sortedEnvKeys(s.payload.Labels) {
		args = append(args, "--label", key+"="+s.payload.Labels[key])
	}
	env := make([]string, 0, len(s.payload.Env)+len(s.payload.SecretEnvKeys))
	for _, key := range sortedEnvKeys(s.payload.Env) {
		// The key alone tells Docker to read the value from the client
		// environment, so values do not appear in the node's process list.
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
