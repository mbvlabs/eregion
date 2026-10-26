package bundle

import (
	"errors"
	"strings"
	"testing"
)

func TestRoundTripFromCopiedRecord(t *testing.T) {
	pass, err := NewPassphrase()
	if err != nil {
		t.Fatal(err)
	}
	if n := len(strings.Split(pass, "-")); n != Words {
		t.Fatalf("passphrase has %d words", n)
	}
	armored, err := Encrypt([]byte("the record"), pass)
	if err != nil {
		t.Fatal(err)
	}
	pasted := "eregion secrets handoff\n## Transfer bundle\nPassphrase: " + pass + "\n" + string(armored) + "trailing\n"
	block, err := Read(strings.NewReader(pasted))
	if err != nil {
		t.Fatal(err)
	}
	plain, err := Decrypt(block, pass)
	if err != nil || string(plain) != "the record" {
		t.Fatalf("decrypt = %q, %v", plain, err)
	}
	if _, err := Decrypt(block, "wrong-words"); !errors.Is(err, ErrPassphrase) {
		t.Fatalf("wrong passphrase: %v", err)
	}
}
