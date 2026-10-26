package upgrade

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
)

// HealthProbe runs after an atomic replacement and before the journal records
// the release as installed. A non-nil error means the new binary must not stay
// active; the manager restores the verified backup automatically.
type HealthProbe func(ctx context.Context, binaryPath string) (string, error)

// ProbeConfig builds the default version/readiness probe for one release. It
// executes the newly installed binary directly (never through a shell) and
// keeps its output bounded and secret-free.
type ProbeConfig struct {
	Target      Target
	Manifest    Manifest
	DatabaseURL string
	Timeout     time.Duration
}

const maxProbeOutput = 64 << 10

// NewHealthProbe returns the health probe used by `eregion upgrade apply`.
//
// The operator binary must report the manifest version through `version
// --json`. When a database URL is available, the probe also runs the read-only
// `system migrate --dry-run` and requires the new binary's required schema to
// match the manifest and the database to be current. The node binary reports
// the same build metadata through `version --json`; a plain `version` fallback
// is accepted for older node builds, but either form must report the exact
// manifest version. A development node binary never satisfies a release.
func NewHealthProbe(config ProbeConfig) HealthProbe {
	timeout := config.Timeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	return func(ctx context.Context, binaryPath string) (string, error) {
		probeCtx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()
		version, err := probeVersion(probeCtx, binaryPath, config.Target, config.Manifest.Version)
		if err != nil {
			return "", err
		}
		// Readiness is a control-plane capability. A portable operator CLI on
		// macOS or Windows still gets the version probe; it cannot run the
		// Linux-only migration plan.
		if config.Target == TargetNode || runtime.GOOS != "linux" || strings.TrimSpace(config.DatabaseURL) == "" {
			return version, nil
		}
		readiness, err := probeReadiness(probeCtx, binaryPath, config.DatabaseURL, config.Manifest.SchemaVersion)
		if err != nil {
			return "", err
		}
		return version + "; " + readiness, nil
	}
}

func probeVersion(ctx context.Context, binaryPath string, target Target, want string) (string, error) {
	output, err := runProbe(ctx, binaryPath, nil, "version", "--json")
	if err != nil {
		return "", contract.NewFault(contract.Unavailable, "the installed operator binary failed its version probe", "The previous binary was restored; inspect the artifact and retry the release.")
	}
	var envelope struct {
		OK   bool `json:"ok"`
		Data struct {
			Version string `json:"version"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(output), &envelope); err != nil || !envelope.OK || envelope.Data.Version == "" {
		return "", contract.NewFault(contract.PreconditionFailed, "the installed operator binary did not return a valid version result", "The previous binary was restored; publish a binary that implements the result contract.")
	}
	if envelope.Data.Version != want {
		return "", contract.NewFault(contract.PreconditionFailed, "the installed operator binary reports a different version than the manifest", "The previous binary was restored; publish the artifact that matches the signed manifest.")
	}
	return "version probe: " + envelope.Data.Version, nil
}

func probeReadiness(ctx context.Context, binaryPath, databaseURL string, manifestSchema int) (string, error) {
	output, err := runProbe(ctx, binaryPath, []string{"EREGION_DATABASE_URL=" + databaseURL}, "system", "migrate", "--dry-run", "--json")
	if err != nil {
		return "", contract.NewFault(contract.PreconditionFailed, "the new binary could not read the database migration plan", "Run system migrate with the new binary before activating it; the previous binary was restored.")
	}
	var envelope struct {
		OK   bool `json:"ok"`
		Data struct {
			Current  int `json:"current"`
			Required int `json:"required"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(output), &envelope); err != nil || !envelope.OK {
		return "", contract.NewFault(contract.PreconditionFailed, "the new binary did not return a valid migration plan", "The previous binary was restored; check the release artifact and the database URL.")
	}
	if envelope.Data.Required != manifestSchema {
		return "", contract.NewFault(contract.PreconditionFailed, "the new binary requires a different schema than the signed manifest", "The previous binary was restored; republish a consistent release.")
	}
	if envelope.Data.Current != envelope.Data.Required {
		return "", contract.NewFault(contract.PreconditionFailed, fmt.Sprintf("database schema is %d and the new binary requires %d", envelope.Data.Current, envelope.Data.Required), "Run eregion system migrate before activating this release; the previous binary was restored.")
	}
	return fmt.Sprintf("readiness probe: schema %d current", envelope.Data.Current), nil
}

// runProbe executes one bounded command with a minimal environment. It never
// uses a shell, never forwards unrelated credentials, and never includes the
// child's raw output in an error.
func runProbe(ctx context.Context, binaryPath string, extraEnv []string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, binaryPath, args...)
	env := []string{"PATH=" + os.Getenv("PATH"), "HOME=" + os.Getenv("HOME"), "LANG=C.UTF-8", "LC_ALL=C.UTF-8"}
	env = append(env, extraEnv...)
	command.Env = env
	stdout := &boundedBuffer{limit: maxProbeOutput}
	stderr := &boundedBuffer{limit: maxProbeOutput}
	command.Stdout, command.Stderr = stdout, stderr
	if err := command.Run(); err != nil {
		return "", err
	}
	return strings.TrimSpace(stdout.String()), nil
}

type boundedBuffer struct {
	buffer bytes.Buffer
	limit  int
}

func (b *boundedBuffer) Write(raw []byte) (int, error) {
	if remaining := b.limit - b.buffer.Len(); remaining > 0 {
		if len(raw) > remaining {
			_, _ = b.buffer.Write(raw[:remaining])
		} else {
			_, _ = b.buffer.Write(raw)
		}
	}
	return len(raw), nil
}

func (b *boundedBuffer) String() string { return b.buffer.String() }

func firstLine(text string) string {
	if index := strings.IndexByte(text, '\n'); index >= 0 {
		text = text[:index]
	}
	return strings.TrimSpace(text)
}
