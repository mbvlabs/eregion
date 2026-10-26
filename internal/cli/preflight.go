package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/host"
)

// Local preflight and first-contact checks for `server init` (MASTER 6.1
// steps 1 and 3): the operator needs a usable OS, an SSH client for break-glass
// and fingerprint checks, and a reachable target. Missing tools fail with
// missing_tool and the exact install command; nothing is installed silently.

type localTool struct {
	name     string
	binaries []string
	packages map[string]string
	reason   string
}

var operatorSSHTool = localTool{
	name:     "the OpenSSH client",
	binaries: []string{"ssh", "ssh-keygen"},
	packages: map[string]string{
		"apt-get": "openssh-client",
		"dnf":     "openssh-clients",
		"pacman":  "openssh",
		"brew":    "openssh",
	},
	reason: "reach the server for break-glass access and check its host-key fingerprint",
}

// localPackageManager returns the first supported package manager on PATH.
func localPackageManager() string {
	order := []string{"apt-get", "dnf", "pacman"}
	if runtime.GOOS == "darwin" {
		order = []string{"brew"}
	}
	for _, manager := range order {
		if _, err := exec.LookPath(manager); err == nil {
			return manager
		}
	}
	return ""
}

// localInstallCommand returns the exact command that installs tool here.
func localInstallCommand(tool localTool) string {
	manager := localPackageManager()
	pkg, ok := tool.packages[manager]
	if !ok {
		return ""
	}
	var parts []string
	switch manager {
	case "apt-get":
		parts = []string{"apt-get", "install", "-y", pkg}
	case "dnf":
		parts = []string{"dnf", "install", "-y", pkg}
	case "pacman":
		parts = []string{"pacman", "-S", "--noconfirm", pkg}
	case "brew":
		return "brew install " + pkg
	default:
		return ""
	}
	if os.Geteuid() != 0 {
		parts = append([]string{"sudo"}, parts...)
	}
	return strings.Join(parts, " ")
}

