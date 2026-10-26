package cli

import (
	"testing"
)

func attachUpgrade(t *testing.T) *app {
	t.Helper()
	a := newApp()
	for _, spec := range a.upgradeSpecs() {
		a.attach(a.root, spec)
	}
	if err := a.meta.validate(a.root); err != nil {
		t.Fatalf("upgrade metadata does not validate: %v", err)
	}
	return a
}

func TestUpgradeSpecsMetadata(t *testing.T) {
	a := attachUpgrade(t)
	cmd, _, err := a.root.Find([]string{"upgrade"})
	if err != nil {
		t.Fatal(err)
	}
	meta := a.meta[cmd]
	if !meta.Mutating || !meta.DryRun || !meta.Prompts || meta.Destructive {
		t.Fatalf("upgrade metadata is wrong: %+v", meta)
	}
	if meta.Confirmation != "" {
		t.Fatalf("upgrade unexpectedly requires a plan-bound confirmation: %q", meta.Confirmation)
	}
	for _, platform := range meta.Platforms {
		if platform == "windows" {
			t.Fatal("upgrade advertises a Windows build")
		}
	}
	for _, name := range []string{"version", "channel", "yes"} {
		if flagFor(cmd, name) == nil {
			t.Fatalf("upgrade is missing --%s", name)
		}
	}
	if cmd.HasSubCommands() {
		t.Fatal("the removed upgrade subcommands are still attached")
	}
}
