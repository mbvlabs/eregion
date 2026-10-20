package cli

import (
	"encoding/json"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
)

// platformFleetSpec is a small, read-only fleet snapshot. The actionable Charm
// view belongs to the terminal-view work; this command deliberately prints
// append-only, no-colour text and never submits work or opens SSH.
func (a *app) platformFleetSpec() spec {
	return spec{
		use:  "fleet",
		meta: platformMeta("Print a read-only fleet snapshot of nodes, apps, and environments", "eregion platform fleet --json"),
		args: platformReadArgs,
		run:  a.platformFleet,
	}
}

func (a *app) platformFleet(cmd *cobra.Command, _ []string) (result, error) {
	filters := url.Values{"limit": {"200"}}
	nodes, err := a.platformReadInventoryWith(cmd, "nodes", filters)
	if err != nil {
		return result{}, err
	}
	apps, err := a.platformReadInventoryWith(cmd, "apps", filters)
	if err != nil {
		return result{}, err
	}
	environments, err := a.platformReadInventoryWith(cmd, "environments", filters)
	if err != nil {
		return result{}, err
	}
	data := map[string]json.RawMessage{
		"nodes":        nodes,
		"apps":         apps,
		"environments": environments,
	}
	encoded, _ := json.Marshal(data)
	var plain strings.Builder
	plain.WriteString("Fleet snapshot (read-only)\n")
	plain.WriteString("NODES\n")
	plain.WriteString(platformTable(nodes, []string{"name", "status", "roles", "capabilities", "last_seen_at"}))
	plain.WriteString("\nAPPS\n")
	plain.WriteString(platformTable(apps, []string{"id", "name", "updated_at"}))
	plain.WriteString("\nENVIRONMENTS\n")
	plain.WriteString(platformTable(environments, []string{"app", "name", "node", "serving_release_id", "health", "updated_at"}))
	plain.WriteString("\nRead-only snapshot limited to 200 items per collection; use platform inventory for focused queries.")
	return result{data: encoded, plain: plain.String()}, nil
}
