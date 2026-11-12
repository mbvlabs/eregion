package controlplane

import (
	"strings"
	"testing"
)

func testRegistryHostIdentity() RegistryHostIdentity {
	return RegistryHostIdentity{
		NodeName:           "main",
		HostKeyType:        "ssh-ed25519",
		HostKeyFingerprint: "SHA256:" + strings.Repeat("A", 43),
		StateDir:           "/var/lib/eregion/node",
	}
}

func testRegistryHostIdentityFor(node, stateDir string) RegistryHostIdentity {
	identity := testRegistryHostIdentity()
	identity.NodeName = node
	identity.StateDir = stateDir
	return identity
}

func TestRegistryHostIdentityValidationAndExactComparison(t *testing.T) {
	binding := testRegistryHostIdentity()
	if err := binding.Validate(); err != nil {
		t.Fatal(err)
	}
	if !binding.Equal(testRegistryHostIdentity()) {
		t.Fatal("an identical binding did not compare equal")
	}
	if binding.Equal(testRegistryHostIdentityFor("main", "/var/lib/other")) {
		t.Fatal("a different canonical state directory was accepted")
	}
	if binding.Equal(testRegistryHostIdentityFor("other", "/var/lib/eregion/node")) {
		t.Fatal("a name-only match was accepted")
	}
	otherKey := testRegistryHostIdentity()
	otherKey.HostKeyFingerprint = "SHA256:" + "B" + otherKey.HostKeyFingerprint[len("SHA256:A"):]
	if binding.Equal(otherKey) {
		t.Fatal("a different public host-key fingerprint was accepted")
	}
	for name, mutate := range map[string]func(*RegistryHostIdentity){
		"node":        func(i *RegistryHostIdentity) { i.NodeName = "Bad Name" },
		"key type":    func(i *RegistryHostIdentity) { i.HostKeyType = "" },
		"fingerprint": func(i *RegistryHostIdentity) { i.HostKeyFingerprint = "MD5:abc" },
		"relative":    func(i *RegistryHostIdentity) { i.StateDir = "relative/node" },
		"unclean":     func(i *RegistryHostIdentity) { i.StateDir = "/var/lib/eregion/../node" },
	} {
		candidate := binding
		mutate(&candidate)
		if err := candidate.Validate(); err == nil {
			t.Fatalf("%s binding was accepted", name)
		}
	}
}
