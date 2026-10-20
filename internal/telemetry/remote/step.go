// Package remote implements the narrow node step family used to apply the
// telemetry install plan through the existing node protocol. The protocol has
// no arbitrary filesystem or shell request; instead the control plane sends
// bounded, versioned steps that this family validates on the node.
//
// It deliberately supports only:
//
//   - creating an absolute directory beneath a declared root,
//   - writing one bounded regular file atomically beneath that root, and
//   - running a fixed `docker compose` argv vector with no shell.
//
// Anything else is refused. The step payload carries file contents over the
// encrypted transport and they are represented in step inputs only by a hash,
// so the node journal never stores credential plaintext.
package remote

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/engine"
)

// MaxFileBytes bounds one rendered telemetry file. TLS material and collector
// configuration are much smaller; a larger payload is rejected rather than
// streamed into an unbounded node write.
const MaxFileBytes = 4 << 20

// MaxCommandOutput bounds the diagnostic text retained from a compose command.
const MaxCommandOutput = 8192

var (
	mkdirStepPattern = regexp.MustCompile(`^telemetry-mkdir-[0-9]{2}-v[0-9]+$`)
	fileStepPattern  = regexp.MustCompile(`^telemetry-file-[0-9]{2}-v[0-9]+$`)
	argvStepPattern  = regexp.MustCompile(`^telemetry-argv-[0-9]{2}-v[0-9]+$`)
)

// Family is one step family the node registry can register. The shapes match
// node.StepFactory without importing node, which would create an import cycle.
type Family struct {
	Match func(string) bool
	Build func(engine.Execution, json.RawMessage) (engine.Step, error)
}

// StepFamilies returns the telemetry step families.
func StepFamilies() []Family {
	return []Family{
		{Match: mkdirStepPattern.MatchString, Build: buildMkdirStep},
		{Match: fileStepPattern.MatchString, Build: buildFileStep},
		{Match: argvStepPattern.MatchString, Build: buildArgvStep},
	}
}

func decode(raw json.RawMessage, value any) error {
	if len(raw) == 0 {
		return errors.New("telemetry step payload is required")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("invalid telemetry step payload: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("telemetry step payload has trailing data")
	}
	return nil
}

// Root is an absolute directory the step is allowed to touch. It is never the
// filesystem root, and every path is checked to stay beneath it.
func cleanRoot(value string) (string, error) {
	if value == "" || !filepath.IsAbs(value) {
		return "", errors.New("telemetry step root must be absolute")
	}
	root := filepath.Clean(value)
	if root == string(filepath.Separator) || root == "." {
		return "", errors.New("telemetry step root must not be the filesystem root")
	}
	return root, nil
}

func within(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return false
	}
	return true
}

func cleanPathOrRoot(root, value string) (string, error) {
	if value == "" || !filepath.IsAbs(value) {
		return "", errors.New("telemetry step path must be absolute")
	}
	path := filepath.Clean(value)
	if path != root && !within(root, path) {
		return "", errors.New("telemetry step path escapes its declared root")
	}
	return path, nil
}

func cleanPath(root, value string) (string, error) {
	if value == "" || !filepath.IsAbs(value) {
		return "", errors.New("telemetry step path must be absolute")
	}
	path := filepath.Clean(value)
	if path == root || !within(root, path) {
		return "", errors.New("telemetry step path escapes its declared root")
	}
	return path, nil
}

func cleanMode(value uint32, _ bool) (fs.FileMode, error) {
	mode := fs.FileMode(value)
	if mode == 0 || mode&^fs.FileMode(0o777) != 0 || mode&0o002 != 0 {
		return 0, errors.New("telemetry step mode must be 0600–0775 and never world-writable")
	}
	return mode, nil
}

// ---------------------------------------------------------------- mkdir step

type MkdirPayload struct {
	Step string `json:"step"`
	Root string `json:"root"`
	Path string `json:"path"`
	Mode uint32 `json:"mode"`
}

type MkdirStep struct {
	exec    engine.Execution
	payload MkdirPayload
}

