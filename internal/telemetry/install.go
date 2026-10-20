package telemetry

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

// Runner is the only process boundary in this package. Commands are argv
// vectors; there is deliberately no shell, no environment interpolation, and no
// place for a credential to appear on a command line.
type Runner interface {
	Run(context.Context, string, ...string) error
}

// ExecRunner executes directly, without a shell.
type ExecRunner struct{}

func (ExecRunner) Run(ctx context.Context, name string, args ...string) error {
	command := exec.CommandContext(ctx, name, args...)
	command.Env = append(os.Environ(), "DOCKER_CLI_HINTS=false")
	if output, err := command.CombinedOutput(); err != nil {
		// Output from docker can contain registry URLs and paths. It is not a
		// credential store, but trim it to one bounded line and never include
		// the environment or the command's full argument list.
		return fmt.Errorf("%s failed: %s", name, bounded(string(output), 2048))
	}
	return nil
}

func bounded(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}

// TargetIdentity binds one install to a concrete enrolled node. The host ID is
// the pinned host-key identity recorded by enrollment; a dispatch must verify it
// before opening an executor. This is what prevents a remote telemetry action
// from silently installing on the control-plane worker.
type TargetIdentity struct {
	Node           string
	PrivateAddress string
	HostID         string
}

func (t TargetIdentity) Validate() error {
	if !nodeNamePattern.MatchString(t.Node) {
		return contract.NewFault(contract.ValidationFailed, "invalid telemetry target node", "Use the enrolled node name.")
	}
	ip := net.ParseIP(t.PrivateAddress)
	if ip == nil || !privateAddress(ip) {
		return contract.NewFault(contract.ValidationFailed, "invalid telemetry target private address", "Use the private address recorded during enrollment.")
	}
	if len(t.HostID) < 16 || len(t.HostID) > 256 || strings.ContainsAny(t.HostID, "\x00\r\n\t ") {
		return contract.NewFault(contract.ValidationFailed, "telemetry target has no pinned host identity", "Require the host-key fingerprint recorded at enrollment before dispatching.")
	}
	return nil
}

func (t TargetIdentity) Equal(other TargetIdentity) bool {
	return t.Node == other.Node && t.PrivateAddress == other.PrivateAddress && t.HostID == other.HostID
}

// Filesystem is the node-local file boundary. It is intentionally separate from
// Runner so a real dispatch must provide both command execution and file
// transport; a command-only runner cannot be mistaken for an install target.
type Filesystem interface {
	MkdirAll(context.Context, string, fs.FileMode) error
	WriteFile(context.Context, string, []byte, fs.FileMode) error
}

// NodeExecutor is a dispatched executor bound to one verified target identity.
type NodeExecutor interface {
	Runner
	Filesystem
	Identity() TargetIdentity
}

// Dispatch opens an executor for a pinned target. Open must verify the host
// identity before returning; a command-only or local fallback is not acceptable.
type Dispatch interface {
	Open(context.Context, TargetIdentity) (NodeExecutor, error)
}

// OperationDispatch is the durable production form of Dispatch. It carries the
// accepted operation identity and the prepared install root so every node step
// is journaled under a stable effect key and cannot write outside that root.
// Implementations that only expose Open remain usable by tests and local
// previews.
type OperationDispatch interface {
	OpenOperation(context.Context, TargetIdentity, contract.OperationID, uint32, string) (NodeExecutor, error)
}

func openDispatch(ctx context.Context, dispatch Dispatch, identity TargetIdentity, exec platform.Execution, root string) (NodeExecutor, error) {
	if operationDispatch, ok := dispatch.(OperationDispatch); ok {
		return operationDispatch.OpenOperation(ctx, identity, exec.OperationID, exec.Attempt, root)
	}
	return dispatch.Open(ctx, identity)
}

// LocalFilesystem is the explicit local target used by CLI/bootstrap installs.
// Platform actions never use it implicitly.
type LocalFilesystem struct{}

func (LocalFilesystem) MkdirAll(_ context.Context, path string, mode fs.FileMode) error {
	return os.MkdirAll(path, mode)
}

func (LocalFilesystem) WriteFile(_ context.Context, path string, data []byte, mode fs.FileMode) error {
	return writeAtomic(path, data, mode)
}

