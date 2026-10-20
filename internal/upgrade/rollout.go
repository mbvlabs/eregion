package upgrade

import (
	"fmt"
	"strings"

	"github.com/mbvlabs/eregion/internal/contract"
)

// DatabasePlan is the read-only migration plan of the database the rollout
// would run against. Current is the forward-only schema already applied;
// Required is the schema this binary knows how to migrate to.
type DatabasePlan struct {
	Current       int `json:"current"`
	Required      int `json:"required"`
	RiverCurrent  int `json:"river_current"`
	RiverRequired int `json:"river_required"`
}

// RolloutStep is one ordered stage of a coordinated control-plane upgrade.
// Commands are illustrative operator commands for that stage, not something
// this CLI executes on the operator's behalf.
type RolloutStep struct {
	Order     int      `json:"order"`
	Stage     string   `json:"stage"`
	Name      string   `json:"name"`
	Action    string   `json:"action"`
	Commands  []string `json:"commands"`
	Verify    []string `json:"verify"`
	OnFailure string   `json:"on_failure"`
}

// RolloutPlan is the dry-run output for a coordinated upgrade. It documents the
// rolling order migrate -> workers -> API -> nodes. It never mutates anything
// and does not prove that a live rollout or failover works.
type RolloutPlan struct {
	Direction      string                 `json:"direction"`
	ReleaseVersion string                 `json:"release_version"`
	SchemaVersion  int                    `json:"schema_version"`
	Contract       string                 `json:"contract"`
	NodeProtocol   int                    `json:"node_protocol"`
	Database       DatabasePlan           `json:"database"`
	Compatibility  Compatibility          `json:"compatibility"`
	Inventory      []ControlPlaneInstance `json:"inventory,omitempty"`
	Steps          []RolloutStep          `json:"steps"`
	Warnings       []string               `json:"warnings"`
	Limitations    []string               `json:"limitations"`
}

// BuildRolloutPlan verifies the release and returns the documented rolling
// order. When inventory is supplied, every listed process must be a supported
// upgrade source and must agree on schema, contract, and node protocol. When it
// is empty, live process compatibility is explicitly unverified.
func BuildRolloutPlan(m Manifest, env Environment, database DatabasePlan, inventory []ControlPlaneInstance) (RolloutPlan, error) {
	plan := RolloutPlan{
		Direction:      "rolling-upgrade",
		ReleaseVersion: m.Version,
		SchemaVersion:  m.SchemaVersion,
		Contract:       m.Contract,
		NodeProtocol:   m.NodeProtocol,
		Database:       database,
		Inventory:      append([]ControlPlaneInstance(nil), inventory...),
	}
	if err := m.Validate(); err != nil {
		return plan, err
	}
	if env.Target == "" {
		env.Target = TargetOperator
	}
	report := CheckCompatibility(m, env)
	plan.Compatibility = report
	if !report.Compatible {
		return plan, report.Err()
	}
	if _, err := SelectArtifact(m, env.Target, env.OS, env.Arch); err != nil {
		return plan, err
	}
	if len(inventory) > 0 {
		if err := CheckControlPlaneCompatibility(m, inventory); err != nil {
			return plan, err
		}
	} else {
		plan.Warnings = append(plan.Warnings, "no control-plane inventory was supplied; live process versions, contract, and node protocol remain unverified")
	}
	if database.Required > 0 && database.Current < database.Required {
		plan.Warnings = append(plan.Warnings, fmt.Sprintf("database schema %d has pending migrations to %d; run system migrate before starting workers or the API", database.Current, database.Required))
	}
	manifest := "--manifest MANIFEST"
	applyOperator := fmt.Sprintf("eregion upgrade apply %s --target operator --confirm TOKEN", manifest)
	plan.Steps = []RolloutStep{
		{
			Order: 1, Stage: "migrate", Name: "Migrate the database once",
			Action: "Install the target operator binary on one designated migration host and apply the forward-only schema once, before restarting any worker or API process. Older binaries must remain compatible with additive migrations for the length of the rollout.",
			Commands: []string{
				applyOperator,
				"eregion system migrate",
				"eregion system migrate --dry-run --json",
			},
			Verify: []string{
				fmt.Sprintf("upgrade dry-run reports forward-only schema %d -> %d", database.Current, m.SchemaVersion),
				fmt.Sprintf("system migrate --dry-run reports current == required == %d", m.SchemaVersion),
				"the migration ran exactly once; a second concurrent migrate must wait on the advisory lock",
			},
			OnFailure: "Stop before touching workers, the API, or nodes. Do not downgrade the schema; restore the database from the pre-migration backup and retry with a corrected release.",
		},
		{
			Order: 2, Stage: "workers", Name: "Roll worker processes one at a time",
			Action:   "Replace the binary on one worker host at a time and restart only its worker service after the version and readiness probes pass. Keep the remaining old workers running so accepted operations continue to drain.",
			Commands: []string{applyOperator, "systemctl restart eregion-worker.service", "eregion version --json"},
			Verify: []string{
				"the new process reports the release version",
				"the readiness probe reaches the migrated database with current == required",
				"each restarted worker claims and completes an operation before the next host is changed",
			},
			OnFailure: "Stop rolling workers, leave untouched workers on the previous version, and restore the failed host's binary with the recorded backup. Rollback the schema only by restoring the database out of band.",
		},
		{
			Order: 3, Stage: "api", Name: "Roll API processes one at a time",
			Action:   "Replace the binary on one API host at a time, restart it, and remove it from the load balancer only for that restart. Do not change every API process at once; keep at least one ready process serving requests.",
			Commands: []string{applyOperator, "systemctl restart eregion-api.service", "curl --fail --silent --show-error -H \"Authorization: Bearer $EREGION_API_TOKEN\" https://api.example.com/readyz"},
			Verify: []string{
				"GET /readyz returns ready before the next API host changes",
				"the process reports the release version and the node protocol from the manifest",
				"a scoped operation submission succeeds through the new process",
			},
			OnFailure: "Stop rolling the API, drain the failed host, and restore its recorded backup. Keep the already-upgraded API processes serving; do not roll the schema backwards.",
		},
		{
			Order: 4, Stage: "nodes", Name: "Roll node agents last",
			Action: "Replace eregion-node on one node at a time and restart only that node service. Nodes must speak the release's node protocol; keep the control plane and nodes on the same protocol version before accepting new work on a node.",
			Commands: []string{
				fmt.Sprintf("eregion upgrade apply %s --target node --confirm TOKEN", manifest),
				"systemctl restart eregion-node.service",
				"eregion-node version",
			},
			Verify: []string{
				"the node process starts and reports its identity",
				"an operation on that node reaches a terminal state before the next node changes",
				"the control-plane inventory reports the release's node protocol for every rolled node",
			},
			OnFailure: "Stop rolling nodes, restore the failed node's recorded backup, and diagnose before continuing. A node binary that cannot start is not an active node.",
		},
	}
	plan.Limitations = []string{
		"This plan is dry-run documentation; the CLI does not drive multiple hosts, drain traffic, or hold a cluster-wide lock.",
		"Inventory checks read operator-supplied facts, not live processes. They are not proof of quorum or high availability.",
		"Multiple workers and a shared database are not by themselves an HA guarantee; publish only failover behavior that was actually drilled.",
		"Database migrations are forward-only. Restoring an older schema requires an explicit database restore outside this tool.",
	}
	return plan, nil
}

