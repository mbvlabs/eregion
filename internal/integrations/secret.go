package integrations

import (
	"io"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/mbvlabs/eregion/internal/contract"
	"golang.org/x/sys/unix"
)

const credentialServiceGroup = "eregion"

type credentialFilePolicy struct {
	CallerUID       uint32
	CallerGID       uint32
	ServiceGID      uint32
	ServiceGIDKnown bool
}

func credentialPolicyForProcess() credentialFilePolicy {
	policy := credentialFilePolicy{CallerUID: uint32(os.Geteuid()), CallerGID: uint32(os.Getegid())}
	if policy.CallerUID != 0 {
		// The service worker runs as the service user; its primary group is the
		// strict group allowed to read the root-owned 0640 credential file.
		policy.ServiceGID = policy.CallerGID
		policy.ServiceGIDKnown = true
		return policy
	}
	group, err := user.LookupGroup(credentialServiceGroup)
	if err != nil {
		return policy
	}
	gid, err := strconv.ParseUint(group.Gid, 10, 32)
	if err != nil {
		return policy
	}
	policy.ServiceGID = uint32(gid)
	policy.ServiceGIDKnown = true
	return policy
}

func credentialFileAllowed(mode os.FileMode, fileUID, fileGID uint32, policy credentialFilePolicy) bool {
	if mode&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		return false
	}
	switch mode.Perm() {
	case 0o600:
		return fileUID == policy.CallerUID
	case 0o640:
		if fileUID != 0 {
			return false
		}
		if policy.CallerUID == 0 {
			return policy.ServiceGIDKnown && policy.ServiceGID != 0 && fileGID == policy.ServiceGID
		}
		return policy.CallerGID != 0 && fileGID == policy.CallerGID
	default:
		return false
	}
}

func credentialDirectoryAllowed(mode uint32, uid uint32, policy credentialFilePolicy) bool {
	if mode&unix.S_IFMT != unix.S_IFDIR {
		return false
	}
	// Non-sticky group/world-writable directories let another member replace
	// approved child directory entries. Only a sticky shared directory (for
	// example /tmp) may be writable above the credential file.
	if mode&0o022 != 0 && mode&unix.S_ISVTX == 0 {
		return false
	}
	return uid == 0 || uid == policy.CallerUID
}

func validateCredentialFileInfo(info os.FileInfo, max int64, policy credentialFilePolicy) error {
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return contract.NewFault(contract.PreconditionFailed, "credential file is not a safe regular file", "Use a regular file, never a symlink or FIFO; the token file is not followed.")
	}
	if info.Size() > max {
		return contract.NewFault(contract.PreconditionFailed, "credential file exceeds the size bound", "Use a token file no larger than the documented bound.")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return contract.NewFault(contract.Unavailable, "credential file ownership could not be verified", "Use a local Unix credential file; ownership and mode are checked before reading.")
	}
	if !credentialFileAllowed(info.Mode(), stat.Uid, stat.Gid, policy) {
		return contract.NewFault(contract.PreconditionFailed, "credential file ownership or mode is not approved", "Use a regular owner-only 0600 file, or a root-owned 0640 file in the eregion service group. Group and other write/read permissions are refused, as are symlinks and special mode bits.")
	}
	return nil
}

func validateCredentialDirectory(fd int, policy credentialFilePolicy, immediate bool) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return contract.NewFault(contract.Unavailable, "credential directory could not be verified", "Retry after checking the credential path.")
	}
	mode := uint32(stat.Mode)
	allowed := credentialDirectoryAllowed(mode, stat.Uid, policy)
	if immediate {
		// The directory directly containing the token must not be writable by
		// group or others at all; a sticky shared directory is only acceptable
		// higher in the path.
		allowed = allowed && mode&0o022 == 0
	}
	if !allowed {
		return contract.NewFault(contract.PreconditionFailed, "credential directory has an unsafe owner or mode", "Every credential-path directory must be owned by root or the reader and not writable by group or others; use a root-owned 0750 directory for the service file.")
	}
	return nil
}

// ReadSecretFile reads one approved credential file. The same file works for
// the root node RPC and the eregion worker: root-owned 0640 with the service
// group is readable by root as owner and by the worker through its primary
// group. An owner-only 0600 file is also accepted for the running user (and for
// root), but not for a different non-root user. The action input carries only
// the path; the value never enters operation rows, events, River arguments, or
// logs.
func ReadSecretFile(path string, max int64) ([]byte, error) {
	return readSecretFile(path, max, credentialPolicyForProcess())
}

func readSecretFile(path string, max int64, policy credentialFilePolicy) ([]byte, error) {
	if err := ValidateSecretRef(path); err != nil {
		return nil, err
	}
	clean := filepath.Clean(path)
	components := strings.Split(strings.TrimPrefix(clean, "/"), "/")
	if len(components) == 0 || components[0] == "" {
		return nil, contract.NewFault(contract.ValidationFailed, "credential path is not a file", "Reference a regular credential file, not a directory.")
	}

	dirFD, err := unix.Open("/", unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, contract.NewFault(contract.Unavailable, "credential path root could not be opened", "Retry after checking the credential path.")
	}
	if err := validateCredentialDirectory(dirFD, policy, true); err != nil {
		unix.Close(dirFD)
		return nil, err
	}
	for index, component := range components[:len(components)-1] {
		next, openErr := unix.Openat(dirFD, component, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		unix.Close(dirFD)
		if openErr != nil {
			return nil, contract.NewFault(contract.PreconditionFailed, "credential path contains an unsafe directory or symlink", "Use a real root-owned directory hierarchy without symlinks or group/world-writable ancestors.")
		}
		if err := validateCredentialDirectory(next, policy, index == len(components)-2); err != nil {
			unix.Close(next)
			return nil, err
		}
		dirFD = next
	}

	fd, openErr := unix.Openat(dirFD, components[len(components)-1], unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	unix.Close(dirFD)
	if openErr != nil {
		return nil, contract.NewFault(contract.PreconditionFailed, "credential file could not be opened safely", "Use a regular non-symlink credential file; FIFOs, devices and symlinks are refused without blocking.")
	}
	file := os.NewFile(uintptr(fd), "credential")
	if file == nil {
		unix.Close(fd)
		return nil, contract.NewFault(contract.Unavailable, "credential file could not be opened safely", "Retry after checking the credential path.")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, contract.NewFault(contract.Unavailable, "credential file could not be verified after opening", "Retry after checking the credential file.")
	}
	if err := validateCredentialFileInfo(info, max, policy); err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(io.LimitReader(file, max+1))
	if err != nil || int64(len(raw)) > max {
		return nil, contract.NewFault(contract.PreconditionFailed, "credential file could not be read safely", "Check the file size and permissions.")
	}
	return raw, nil
}

func ValidateSecretRef(path string) error {
	if path == "" || !filepath.IsAbs(path) || strings.ContainsAny(path, "\r\n\x00") || strings.Contains(path, "..") {
		return contract.NewFault(contract.ValidationFailed, "a safe absolute credential-file reference is required", "Reference a regular root-owned 0640 service-group file (or an owner-only 0600 file); never put credential contents in action input.")
	}
	return nil
}
