package cli

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/protect"
	"github.com/mbvlabs/eregion/internal/rpc"
	"github.com/mbvlabs/eregion/internal/totp"
)

// configuredProtectStore seeds a temp-path store and returns the first code
// Verify accepts after the confirmation code.
func configuredProtectStore(t *testing.T) (*protect.Store, string) {
	t.Helper()
	now := time.Unix(1800000000, 0).UTC()
	store := &protect.Store{Path: filepath.Join(t.TempDir(), "protect.json"), Now: func() time.Time { return now }}
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
	// Confirm consumed its step, so verification needs the next code.
	now = now.Add(totp.Period)
	return store, totp.Code(key, now)
}

func factorCode(code string) map[string]string {
	return map[string]string{protect.CodeSecret: code}
}

func TestVaultExportVerifiesTheFactorInsideItsRequest(t *testing.T) {
	store, code := configuredProtectStore(t)
	for name, request := range map[string]rpc.Request{
		"missing": {Method: methodSecretExport},
		"wrong":   {Method: methodSecretExport, Secrets: factorCode("000000")},
	} {
		t.Run(name, func(t *testing.T) {
			err := guardLocalFactor(store, request)
			if err == nil || contract.Classify(err).Code != contract.PreconditionFailed {
				t.Fatalf("export factor error = %v, want precondition_failed", err)
			}
		})
	}
	if err := guardLocalFactor(store, rpc.Request{Method: methodVersion}); err != nil {
		t.Fatalf("a non-export method asked for the factor: %v", err)
	}
	verified := rpc.Request{Method: methodSecretExport, Secrets: factorCode(code)}
	if err := guardLocalFactor(store, verified); err != nil {
		t.Fatalf("valid export code rejected: %v", err)
	}
	if err := guardLocalFactor(store, verified); err == nil {
		t.Fatal("one code was accepted twice, so an export could verify twice")
	}
}

func TestDeleteFactorPolicyUsesDurableTargetState(t *testing.T) {
	if !platform.RequiresHumanFactor(platform.KindAppDelete) {
		t.Fatal("app delete must always require the human factor")
	}
	if platform.RequiresHumanFactor(platform.KindEnvironmentDelete) {
		t.Fatal("environment delete is conditional, not an unconditional factor kind")
	}
	for _, tc := range []struct {
		name      string
		protected bool
		last      bool
		want      bool
	}{
		{name: "protected non-last", protected: true, want: true},
		{name: "unprotected non-last", want: false},
		{name: "protected last", protected: true, last: true, want: true},
		{name: "unprotected last deletes the app", last: true, want: true},
	} {
		if got := platform.EnvironmentDeleteRequiresHumanFactor(tc.protected, tc.last); got != tc.want {
			t.Fatalf("%s: factor = %t, want %t", tc.name, got, tc.want)
		}
	}
	if !platform.ProtectionChangeRequiresHumanFactor(true, false) {
		t.Fatal("protected -> unprotected must require the factor")
	}
	if platform.ProtectionChangeRequiresHumanFactor(false, true) || platform.ProtectionChangeRequiresHumanFactor(true, true) || platform.ProtectionChangeRequiresHumanFactor(false, false) {
		t.Fatal("only protected -> unprotected may require the factor")
	}
}

func TestFactorGateWithoutSetupPointsAtProtectInit(t *testing.T) {
	store := &protect.Store{Path: filepath.Join(t.TempDir(), "protect.json")}
	err := requireFactor(store, rpc.Request{Secrets: factorCode("123456")})
	fault := contract.Classify(err)
	if err == nil || fault.Code != contract.PreconditionFailed || !strings.Contains(fault.Hint, "protect init") {
		t.Fatalf("unconfigured store error = %+v, want the protect init precondition fault", fault)
	}
}
