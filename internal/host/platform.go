package host

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// Role is the node responsibility selected at bootstrap time. It is an
// explicit placement decision, not a hint. Telemetry has its own dedicated
// role: bootstrap prepares Docker/Compose prerequisites there and never
// installs a telemetry stack or co-locates it with the control plane.
type Role string

const (
	RoleMain        Role = "main"
	RoleApplication Role = "application"
	RoleBuilder     Role = "builder"
	RoleTelemetry   Role = "telemetry"
)

// Roles lists every role this increment can configure.
func Roles() []Role { return []Role{RoleMain, RoleApplication, RoleBuilder, RoleTelemetry} }

func (r Role) Valid() bool {
	for _, candidate := range Roles() {
		if r == candidate {
			return true
		}
	}
	return false
}

// RoleConfig is the exported, reusable description of what a role runs. It is
// the single source of truth for step selection and for later node enrollment
// code that needs to check capabilities without importing the CLI.
type RoleConfig struct {
	Role Role `json:"role"`
	// ControlPlane installs the API/worker services and the external database
	// migration step.
	ControlPlane bool `json:"control_plane"`
	// Runtime installs the Docker container runtime.
	Runtime bool `json:"runtime"`
	// Builder installs the pinned Cloud Native Buildpacks pack CLI.
	Builder bool `json:"buildpacks"`
	// Registry runs the local OCI registry. Only the main role has it by
	// default: builds run on the main node before v1.
	Registry bool `json:"registry"`
	// NodeAgent installs the node binary, node configuration, and state
	// directory reached over the pinned node transport.
	NodeAgent bool `json:"node_agent"`
	// Telemetry prepares a dedicated telemetry host. It installs the runtime
	// and Compose prerequisites only: no collector, storage, or telemetry
	// stack is installed by bootstrap.
	Telemetry bool `json:"telemetry"`
}

// ConfigForRole returns the explicit configuration for role. Unknown and
// out-of-scope roles fail closed; an operator cannot silently get a partial
// control plane by misspelling a role.
func ConfigForRole(role Role) (RoleConfig, error) {
	switch role {
	case RoleMain:
		return RoleConfig{Role: role, ControlPlane: true, Runtime: true, Builder: true, Registry: true}, nil
	case RoleApplication:
		return RoleConfig{Role: role, Runtime: true, NodeAgent: true}, nil
	case RoleBuilder:
		return RoleConfig{Role: role, Runtime: true, Builder: true, NodeAgent: true}, nil
	case RoleTelemetry:
		// Telemetry is a dedicated node. Bootstrap prepares Docker and Compose
		// and the node execution account; it never installs a telemetry stack
		// and never co-locates it with the control plane.
		return RoleConfig{Role: role, Runtime: true, NodeAgent: true, Telemetry: true}, nil
	case "edge":
		return RoleConfig{}, fmt.Errorf("the edge role is not implemented; its scope is an open placement decision")
	default:
		return RoleConfig{}, fmt.Errorf("unknown host role %q; use main, application, builder, or telemetry", role)
	}
}

// Platform is the distribution identity bootstrap verified.
type Platform struct {
	ID         string `json:"id"`
	VersionID  string `json:"version_id"`
	Codename   string `json:"codename,omitempty"`
	PrettyName string `json:"pretty_name,omitempty"`
	Arch       string `json:"arch"`
}

func (p Platform) String() string {
	name := p.ID
	if p.VersionID != "" {
		name += " " + p.VersionID
	}
	if p.Codename != "" {
		name += " (" + p.Codename + ")"
	}
	if p.Arch != "" {
		name += " " + p.Arch
	}
	return name
}

// SupportEntry is one supported distribution/architecture pair.
type SupportEntry struct {
	ID        string `json:"id"`
	VersionID string `json:"version_id"`
	Arch      string `json:"arch"`
}

// SupportMatrix is the explicit compatibility promise. A release not listed
// here is rejected before any package, service, or file mutation. amd64 and
// arm64 are the only configured artifact architectures. Real VM verification
// of the matrix is a separate release gate, not implied by unit tests.
func SupportMatrix() []SupportEntry {
	var entries []SupportEntry
	for _, distro := range []struct {
		id       string
		versions []string
	}{
		{"debian", []string{"13"}},
		{"ubuntu", []string{"26.04"}},
	} {
		for _, version := range distro.versions {
			for _, arch := range []string{"amd64", "arm64"} {
				entries = append(entries, SupportEntry{ID: distro.id, VersionID: version, Arch: arch})
			}
		}
	}
	return entries
}

// Supported reports whether the exact distribution, release, and architecture
// are in the verified matrix.
func (p Platform) Supported() bool {
	for _, entry := range SupportMatrix() {
		if p.ID == entry.ID && p.VersionID == entry.VersionID && p.Arch == entry.Arch {
			return true
		}
	}
	return false
}

// ErrPlatformUnsupported is permanent: no retry changes the distribution.
var ErrPlatformUnsupported = errors.New("unsupported host platform")

// ParsePlatform reads /etc/os-release style content. arch must be a Go
// architecture name (amd64 or arm64); normalizing x86_64/aarch64 here keeps the
// matrix honest when tests provide fixture content.
func ParsePlatform(raw []byte, arch string) (Platform, error) {
	values := map[string]string{}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 0, 4096), 64*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if unquoted, err := strconv.Unquote(value); err == nil {
			value = unquoted
		} else if len(value) >= 2 && (value[0] == '\'' && value[len(value)-1] == '\'') {
			value = value[1 : len(value)-1]
		}
		values[key] = value
	}
	if err := scanner.Err(); err != nil {
		return Platform{}, fmt.Errorf("read os-release: %w", err)
	}
	id := strings.ToLower(strings.TrimSpace(values["ID"]))
	version := strings.TrimSpace(values["VERSION_ID"])
	if id == "" || version == "" {
		return Platform{}, fmt.Errorf("os-release does not identify a distribution and release")
	}
	switch arch {
	case "x86_64":
		arch = "amd64"
	case "aarch64", "arm64":
		arch = "arm64"
	}
	return Platform{
		ID:         id,
		VersionID:  version,
		Codename:   strings.TrimSpace(values["VERSION_CODENAME"]),
		PrettyName: strings.TrimSpace(values["PRETTY_NAME"]),
		Arch:       arch,
	}, nil
}

// DetectPlatform reads osReleasePath and the current process architecture.
func DetectPlatform(osReleasePath string, arch string) (Platform, error) {
	if osReleasePath == "" {
		osReleasePath = "/etc/os-release"
	}
	if arch == "" {
		arch = runtime.GOARCH
	}
	raw, err := os.ReadFile(osReleasePath) //nolint:gosec // G304: fixed system metadata path or an explicit test fixture.
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Platform{}, fmt.Errorf("cannot verify the host distribution: %s is missing", osReleasePath)
		}
		return Platform{}, fmt.Errorf("cannot read %s: %w", osReleasePath, err)
	}
	return ParsePlatform(raw, arch)
}
