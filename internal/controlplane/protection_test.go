package controlplane

import (
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
)

func TestRequireHumanFactorFailsClosedWithoutAVerifier(t *testing.T) {
	service := &Service{}
	if err := service.requireHumanFactorTx("123456"); err == nil {
		t.Fatal("a service without a verifier accepted a factor-gated action")
	} else if contract.Classify(err).Code != contract.PreconditionFailed {
		t.Fatalf("error = %v, want precondition_failed", err)
	}
}

func TestRequireHumanFactorUsesTheInjectedVerifier(t *testing.T) {
	var got string
	service := &Service{humanFactor: func(code string) error {
		got = code
		return nil
	}}
	if err := service.requireHumanFactorTx("654321"); err != nil {
		t.Fatal(err)
	}
	if got != "654321" {
		t.Fatalf("verifier saw %q", got)
	}
}
