package cli

import (
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/mbvlabs/eregion/internal/cli/ui"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/spf13/cobra"
)

// askNode lets the operator pick a registered node that can take a placement:
// the role and capability the control plane checks before accepting work.
// With sameAs set, an empty answer is offered under that label and the node
// named same is left out, since picking it means the same. When the
// inventory cannot be read or no node qualifies, the name is typed instead.
func (a *app) askNode(cmd *cobra.Command, title, description, def, role, capability, sameAs, same string) (string, error) {
	typed := func() (string, error) {
		validate := platform.ValidateName
		if sameAs != "" {
			validate = validateOptionalName
		}
		return askValidated(title, description, def, validate)
	}
	raw, err := a.platformReadInventoryWith(cmd, "nodes", nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, ui.Warn("Could not list the nodes: "+err.Error()))
		return typed()
	}
	nodes, err := decodeInventoryItems[platform.Node](raw)
	if err != nil {
		return typed()
	}
	var options [][2]string
	if sameAs != "" {
		options = append(options, [2]string{sameAs, ""})
	}
	for _, node := range nodes {
		if (sameAs != "" && node.Name == same) || !slices.Contains(node.Roles, role) || (capability != "" && !slices.Contains(node.Capabilities, capability)) {
			continue
		}
		label := node.Name
		if node.Status != "" && node.Status != platform.NodeReady {
			label += " (" + string(node.Status) + ")"
		}
		options = append(options, [2]string{label, node.Name})
	}
	if len(options) == 0 || (sameAs != "" && len(options) == 1) {
		if sameAs != "" {
			return "", nil
		}
		need := role
		if capability != "" {
			need += " role and " + capability + " capability"
		}
		fmt.Fprintln(os.Stderr, ui.Warn("No registered node has the "+strings.TrimSpace(need)+"."))
		return typed()
	}
	return ui.ChooseDefault(title, description, options, def)
}
