package totp

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestVerifyAcceptsOneCodeOnce(t *testing.T) {
	secret, err := NewSecret()
	if err != nil {
		t.Fatal(err)
	}
	key, err := DecodeSecret(secret)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1800000000, 0).UTC()
	code := Code(key, now)
	step, err := Verify(key, code, now, 0)
	if err != nil || step != Step(now) {
		t.Fatalf("verify = %d, %v", step, err)
	}
	if _, err := Verify(key, code, now, step); !errors.Is(err, ErrReused) {
		t.Fatalf("reused code error = %v", err)
	}
	if _, err := Verify(key, "000000", now, 0); err == nil {
		t.Fatal("wrong code accepted")
	}
}

func TestURIIsScannableForm(t *testing.T) {
	uri := URI("eregion", "main", "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567")
	for _, want := range []string{"otpauth://totp/eregion:main?", "algorithm=SHA1", "digits=6", "period=30"} {
		if !strings.Contains(uri, want) {
			t.Fatalf("%s missing %q", uri, want)
		}
	}
}
