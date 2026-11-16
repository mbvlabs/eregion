// Package buildpacks holds the canonical Buildpacks recipe and execution settings.
package buildpacks

import (
	"fmt"
	"path"
	"regexp"
	"strings"
	"unicode"

	"github.com/google/go-containerregistry/pkg/name"
)

// GoTarget associates a process name with a repository-relative Go package.
type GoTarget struct {
	Process string `json:"process" toml:"process"`
	Target  string `json:"target" toml:"target"`
}

type Frontend struct {
	Directory   string   `json:"directory" toml:"directory"`
	Scripts     []string `json:"scripts" toml:"scripts"`
	StartScript string   `json:"start_script,omitempty" toml:"start_script"`
}

type Advanced struct {
	GoVersion    string `json:"go_version,omitempty" toml:"go_version"`
	GoBuildFlags string `json:"go_build_flags,omitempty" toml:"go_build_flags"`
	NodeVersion  string `json:"node_version,omitempty" toml:"node_version"`
}

type Settings struct {
	Order     []string  `json:"order,omitempty" toml:"order"`
	KeepFiles []string  `json:"keep_files,omitempty" toml:"keep_files"`
	Frontend  *Frontend `json:"frontend,omitempty" toml:"frontend"`
	Advanced  *Advanced `json:"advanced,omitempty" toml:"advanced"`
}

