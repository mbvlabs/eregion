package host

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
)

type failingCommandRunner struct {
	errs  []error
	calls int
}

func (r *failingCommandRunner) Run(context.Context, string, ...string) error {
	r.calls++
	if r.calls <= len(r.errs) {
		return r.errs[r.calls-1]
	}
	return nil
}

func TestTelemetryCommandWrappersPreserveCallerInterruption(t *testing.T) {
	const canary = "runner stderr canary SECRET"
	cases := []struct {
		name       string
		call       func(context.Context, hostCommandRunner) error
		normalCode contract.Code
	}{
		{name: "validate", call: validateTelemetryCaddyWithRunner, normalCode: contract.PreconditionFailed},
		{name: "restart", call: restartTelemetryServiceWithRunner, normalCode: contract.Unavailable},
		{name: "stop", call: stopTelemetryServiceWithRunner, normalCode: contract.Unavailable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Run("normal failure is static and redacted", func(t *testing.T) {
				runner := &failingCommandRunner{errs: []error{errors.New(canary)}}
				err := tc.call(context.Background(), runner)
				if err == nil || contract.Classify(err).Code != tc.normalCode {
					t.Fatalf("error = %v, want %s", err, tc.normalCode)
				}
				if strings.Contains(err.Error(), canary) {
					t.Fatalf("static wrapper error leaked runner stderr: %v", err)
				}
				if contract.Classify(err).Cause != nil {
					t.Fatalf("static wrapper retained the runner error as a cause: %v", err)
				}
			})

			t.Run("canceled caller", func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				runner := &failingCommandRunner{errs: []error{errors.New(canary)}}
				err := tc.call(ctx, runner)
				if !errors.Is(err, context.Canceled) || contract.Classify(err).Code != contract.Cancelled {
					t.Fatalf("error = %v class=%s, want context.Canceled", err, contract.Classify(err).Code)
				}
				if strings.Contains(err.Error(), canary) {
					t.Fatalf("canceled wrapper error leaked runner stderr: %v", err)
				}
			})

			t.Run("deadline caller", func(t *testing.T) {
				ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
				defer cancel()
				runner := &failingCommandRunner{errs: []error{errors.New(canary)}}
				err := tc.call(ctx, runner)
				if !errors.Is(err, context.DeadlineExceeded) || contract.Classify(err).Code != contract.Unavailable {
					t.Fatalf("error = %v class=%s, want context.DeadlineExceeded", err, contract.Classify(err).Code)
				}
				if strings.Contains(err.Error(), canary) {
					t.Fatalf("deadline wrapper error leaked runner stderr: %v", err)
				}
			})

			t.Run("runner canceled error", func(t *testing.T) {
				runner := &failingCommandRunner{errs: []error{context.Canceled}}
				err := tc.call(context.Background(), runner)
				if !errors.Is(err, context.Canceled) || contract.Classify(err).Code != contract.Cancelled {
					t.Fatalf("error = %v class=%s, want context.Canceled", err, contract.Classify(err).Code)
				}
			})

			t.Run("runner deadline error", func(t *testing.T) {
				runner := &failingCommandRunner{errs: []error{context.DeadlineExceeded}}
				err := tc.call(context.Background(), runner)
				if !errors.Is(err, context.DeadlineExceeded) || contract.Classify(err).Code != contract.Unavailable {
					t.Fatalf("error = %v class=%s, want context.DeadlineExceeded", err, contract.Classify(err).Code)
				}
			})
		})
	}
}