// RenderRolloutPlan renders the documented rolling order for humans.
func RenderRolloutPlan(plan RolloutPlan) string {
	var out strings.Builder
	fmt.Fprintf(&out, "Rollout plan (%s)\n\nrelease: %s\ncontract: %s\nnode protocol: %d\ndatabase schema: %d, release requires %d\n", plan.Direction, plan.ReleaseVersion, plan.Contract, plan.NodeProtocol, plan.Database.Current, plan.SchemaVersion)
	out.WriteString("\nCompatibility:\n")
	for _, check := range plan.Compatibility.Checks {
		verdict := "ok"
		if !check.OK {
			verdict = "FAIL"
		}
		fmt.Fprintf(&out, "  [%s] %s: %s\n", verdict, check.Name, check.Detail)
	}
	if len(plan.Inventory) > 0 {
		fmt.Fprintf(&out, "\nInventory: %d control-plane instance(s) passed the shared checks\n", len(plan.Inventory))
	}
	out.WriteString("\nRolling order:\n")
	for _, step := range plan.Steps {
		fmt.Fprintf(&out, "\n%d. %s — %s\n", step.Order, step.Stage, step.Name)
		fmt.Fprintf(&out, "   %s\n", step.Action)
		out.WriteString("   commands:\n")
		for _, command := range step.Commands {
			fmt.Fprintf(&out, "     %s\n", command)
		}
		out.WriteString("   verify:\n")
		for _, item := range step.Verify {
			fmt.Fprintf(&out, "     - %s\n", item)
		}
		fmt.Fprintf(&out, "   on failure: %s\n", step.OnFailure)
	}
	if len(plan.Warnings) > 0 {
		out.WriteString("\nWarnings:\n")
		for _, warning := range plan.Warnings {
			fmt.Fprintf(&out, "  - %s\n", warning)
		}
	}
	out.WriteString("\nLimitations:\n")
	for _, limitation := range plan.Limitations {
		fmt.Fprintf(&out, "  - %s\n", limitation)
	}
	out.WriteString("\nNo command was run and no binary was replaced. Publish only failover guarantees that were actually tested.\n")
	return out.String()
}

// ValidateRolloutInventory checks operator-supplied inventory entries before
// they reach a compatibility decision. Labels are diagnostics, never identity
// inferred from the network.
func ValidateRolloutInventory(instances []ControlPlaneInstance) error {
	if len(instances) == 0 {
		return nil
	}
	if len(instances) > 256 {
		return contract.NewFault(contract.ValidationFailed, "control-plane inventory is too large", "List at most 256 instances.")
	}
	seen := map[string]bool{}
	for _, instance := range instances {
		label := strings.TrimSpace(instance.ID)
		if label == "" {
			return contract.NewFault(contract.ValidationFailed, "control-plane inventory entry has no id", "Give every instance a stable operator label.")
		}
		if seen[label] {
			return contract.NewFault(contract.ValidationFailed, "control-plane inventory repeats an id", "Use one label per running control-plane process.")
		}
		seen[label] = true
		if strings.TrimSpace(instance.Version) == "" {
			return contract.NewFault(contract.ValidationFailed, "control-plane inventory entry has no version", "Report the installed binary version for every instance.")
		}
		if instance.NodeProtocol < 1 {
			return contract.NewFault(contract.ValidationFailed, "control-plane inventory node protocol must be positive", "Report the node protocol the process speaks.")
		}
		if instance.SchemaVersion < 1 {
			return contract.NewFault(contract.ValidationFailed, "control-plane inventory schema version must be positive", "Report the database schema the process expects.")
		}
	}
	return nil
}