var (
	processPattern    = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,62}$`)
	targetPattern     = regexp.MustCompile(`^[A-Za-z0-9_./*-]+$`)
	scriptPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:_-]{0,63}$`)
	versionPattern    = regexp.MustCompile(`^[0-9]+(\.[0-9]+){0,2}([-.][0-9A-Za-z]+)?$`)
	builderURNPattern = regexp.MustCompile(`^urn:cnb:builder:[a-zA-Z0-9][a-zA-Z0-9._/-]*@[a-zA-Z0-9][a-zA-Z0-9._+-]*$`)
)

// NormalizeTargets copies, normalizes and validates target entries. Blank
// entries are rejected rather than silently losing a partially supplied target.
func NormalizeTargets(targets []GoTarget) ([]GoTarget, error) {
	if targets == nil {
		return nil, nil
	}
	out := make([]GoTarget, 0, len(targets))
	seen := make(map[string]bool, len(targets))
	for _, target := range targets {
		target.Process = strings.ToLower(strings.TrimSpace(target.Process))
		target.Target = strings.TrimSpace(target.Target)
		if target.Process == "" || target.Target == "" {
			return nil, fmt.Errorf("go target requires both process and target")
		}
		if !processPattern.MatchString(target.Process) || seen[target.Process] {
			return nil, fmt.Errorf("go target process %q is invalid or duplicated", target.Process)
		}
		if !targetPattern.MatchString(target.Target) || strings.HasPrefix(target.Target, "/") || strings.Contains(target.Target, "..") || len(target.Target) > 1024 {
			return nil, fmt.Errorf("go target %q must be a repository-relative Go package path", target.Target)
		}
		seen[target.Process] = true
		out = append(out, target)
	}
	return out, nil
}

// NormalizeSettings returns an independent canonical copy of the recipe.
func NormalizeSettings(input *Settings) (*Settings, error) {
	if input == nil {
		return nil, nil
	}
	out := *input
	keepFiles, err := normalizeKeepFiles(input.KeepFiles)
	if err != nil {
		return nil, err
	}
	order, err := normalizeOrder(input.Order)
	if err != nil {
		return nil, err
	}
	frontend, err := normalizeFrontend(input.Frontend)
	if err != nil {
		return nil, err
	}
	advanced, err := normalizeAdvanced(input.Advanced)
	if err != nil {
		return nil, err
	}
	out.KeepFiles, out.Order = keepFiles, order
	out.Frontend, out.Advanced = frontend, advanced
	return &out, nil
}

func normalizeKeepFiles(patterns []string) ([]string, error) {
	var out []string
	for _, pattern := range patterns {
		if strings.TrimSpace(pattern) == "" || strings.HasPrefix(pattern, "/") || strings.ContainsAny(pattern, ":\\") || len(pattern) > 1024 || strings.IndexFunc(pattern, unicode.IsControl) >= 0 {
			return nil, fmt.Errorf("keep file pattern %q must be a repository-relative glob without colon or control characters", pattern)
		}
		for _, part := range strings.Split(pattern, "/") {
			if part == ".." {
				return nil, fmt.Errorf("keep file pattern %q cannot leave the build context", pattern)
			}
		}
		out = append(out, pattern)
	}
	return out, nil
}

func normalizeOrder(input []string) ([]string, error) {
	if len(input) == 0 {
		return nil, nil
	}
	order := append([]string{}, input...)
	for _, ref := range order {
		if builderURNPattern.MatchString(ref) {
			continue
		}
		parts := strings.SplitN(ref, "/", 2)
		if strings.HasPrefix(ref, ".") || len(parts) != 2 || (!strings.ContainsAny(parts[0], ".:") && parts[0] != "localhost") || (!strings.Contains(parts[1], ":") && !strings.Contains(parts[1], "@")) {
			return nil, fmt.Errorf("buildpack %q must be a fully qualified OCI reference with tag or digest, or a builder URN", ref)
		}
		if _, err := name.ParseReference(ref, name.StrictValidation); err != nil {
			return nil, fmt.Errorf("invalid buildpack reference %q: %w", ref, err)
		}
	}
	return order, nil
}

func normalizeFrontend(input *Frontend) (*Frontend, error) {
	if input == nil {
		return nil, nil
	}
	frontend := *input
	frontend.StartScript = strings.TrimSpace(frontend.StartScript)
	if input.StartScript != "" && frontend.StartScript == "" {
		return nil, fmt.Errorf("frontend start script must be a package script name")
	}
	if frontend.StartScript != "" && !scriptPattern.MatchString(frontend.StartScript) {
		return nil, fmt.Errorf("frontend start script must be a package script name")
	}
	frontend.Directory = strings.TrimSpace(frontend.Directory)
	if strings.HasPrefix(frontend.Directory, "/") || strings.ContainsAny(frontend.Directory, "\x00\r\n") {
		return nil, fmt.Errorf("frontend directory must be relative")
	}
	if frontend.Directory != "" {
		frontend.Directory = path.Clean(frontend.Directory)
	}
	if frontend.Directory == ".." || strings.HasPrefix(frontend.Directory, "../") {
		return nil, fmt.Errorf("frontend directory cannot leave the build context")
	}
	frontend.Scripts = make([]string, 0, len(input.Scripts))
	for _, script := range input.Scripts {
		script = strings.TrimSpace(script)
		if script == "" {
			continue
		}
		if !scriptPattern.MatchString(script) {
			return nil, fmt.Errorf("frontend build script must be a package script name")
		}
		frontend.Scripts = append(frontend.Scripts, script)
	}
	return &frontend, nil
}

func normalizeAdvanced(input *Advanced) (*Advanced, error) {
	if input == nil {
		return nil, nil
	}
	advanced := *input
	advanced.GoVersion = strings.TrimSpace(advanced.GoVersion)
	advanced.GoBuildFlags = strings.TrimSpace(advanced.GoBuildFlags)
	advanced.NodeVersion = strings.TrimSpace(advanced.NodeVersion)
	if advanced.GoVersion != "" && !versionPattern.MatchString(advanced.GoVersion) {
		return nil, fmt.Errorf("buildpack Go version must look like 1.23.4")
	}
	if advanced.NodeVersion != "" && !versionPattern.MatchString(advanced.NodeVersion) {
		return nil, fmt.Errorf("buildpack Node version must look like 22.11.0")
	}
	if strings.ContainsAny(advanced.GoBuildFlags, "\r\n") || len(advanced.GoBuildFlags) > 512 {
		return nil, fmt.Errorf("buildpack Go build flags cannot contain line breaks or exceed 512 bytes")
	}
	if advanced == (Advanced{}) {
		return nil, nil
	}
	return &advanced, nil
}

// Environment returns generated pack variables; callers must give them
// precedence over user-supplied BuildEnv entries.
func Environment(settings *Settings, targets []GoTarget) map[string]string {
	env := make(map[string]string)
	if len(targets) > 0 {
		values := make([]string, 0, len(targets))
		for _, target := range targets {
			values = append(values, target.Target)
		}
		env["BP_GO_TARGETS"] = strings.Join(values, ":")
	}
	if settings == nil {
		return env
	}
	if len(settings.KeepFiles) > 0 {
		env["BP_KEEP_FILES"] = strings.Join(settings.KeepFiles, ":")
	}
	if settings.Frontend != nil {
		if settings.Frontend.StartScript != "" {
			env["BP_NPM_START_SCRIPT"] = settings.Frontend.StartScript
		}
		if len(settings.Frontend.Scripts) > 0 {
			env["BP_NODE_RUN_SCRIPTS"] = strings.Join(settings.Frontend.Scripts, ",")
		}
		if settings.Frontend.Directory != "" {
			env["BP_NODE_PROJECT_PATH"] = settings.Frontend.Directory
		}
	}
	if settings.Advanced != nil {
		if settings.Advanced.GoVersion != "" {
			env["BP_GO_VERSION"] = settings.Advanced.GoVersion
		}
		if settings.Advanced.GoBuildFlags != "" {
			env["BP_GO_BUILD_FLAGS"] = settings.Advanced.GoBuildFlags
		}
		if settings.Advanced.NodeVersion != "" {
			env["BP_NODE_VERSION"] = settings.Advanced.NodeVersion
		}
	}
	return env
}
