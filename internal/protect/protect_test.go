package protect

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/totp"
)

func TestInitConfirmAndVerifyUseOneTimeCodes(t *testing.T) {
	now := time.Unix(1800000000, 0).UTC()
	store := &Store{Path: filepath.Join(t.TempDir(), "protect.json"), Now: func() time.Time { return now }}
	setup, err := store.Init(false, "main", "")
	if err != nil {
		t.Fatal(err)
	}
	key, err := totp.DecodeSecret(setup.Secret)
	if err != nil {
		t.Fatal(err)
	}
	status, err := store.Confirm(totp.Code(key, now))
	if err != nil || !status.Configured {
		t.Fatalf("confirm = %+v, %v", status, err)
	}
	now = now.Add(totp.Period)
	if err := store.Verify(totp.Code(key, now)); err != nil {
		t.Fatalf("verify = %v", err)
	}
	if err := store.Verify(totp.Code(key, now)); err == nil {
		t.Fatal("a reused code was accepted")
	}
	if err := store.Verify("000000"); err == nil {
		t.Fatal("a wrong code was accepted")
	}
}

const (
	helperModeEnv = "EREGION_PROTECT_HELPER_MODE"
	helperPathEnv = "EREGION_PROTECT_HELPER_PATH"
	helperNowEnv  = "EREGION_PROTECT_HELPER_NOW"
	helperCodeEnv = "EREGION_PROTECT_HELPER_CODE"
)

// TestProtectStoreHelperProcess is run as an independent test binary by the
// multiprocess tests below. Without the helper environment it is a no-op.
func TestProtectStoreHelperProcess(t *testing.T) {
	mode := os.Getenv(helperModeEnv)
	if mode == "" {
		return
	}
	nowUnix, err := strconv.ParseInt(os.Getenv(helperNowEnv), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	store := &Store{
		Path: os.Getenv(helperPathEnv),
		Now:  func() time.Time { return time.Unix(nowUnix, 0).UTC() },
	}
	code := os.Getenv(helperCodeEnv)
	switch mode {
	case "verify":
		if err := store.Verify(code); err != nil {
			t.Fatal(err)
		}
	case "confirm":
		if _, err := store.Confirm(code); err != nil {
			t.Fatal(err)
		}
	case "rotate":
		if _, err := store.Init(true, "main", code); err != nil {
			t.Fatal(err)
		}
	default:
		t.Fatalf("unknown helper mode %q", mode)
	}
}

func runProtectHelpers(t *testing.T, count int, mode, path string, now time.Time, code string) []error {
	t.Helper()
	var wait sync.WaitGroup
	errs := make([]error, count)
	for index := range count {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			command := exec.Command(os.Args[0], "-test.run=^TestProtectStoreHelperProcess$", "-test.count=1")
			command.Env = append(os.Environ(),
				helperModeEnv+"="+mode,
				helperPathEnv+"="+path,
				helperNowEnv+"="+strconv.FormatInt(now.Unix(), 10),
				helperCodeEnv+"="+code,
			)
			output, err := command.CombinedOutput()
			if err != nil {
				errs[index] = &helperError{err: err, output: string(output)}
			}
		}(index)
	}
	wait.Wait()
	return errs
}

type helperError struct {
	err    error
	output string
}

func (e *helperError) Error() string {
	return e.err.Error() + ": " + e.output
}

func confirmedStore(t *testing.T) (*Store, []byte, time.Time) {
	t.Helper()
	now := time.Unix(1800000000, 0).UTC()
	store := &Store{Path: filepath.Join(t.TempDir(), "protect.json"), Now: func() time.Time { return now }}
	setup, err := store.Init(false, "main", "")
	if err != nil {
		t.Fatal(err)
	}
	key, err := totp.DecodeSecret(setup.Secret)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Confirm(totp.Code(key, now)); err != nil {
		t.Fatal(err)
	}
	return store, key, now
}

func countHelperSuccesses(errs []error) int {
	successes := 0
	for _, err := range errs {
		if err == nil {
			successes++
		}
	}
	return successes
}

// TestMultiprocessVerifyAcceptsOneReplay proves independent node RPC
// processes cannot each consume the same TOTP step: the process lock serializes
// the read-modify-write, so exactly one verifier succeeds and every other
// process observes the reused step.
func TestMultiprocessVerifyAcceptsOneReplay(t *testing.T) {
	store, key, now := confirmedStore(t)
	next := now.Add(totp.Period)
	errs := runProtectHelpers(t, 8, "verify", store.Path, next, totp.Code(key, next))
	if successes := countHelperSuccesses(errs); successes != 1 {
		t.Fatalf("successful verifications = %d, want exactly 1; errors=%v", successes, errs)
	}
}

// TestMultiprocessConfirmAcceptsOneConfirmation proves confirmation is also a
// serialized mutation: two processes cannot both confirm the same pending
// secret and claim success.
func TestMultiprocessConfirmAcceptsOneConfirmation(t *testing.T) {
	now := time.Unix(1800000000, 0).UTC()
	store := &Store{Path: filepath.Join(t.TempDir(), "protect.json"), Now: func() time.Time { return now }}
	setup, err := store.Init(false, "main", "")
	if err != nil {
		t.Fatal(err)
	}
	key, err := totp.DecodeSecret(setup.Secret)
	if err != nil {
		t.Fatal(err)
	}
	errs := runProtectHelpers(t, 2, "confirm", store.Path, now, totp.Code(key, now))
	if successes := countHelperSuccesses(errs); successes != 1 {
		t.Fatalf("successful confirmations = %d, want exactly 1; errors=%v", successes, errs)
	}
	status, err := store.Status()
	if err != nil || !status.Configured || status.Pending {
		t.Fatalf("status after confirmation = %+v, %v", status, err)
	}
}

// TestMultiprocessRotateAcceptsOneRotation proves rotate's verify-and-replace
// sequence is serialized, so the same code cannot rotate twice.
func TestMultiprocessRotateAcceptsOneRotation(t *testing.T) {
	store, key, now := confirmedStore(t)
	rotateAt := now.Add(totp.Period)
	errs := runProtectHelpers(t, 2, "rotate", store.Path, rotateAt, totp.Code(key, rotateAt))
	if successes := countHelperSuccesses(errs); successes != 1 {
		t.Fatalf("successful rotations = %d, want exactly 1; errors=%v", successes, errs)
	}
	status, err := store.Status()
	if err != nil || !status.Configured || !status.Pending {
		t.Fatalf("status after rotation = %+v, %v", status, err)
	}
}

// TestMultiprocessFailuresReachLockout proves failure counters are not lost
// between processes: five wrong codes from independent node RPC processes
// lock the gate. A read-modify-write race can lose increments and leave the
// gate unlocked.
func TestMultiprocessFailuresReachLockout(t *testing.T) {
	store, key, now := confirmedStore(t)
	wrong := "000000"
	for _, offset := range []time.Duration{-totp.Period, 0, totp.Period} {
		if wrong == totp.Code(key, now.Add(offset)) {
			wrong = "000001"
		}
	}
	errs := runProtectHelpers(t, maxFailures+1, "verify", store.Path, now, wrong)
	if successes := countHelperSuccesses(errs); successes != 0 {
		t.Fatalf("wrong code unexpectedly succeeded: %v", errs)
	}
	status, err := store.Status()
	if err != nil {
		t.Fatal(err)
	}
	if status.LockedUntil == "" {
		t.Fatalf("five concurrent wrong codes did not lock the gate: %+v", status)
	}
}
