package platform

import (
	"bytes"
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
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

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
	argvStepPattern      = regexp.MustCompile(`^platform-(argv|node|pull|image|command|cleanup)-[0-9]{2}-v1$`)
	containerStepPattern = regexp.MustCompile(`^platform-container-[0-9]{2}-v1$`)
	healthStepPattern    = regexp.MustCompile(`^platform-health-[0-9]{2}-v1$`)
	packStepPattern      = regexp.MustCompile(`^platform-pack-[0-9]{2}-v1$`)
	containerNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)
	labelKeyPattern      = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,61}[a-z0-9])?$`)
)

// StepFamilies returns the platform step families for the node registry.
func StepFamilies() []Family {
	return []Family{
		{Match: argvStepPattern.MatchString, Build: buildArgvStep},
		{Match: containerStepPattern.MatchString, Build: buildContainerStep},
		{Match: healthStepPattern.MatchString, Build: buildHealthStep},
		{Match: packStepPattern.MatchString, Build: buildPackStep},
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
	Step           string            `json:"step"`
	Argv           []string          `json:"argv"`
	CheckArgv      []string          `json:"check_argv,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	SecretEnvKeys  []string          `json:"secret_env_keys,omitempty"`
	AllowFailure   bool              `json:"allow_failure,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"`
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
	if len(payload.CheckArgv) > 0 {
		if err := validateArgv(payload.CheckArgv); err != nil {
			return nil, err
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
	return &ArgvStep{exec: exec, payload: payload}, nil
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
	_, _, err := runArgv(probeCtx, s.payload.CheckArgv, nil, rt, s.ID(), nil)
	if err != nil {
		return engine.CheckResult{Complete: false, Detail: "node command check did not succeed"}, nil
	}
	return engine.CheckResult{Complete: true, Detail: "node command check already succeeded"}, nil
}

func (s *ArgvStep) Apply(ctx context.Context, rt engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	timeout := time.Duration(s.payload.TimeoutSeconds) * time.Second
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	extraEnv, err := stepEnv(s.payload.Env, s.payload.SecretEnvKeys, rt)
	if err != nil {
		return engine.ApplyResult{}, fmt.Errorf("%w: %s", engine.ErrPermanent, err)
	}
	stdout, stderr, err := runArgv(runCtx, s.payload.Argv, extraEnv, rt, s.ID(), report)
	if err != nil && s.payload.AllowFailure {
		return engine.ApplyResult{Result: map[string]any{
			"stdout": truncate(stdout, 8192), "stderr": truncate(stderr, 4096), "exit_ignored": true,
		}}, nil
	}
	if err != nil {
		return engine.ApplyResult{}, permanentCommandError(err)
	}
	return engine.ApplyResult{Result: map[string]any{"stdout": truncate(stdout, 8192)}}, nil
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
	Step          string            `json:"step"`
	Name          string            `json:"name"`
	Image         string            `json:"image"`
	FallbackImage string            `json:"fallback_image,omitempty"`
	ContainerPort int               `json:"container_port"`
	HostPort      int               `json:"host_port"`
	Bind          string            `json:"bind"`
	Env           map[string]string `json:"env,omitempty"`
	// SecretEnvKeys names environment variables resolved from the transient
	// request secrets. Values are never part of this payload or the journal.
	SecretEnvKeys []string          `json:"secret_env_keys,omitempty"`
	Labels        map[string]string `json:"labels,omitempty"`
	Restart       string            `json:"restart,omitempty"`
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
	if payload.ContainerPort < 1 || payload.ContainerPort > 65535 || payload.HostPort < 1 || payload.HostPort > 65535 {
		return nil, errors.New("container and host ports must be 1–65535")
	}
	if net.ParseIP(payload.Bind) == nil {
		return nil, errors.New("container bind address must be a literal IP")
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
	for key, value := range payload.Labels {
		if !strings.Contains(key, "/") {
			return nil, fmt.Errorf("container label %q must have a DNS-style prefix", key)
		}
		if len(value) > 128 {
			return nil, fmt.Errorf("container label %q is too long", key)
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
		Bind          string            `json:"bind"`
		Env           map[string]string `json:"env"`
		SecretEnvKeys []string          `json:"secret_env_keys"`
		Labels        map[string]string `json:"labels"`
		Restart       string            `json:"restart"`
	}{payload.Image, payload.ContainerPort, payload.HostPort, payload.Bind, payload.Env, payload.SecretEnvKeys, payload.Labels, payload.Restart})
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
	removeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	_, _, _ = runArgv(removeCtx, []string{"docker", "rm", "-f", s.payload.Name}, nil, rt, s.ID(), nil)
	cancel()
	args, env, err := s.runArgs(s.payload.Image, rt)
	if err != nil {
		return engine.ApplyResult{}, fmt.Errorf("%w: %s", engine.ErrPermanent, err)
	}
	runCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	_, stderr, err := runArgv(runCtx, args, env, rt, s.ID(), report)
	cancel()
	if err != nil {
		if s.payload.FallbackImage != "" {
			fallbackArgs, fallbackEnv, fallbackEnvErr := s.runArgs(s.payload.FallbackImage, rt)
			if fallbackEnvErr != nil {
				return engine.ApplyResult{}, fmt.Errorf("%w: %s", engine.ErrPermanent, fallbackEnvErr)
			}
			fallbackCtx, fallbackCancel := context.WithTimeout(ctx, 2*time.Minute)
			_, _, fallbackErr := runArgv(fallbackCtx, fallbackArgs, fallbackEnv, rt, s.ID(), report)
			fallbackCancel()
			if fallbackErr == nil {
				return engine.ApplyResult{}, fmt.Errorf("%w: starting %s failed (%s); the previous image was restored", engine.ErrPermanent, s.payload.Image, firstLine(stderr))
			}
		}
		return engine.ApplyResult{}, fmt.Errorf("%w: docker run failed: %s", engine.ErrPermanent, firstLine(stderr))
	}
	if report != nil {
		report(engine.Event{Kind: "log", Step: s.ID(), Message: "container " + s.payload.Name + " runs " + s.payload.Image})
	}
	return engine.ApplyResult{Result: map[string]any{
		"container": s.payload.Name,
		"image":     s.payload.Image,
		"host_port": s.payload.HostPort,
	}}, nil
}

func (s *ContainerStep) runArgs(image string, rt engine.Runtime) ([]string, []string, error) {
	args := []string{"docker", "run", "-d", "--name", s.payload.Name, "--pull", "never", "--restart", s.payload.Restart,
		"-p", fmt.Sprintf("%s:%d:%d", s.payload.Bind, s.payload.HostPort, s.payload.ContainerPort),
		"--label", "io.eregion.config=" + s.hash}
	keys := sortedEnvKeys(s.payload.Labels)
	for _, key := range keys {
		args = append(args, "--label", key+"="+s.payload.Labels[key])
	}
	env := make([]string, 0, len(s.payload.Env)+len(s.payload.SecretEnvKeys))
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
			return nil, nil, errors.New("secret value for " + key + " is unavailable")
		}
		args = append(args, "-e", key)
		env = append(env, key+"="+value)
	}
	args = append(args, image)
	return args, env, nil
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

type PackPayload struct {
	Step           string            `json:"step"`
	Image          string            `json:"image"`
	Path           string            `json:"path,omitempty"`
	GitURL         string            `json:"git_url,omitempty"`
	GitRef         string            `json:"git_ref,omitempty"`
	Builder        string            `json:"builder"`
	Env            map[string]string `json:"env,omitempty"`
	TimeoutSeconds int               `json:"timeout_seconds,omitempty"`
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
	if err := ValidateImage(payload.Builder); err != nil {
		return nil, err
	}
	if (payload.Path == "") == (payload.GitURL == "") {
		return nil, errors.New("pack source must have exactly one of path or git_url")
	}
	if payload.Path != "" && !filepath.IsAbs(payload.Path) {
		return nil, errors.New("pack source path must be absolute")
	}
	if payload.GitURL != "" {
		parsed, err := url.Parse(payload.GitURL)
		if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
			return nil, errors.New("git build source must be an https URL without credentials")
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
	if payload.TimeoutSeconds == 0 {
		payload.TimeoutSeconds = 600
	}
	if payload.TimeoutSeconds < 10 || payload.TimeoutSeconds > 1800 {
		return nil, errors.New("build timeout must be 10–1800 seconds")
	}
	return &PackStep{exec: exec, payload: payload}, nil
}

func (s *PackStep) ID() string { return s.payload.Step }
func (s *PackStep) Describe() string {
	return "Build " + s.payload.Image + " with Cloud Native Buildpacks"
}
func (s *PackStep) CancelBoundary() engine.Boundary { return engine.BoundaryAfterApply }
func (s *PackStep) Inputs() ([]byte, error)         { return json.Marshal(s.payload) }

func (s *PackStep) Check(ctx context.Context, rt engine.Runtime) (engine.CheckResult, error) {
	probeCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	stdout, stderr, err := runArgv(probeCtx, []string{"docker", "image", "inspect", "--format", "{{.Id}}", s.payload.Image}, nil, rt, s.ID(), nil)
	if err != nil {
		if strings.Contains(strings.ToLower(stderr), "no such") || strings.Contains(strings.ToLower(stderr), "not found") {
			return engine.CheckResult{Complete: false, Detail: "built image is absent"}, nil
		}
		return engine.CheckResult{}, permanentCommandError(err)
	}
	return engine.CheckResult{Complete: true, Detail: "built image already exists (" + truncate(strings.TrimSpace(stdout), 80) + ")"}, nil
}

func (s *PackStep) Apply(ctx context.Context, rt engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	work := filepath.Join(rt.Root, "builds", string(s.exec.OperationID)+"-"+s.payload.Step)
	if err := os.RemoveAll(work); err != nil {
		return engine.ApplyResult{}, err
	}
	isolation, err := buildIsolation(work)
	if err != nil {
		return engine.ApplyResult{}, err
	}
	source := s.payload.Path
	if s.payload.GitURL != "" {
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
		cloneCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
		_, cloneStderr, err := runArgvClean(cloneCtx, args, isolation, rt, s.ID(), report)
		cancel()
		if err != nil {
			return engine.ApplyResult{}, fmt.Errorf("%w: git clone failed: %s", engine.ErrPermanent, firstLine(cloneStderr))
		}
		if shaPattern.MatchString(s.payload.GitRef) {
			checkoutCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
			_, checkoutStderr, err := runArgvClean(checkoutCtx, []string{"git", "-C", source, "checkout", "--detach", s.payload.GitRef}, isolation, rt, s.ID(), report)
			cancel()
			if err != nil {
				return engine.ApplyResult{}, fmt.Errorf("%w: git checkout failed: %s", engine.ErrPermanent, firstLine(checkoutStderr))
			}
		}
	}
	info, err := os.Stat(source) //nolint:gosec // G304: resolved source root or a directory under the node root.
	if err != nil || !info.IsDir() {
		return engine.ApplyResult{}, fmt.Errorf("%w: build source %s is not a readable directory on the node", engine.ErrPermanent, source)
	}
	args := []string{"pack", "build", s.payload.Image, "--path", source, "--builder", s.payload.Builder}
	for _, key := range sortedEnvKeys(s.payload.Env) {
		args = append(args, "--env", key+"="+s.payload.Env[key])
	}
	buildCtx, cancel := context.WithTimeout(ctx, time.Duration(s.payload.TimeoutSeconds)*time.Second)
	defer cancel()
	_, stderr, err := runArgvClean(buildCtx, args, isolation, rt, s.ID(), report)
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) || strings.Contains(strings.ToLower(err.Error()), "executable file not found") {
			return engine.ApplyResult{}, fmt.Errorf("%w: pack is not installed on this node; install Buildpacks or use an image release", engine.ErrPermanent)
		}
		return engine.ApplyResult{}, fmt.Errorf("%w: pack build failed: %s", engine.ErrPermanent, firstLine(stderr))
	}
	return engine.ApplyResult{Result: map[string]any{"image": s.payload.Image}}, nil
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
	}, nil
}

