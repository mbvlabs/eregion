package host

import (
	"context"
	"errors"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
)

// managedRecoveryTimeout bounds a rollback after the initiating request has
// been cancelled or has exceeded its deadline. Recovery uses a context derived
// from the original one with its values but without its cancellation, so the
// host can restore the previous config and service state instead of leaving a
// mixed migration. Normal forward work always uses the caller's context.
const managedRecoveryTimeout = 20 * time.Second

func managedRecoveryContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), managedRecoveryTimeout)
}

// joinRecoveryError keeps the initiating error's classification (especially
// cancelled or deadline exceeded) and errors.Is chain while honestly returning
// a failed rollback. Recovery may itself return a contract fault; putting the
// classified initiating error first prevents that fault from reclassifying the
// original request.
func joinRecoveryError(cause, recovery error) error {
	if recovery == nil {
		return cause
	}
	return errors.Join(cause, contract.Classify(cause), recovery)
}

// contextInterruption returns the caller's cancellation sentinel before a
// command failure is converted to a static contract fault. It never returns
// command stderr or other arbitrary error text.
func contextInterruption(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return nil
}

// interruptedError preserves a caller cancellation/deadline classification
// without inspecting error strings.
func interruptedError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if cause := contextInterruption(ctx, err); cause != nil {
		return cause
	}
	return err
}
