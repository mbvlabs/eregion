//go:build linux

package platform

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
)

// This uses real subprocesses, including one in a different session. It does
// not substitute a fake Pack or Docker executable.
func TestIntegrationPackSupervisorDrainsEscapedChild(t *testing.T) {
	if os.Getenv("EREGION_SUBREAPER_TEST") == "1" {
		result, err := supervisePack([]string{"/bin/sh", "-c", "setsid sleep 60 & echo $!; exit 7"})
		if err != nil || result.ExitCode != 7 {
			fmt.Fprintf(os.Stderr, "supervisor: %+v %v\n", result, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestIntegrationPackSupervisorDrainsEscapedChild$")
	cmd.Env = append(os.Environ(), "EREGION_SUBREAPER_TEST=1")
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill() }()
	scanner := bufio.NewScanner(pipe)
	if !scanner.Scan() {
		t.Fatal("subprocess did not report its child")
	}
	pid, err := strconv.Atoi(strings.TrimSpace(scanner.Text()))
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 20*time.Second {
		t.Fatal("supervisor exceeded its bound")
	}
	if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
		t.Fatalf("escaped child %d remains: %v", pid, err)
	}
}

func TestIntegrationPackOwnedNetworkCleanup(t *testing.T) {
	host := os.Getenv("EREGION_TEST_DOCKER_HOST")
	if host == "" {
		t.Skip("set EREGION_TEST_DOCKER_HOST to a disposable Docker daemon")
	}
	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		t.Fatal(err)
	}
	op := contract.OperationID("op_" + hex.EncodeToString(token[:]))
	const step = "platform-pack-03-v1"
	env := []string{"PATH=" + os.Getenv("PATH"), "DOCKER_HOST=" + host}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	rt := engine.Runtime{}
	name, err := createBuildNetwork(ctx, rt, env, op, step, 1)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = removeBuildNetwork(cleanup, rt, env, name, op, step, 1, "")
	})
	if err := removeBuildNetwork(ctx, rt, env, name, op, step, 2, ""); err == nil {
		t.Fatal("cleanup accepted a different attempt's network")
	}
	if _, exists, err := inspectBuildNetwork(ctx, rt, env, name, step); err != nil || !exists {
		t.Fatalf("foreign network changed: exists=%v err=%v", exists, err)
	}
	if err := removeBuildNetwork(ctx, rt, env, name, op, step, 1, ""); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := inspectBuildNetwork(ctx, rt, env, name, step); err != nil || exists {
		t.Fatalf("owned network remains: exists=%v err=%v", exists, err)
	}
}

func TestIntegrationPackSupervisorCancellationDrainsChildren(t *testing.T) {
	if os.Getenv("EREGION_SUBREAPER_CANCEL_TEST") == "1" {
		result, err := supervisePack([]string{"/bin/sh", "-c", "trap '' TERM; setsid sleep 60 & echo $!; wait"})
		if err != nil || result.ExitCode == 0 {
			fmt.Fprintf(os.Stderr, "supervisor: %+v %v\n", result, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestIntegrationPackSupervisorCancellationDrainsChildren$")
	cmd.Env = append(os.Environ(), "EREGION_SUBREAPER_CANCEL_TEST=1")
	pipe, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill() }()
	scanner := bufio.NewScanner(pipe)
	if !scanner.Scan() {
		t.Fatal("subprocess did not report its child")
	}
	pid, err := strconv.Atoi(strings.TrimSpace(scanner.Text()))
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) > 20*time.Second {
		t.Fatal("supervisor exceeded its cancellation bound")
	}
	if err := syscall.Kill(pid, 0); err != syscall.ESRCH {
		t.Fatalf("escaped child %d remains after cancellation: %v", pid, err)
	}
}