func buildMkdirStep(exec engine.Execution, raw json.RawMessage) (engine.Step, error) {
	var payload MkdirPayload
	if err := decode(raw, &payload); err != nil {
		return nil, err
	}
	if payload.Step != exec.Step {
		return nil, fmt.Errorf("step ID %q does not match the payload step %q", exec.Step, payload.Step)
	}
	root, err := cleanRoot(payload.Root)
	if err != nil {
		return nil, err
	}
	path, err := cleanPathOrRoot(root, payload.Path)
	if err != nil {
		return nil, err
	}
	if _, err := cleanMode(payload.Mode, false); err != nil {
		return nil, err
	}
	payload.Root, payload.Path = root, path
	return &MkdirStep{exec: exec, payload: payload}, nil
}

func (s *MkdirStep) ID() string                      { return s.payload.Step }
func (s *MkdirStep) Describe() string                { return "Ensure a telemetry directory exists" }
func (s *MkdirStep) CancelBoundary() engine.Boundary { return engine.BoundaryBeforeApply }
func (s *MkdirStep) Inputs() ([]byte, error) {
	return json.Marshal(struct {
		Root string `json:"root"`
		Path string `json:"path"`
		Mode uint32 `json:"mode"`
	}{s.payload.Root, s.payload.Path, s.payload.Mode})
}

func (s *MkdirStep) Check(_ context.Context, _ engine.Runtime) (engine.CheckResult, error) {
	info, err := os.Lstat(s.payload.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return engine.CheckResult{Complete: false, Detail: "telemetry directory is absent"}, nil
		}
		return engine.CheckResult{}, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return engine.CheckResult{}, fmt.Errorf("%w: telemetry path is not a real directory", engine.ErrPermanent)
	}
	return engine.CheckResult{Complete: true, Detail: "telemetry directory already exists"}, nil
}

func (s *MkdirStep) Apply(_ context.Context, _ engine.Runtime, _ engine.Reporter) (engine.ApplyResult, error) {
	mode, _ := cleanMode(s.payload.Mode, false)
	if err := os.MkdirAll(s.payload.Path, mode); err != nil {
		return engine.ApplyResult{}, err
	}
	if err := os.Chmod(s.payload.Path, mode); err != nil {
		return engine.ApplyResult{}, err
	}
	return engine.ApplyResult{Result: map[string]any{"path": s.payload.Path}}, nil
}

// ----------------------------------------------------------------- file step

type FilePayload struct {
	Step          string `json:"step"`
	Root          string `json:"root"`
	Path          string `json:"path"`
	Mode          uint32 `json:"mode"`
	ContentBase64 string `json:"content_base64"`
	SHA256        string `json:"sha256"`
}

type FileStep struct {
	exec    engine.Execution
	payload FilePayload
	content []byte
}

func buildFileStep(exec engine.Execution, raw json.RawMessage) (engine.Step, error) {
	var payload FilePayload
	if err := decode(raw, &payload); err != nil {
		return nil, err
	}
	if payload.Step != exec.Step {
		return nil, fmt.Errorf("step ID %q does not match the payload step %q", exec.Step, payload.Step)
	}
	root, err := cleanRoot(payload.Root)
	if err != nil {
		return nil, err
	}
	path, err := cleanPath(root, payload.Path)
	if err != nil {
		return nil, err
	}
	if _, err := cleanMode(payload.Mode, true); err != nil {
		return nil, err
	}
	content, err := base64.StdEncoding.DecodeString(payload.ContentBase64)
	if err != nil {
		return nil, errors.New("telemetry file content is not valid base64")
	}
	if len(content) > MaxFileBytes {
		return nil, fmt.Errorf("telemetry file content exceeds %d bytes", MaxFileBytes)
	}
	sum := sha256.Sum256(content)
	if payload.SHA256 != hex.EncodeToString(sum[:]) {
		return nil, errors.New("telemetry file content hash does not match")
	}
	payload.Root, payload.Path = root, path
	return &FileStep{exec: exec, payload: payload, content: content}, nil
}

func (s *FileStep) ID() string                      { return s.payload.Step }
func (s *FileStep) Describe() string                { return "Write one rendered telemetry file" }
func (s *FileStep) CancelBoundary() engine.Boundary { return engine.BoundaryAfterApply }
func (s *FileStep) Inputs() ([]byte, error) {
	// The content is represented by its hash: the node journal and the control
	// plane step row must never contain credential plaintext.
	return json.Marshal(struct {
		Root string `json:"root"`
		Path string `json:"path"`
		Mode uint32 `json:"mode"`
		Hash string `json:"sha256"`
	}{s.payload.Root, s.payload.Path, s.payload.Mode, s.payload.SHA256})
}

