package host

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
)

func TestCredentialsStepPreservesMintedAndRevokedTokens(t *testing.T) {
	cfg := fixture(t, RoleMain)
	credentials, err := LoadCredentials(cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	step := &credentialsStep{cfg: cfg, credentials: credentials}
	if check, err := step.Check(context.Background(), engine.Runtime{}); err != nil || check.Complete {
		t.Fatalf("a missing auth file was already complete: %+v %v", check, err)
	}
	if _, err := step.Apply(context.Background(), engine.Runtime{}, nil); err != nil {
		t.Fatal(err)
	}
	if check, err := step.Check(context.Background(), engine.Runtime{}); err != nil || !check.Complete {
		t.Fatalf("the initialized store was not complete: %+v %v", check, err)
	}

	// Mint exactly as token create does: hash and metadata through the same
	// owner-only writer, with the API reading the same path.
	token := strings.Repeat("m", 43)
	minted := []api.Principal{principal(contract.Actor{Kind: "ci", Subject: "ci-test"}, token, []string{"operations:read", "operations:submit"})}
	if err := WriteAuthFile(step.authPath(), minted, cfg.Layout.User, cfg.Layout.Group); err != nil {
		t.Fatal(err)
	}
	mintedRaw, err := os.ReadFile(step.authPath())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := api.LoadAuth(step.authPath()); err != nil {
		t.Fatalf("the API reader refused the writer-minted store: %v", err)
	}
	if check, err := step.Check(context.Background(), engine.Runtime{}); err != nil || !check.Complete {
		t.Fatalf("a minted token presented as drift: %+v %v", check, err)
	}
	changes, err := AccessChanges(cfg, credentials)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range changes {
		if change.Path == step.authPath() {
			t.Fatalf("minting a token armed an access change: %+v", change)
		}
	}
	if _, err := step.Apply(context.Background(), engine.Runtime{}, nil); err != nil {
		t.Fatal(err)
	}
	afterApply, err := os.ReadFile(step.authPath())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(mintedRaw, afterApply) {
		t.Fatalf("apply overwrote the minted token store:\nbefore=%s\nafter=%s", mintedRaw, afterApply)
	}

	// Revocation is the same store mutation and must also survive convergence.
	if err := WriteAuthFile(step.authPath(), []api.Principal{}, cfg.Layout.User, cfg.Layout.Group); err != nil {
		t.Fatal(err)
	}
	revokedRaw, err := os.ReadFile(step.authPath())
	if err != nil {
		t.Fatal(err)
	}
	if check, err := step.Check(context.Background(), engine.Runtime{}); err != nil || !check.Complete {
		t.Fatalf("an emptied store presented as drift: %+v %v", check, err)
	}
	if _, err := step.Apply(context.Background(), engine.Runtime{}, nil); err != nil {
		t.Fatal(err)
	}
	afterRevokeApply, err := os.ReadFile(step.authPath())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(revokedRaw, afterRevokeApply) {
		t.Fatalf("apply resurrected revoked tokens:\nbefore=%s\nafter=%s", revokedRaw, afterRevokeApply)
	}
	if _, err := api.LoadAuth(step.authPath()); err != nil {
		t.Fatalf("the API reader refused the revoked store: %v", err)
	}
}

func TestCredentialsStepFailsClosedOnMalformedStore(t *testing.T) {
	cfg := fixture(t, RoleMain)
	credentials, err := LoadCredentials(cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	step := &credentialsStep{cfg: cfg, credentials: credentials}
	put(t, step.authPath(), `[{"actor":`, 0o600)
	before, err := os.ReadFile(step.authPath())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := step.Check(context.Background(), engine.Runtime{}); err == nil {
		t.Fatal("a malformed token store passed Check")
	}
	if _, err := step.Apply(context.Background(), engine.Runtime{}, nil); err == nil {
		t.Fatal("a malformed token store was overwritten by Apply")
	}
	after, err := os.ReadFile(step.authPath())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("apply rewrote a malformed store:\nbefore=%s\nafter=%s", before, after)
	}
}

func TestCredentialsStepConvergesModeWithoutRewritingTokens(t *testing.T) {
	cfg := fixture(t, RoleMain)
	credentials, err := LoadCredentials(cfg, true)
	if err != nil {
		t.Fatal(err)
	}
	step := &credentialsStep{cfg: cfg, credentials: credentials}
	token := strings.Repeat("n", 43)
	if err := WriteAuthFile(step.authPath(), []api.Principal{principal(contract.Actor{Kind: "ci", Subject: "ci-mode"}, token, []string{"operations:read"})}, cfg.Layout.User, cfg.Layout.Group); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(step.authPath())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(step.authPath(), 0o640); err != nil {
		t.Fatal(err)
	}
	if check, err := step.Check(context.Background(), engine.Runtime{}); err != nil || check.Complete {
		t.Fatalf("unsafe mode was not reported: %+v %v", check, err)
	}
	if _, err := step.Apply(context.Background(), engine.Runtime{}, nil); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(step.authPath())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("mode convergence rewrote the token store:\nbefore=%s\nafter=%s", before, after)
	}
	info, err := os.Stat(step.authPath())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode was not converged: %o", info.Mode().Perm())
	}
}
