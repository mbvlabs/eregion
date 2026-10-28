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

func TestDeleteSubmissionsCarryTheFactorOnlyWhenTheyPerform(t *testing.T) {
	store, code := configuredProtectStore(t)
	request := rpc.Request{Method: methodSubmit, Confirm: "api"}
	for _, kind := range []string{platform.KindAppDelete, platform.KindEnvironmentDelete} {
		if err := submitFactor(store, request, kind); err == nil || contract.Classify(err).Code != contract.PreconditionFailed {
			t.Fatalf("%s without a code = %v, want precondition_failed", kind, err)
		}
	}
	if err := submitFactor(store, rpc.Request{Method: methodSubmit, DryRun: true}, platform.KindAppDelete); err != nil {
		t.Fatalf("a delete dry run asked for the factor: %v", err)
	}
	if err := submitFactor(store, request, platform.KindDeploy); err != nil {
		t.Fatalf("deploy asked for the factor: %v", err)
	}
	withCode := rpc.Request{Method: methodSubmit, Secrets: factorCode(code)}
	if err := submitFactor(store, withCode, platform.KindEnvironmentDelete); err != nil {
		t.Fatalf("valid delete code rejected: %v", err)
	}
	if err := submitFactor(store, withCode, platform.KindEnvironmentDelete); err == nil {
		t.Fatal("one code was accepted for two delete submissions")
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