func fileMatches(path, wantHash string) (bool, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return false, fmt.Errorf("%w: telemetry file path is not a regular file", engine.ErrPermanent)
	}
	file, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, io.LimitReader(file, MaxFileBytes+1)); err != nil {
		return false, err
	}
	if info.Size() > MaxFileBytes {
		return false, fmt.Errorf("%w: telemetry file is larger than the step bound", engine.ErrPermanent)
	}
	return hex.EncodeToString(hash.Sum(nil)) == wantHash, nil
}

func (s *FileStep) Check(_ context.Context, _ engine.Runtime) (engine.CheckResult, error) {
	ok, err := fileMatches(s.payload.Path, s.payload.SHA256)
	if err != nil {
		return engine.CheckResult{}, err
	}
	if ok {
		return engine.CheckResult{Complete: true, Detail: "telemetry file already has the rendered content"}, nil
	}
	return engine.CheckResult{Complete: false, Detail: "telemetry file is absent or differs"}, nil
}

func (s *FileStep) Apply(_ context.Context, _ engine.Runtime, _ engine.Reporter) (engine.ApplyResult, error) {
	mode, _ := cleanMode(s.payload.Mode, true)
	if err := s.ensureParent(); err != nil {
		return engine.ApplyResult{}, err
	}
	if err := writeAtomic(s.payload.Path, s.content, mode); err != nil {
		return engine.ApplyResult{}, err
	}
	return engine.ApplyResult{Result: map[string]any{
		"path": s.payload.Path, "bytes": len(s.content), "sha256": s.payload.SHA256,
	}}, nil
}

// ensureParent refuses to write through a symlinked parent that escapes the
// declared root.
func (s *FileStep) ensureParent() error {
	parent := filepath.Dir(s.payload.Path)
	if err := os.MkdirAll(parent, 0o750); err != nil {
		return err
	}
	realRoot, err := filepath.EvalSymlinks(s.payload.Root)
	if err != nil {
		return err
	}
	realParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return err
	}
	if !within(realRoot, realParent) {
		return fmt.Errorf("%w: telemetry file parent escapes its declared root", engine.ErrPermanent)
	}
	return nil
}

func writeAtomic(path string, data []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, ".eregion-telemetry-*")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer func() { _ = os.Remove(tempName) }()
	if err := temp.Chmod(mode); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempName, path)
}

// ----------------------------------------------------------------- argv step

type ArgvPayload struct {
	Step           string   `json:"step"`
	Root           string   `json:"root"`
	Argv           []string `json:"argv"`
	TimeoutSeconds int      `json:"timeout_seconds,omitempty"`
}

type ArgvStep struct {
	exec    engine.Execution
	payload ArgvPayload
}

func buildArgvStep(exec engine.Execution, raw json.RawMessage) (engine.Step, error) {
	var payload ArgvPayload
	if err := decode(raw, &payload); err != nil {
		return nil, err
	}
	if payload.Step != exec.Step {
		return nil, fmt.Errorf("step ID %q does not match the payload step %q", exec.Step, payload.Step)
	}
	root, err := cleanRoot(payload.Root)
	if err != nil {
		return nil, err
	}
	if err := validateComposeArgv(root, payload.Argv); err != nil {
		return nil, err
	}
	if payload.TimeoutSeconds == 0 {
		payload.TimeoutSeconds = 300
	}
	if payload.TimeoutSeconds < 1 || payload.TimeoutSeconds > 900 {
		return nil, errors.New("telemetry command timeout must be 1–900 seconds")
	}
	payload.Root = root
	return &ArgvStep{exec: exec, payload: payload}, nil
}

func (s *ArgvStep) ID() string                      { return s.payload.Step }
func (s *ArgvStep) Describe() string                { return "Run a bounded docker compose command" }
func (s *ArgvStep) CancelBoundary() engine.Boundary { return engine.BoundaryAfterApply }
func (s *ArgvStep) Inputs() ([]byte, error) {
	return json.Marshal(struct {
		Root string   `json:"root"`
		Argv []string `json:"argv"`
	}{s.payload.Root, s.payload.Argv})
}

