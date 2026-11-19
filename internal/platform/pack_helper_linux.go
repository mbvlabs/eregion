//go:build linux

package platform

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// PackHelperInvocation is an internal entry point, not a general command runner.
const PackHelperInvocation = "__internal-pack-helper"

type packHelperResult struct {
	ExitCode int `json:"exit_code"`
}

func packHelperSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
}

// RunPackHelper runs only pack build, in a separate subreaper process. An
// acknowledgement on fd 3 is written only after every descendant has exited.
func RunPackHelper() int {
	var argv []string
	if err := json.NewDecoder(io.LimitReader(os.Stdin, 1<<20)).Decode(&argv); err != nil ||
		len(argv) < 2 || argv[0] != "pack" || argv[1] != "build" || validatePackArgv(argv) != nil {
		return 1
	}
	result, err := supervisePack(argv)
	if err != nil {
		return 1
	}
	f := os.NewFile(3, "pack-result")
	if f == nil {
		return 1
	}
	defer func() { _ = f.Close() }()
	if json.NewEncoder(f).Encode(result) != nil {
		return 1
	}
	return 0
}

//nolint:gocognit // Keep the wait, cancellation and deadline state in one loop.
func supervisePack(argv []string) (packHelperResult, error) {
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		return packHelperResult{}, err
	}
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(signals)
	// wait4, not CommandContext, owns reaping and bounded descendant shutdown.
	cmd := exec.Command(argv[0], argv[1:]...) //nolint:gosec,noctx // Only validated pack builds enter this helper.
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return packHelperResult{}, err
	}
	defer func() { _ = cmd.Process.Release() }()
	var result packHelperResult
	packExited := false
	var deadline, hardDeadline time.Time
	for {
		var status syscall.WaitStatus
		pid, err := syscall.Wait4(-1, &status, syscall.WNOHANG, nil)
		if pid > 0 && pid != cmd.Process.Pid {
			continue
		}
		if pid > 0 {
			packExited = true
			result.ExitCode = status.ExitStatus()
			if result.ExitCode < 0 {
				result.ExitCode = 128 + int(status.Signal())
			}
			if deadline.IsZero() {
				deadline = time.Now().Add(packBuildKillGrace)
			}
			continue
		}
		if errors.Is(err, syscall.ECHILD) {
			if !packExited {
				return packHelperResult{}, errors.New("pack exit was not observed")
			}
			return result, nil
		}
		if err != nil && !errors.Is(err, syscall.EINTR) {
			return packHelperResult{}, err
		}
		now := time.Now()
		if !hardDeadline.IsZero() && now.After(hardDeadline) {
			return packHelperResult{}, errors.New("pack descendants did not drain")
		}
		if !deadline.IsZero() && now.After(deadline) {
			if hardDeadline.IsZero() {
				hardDeadline = now.Add(10 * time.Second)
			}
			// Descendants may have changed process group or session. Repeatedly
			// kill direct children as they are adopted, until wait4 proves ECHILD.
			_ = killChildren()
		}
		select {
		case <-signals:
			if !packExited {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM)
			}
			if deadline.IsZero() {
				deadline = time.Now().Add(packBuildKillGrace)
			}
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func killChildren() error {
	pid := os.Getpid()
	paths, err := filepath.Glob(fmt.Sprintf("/proc/%d/task/*/children", pid))
	if err != nil || len(paths) == 0 {
		return errors.New("cannot enumerate process children")
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			continue // A thread can exit during enumeration; retry on the next pass.
		}
		for _, field := range strings.Fields(string(data)) {
			child, err := strconv.Atoi(field)
			if err != nil || child <= 0 {
				continue
			}
			killChild(pid, child)
		}
	}
	return nil
}

// A pidfd prevents PID reuse from directing a signal at an unrelated process.
// Only signal children still parented here; their children are adopted later.
func killChild(parent, child int) {
	fd, err := unix.PidfdOpen(child, 0)
	if err != nil {
		return
	}
	defer func() { _ = unix.Close(fd) }()
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", child))
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(status), "\n") {
		if strings.TrimSpace(line) == fmt.Sprintf("PPid:\t%d", parent) {
			_ = unix.PidfdSendSignal(fd, syscall.SIGKILL, nil, 0)
			return
		}
	}
}
