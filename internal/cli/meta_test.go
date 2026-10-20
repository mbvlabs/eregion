package cli

import (
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/spf13/cobra"
)

func TestMetadataGate(t *testing.T) {
	if err := newApp().meta.validate(newApp().root); err == nil {
		t.Fatal("metadata from another invocation must not validate this tree")
	}
	for _, test := range []struct {
		name   string
		change func(*app, *cobra.Command, *CommandMeta)
		want   string
	}{
		{"missing", func(a *app, c *cobra.Command, _ *CommandMeta) { delete(a.meta, c) }, "missing metadata"},
		{"summary", func(_ *app, _ *cobra.Command, m *CommandMeta) { m.Summary = "" }, "summary"},
		{"examples", func(_ *app, _ *cobra.Command, m *CommandMeta) { m.Examples = nil }, "examples"},
		{"use", func(_ *app, _ *cobra.Command, m *CommandMeta) { m.WhenToUse = nil }, "when_to_use"},
		{"audience", func(_ *app, _ *cobra.Command, m *CommandMeta) { m.Audience = []string{"everyone"} }, "audience"},
		{"platform", func(_ *app, _ *cobra.Command, m *CommandMeta) { m.Platforms = nil }, "platforms"},
		{"node", func(_ *app, _ *cobra.Command, m *CommandMeta) { m.Audience = []string{"node"} }, "Linux-only"},
		{"restart", func(_ *app, _ *cobra.Command, m *CommandMeta) { m.NeedsRestart = "maybe" }, "needs_restart"},
		{"output", func(_ *app, _ *cobra.Command, m *CommandMeta) { m.Output = "text" }, "output"},
		{"errors", func(_ *app, _ *cobra.Command, m *CommandMeta) { m.ErrorCodes = []contract.Code{"made_up"} }, "error code"},
		{"mutation", func(_ *app, _ *cobra.Command, m *CommandMeta) { m.Mutating = true }, "dry-run"},
		{"dry-run", func(_ *app, _ *cobra.Command, m *CommandMeta) { m.DryRun = true }, "dry-run"},
		{"destructive", func(_ *app, _ *cobra.Command, m *CommandMeta) { m.Destructive = true }, "confirmation"},
		{"confirmation", func(_ *app, _ *cobra.Command, m *CommandMeta) { m.Confirmation = "yes" }, "plan-bound"},
		{"prompt", func(_ *app, _ *cobra.Command, m *CommandMeta) { m.Prompts = true }, "noninteractive"},
		{"flag", func(_ *app, _ *cobra.Command, m *CommandMeta) { m.InputFlags = []string{"absent"} }, "undefined"},
		{"destructive-flag", func(_ *app, c *cobra.Command, m *CommandMeta) {
			c.Flags().Bool("erase", false, "erase")
			m.DestructiveFlags = []string{"erase"}
		}, "also be mutating"},
		{"handler", func(_ *app, c *cobra.Command, _ *CommandMeta) { c.RunE = nil }, "handler"},
		{"next", func(_ *app, _ *cobra.Command, m *CommandMeta) { m.Next = []string{"eregion deploy"} }, "implemented command"},
		{"dry-run-type", func(_ *app, c *cobra.Command, m *CommandMeta) {
			c.Flags().String("dry-run", "", "bad")
			m.DryRun = true
		}, "boolean"},
		{"confirm-type", func(_ *app, c *cobra.Command, m *CommandMeta) {
			c.Flags().Bool("confirm", false, "bad")
			m.Confirmation = "plan-bound"
		}, "token string"},
	} {
		t.Run(test.name, func(t *testing.T) {
			a := newApp()
			// Use a portable command so the metadata gate test exercises the
			// validator, not the first Linux-only command added to the tree.
			c, _, _ := a.root.Find([]string{"version"})
			meta := a.meta[c]
			test.change(a, c, &meta)
			if test.name != "missing" {
				a.meta[c] = meta
			}
			err := a.meta.validate(a.root)
			if err == nil {
				t.Fatal("invalid metadata passed the gate")
			}
			fault := contract.Classify(err)
			found := false
			for _, violation := range fault.Details["violations"].([]string) {
				found = found || strings.Contains(violation, test.want)
			}
			if !found {
				t.Fatalf("expected %q in %v", test.want, fault.Details)
			}
		})
	}
}

func TestValidMutationMetadata(t *testing.T) {
	a := newApp()
	c, _, _ := a.root.Find([]string{"version"})
	meta := a.meta[c]
	meta.Mutating, meta.Destructive, meta.DryRun = true, true, true
	meta.Confirmation = "plan-bound"
	c.Flags().Bool("dry-run", false, "preview")
	c.Flags().String("confirm", "", "token")
	a.meta[c] = meta
	if err := a.meta.validate(a.root); err != nil {
		t.Fatal(err)
	}
}