// requireLocalTools checks the operator machine, not the server.
func requireLocalTools() error {
	var missing []string
	for _, binary := range operatorSSHTool.binaries {
		if _, err := exec.LookPath(binary); err != nil {
			missing = append(missing, binary)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	install := localInstallCommand(operatorSSHTool)
	hint := "Install the OpenSSH client and retry."
	if install != "" {
		hint = "Run `" + install + "`, then retry."
	}
	fault := contract.NewFault(
		contract.MissingTool,
		operatorSSHTool.name+" is needed to "+operatorSSHTool.reason+" but "+strings.Join(missing, ", ")+" is not on PATH",
		hint,
	)
	fault.Details = map[string]any{"tool": "openssh-client", "missing": missing, "install_command": install}
	return fault
}

// checkReachable dials the SSH target once, before any host-key or credential
// work. A timeout and a refused connection get different fixes.
func checkReachable(ctx context.Context, address string, timeout time.Duration) error {
	conn, err := (&net.Dialer{Timeout: timeout}).DialContext(ctx, "tcp", address)
	if err == nil {
		_ = conn.Close()
		return nil
	}
	hint := "Check the address and that SSH is open on it."
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Timeout() {
		hint = "The connection timed out. Check the provider firewall and the address."
	}
	fault := contract.NewFault(contract.RemoteUnreachable, "the server does not answer on "+address, hint)
	fault.Details = map[string]any{"address": address}
	return fault
}

// remoteProbeScript reports the remote preflight facts as key=value lines.
// It is read-only and runs as the first-contact user before any change.
const remoteProbeScript = `. /etc/os-release
printf 'id=%s\n' "$ID"
printf 'version=%s\n' "$VERSION_ID"
printf 'pretty=%s\n' "$PRETTY_NAME"
printf 'arch=%s\n' "$(uname -m)"
printf 'uid=%s\n' "$(id -u)"
printf 'mem_kib=%s\n' "$(awk '/^MemTotal:/{print $2}' /proc/meminfo 2>/dev/null)"
printf 'disk_kib=%s\n' "$(df -Pk / | awk 'NR==2{print $4}')"
printf 'tar=%s\n' "$(command -v tar 2>/dev/null || true)"
printf 'state=%s\n' "$(ls /etc/eregion/node.json /etc/eregion/control-plane.env /var/lib/eregion/bootstrap/state.json 2>/dev/null | tr '\n' ' ')"`

// Remote resource floors. The main node runs the control plane, builds,
// registry, Caddy and applications; these are refusals, not warnings.
const (
	minRemoteMemoryMiB = 1024
	minRemoteDiskMiB   = 8 * 1024
)

// remoteFacts is the observed preflight state of the target.
type remoteFacts struct {
	ID         string
	VersionID  string
	PrettyName string
	Arch       string
	UID        string
	MemMiB     int
	DiskMiB    int
	Tar        string
	// Existing lists eregion files that mean the host is already set up.
	Existing []string
}

func (f remoteFacts) platform() host.Platform {
	arch := f.Arch
	switch arch {
	case "x86_64":
		arch = "amd64"
	case "aarch64":
		arch = "arm64"
	}
	return host.Platform{ID: f.ID, VersionID: f.VersionID, PrettyName: f.PrettyName, Arch: arch}
}

// parseRemoteFacts turns the probe output into facts and validates the
// supported OS/architecture matrix, root access, resources and existing state.
// The caller decides whether existing state is acceptable (a matching resume).
func parseRemoteFacts(raw string) (remoteFacts, error) {
	facts := remoteFacts{}
	for _, line := range strings.Split(strings.TrimSpace(raw), "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		value = strings.TrimSpace(value)
		switch key {
		case "id":
			facts.ID = strings.ToLower(value)
		case "version":
			facts.VersionID = value
		case "pretty":
			facts.PrettyName = value
		case "arch":
			facts.Arch = value
		case "uid":
			facts.UID = value
		case "mem_kib":
			facts.MemMiB = kibToMiB(value)
		case "disk_kib":
			facts.DiskMiB = kibToMiB(value)
		case "tar":
			facts.Tar = value
		case "state":
			facts.Existing = strings.Fields(value)
		}
	}
	if facts.ID == "" || facts.VersionID == "" {
		return facts, contract.NewFault(contract.RemoteUnreachable, "the server did not report its operating system", "Check that /etc/os-release exists and the first-contact user can read it.")
	}
	return facts, nil
}

func kibToMiB(value string) int {
	kib, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || kib <= 0 {
		return 0
	}
	return kib / 1024
}

// checkRemotePreflight refuses an unsupported or already-used host before any
// change. allowExisting permits a host that a resumable init already touched.
func (r *serverInit) checkRemotePreflight(facts remoteFacts, allowExisting bool) error {
	platform, err := facts.preflight(allowExisting)
	if err != nil {
		return err
	}
	r.platform = platform
	return nil
}

// preflight is the pure part of the remote gate: OS, architecture, root,
// resources, the upload tool and existing state.
func (f remoteFacts) preflight(allowExisting bool) (host.Platform, error) {
	facts := f
	platform := facts.platform()
	if !platform.Supported() {
		name := facts.PrettyName
		if name == "" {
			name = platform.String()
		}
		fault := contract.NewFault(contract.PreconditionFailed, "the server runs "+name+", which eregion does not support", "Use Debian 13 or Ubuntu 26.04 on amd64 or arm64.")
		fault.Details = map[string]any{"reason": "os_unsupported", "os": platform}
		return platform, fault
	}
	if facts.UID != "0" {
		fault := contract.NewFault(contract.PreconditionFailed, "server init needs root on the server", "Use root@host; the setup installs packages and system services.")
		fault.Details = map[string]any{"reason": "not_root", "uid": facts.UID}
		return platform, fault
	}
	if facts.MemMiB > 0 && facts.MemMiB < minRemoteMemoryMiB {
		fault := contract.NewFault(contract.PreconditionFailed, fmt.Sprintf("the server has %d MiB of RAM; the main node needs at least %d MiB", facts.MemMiB, minRemoteMemoryMiB), "Resize the server and run server init again.")
		fault.Details = map[string]any{"reason": "ram_too_small", "ram_mib": facts.MemMiB}
		return platform, fault
	}
	if facts.DiskMiB > 0 && facts.DiskMiB < minRemoteDiskMiB {
		fault := contract.NewFault(contract.PreconditionFailed, fmt.Sprintf("the server has %d MiB free on /; the main node needs at least %d MiB", facts.DiskMiB, minRemoteDiskMiB), "Free space or resize the server and run server init again.")
		fault.Details = map[string]any{"reason": "disk_too_small", "disk_mib": facts.DiskMiB}
		return platform, fault
	}
	if strings.TrimSpace(facts.Tar) == "" {
		fault := contract.NewFault(contract.MissingTool, "tar is needed to upload the setup bundle but is not installed on the server", "Run `apt-get install -y tar` on the server, then retry.")
		fault.Details = map[string]any{"tool": "tar", "install_command": "apt-get install -y tar"}
		return platform, fault
	}
	if len(facts.Existing) > 0 && !allowExisting {
		fault := contract.NewFault(contract.PreconditionFailed, "the server already has eregion state ("+strings.Join(facts.Existing, ", ")+")", "Operate it with an existing context, or rebuild the server before a new server init. An interrupted init for this server resumes with --resume.")
		fault.Details = map[string]any{"reason": "existing_state", "paths": facts.Existing}
		return platform, fault
	}
	return platform, nil
}
