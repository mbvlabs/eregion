package platform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
)

// These node effects have no externally exposed action or worker dispatch yet.
const DiagnosticExecutionStep = "platform-diagnostic-execution-01-v1"
const DiagnosticCleanupStep = "platform-diagnostic-cleanup-01-v1"

var diagnosticExecutionPattern = regexp.MustCompile(`^platform-diagnostic-(execution|cleanup)-01-v1$`)

// The provisioned Debian/Ubuntu docker.io package installs this binary here.
// Do not resolve a privileged executable through the caller's ambient PATH.
const diagnosticDockerBinary = "/usr/bin/docker"

// DiagnosticExecutionPayload must be built from admission-pinned historical
// placement and image identity. Arguments are non-secret operation input.
type DiagnosticExecutionPayload struct {
	Step        string               `json:"step"`
	OperationID contract.OperationID `json:"operation_id"`
	ImageID     string               `json:"image_id"`
	Argv        []string             `json:"argv"`
}

// DiagnosticExecutionResult is durable before removal. Output is deliberately
// withheld: an image may contain secrets unknown to the runtime registry.
type DiagnosticExecutionResult struct {
	Outcome        string `json:"outcome"`
	ExitCode       *int   `json:"exit_code,omitempty"`
	OOMKilled      bool   `json:"oom_killed"`
	TimedOut       bool   `json:"timed_out"`
	OutputWithheld bool   `json:"output_withheld"`
	OutputLoss     bool   `json:"output_loss"`
	Cleanup        string `json:"cleanup"`
}

type diagnosticExecutionState struct {
	Version     int                        `json:"version"`
	InputHash   string                     `json:"input_hash"`
	ImageID     string                     `json:"image_id"`
	StartIntent bool                       `json:"start_intent"`
	Deadline    time.Time                  `json:"deadline,omitempty"`
	Cancelled   bool                       `json:"cancelled"`
	Result      *DiagnosticExecutionResult `json:"result,omitempty"`
}

type diagnosticExecutionStep struct {
	execution    engine.Execution
	payload      DiagnosticExecutionPayload
	dockerConfig string
}

func buildDiagnosticExecutionStep(execution engine.Execution, raw json.RawMessage) (engine.Step, error) {
	var p DiagnosticExecutionPayload
	if err := decodePayload(execution, raw, &p); err != nil {
		return nil, err
	}
	if execution.Validate() != nil || p.OperationID != execution.OperationID || p.Step != execution.Step || !diagnosticExecutionPattern.MatchString(p.Step) || !diagnosisImageIDPattern.MatchString(p.ImageID) || len(p.Argv) == 0 || len(p.Argv) > 128 || !filepath.IsAbs(p.Argv[0]) || strings.ContainsRune(p.Argv[0], 0) {
		return nil, errors.New("invalid diagnostic execution identity or command")
	}
	bytes := 0
	for _, arg := range p.Argv {
		bytes += len(arg)
		if strings.ContainsRune(arg, 0) {
			return nil, errors.New("invalid diagnostic argument")
		}
	}
	if bytes > 32768 {
		return nil, errors.New("diagnostic arguments exceed limit")
	}
	return &diagnosticExecutionStep{execution: execution, payload: p}, nil
}

func (s *diagnosticExecutionStep) ID() string                    { return s.payload.Step }
func (*diagnosticExecutionStep) Describe() string                { return "Reconcile isolated diagnostic execution" }
func (*diagnosticExecutionStep) CancelBoundary() engine.Boundary { return engine.BoundaryAfterApply }
func (*diagnosticExecutionStep) IndependentOfHostLock() bool     { return true }
func (s *diagnosticExecutionStep) Inputs() ([]byte, error)       { return json.Marshal(s.payload) }
func (*diagnosticExecutionStep) Check(context.Context, engine.Runtime) (engine.CheckResult, error) {
	// Apply also returns the persisted result after an interrupted acknowledgement.
	return engine.CheckResult{Detail: "diagnostic result requires reconciliation"}, nil
}

func (s *diagnosticExecutionStep) identity() (string, string) {
	canonical, _ := json.Marshal(struct {
		OperationID contract.OperationID `json:"operation_id"`
		ImageID     string               `json:"image_id"`
		Argv        []string             `json:"argv"`
	}{s.payload.OperationID, s.payload.ImageID, s.payload.Argv})
	sum := sha256.Sum256(canonical)
	op := sha256.Sum256([]byte(s.payload.OperationID))
	return "eregion-diagnostic-" + hex.EncodeToString(op[:16]), hex.EncodeToString(sum[:])
}

