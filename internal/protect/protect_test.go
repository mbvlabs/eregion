package protect

import (
	"path/filepath"
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
