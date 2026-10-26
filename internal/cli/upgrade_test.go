package cli

import (
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/upgrade"
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

func TestUpgradeGuideDescribesLocalOnlyUpgrade(t *testing.T) {
	a := attachUpgrade(t)
	guide := a.currentUpgradeGuide()
	if guide.Schema != upgrade.GuideSchema || guide.GuideVersion != upgrade.GuideVersion {
		t.Fatalf("guide identity = %+v", guide)
	}
	found := false
	for _, command := range guide.Commands {
		if command.Path == "eregion upgrade" {
			found = true
			if !command.Mutating || !command.DryRun || !command.Prompts || !command.Remote {
				t.Fatalf("guide lost upgrade safety metadata: %+v", command)
			}
		}
	}
	if !found {
		t.Fatal("generated guide does not contain upgrade")
	}
	markdown := upgrade.RenderMarkdown(guide)
	for _, want := range []string{"Upgrade workflow", "upgrade --dry-run", "Nodes are not touched", "apply --auto-upgrade-nodes"} {
		if !strings.Contains(markdown, want) {
			t.Fatalf("markdown guide is missing %q:\n%s", want, markdown)
		}
	}
}

func TestUpgradeGuideFormatValidation(t *testing.T) {
	a := attachUpgrade(t)
	cmd, _, err := a.root.Find([]string{"guide"})
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("format", "yaml"); err != nil {
		t.Fatal(err)
	}
	if _, err := a.upgradeGuide(cmd, nil); err == nil {
		t.Fatal("unsupported guide format accepted")
	}
	if err := cmd.Flags().Set("format", "json"); err != nil {
		t.Fatal(err)
	}
	result, err := a.upgradeGuide(cmd, nil)
	if err != nil || result.plain == "" {
		t.Fatalf("json guide: %v %q", err, result.plain)
	}
}

func TestGuideCoversEveryImplementedCommand(t *testing.T) {
	a := newApp()
	if err := a.meta.validate(a.root); err != nil {
		t.Fatal(err)
	}
	guide := a.currentUpgradeGuide()
	covered := make(map[string]upgrade.GuideCommand, len(guide.Commands))
	for _, command := range guide.Commands {
		covered[command.Path] = command
	}
	for _, cmd := range walk(a.root) {
		if _, ok := covered[cmd.CommandPath()]; !ok {
			t.Fatalf("guide does not cover implemented command %q", cmd.CommandPath())
		}
	}
	for path, command := range covered {
		if strings.TrimSpace(command.Summary) == "" || len(command.Examples) == 0 || len(command.ErrorCodes) == 0 {
			t.Fatalf("guide entry %q is incomplete: %+v", path, command)
		}
	}
	if _, _, err := a.root.Find([]string{"guide"}); err != nil {
		t.Fatalf("top-level guide command is missing: %v", err)
	}
}