func (s *diagnosticExecutionStep) statePath(rt engine.Runtime) (string, error) {
	if rt.Root == "" || !filepath.IsAbs(rt.Root) {
		return "", errors.New("diagnostic node state root must be absolute")
	}
	return filepath.Join(rt.Root, "effects", string(s.execution.OperationID), "diagnostic-execution-state.json"), nil
}

func (s *diagnosticExecutionStep) load(path, hash string) (diagnosticExecutionState, error) {
	state := diagnosticExecutionState{Version: 1, InputHash: hash, ImageID: s.payload.ImageID}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return state, nil
	}
	if err != nil {
		return state, err
	}
	if len(raw) > 16384 || json.Unmarshal(raw, &state) != nil || state.Version != 1 || state.InputHash != hash || state.ImageID != s.payload.ImageID || (state.StartIntent && state.Deadline.IsZero()) {
		return state, fmt.Errorf("%w: diagnostic state corrupt or input mismatch", engine.ErrPermanent)
	}
	return state, nil
}

func saveDiagnosticState(path string, state diagnosticExecutionState) error {
	if err := engine.EnsureDir(filepath.Dir(path)); err != nil {
		return err
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return engine.WriteFileAtomic(path, raw, 0600)
}

// Do not inherit Docker context, host, config, TLS, proxies or arbitrary env.
func diagnosticDocker(ctx context.Context, config string, args ...string) (string, string, error) {
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	argv := append([]string{"--host", "unix:///var/run/docker.sock", "--config", config}, args...)
	cmd := exec.CommandContext(ctx, diagnosticDockerBinary, argv...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/nonexistent", "DOCKER_CONFIG=" + config}
	out, stderr := &limitedDiagnosticBuffer{limit: 1024 * 1024}, &limitedDiagnosticBuffer{limit: 64 * 1024}
	cmd.Stdout, cmd.Stderr = out, stderr
	err := cmd.Run()
	if out.overflow || stderr.overflow {
		return "", "", errors.New("docker response exceeded bound")
	}
	return out.String(), stderr.String(), err
}

// diagnosticConfigDir creates no Docker credentials and refuses pre-existing
// content. Every component below the engine root must be private and owned by
// this process, so a local user cannot redirect Docker to a host config.
func diagnosticConfigDir(root string, id contract.OperationID) (string, error) {
	paths := []string{root, filepath.Join(root, "effects"), filepath.Join(root, "effects", string(id)), filepath.Join(root, "effects", string(id), "diagnostic-docker-config")}
	for i, path := range paths {
		if i > 0 {
			if err := os.Mkdir(path, 0700); err != nil && !errors.Is(err, os.ErrExist) {
				return "", err
			}
		}
		info, err := os.Lstat(path)
		if err != nil {
			return "", err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || int(stat.Uid) != os.Geteuid() || !info.IsDir() || info.Mode().Perm() != 0700 || info.Mode()&(os.ModeSymlink|os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
			return "", errors.New("diagnostic Docker configuration directory is not private and owned")
		}
	}
	config := paths[len(paths)-1]
	entries, err := os.ReadDir(config)
	if err != nil {
		return "", err
	}
	if len(entries) != 0 {
		return "", errors.New("diagnostic Docker configuration directory is not empty")
	}
	return config, nil
}

type diagnosticContainer struct {
	ID     string `json:"Id"`
	Name   string `json:"Name"`
	Image  string `json:"Image"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	State struct {
		Status     string `json:"Status"`
		ExitCode   int    `json:"ExitCode"`
		OOMKilled  bool   `json:"OOMKilled"`
		FinishedAt string `json:"FinishedAt"`
	} `json:"State"`
}

func (s *diagnosticExecutionStep) inspect(ctx context.Context, name, hash string) (*diagnosticContainer, error) {
	out, stderr, err := diagnosticDocker(ctx, s.dockerConfig, "container", "inspect", "--format", `{{json .Id}}{{"\n"}}{{json .Name}}{{"\n"}}{{json .Image}}{{"\n"}}{{json .Config.Labels}}{{"\n"}}{{json .State.Status}}{{"\n"}}{{json .State.ExitCode}}{{"\n"}}{{json .State.OOMKilled}}{{"\n"}}{{json .State.FinishedAt}}`, name)
	if err != nil {
		if strings.Contains(stderr, "No such object:") || strings.Contains(stderr, "No such container:") {
			return nil, nil
		}
		return nil, errors.New("diagnostic container inspection unavailable")
	}
	var c diagnosticContainer
	parts := strings.SplitN(out, "\n", 8)
	if len(parts) != 8 || json.Unmarshal([]byte(parts[0]), &c.ID) != nil || json.Unmarshal([]byte(parts[1]), &c.Name) != nil || json.Unmarshal([]byte(parts[2]), &c.Image) != nil || json.Unmarshal([]byte(parts[3]), &c.Config.Labels) != nil || json.Unmarshal([]byte(parts[4]), &c.State.Status) != nil || json.Unmarshal([]byte(parts[5]), &c.State.ExitCode) != nil || json.Unmarshal([]byte(parts[6]), &c.State.OOMKilled) != nil || json.Unmarshal([]byte(parts[7]), &c.State.FinishedAt) != nil || strings.TrimPrefix(c.Name, "/") != name || c.ID == "" || c.Image != s.payload.ImageID || c.Config.Labels["io.eregion.diagnostic.operation"] != string(s.payload.OperationID) || c.Config.Labels["io.eregion.diagnostic.input"] != hash || c.Config.Labels["io.eregion.diagnostic.image"] != s.payload.ImageID {
		return nil, fmt.Errorf("%w: diagnostic container identity mismatch", engine.ErrPermanent)
	}
	return &c, nil
}

func (s *diagnosticExecutionStep) verifyImage(ctx context.Context) error {
	out, _, err := diagnosticDocker(ctx, s.dockerConfig, "image", "inspect", "--format", `{{json .Id}}{{"\n"}}{{json .Config.Volumes}}{{"\n"}}{{json .Config.Cmd}}`, s.payload.ImageID)
	if err != nil {
		return errors.New("pinned local diagnostic image unavailable")
	}
	parts := strings.SplitN(out, "\n", 3)
	var id string
	var volumes map[string]json.RawMessage
	var imageCmd []string
	if len(parts) != 3 || json.Unmarshal([]byte(parts[0]), &id) != nil || id != s.payload.ImageID || json.Unmarshal([]byte(parts[1]), &volumes) != nil || len(volumes) != 0 || json.Unmarshal([]byte(parts[2]), &imageCmd) != nil {
		return fmt.Errorf("%w: pinned image identity invalid or image declares volumes", engine.ErrPermanent)
	}
	// Docker's CLI otherwise inherits image CMD when no command arguments are
	// supplied, violating exact argv. Refuse rather than add an empty argument.
	if len(s.payload.Argv) == 1 && len(imageCmd) != 0 {
		return fmt.Errorf("%w: image CMD cannot be suppressed by the Docker CLI for an argumentless executable", engine.ErrPermanent)
	}
	return nil
}

func (s *diagnosticExecutionStep) create(ctx context.Context, name, hash string) error {
	args := []string{"container", "create", "--name", name, "--pull=never", "--user", "65534:65534", "--read-only", "--network", "none", "--cap-drop", "ALL", "--security-opt", "no-new-privileges", "--no-healthcheck", "--restart", "no", "--pids-limit", "64", "--memory", "256m", "--memory-swap", "256m", "--cpus", "0.5", "--tmpfs", "/tmp:rw,nosuid,nodev,noexec,size=16m", "--log-driver", "local", "--log-opt", "max-size=64k", "--log-opt", "max-file=1", "--label", "io.eregion.diagnostic.operation=" + string(s.payload.OperationID), "--label", "io.eregion.diagnostic.input=" + hash, "--label", "io.eregion.diagnostic.image=" + s.payload.ImageID, "--entrypoint", s.payload.Argv[0], s.payload.ImageID}
	args = append(args, s.payload.Argv[1:]...)
	_, _, err := diagnosticDocker(ctx, s.dockerConfig, args...)
	if err != nil {
		return errors.New("diagnostic container creation unavailable")
	}
	return nil
}

func (s *diagnosticExecutionStep) Apply(ctx context.Context, rt engine.Runtime, _ engine.Reporter) (engine.ApplyResult, error) {
	path, err := s.statePath(rt)
	if err != nil {
		return engine.ApplyResult{}, err
	}
	s.dockerConfig, err = diagnosticConfigDir(rt.Root, s.execution.OperationID)
	if err != nil {
		return engine.ApplyResult{}, err
	}
	name, hash := s.identity()
	// A separate short node-local lock serializes start with the cleanup effect.
	lock, err := engine.Acquire(ctx, path+".lock", engine.LockExclusive, 15*time.Second)
	if err != nil {
		return engine.ApplyResult{}, err
	}
	state, err := s.load(path, hash)
	if err != nil {
		lock()
		return engine.ApplyResult{}, err
	}
	if s.payload.Step == DiagnosticCleanupStep {
		state.Cancelled = true
		if err = saveDiagnosticState(path, state); err == nil {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
			err = s.finish(cleanupCtx, path, name, hash, &state, true)
			cancel()
		}
		lock()
		if err != nil {
			return engine.ApplyResult{}, err
		}
		return engine.ApplyResult{Result: state.Result}, nil
	}
	if state.Result != nil && state.Result.Cleanup == "complete" {
		lock()
		return engine.ApplyResult{Result: state.Result}, nil
	}
	if state.Cancelled || state.Result != nil {
		err = s.finish(ctx, path, name, hash, &state, state.Cancelled)
		lock()
		if err != nil {
			return engine.ApplyResult{}, err
		}
		return engine.ApplyResult{Result: state.Result}, nil
	}
	container, err := s.inspect(ctx, name, hash)
	if err == nil && container == nil && state.StartIntent {
		state.Result = &DiagnosticExecutionResult{Outcome: "unknown", OutputWithheld: true, OutputLoss: true, Cleanup: "complete"}
		err = saveDiagnosticState(path, state)
		lock()
		if err != nil {
			return engine.ApplyResult{}, err
		}
		return engine.ApplyResult{Result: state.Result}, nil
	}
	if err == nil && container == nil {
		err = s.verifyImage(ctx)
		if err == nil {
			err = s.create(ctx, name, hash)
		}
		if err == nil {
			container, err = s.inspect(ctx, name, hash)
			if err == nil && container == nil {
				err = errors.New("created diagnostic container is unavailable")
			}
		}
	}
	if err == nil && !state.StartIntent {
		if container.State.Status != "created" {
			err = fmt.Errorf("%w: diagnostic container started without durable intent", engine.ErrPermanent)
		} else {
			state.StartIntent = true
			state.Deadline = time.Now().UTC().Add(30 * time.Second)
			if err = saveDiagnosticState(path, state); err == nil {
				_, _, err = diagnosticDocker(ctx, s.dockerConfig, "container", "start", container.ID)
			}
		}
	}
	lock()
	if err != nil {
		// A failed or interrupted start request does not prove the daemon did
		// not start the container. Reconcile it using an independent budget.
		if state.StartIntent {
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
			defer cancel()
			cleanupLock, lockErr := engine.Acquire(cleanupCtx, path+".lock", engine.LockExclusive, 10*time.Second)
			if lockErr != nil {
				return engine.ApplyResult{}, lockErr
			}
			defer cleanupLock()
			state, lockErr = s.load(path, hash)
			if lockErr != nil {
				return engine.ApplyResult{}, lockErr
			}
			if lockErr = s.finish(cleanupCtx, path, name, hash, &state, true); lockErr != nil {
				return engine.ApplyResult{}, lockErr
			}
			return engine.ApplyResult{Result: state.Result}, nil
		}
		return engine.ApplyResult{}, err
	}
	// Wait only until the persisted deadline, not 30 seconds per River retry.
	waitCtx, cancel := context.WithDeadline(ctx, state.Deadline)
	_, _, waitErr := diagnosticDockerWait(waitCtx, s.dockerConfig, container.ID)
	timedOut := errors.Is(waitCtx.Err(), context.DeadlineExceeded)
	cancel()
	cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), 20*time.Second)
	defer cleanupCancel()
	lock, err = engine.Acquire(cleanupCtx, path+".lock", engine.LockExclusive, 10*time.Second)
	if err != nil {
		return engine.ApplyResult{}, err
	}
	defer lock()
	state, err = s.load(path, hash)
	if err != nil {
		return engine.ApplyResult{}, err
	}
	if timedOut && state.Result == nil {
		observed, inspectErr := s.inspect(cleanupCtx, name, hash)
		if inspectErr != nil {
			return engine.ApplyResult{}, inspectErr
		}
		switch {
		case observed == nil:
			state.Result = &DiagnosticExecutionResult{Outcome: "unknown", OutputWithheld: true, OutputLoss: true, Cleanup: "pending"}
		case observed.State.Status == "running" || observed.State.Status == "paused" || observed.State.Status == "restarting":
			// A process can exit between inspection and signalling. Claim a
			// timeout only when the daemon acknowledges the enforcement signal.
			if _, _, killErr := diagnosticDocker(cleanupCtx, s.dockerConfig, "container", "kill", observed.ID); killErr == nil {
				state.Result = &DiagnosticExecutionResult{Outcome: "timeout", TimedOut: true, OutputWithheld: true, Cleanup: "pending"}
			} else {
				state.Result = &DiagnosticExecutionResult{Outcome: "unknown", OutputWithheld: true, Cleanup: "pending"}
			}
		case observed.State.Status == "exited" || observed.State.Status == "dead":
			finished, parseErr := time.Parse(time.RFC3339Nano, observed.State.FinishedAt)
			if parseErr != nil || finished.IsZero() || finished.After(state.Deadline) {
				// A late or unverifiable exit does not prove timeout enforcement.
				state.Result = &DiagnosticExecutionResult{Outcome: "unknown", OutputWithheld: true, Cleanup: "pending"}
			}
		default:
			state.Result = &DiagnosticExecutionResult{Outcome: "unknown", OutputWithheld: true, Cleanup: "pending"}
		}
		if state.Result != nil {
			if err = saveDiagnosticState(path, state); err != nil {
				return engine.ApplyResult{}, err
			}
		}
	}
	if err = s.finish(cleanupCtx, path, name, hash, &state, state.Cancelled || timedOut || ctx.Err() != nil || waitErr != nil); err != nil {
		return engine.ApplyResult{}, err
	}
	return engine.ApplyResult{Result: state.Result}, nil
}

func diagnosticDockerWait(ctx context.Context, config, id string) (string, string, error) {
	cmd := exec.CommandContext(ctx, diagnosticDockerBinary, "--host", "unix:///var/run/docker.sock", "--config", config, "container", "wait", id)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "HOME=/nonexistent", "DOCKER_CONFIG=" + config}
	out, stderr := &limitedDiagnosticBuffer{limit: 1024}, &limitedDiagnosticBuffer{limit: 4096}
	cmd.Stdout, cmd.Stderr = out, stderr
	err := cmd.Run()
	return out.String(), stderr.String(), err
}

// finish records evidence before removal. Errors leave cleanup pending for retry.
func (s *diagnosticExecutionStep) finish(ctx context.Context, path, name, hash string, state *diagnosticExecutionState, stop bool) error {
	c, err := s.inspect(ctx, name, hash)
	if err != nil {
		return err
	}
	if c != nil && stop && (c.State.Status == "running" || c.State.Status == "paused" || c.State.Status == "restarting") {
		_, _, _ = diagnosticDocker(ctx, s.dockerConfig, "container", "kill", c.ID)
		c, err = s.inspect(ctx, name, hash)
		if err != nil {
			return err
		}
	}
	if state.Result == nil {
		result := &DiagnosticExecutionResult{Outcome: "unknown", OutputWithheld: true, OutputLoss: true, Cleanup: "pending"}
		if state.Cancelled {
			result.Outcome = "cancelled"
		}
		if c != nil && (c.State.Status == "exited" || c.State.Status == "dead") {
			code := c.State.ExitCode
			result.ExitCode = &code
			result.OOMKilled = c.State.OOMKilled
			result.Outcome = "exited"
			if state.Cancelled {
				result.Outcome = "cancelled"
			}
		}
		state.Result = result
	}
	if c != nil {
		if c.State.Status == "exited" || c.State.Status == "dead" {
			code := c.State.ExitCode
			state.Result.ExitCode = &code
			state.Result.OOMKilled = c.State.OOMKilled
		}
		// Never persist text: image-baked secrets are outside the registry. The
		// rotating daemon log may already have discarded earlier output.
		_, _, _ = diagnosticDocker(ctx, s.dockerConfig, "container", "logs", c.ID)
		state.Result.OutputLoss = true
	}
	state.Result.Cleanup = "pending"
	if err := saveDiagnosticState(path, *state); err != nil {
		return err
	}
	if c != nil {
		if _, _, err := diagnosticDocker(ctx, s.dockerConfig, "container", "rm", "--force", c.ID); err != nil {
			return errors.New("diagnostic cleanup pending")
		}
	}
	remaining, err := s.inspect(ctx, name, hash)
	if err != nil {
		return err
	}
	if remaining != nil {
		return errors.New("diagnostic cleanup pending")
	}
	state.Result.Cleanup = "complete"
	return saveDiagnosticState(path, *state)
}
