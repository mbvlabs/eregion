package release

import (
	"context"
	"errors"

	"github.com/mbvlabs/eregion/internal/contract"
)

// Verifier checks a sigstore bundle over raw bytes for an expected identity.
type Verifier interface {
	VerifyBlob(ctx context.Context, content, bundle []byte, identity string) error
}

func verificationFailed(message string) error {
	return contract.NewFault(
		contract.PreconditionFailed,
		message,
		"Do not retry. Report this to a human.",
	)
}

func verificationWrap(err error) error {
	if err == nil {
		return nil
	}
	var fault *contract.Fault
	if errors.As(err, &fault) {
		return err
	}
	return &contract.Fault{
		Code:    contract.PreconditionFailed,
		Message: "release signature verification failed",
		Hint:    "Do not retry. Report this to a human.",
		Cause:   err,
	}
}

func unreachable(url string, err error) error {
	return &contract.Fault{
		Code:    contract.Unavailable,
		Message: "release URL is unreachable",
		Details: map[string]any{"url": url},
		Cause:   err,
	}
}
