package platform

import (
	"errors"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
)

func TestValidateConfirmation(t *testing.T) {
	target, err := NewEnvironmentTarget("api", "production")
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateConfirmation(KindRollback, target.String(), "api/production"); err != nil {
		t.Fatalf("matching confirmation rejected: %v", err)
	}
	for _, confirm := range []string{"", "api/staging", "production", "api/production/extra"} {
		err := ValidateConfirmation(KindRollback, target.String(), confirm)
		var fault *contract.Fault
		if !errors.As(err, &fault) {
			t.Fatalf("confirmation %q: want a fault, got %v", confirm, err)
		}
		switch {
		case confirm == "" && fault.Code != contract.ConfirmationRequired:
			t.Fatalf("empty confirmation: %s", fault.Code)
		case confirm != "" && fault.Code != contract.PreconditionFailed:
			t.Fatalf("mismatched confirmation: %s", fault.Code)
		}
	}
	if err := ValidateConfirmation(KindDeploy, target.String(), "wrong"); err != nil {
		t.Fatalf("deploy does not take a target confirmation: %v", err)
	}
	if err := ValidateConfirmation(KindRollback, "build/bld_0123456789abcdef0123456789abcdef", "api/production"); err == nil {
		t.Fatal("a non-environment target accepted a target confirmation")
	}

	app, err := NewAppTarget("api")
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateConfirmation(KindAppDelete, app.String(), "api"); err != nil {
		t.Fatalf("app delete confirmation rejected: %v", err)
	}
	if err := ValidateConfirmation(KindEnvironmentDelete, target.String(), "api/production"); err != nil {
		t.Fatalf("env delete confirmation rejected: %v", err)
	}
	if err := ValidateConfirmation(KindEnvironmentDelete, target.String(), "api"); err == nil {
		t.Fatal("app token accepted for env delete")
	}
}