// Installer renders and applies a plan idempotently. Re-applying rewrites the
// same files and asks Docker Compose to converge the same services; removed
// credentials disappear from the token file on the next apply.
type Installer struct {
	prepared Prepared
	agent    *PreparedAgent
	runner   Runner
	files    Filesystem
}

// NewInstaller is the explicit local CLI/bootstrap constructor. It writes to the
// local filesystem and runs commands locally.
func NewInstaller(prepared Prepared, runner Runner) *Installer {
	if runner == nil {
		runner = ExecRunner{}
	}
	return &Installer{prepared: prepared, runner: runner, files: LocalFilesystem{}}
}

// NewAgentInstaller is the explicit local agent constructor.
func NewAgentInstaller(prepared PreparedAgent, runner Runner) *Installer {
	if runner == nil {
		runner = ExecRunner{}
	}
	return &Installer{agent: &prepared, runner: runner, files: LocalFilesystem{}}
}

// NewInstallerWithExecutor applies a stack only through a verified node
// executor. It is the constructor used by durable platform actions.
func NewInstallerWithExecutor(prepared Prepared, executor NodeExecutor) *Installer {
	return &Installer{prepared: prepared, runner: executor, files: executor}
}

// NewAgentInstallerWithExecutor applies an agent only through a verified node
// executor.
func NewAgentInstallerWithExecutor(prepared PreparedAgent, executor NodeExecutor) *Installer {
	return &Installer{agent: &prepared, runner: executor, files: executor}
}

// Result records exactly what was written and which argv vectors ran. It never
// includes secret file contents.
type Result struct {
	RootDir  string
	Files    []string
	Commands []string
}

func (i *Installer) Apply(ctx context.Context) (Result, error) {
	if i == nil {
		return Result{}, contract.NewFault(contract.ValidationFailed, "no telemetry installer", "Construct an installer with NewInstaller or NewAgentInstaller.")
	}
	var plan Plan
	var err error
	switch {
	case i.agent != nil:
		plan, err = RenderAgentPlan(*i.agent)
	case i.prepared.Config.Node.Name != "":
		plan, err = RenderPlan(i.prepared)
	default:
		return Result{}, contract.NewFault(contract.ValidationFailed, "no telemetry configuration", "Prepare a telemetry node or node-agent configuration first.")
	}
	if err != nil {
		return Result{}, err
	}
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	files := i.files
	if files == nil {
		return Result{}, contract.NewFault(contract.Unavailable, "telemetry installer has no filesystem executor", "Use an explicit local installer or a dispatched node executor.")
	}
	if err := files.MkdirAll(ctx, plan.RootDir, 0750); err != nil {
		return Result{}, safeInstallError("create telemetry root", err)
	}
	result := Result{RootDir: plan.RootDir}
	for _, file := range plan.Files {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if err := files.WriteFile(ctx, file.Path, file.Data, file.Mode); err != nil {
			return result, safeInstallError("write "+filepath.Base(file.Path), err)
		}
		result.Files = append(result.Files, file.Path)
	}
	for _, command := range plan.Commands {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		if err := i.runner.Run(ctx, command.Name, command.Args...); err != nil {
			return result, safeInstallError("run "+command.Name, err)
		}
		result.Commands = append(result.Commands, command.Name+" "+joinArgs(command.Args))
	}
	sort.Strings(result.Files)
	return result, nil
}

func joinArgs(args []string) string {
	result := ""
	for index, arg := range args {
		if index > 0 {
			result += " "
		}
		result += arg
	}
	return result
}

func safeInstallError(action string, err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	// A node step's machine-readable refusal (for example changed inputs for an
	// existing effect) must not be flattened into a generic retryable failure.
	var fault *contract.Fault
	if errors.As(err, &fault) && fault.Code != contract.Unavailable {
		return fault
	}
	return contract.NewFault(contract.Unavailable, action+" failed", "Inspect node-local permissions and the container runtime; no partial state is assumed to be healthy.")
}

func writeAtomic(path string, data []byte, mode fs.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return err
	}
	temp, err := os.CreateTemp(dir, ".eregion-*")
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
	if err := os.Rename(tempName, path); err != nil {
		return err
	}
	handle, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer handle.Close()
	return handle.Sync()
}