func (s *ArgvStep) Check(context.Context, engine.Runtime) (engine.CheckResult, error) {
	return engine.CheckResult{Complete: false, Detail: "compose reconciliation always runs"}, nil
}

func (s *ArgvStep) Apply(ctx context.Context, _ engine.Runtime, report engine.Reporter) (engine.ApplyResult, error) {
	runCtx, cancel := context.WithTimeout(ctx, time.Duration(s.payload.TimeoutSeconds)*time.Second)
	defer cancel()
	command := exec.CommandContext(runCtx, s.payload.Argv[0], s.payload.Argv[1:]...) //nolint:gosec // G204: argv is a validated compose vector and no shell is used.
	command.Env = commandEnvironment()
	output, err := command.CombinedOutput()
	bounded := output
	if len(bounded) > MaxCommandOutput {
		bounded = bounded[len(bounded)-MaxCommandOutput:]
	}
	if err != nil {
		if runCtx.Err() != nil {
			return engine.ApplyResult{}, runCtx.Err()
		}
		return engine.ApplyResult{}, fmt.Errorf("docker compose failed: %s", strings.TrimSpace(string(bounded)))
	}
	if report != nil {
		report(engine.Event{Kind: "log", Step: s.ID(), Message: "docker compose converged"})
	}
	return engine.ApplyResult{Result: map[string]any{"stdout": strings.TrimSpace(string(bounded))}}, nil
}

func commandEnvironment() []string {
	env := []string{"DOCKER_CLI_HINTS=false"}
	for _, name := range []string{"PATH", "HOME", "DOCKER_CONFIG", "XDG_RUNTIME_DIR"} {
		if value := os.Getenv(name); value != "" {
			env = append(env, name+"="+value)
		}
	}
	if os.Getenv("PATH") == "" {
		env = append(env, "PATH=/usr/local/bin:/usr/bin:/bin")
	}
	return env
}

// validateComposeArgv accepts exactly the compose shape the installer renders.
// It is intentionally narrower than the platform argv validator: telemetry
// installs never need an arbitrary program.
func validateComposeArgv(root string, argv []string) error {
	if len(argv) < 4 || argv[0] != "docker" || argv[1] != "compose" {
		return errors.New("telemetry command must be docker compose <subcommand>")
	}
	for _, arg := range argv {
		if arg == "" || len(arg) > 4096 || strings.ContainsAny(arg, "\x00\r\n") {
			return errors.New("telemetry command argument is empty, too long, or contains a control character")
		}
	}
	index := 2
	composeFile := ""
	for index < len(argv) {
		switch argv[index] {
		case "--file":
			if index+1 >= len(argv) || composeFile != "" {
				return errors.New("telemetry compose command needs exactly one --file")
			}
			composeFile = argv[index+1]
			index += 2
		case "pull", "up", "ps":
			goto subcommand
		default:
			return fmt.Errorf("unsupported telemetry compose option %q", argv[index])
		}
	}
	return errors.New("telemetry compose command is missing a subcommand")

subcommand:
	if composeFile == "" || !filepath.IsAbs(composeFile) {
		return errors.New("telemetry compose command needs an absolute --file")
	}
	file := filepath.Clean(composeFile)
	if !within(root, file) {
		return errors.New("telemetry compose file escapes its declared root")
	}
	info, err := os.Lstat(file)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("telemetry compose file is not a readable regular file")
	}
	switch argv[index] {
	case "pull":
		if index+1 != len(argv) {
			return errors.New("docker compose pull accepts no extra arguments")
		}
	case "up":
		for _, arg := range argv[index+1:] {
			switch arg {
			case "--detach", "--remove-orphans", "--wait":
			default:
				return fmt.Errorf("unsupported docker compose up option %q", arg)
			}
		}
	case "ps":
		if len(argv) != index+2 || argv[index+1] != "--format=json" && argv[index+1] != "json" {
			return errors.New("docker compose ps is limited to --format json")
		}
	}
	return nil
}
