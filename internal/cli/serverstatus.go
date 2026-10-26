package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/mbvlabs/eregion/internal/host"
	"github.com/spf13/cobra"
)

// attachServerExtras adds commands to the `server` parent without editing the
// server-init file, keeping the two workstreams separate.
func (a *app) attachServerExtras() {
	server, _, err := a.root.Find([]string{"server"})
	if err != nil {
		return
	}
	meta := operationMeta("Show the node's versions, services, schema, uptime and disk", "eregion server status --json")
	meta.Next = []string{"eregion doctor"}
	a.attach(server, spec{use: "status", meta: meta, run: a.serverStatus})
}

func (a *app) serverStatus(cmd *cobra.Command, _ []string) (result, error) {
	client, err := a.nodeClient(cmd)
	if err != nil {
		return result{}, err
	}
	status, err := client.HostStatus(cmd.Context())
	if err != nil {
		return result{}, err
	}
	return result{data: status, plain: statusText(status)}, nil
}

func statusText(status host.NodeStatus) string {
	var out strings.Builder
	fmt.Fprintf(&out, "node %s · role %s · eregion %s", status.Node, status.Role, status.NodeVersion)
	if status.CLI != "" && status.CLI != status.NodeVersion {
		fmt.Fprintf(&out, " · cli %s", status.CLI)
	}
	fmt.Fprintf(&out, "\n  uptime  %s", uptimeText(status.UptimeSeconds))
	if status.Disk.TotalBytes > 0 {
		fmt.Fprintf(&out, "\n  disk    %s free of %s on %s (%.0f%% free)", humanBytes(status.Disk.FreeBytes), humanBytes(status.Disk.TotalBytes), status.Disk.Path, status.Disk.FreePct)
	}
	schema := fmt.Sprintf("  schema  %d of %d (River %d of %d)", status.Schema.Current, status.Schema.Required, status.Schema.RiverCurrent, status.Schema.RiverRequired)
	if status.SchemaError != "" {
		schema += ": " + status.SchemaError
	}
	out.WriteString("\n" + schema)
	units := make([]string, 0, len(status.Services))
	for unit := range status.Services {
		units = append(units, unit)
	}
	sort.Strings(units)
	for _, unit := range units {
		state := "inactive"
		if status.Services[unit] {
			state = "active"
		}
		fmt.Fprintf(&out, "\n  %-28s %s", unit, state)
	}
	return out.String()
}

func uptimeText(seconds int64) string {
	if seconds <= 0 {
		return "unknown"
	}
	days := seconds / 86400
	hours := (seconds % 86400) / 3600
	minutes := (seconds % 3600) / 60
	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh %dm", days, hours, minutes)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, minutes)
	default:
		return fmt.Sprintf("%dm", minutes)
	}
}

func humanBytes(value uint64) string {
	const unit = 1024
	if value < unit {
		return fmt.Sprintf("%d B", value)
	}
	div, exp := uint64(unit), 0
	for n := value / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(value)/float64(div), "KMGTPE"[exp])
}