// -------------------------------------------------------------- command run

func validateArgv(argv []string) error {
	if len(argv) == 0 || len(argv) > 64 {
		return errors.New("a platform command needs 1–64 arguments")
	}
	for _, arg := range argv {
		if len(arg) > 4096 || strings.ContainsAny(arg, "\x00\r\n") {
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
		case "version", "pull", "run", "stop", "rm", "inspect", "port", "ps", "network":
		case "image":
			if len(argv) < 3 || (argv[2] != "inspect" && argv[2] != "ls") {
				return errors.New("unsupported docker image command")
			}
		default:
			return errors.New("unsupported docker command")
		}
		for _, arg := range argv[1:] {
			switch {
			case arg == "--privileged", strings.HasPrefix(arg, "--cap-add"), strings.HasPrefix(arg, "--device"),
				strings.HasPrefix(arg, "--security-opt"), strings.HasPrefix(arg, "--network=host"),
				strings.HasPrefix(arg, "--pid=host"), strings.HasPrefix(arg, "--mount"),
				arg == "-v", strings.HasPrefix(arg, "--volume"):
				return errors.New("privileged or host-mutating docker option is not allowed")
			}
		}
	case "pack":
		if len(argv) < 2 || argv[1] != "build" {
			return errors.New("only pack build is supported")
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
		case "clone", "checkout":
		default:
			return errors.New("only git clone and checkout are supported")
		}
	default:
		return fmt.Errorf("unsupported platform command %q", program)
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

func runArgvWithEnv(ctx context.Context, argv []string, env []string, rt engine.Runtime, step string, report engine.Reporter) (string, string, error) {
	if err := validateArgv(argv); err != nil {
		return "", "", fmt.Errorf("%w: %s", engine.ErrPermanent, err)
	}
	command := exec.CommandContext(ctx, argv[0], argv[1:]...) //nolint:gosec // G204: argv is validated and no shell is used.
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if len(env) > 0 {
		command.Env = env
	}
	registry := rt.Secrets
	if registry == nil {
		registry = &secrets.Registry{}
	}
	stdout := newLineSink(registry, step, report, "stdout")
	stderr := newLineSink(registry, step, report, "stderr")
	command.Stdout, command.Stderr = stdout, stderr
	err := command.Run()
	stdout.Flush()
	stderr.Flush()
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

type lineSink struct {
	mu       sync.Mutex
	registry *secrets.Registry
	step     string
	report   engine.Reporter
	stream   string
	buf      bytes.Buffer
	pending  []byte
	partial  []byte
	total    int
	emitted  int
}

func newLineSink(registry *secrets.Registry, step string, report engine.Reporter, stream string) *lineSink {
	return &lineSink{registry: registry, step: step, report: report, stream: stream}
}

func (s *lineSink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.total += len(p)
	if s.buf.Len() < 65536 {
		room := 65536 - s.buf.Len()
		if len(p) < room {
			room = len(p)
		}
		_, _ = s.buf.Write(p[:room])
	}
	s.pending = append(s.pending, p...)
	for {
		index := bytes.IndexByte(s.pending, '\n')
		if index < 0 {
			break
		}
		line := strings.TrimRight(string(s.pending[:index]), "\r")
		s.pending = s.pending[index+1:]
		s.emit(line)
	}
	return len(p), nil
}

func (s *lineSink) Flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) > 0 {
		s.emit(string(s.pending))
		s.pending = nil
	}
}

func (s *lineSink) emit(line string) {
	if s.report == nil || s.emitted >= 100 || strings.TrimSpace(line) == "" {
		return
	}
	message := truncate(output.SafeText(s.registry.Redact(line)), 1024)
	if message == "" {
		return
	}
	s.emitted++
	s.report(engine.Event{Kind: "log", Step: s.step, Message: s.stream + ": " + message})
}

func (s *lineSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func firstLine(value string) string {
	value = strings.TrimSpace(value)
	if index := strings.IndexByte(value, '\n'); index >= 0 {
		value = value[:index]
	}
	return truncate(value, 500)
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}
