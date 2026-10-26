package host

import (
	"path/filepath"
	"testing"
)

func TestPendingInitTransitions(t *testing.T) {
	pending := &PendingInit{Server: "prod", Phases: map[string]string{}}
	if pending.Done("first_contact") {
		t.Fatal("a new init reported a completed phase")
	}
	pending.Mark("first_contact")
	if !pending.Done("first_contact") || pending.Phases["first_contact"] == "" {
		t.Fatalf("phase not recorded: %+v", pending.Phases)
	}
	first := pending.Phases["first_contact"]
	pending.Mark("first_contact")
	if pending.Phases["first_contact"] != first {
		t.Fatal("re-marking a phase moved its completion time")
	}
	// A nil map (an older or hand-edited state) must not panic on Mark.
	var empty PendingInit
	empty.Mark("host_bootstrapped")
	if !empty.Done("host_bootstrapped") {
		t.Fatal("Mark did not initialise the phase map")
	}
}

func TestPendingInitSaveLoadRoundTrip(t *testing.T) {
	t.Setenv("EREGION_CONTEXT_FILE", filepath.Join(t.TempDir(), "eregion", "contexts.json"))
	pending := &PendingInit{
		Server: "prod", Host: "203.0.113.10", User: "root", Port: 22, Node: "main",
		Context: "prod", Hostname: "app.example.com", AdminUser: "admin",
		DatabaseURLFile: "/root/eregion-db.url", Phases: map[string]string{},
		HostKey: "ssh-ed25519 SHA256:host", HostKeyFingerprint: "SHA256:host",
	}
	pending.Mark("first_contact")
	if err := pending.Save(); err != nil {
		t.Fatal(err)
	}
	if !PendingExists("prod") {
		t.Fatal("saved init is not discoverable")
	}
	loaded, err := LoadPending("prod")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Server != "prod" || loaded.Hostname != "app.example.com" || !loaded.Done("first_contact") {
		t.Fatalf("loaded: %+v", loaded)
	}
	if loaded.HostKey != pending.HostKey || loaded.HostKeyFingerprint != pending.HostKeyFingerprint {
		t.Fatalf("host key was not saved: %+v", loaded)
	}
	if err := SavePendingFile("prod", VaultKeyringName, []byte("{}\n")); err != nil {
		t.Fatal(err)
	}
	if raw, err := LoadPendingFile("prod", VaultKeyringName); err != nil || string(raw) != "{}\n" {
		t.Fatalf("pending file: %q %v", raw, err)
	}
	if err := RemovePending("prod"); err != nil {
		t.Fatal(err)
	}
	if PendingExists("prod") {
		t.Fatal("discard left the init behind")
	}
}

func TestPendingNamesCannotEscapeThePendingRoot(t *testing.T) {
	t.Setenv("EREGION_CONTEXT_FILE", filepath.Join(t.TempDir(), "eregion", "contexts.json"))
	for _, name := range []string{"../escape", "a/b", "Prod", ""} {
		if _, err := PendingDir(name); err == nil {
			t.Errorf("PendingDir(%q) accepted a path", name)
		}
		if err := RemovePending(name); err == nil {
			t.Errorf("RemovePending(%q) accepted a path", name)
		}
		if _, err := LoadPending(name); err == nil {
			t.Errorf("LoadPending(%q) accepted a path", name)
		}
	}
}
