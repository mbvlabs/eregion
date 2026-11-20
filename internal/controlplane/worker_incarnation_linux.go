//go:build linux

package controlplane

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// The boot ID, PID namespace, PID and kernel start ticks identify a process
// incarnation without a lease. Unknown visibility or malformed data is never
// evidence of death. All numeric components use base 36 to fit River's ID.
func localWorkerContext() (string, uint64, bool) {
	raw, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return "", 0, false
	}
	boot, err := hex.DecodeString(strings.ReplaceAll(strings.TrimSpace(string(raw)), "-", ""))
	if err != nil || len(boot) != 16 {
		return "", 0, false
	}
	info, err := os.Stat("/proc/self/ns/pid")
	if err != nil {
		return "", 0, false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Ino == 0 {
		return "", 0, false
	}
	// NSpid lists IDs from the procfs mount's namespace down to our own.
	// Exactly one entry proves procfs and kill(2) use the same PID namespace;
	// comparing numeric PIDs alone could match by coincidence.
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return "", 0, false
	}
	matched := false
	for _, line := range strings.Split(string(status), "\n") {
		if strings.HasPrefix(line, "NSpid:") {
			fields := strings.Fields(strings.TrimPrefix(line, "NSpid:"))
			matched = len(fields) == 1 && fields[0] == strconv.Itoa(os.Getpid())
			break
		}
	}
	if !matched {
		return "", 0, false
	}
	return base64.RawURLEncoding.EncodeToString(boot), stat.Ino, true
}

func processStartTicks(pid int) (uint64, error) {
	raw, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, err
	}
	// comm is parenthesized and can contain spaces and parentheses.
	end := strings.LastIndexByte(string(raw), ')')
	if end < 0 {
		return 0, fmt.Errorf("invalid process stat")
	}
	fields := strings.Fields(string(raw[end+1:]))
	if len(fields) <= 19 {
		return 0, fmt.Errorf("short process stat")
	}
	return strconv.ParseUint(fields[19], 10, 64)
}

func currentWorkerIncarnation() (string, bool) {
	boot, namespace, ok := localWorkerContext()
	if !ok {
		return "", false
	}
	start, err := processStartTicks(os.Getpid())
	if err != nil || start == 0 {
		return "", false
	}
	return "v1." + boot + "." + strconv.FormatUint(namespace, 36) + "." +
		strconv.FormatInt(int64(os.Getpid()), 36) + "." + strconv.FormatUint(start, 36), true
}

func workerIncarnationDead(id string) bool {
	parts := strings.Split(id, ".")
	if len(parts) != 5 || parts[0] != "v1" {
		return false
	}
	startAndNonce := strings.Split(parts[4], "-")
	if len(startAndNonce) != 2 || len(startAndNonce[1]) == 0 {
		return false
	}
	boot, namespace, ok := localWorkerContext()
	if !ok || parts[1] != boot || parts[2] != strconv.FormatUint(namespace, 36) {
		return false
	}
	pid64, err := strconv.ParseInt(parts[3], 36, 32)
	if err != nil || pid64 <= 0 || parts[3] != strconv.FormatInt(pid64, 36) {
		return false
	}
	start, err := strconv.ParseUint(startAndNonce[0], 36, 64)
	if err != nil || start == 0 || startAndNonce[0] != strconv.FormatUint(start, 36) {
		return false
	}
	pid := int(pid64)
	current, err := processStartTicks(pid)
	if err == nil {
		return current != 0 && current != start
	}
	// hidepid and permissions can make procfs report ENOENT for a live
	// process. Only ESRCH from the kernel's PID lookup establishes absence.
	return syscall.Kill(pid, 0) == syscall.ESRCH
}
