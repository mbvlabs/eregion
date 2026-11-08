package host

import (
	"fmt"
	"os"
	"runtime"

	"github.com/mbvlabs/eregion/internal/doctor"
)

// nodeExporterDirInfo is one no-follow observation of an owned directory.
// Ownership is reported separately so callers can refuse an unsafe repair
// instead of chowning/chmodding a path that may have been attacker-controlled.
type nodeExporterDirInfo struct {
	Path       string
	Exists     bool
	Symlink    bool
	Directory  bool
	OwnerKnown bool
	UID        uint32
	GID        uint32
	Mode       uint32
	Error      string
}

func inspectNodeExporterDir(path string) nodeExporterDirInfo {
	info, err := os.Lstat(path)
	if err != nil {
		out := nodeExporterDirInfo{Path: path}
		if !os.IsNotExist(err) {
			out.Error = safeError(err)
		}
		return out
	}
	out := nodeExporterDirInfo{
		Path:      path,
		Exists:    true,
		Symlink:   info.Mode()&os.ModeSymlink != 0,
		Directory: info.IsDir(),
		Mode:      fileModeBits(info.Mode()),
	}
	if uid, gid, ok := fileUIDGID(info); ok {
		out.OwnerKnown, out.UID, out.GID = true, uid, gid
	}
	return out
}

// fileModeBits preserves setuid, setgid and sticky bits alongside the ordinary
// permission bits. FileMode.Perm() drops them, which would let 2755/1755 pass a
// naive exact-0755 check.
func fileModeBits(mode os.FileMode) uint32 {
	bits := uint32(mode.Perm())
	if mode&os.ModeSetuid != 0 {
		bits |= 0o4000
	}
	if mode&os.ModeSetgid != 0 {
		bits |= 0o2000
	}
	if mode&os.ModeSticky != 0 {
		bits |= 0o1000
	}
	return bits
}

func (info nodeExporterDirInfo) doctorFacts() doctor.TelemetryPathFacts {
	return doctor.TelemetryPathFacts{
		Path: info.Path, Exists: info.Exists, Symlink: info.Symlink,
		Directory: info.Directory, OwnerKnown: info.OwnerKnown,
		UID: info.UID, GID: info.GID, Mode: info.Mode, Error: info.Error,
	}
}

// validateForApply allows a missing path (it will be created by the trusted
// parent) and a safe path with the wrong mode (it will be set to exact 0755,
// with setuid/setgid/sticky bits explicitly cleared). It refuses a symlink,
// non-directory,
// unverifiable/non-root ownership, or group/other writability without
// attempting a repair.
func (info nodeExporterDirInfo) validateForApply(name string) error {
	if info.Error != "" {
		return fmt.Errorf("node_exporter %s path %s could not be read: %s", name, info.Path, info.Error)
	}
	if !info.Exists {
		return nil
	}
	if info.Symlink {
		return fmt.Errorf("node_exporter %s path %s is a symlink; refusing to use or modify it", name, info.Path)
	}
	if !info.Directory {
		return fmt.Errorf("node_exporter %s path %s is not a directory", name, info.Path)
	}
	if !info.OwnerKnown {
		return fmt.Errorf("node_exporter %s path %s ownership cannot be verified", name, info.Path)
	}
	if info.UID != 0 || info.GID != 0 {
		return fmt.Errorf("node_exporter %s path %s is owned %d:%d; refusing to change unsafe ownership", name, info.Path, info.UID, info.GID)
	}
	if info.Mode&0o022 != 0 {
		return fmt.Errorf("node_exporter %s path %s is group/other writable (mode %04o); refusing unsafe repair", name, info.Path, info.Mode)
	}
	return nil
}

func validateTrustedAncestor(path string) error {
	info := inspectNodeExporterDir(path)
	if info.Error != "" {
		return fmt.Errorf("trusted ancestor %s could not be read: %s", path, info.Error)
	}
	if !info.Exists {
		return fmt.Errorf("trusted ancestor %s is missing", path)
	}
	if info.Symlink {
		return fmt.Errorf("trusted ancestor %s is a symlink; refusing to follow it", path)
	}
	if !info.Directory {
		return fmt.Errorf("trusted ancestor %s is not a directory", path)
	}
	if !info.OwnerKnown || info.UID != 0 || info.GID != 0 {
		return fmt.Errorf("trusted ancestor %s is not root-owned", path)
	}
	if info.Mode&0o022 != 0 {
		return fmt.Errorf("trusted ancestor %s is group/other writable (mode %04o); refusing", path, info.Mode)
	}
	return nil
}

// validateNodeExporterApplyDirectories runs before any apply mutation. The
// ancestors and an existing parent must be root-owned and not group/other
// writable, so an unprivileged process cannot swap the child path between this
// check and the install/chmod. If the parent is unsafe, apply refuses rather
// than repairing attacker-controlled ownership or permissions.
func validateNodeExporterApplyDirectories(parent, textfile string) error {
	// /var is a normal symlink on some operator test platforms (for example
	// macOS /var -> /private/var), while the production node is Linux. The
	// ancestor hardening applies to the Linux host that actually applies the
	// step; the owned parent/child checks below are platform-independent.
	if runtime.GOOS == "linux" {
		for _, path := range []string{"/var", "/var/lib"} {
			if err := validateTrustedAncestor(path); err != nil {
				return err
			}
		}
	}
	for _, item := range []struct{ name, path string }{{"parent", parent}, {"textfile", textfile}} {
		if err := inspectNodeExporterDir(item.path).validateForApply(item.name); err != nil {
			return err
		}
	}
	return nil
}

func nodeExporterStrictDirectoryState(parent, textfile string) error {
	for _, path := range []string{parent, textfile} {
		if err := doctor.ValidateTelemetryDirectory(inspectNodeExporterDir(path).doctorFacts()); err != nil {
			return err
		}
	}
	return nil
}
