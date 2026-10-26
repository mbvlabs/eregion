package cli

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/buildinfo"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/controlplane"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/upgrade"
	"github.com/spf13/cobra"
)

var upgradeEnvNames = []string{
	"EREGION_UPGRADE_STATE_DIR",
	"EREGION_UPGRADE_PUBLIC_KEY_FILE",
	"EREGION_UPGRADE_PUBLIC_KEY",
	"EREGION_UPGRADE_ALLOWED_HOSTS",
	"EREGION_DATABASE_URL",
}

func upgradeMeta(summary string, when, examples []string, next ...string) CommandMeta {
	meta := readMeta(summary, when, examples, next...)
	meta.Audience = []string{"agent", "human"}
	meta.Platforms = []string{"linux", "darwin", "windows"}
	meta.Environment = append(meta.Environment, upgradeEnvNames...)
	meta.ErrorCodes = []contract.Code{
		contract.Usage, contract.InvalidOutputMode, contract.InputRequired, contract.ValidationFailed,
		contract.NotFound, contract.PreconditionFailed, contract.ConfirmationRequired,
		contract.Conflict, contract.Unavailable, contract.Internal, contract.Cancelled,
	}
	return meta
}

// upgradeSpecs is attached by the root orchestrator. It only advertises the
// upgrade commands implemented here; no roadmap capability is inferred.
func (a *app) upgradeSpecs() []spec {
	groupMeta := upgradeMeta("Verify, plan, apply, and roll back signed releases",
		[]string{"Discover the signed release workflow before replacing an operator or node binary."},
		[]string{"eregion upgrade --help", "eregion guide --json"}, "eregion guide --format markdown")

	checkMeta := upgradeMeta("Verify a signed release and print a dry-run diff",
		[]string{"Check signature, artifact SHA256, platform, contract, node protocol, and forward-only schema compatibility before downloading."},
		[]string{"eregion upgrade check --manifest release.json --target operator --schema-version 3 --json"}, "eregion upgrade apply --manifest release.json --dry-run --json")
	checkMeta.InputFlags = []string{"manifest", "target", "schema-version", "current-version"}
	planMeta := upgradeMeta("Print the documented rolling order without changing anything",
		[]string{"Review the migrate -> workers -> API -> nodes order, database schema direction, and optional control-plane inventory before a coordinated rollout."},
		[]string{"eregion upgrade plan --manifest release.json --schema-version 3 --json", "eregion upgrade plan --manifest release.json --inventory control-plane.json --json"})
	planMeta.InputFlags = []string{"manifest", "target", "schema-version", "current-version", "inventory"}
	verifyMeta := upgradeMeta("Verify a signed release manifest without touching an installation",
		[]string{"Check the Ed25519 signature and manifest structure before downloading or extracting any artifact."},
		[]string{"eregion upgrade verify --manifest release.json --public-key-file release.pub --json"})
	verifyMeta.InputFlags = []string{"manifest", "public-key-file"}
	applyMeta := upgradeMeta("Atomically replace an operator or node binary from a signed release",
		[]string{"Apply a verified release after reviewing a plan-bound dry-run. The new binary is health-checked and the previous binary is restored automatically when that probe fails."},
		[]string{"eregion upgrade apply --manifest release.json --target operator --schema-version 3 --dry-run --json", "eregion upgrade apply --manifest release.json --target operator --schema-version 3 --confirm TOKEN --json"})
	applyMeta.Mutating, applyMeta.Destructive, applyMeta.DryRun = true, true, true
	applyMeta.Confirmation = "plan-bound"
	applyMeta.NeedsRestart = "required"
	applyMeta.InputFlags = []string{"manifest", "target", "schema-version", "current-version", "confirm"}

	rollbackMeta := upgradeMeta("Restore one verified recorded backup explicitly",
		[]string{"Roll back a binary replacement after reviewing the restore diff. Arbitrary files outside the upgrade state are never restored."},
		[]string{"eregion upgrade rollback --target operator --dry-run --json", "eregion upgrade rollback --target operator --confirm TOKEN --json"})
	rollbackMeta.Mutating, rollbackMeta.Destructive, rollbackMeta.DryRun = true, true, true
	rollbackMeta.Confirmation = "plan-bound"
	rollbackMeta.NeedsRestart = "required"
	rollbackMeta.InputFlags = []string{"target", "backup", "confirm"}

	statusMeta := upgradeMeta("Read installed binary identity, journal, and backups",
		[]string{"Inspect whether a prior replacement was interrupted and which rollback copies exist."},
		[]string{"eregion upgrade status --target operator --json"}, "eregion upgrade rollback --dry-run --json")

	guideMeta := upgradeMeta("Generate the versioned agent guide from implemented commands",
		[]string{"Produce discovery text or JSON that matches this binary's implemented command registry."},
		[]string{"eregion guide --format markdown", "eregion guide --format json --json"})
	rootGuideMeta := guideMeta
	rootGuideMeta.Summary = "Generate the versioned agent guide from every implemented command"

	return []spec{{
		use:  "upgrade",
		meta: groupMeta,
		run:  func(cmd *cobra.Command, _ []string) (result, error) { return a.help(cmd), nil },
		children: []spec{
			{
				use:  "check",
				meta: checkMeta,
				flags: func(cmd *cobra.Command) {
					upgradeManifestFlags(cmd)
				},
				run: a.upgradeCheck,
			},
			{
				use:  "plan",
				meta: planMeta,
				flags: func(cmd *cobra.Command) {
					upgradeManifestFlags(cmd)
					cmd.Flags().String("inventory", "", "Optional JSON inventory of running control-plane instances for compatibility checks")
				},
				run: a.upgradePlan,
			},
			{
				use:  "verify",
				meta: verifyMeta,
				flags: func(cmd *cobra.Command) {
					cmd.Flags().String("manifest", "", "Signed release manifest path or direct HTTPS URL")
					cmd.Flags().String("public-key-file", "", "Trusted Ed25519 public key file (defaults to EREGION_UPGRADE_PUBLIC_KEY_FILE)")
				},
				run: a.upgradeVerify,
			},
			{
				use:  "apply",
				meta: applyMeta,
				flags: func(cmd *cobra.Command) {
					upgradeManifestFlags(cmd)
					cmd.Flags().Bool("dry-run", false, "Print the verified plan and confirmation token without downloading or replacing")
					cmd.Flags().String("confirm", "", "Plan-bound confirmation token from --dry-run")
					cmd.Flags().Bool("health-check", true, "Run the new binary's version/readiness probe and restore the backup on failure")
					cmd.Flags().Duration("health-timeout", 20*time.Second, "Maximum time for one health probe")
				},
				run: a.upgradeApply,
			},
			{
				use:  "rollback",
				meta: rollbackMeta,
				flags: func(cmd *cobra.Command) {
					upgradeTargetFlags(cmd)
					cmd.Flags().String("backup", "", "Verified backup path inside the target's state directory (defaults to the journal's last backup)")
					cmd.Flags().Bool("dry-run", false, "Print the restore plan and confirmation token without replacing")
					cmd.Flags().String("confirm", "", "Plan-bound confirmation token from --dry-run")
				},
				run: a.upgradeRollback,
			},
			{
				use:  "status",
				meta: statusMeta,
				flags: func(cmd *cobra.Command) {
					upgradeTargetFlags(cmd)
				},
				run: a.upgradeStatus,
			},
			{
				use:  "guide",
				meta: guideMeta,
				flags: func(cmd *cobra.Command) {
					cmd.Flags().String("format", "markdown", "Output format: markdown or json")
				},
				run: a.upgradeGuide,
			},
		},
	}, {
		use:  "guide",
		meta: rootGuideMeta,
		flags: func(cmd *cobra.Command) {
			cmd.Flags().String("format", "markdown", "Output format: markdown or json")
		},
		run: a.upgradeGuide,
	}}
}

func upgradeManifestFlags(cmd *cobra.Command) {
	upgradeTargetFlags(cmd)
	cmd.Flags().String("manifest", "", "Signed release manifest path or direct HTTPS URL")
	cmd.Flags().String("artifact-dir", "", "Optional offline artifact mirror; files are still verified against the signed SHA256")
	cmd.Flags().String("public-key-file", "", "Trusted Ed25519 public key file (defaults to EREGION_UPGRADE_PUBLIC_KEY_FILE)")
	cmd.Flags().Int("schema-version", -1, "Current database schema version; defaults to inspecting EREGION_DATABASE_URL")
	cmd.Flags().String("current-version", "", "Installed target version for upgrade_from checks; defaults to this operator build, or unknown for node")
}

func upgradeTargetFlags(cmd *cobra.Command) {
	cmd.Flags().String("target", string(upgrade.TargetOperator), "Upgrade target: operator or node")
	cmd.Flags().String("binary", "", "Installed target binary path (defaults to this executable)")
	cmd.Flags().String("state-dir", "", "Upgrade state directory (defaults to EREGION_UPGRADE_STATE_DIR)")
}

type upgradeOptions struct {
	target   upgrade.Target
	stateDir string
	binary   string
	manager  *upgrade.Manager
	manifest upgrade.Manifest
	database upgrade.DatabasePlan
}

func (a *app) upgradeOptions(cmd *cobra.Command, manifest upgrade.Manifest) (upgradeOptions, error) {
	target, err := upgrade.ParseTarget(upgradeFlag(cmd, "target"))
	if err != nil {
		return upgradeOptions{}, err
	}
	stateDir := upgradeFlag(cmd, "state-dir")
	if stateDir == "" {
		stateDir = upgrade.DefaultStateDir()
	}
	stateDir, err = filepath.Abs(stateDir)
	if err != nil {
		return upgradeOptions{}, contract.NewFault(contract.ValidationFailed, "upgrade state directory is invalid", "Pass an absolute writable path with --state-dir.")
	}
	binary, err := upgradeBinaryPath(cmd, target)
	if err != nil {
		return upgradeOptions{}, err
	}
	env, database, err := a.upgradeEnvironment(cmd, target)
	if err != nil {
		return upgradeOptions{}, err
	}
	allowed := upgradeHosts(os.Getenv("EREGION_UPGRADE_ALLOWED_HOSTS"))
	manager := &upgrade.Manager{
		StateDir: stateDir, Target: target, BinaryPath: binary, Env: env,
		Downloader: upgrade.NewDownloader(allowed), ArtifactDir: upgradeFlag(cmd, "artifact-dir"),
	}
	return upgradeOptions{target: target, stateDir: stateDir, binary: binary, manager: manager, manifest: manifest, database: database}, nil
}

func (a *app) upgradeEnvironment(cmd *cobra.Command, target upgrade.Target) (upgrade.Environment, upgrade.DatabasePlan, error) {
	database, err := a.upgradeDatabasePlan(cmd)
	if err != nil {
		return upgrade.Environment{}, upgrade.DatabasePlan{}, err
	}
	version := buildinfo.Current().Version
	if target == upgrade.TargetNode {
		version = ""
	}
	if cmd.Flags().Changed("current-version") {
		version = upgradeFlag(cmd, "current-version")
	}
	return upgrade.Environment{
		Target: target, OS: runtime.GOOS, Arch: runtime.GOARCH, Version: version,
		Contract: contract.Schema, NodeProtocol: node.ProtocolVersion, SchemaVersion: database.Current,
	}, database, nil
}

// upgradeDatabasePlan reads the current schema through the same read-only
// migration plan that `system migrate --dry-run` uses. It never applies DDL.
func (a *app) upgradeDatabasePlan(cmd *cobra.Command) (upgrade.DatabasePlan, error) {
	if cmd.Flags().Lookup("schema-version") == nil {
		return upgrade.DatabasePlan{}, nil
	}
	value, _ := cmd.Flags().GetInt("schema-version")
	if cmd.Flags().Changed("schema-version") {
		if value < 1 {
			return upgrade.DatabasePlan{}, contract.NewFault(contract.ValidationFailed, "--schema-version must be positive", "Pass the current eregion database schema version.")
		}
		return upgrade.DatabasePlan{Current: value}, nil
	}
	if os.Getenv("EREGION_DATABASE_URL") == "" {
		return upgrade.DatabasePlan{}, contract.NewFault(contract.InputRequired, "current database schema version is required", "Pass --schema-version N or set EREGION_DATABASE_URL so migrations can be inspected without applying them.")
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Second)
	defer cancel()
	pool, err := a.database(ctx)
	if err != nil {
		return upgrade.DatabasePlan{}, err
	}
	defer pool.Close()
	plan, err := controlplane.PlanMigrations(ctx, pool)
	if err != nil {
		return upgrade.DatabasePlan{}, err
	}
	return upgrade.DatabasePlan{
		Current: plan.Current, Required: plan.Required,
		RiverCurrent: plan.RiverCurrent, RiverRequired: plan.RiverRequired,
	}, nil
}

func upgradeBinaryPath(cmd *cobra.Command, target upgrade.Target) (string, error) {
	path := upgradeFlag(cmd, "binary")
	if path == "" {
		executable, err := os.Executable()
		if err != nil {
			return "", contract.NewFault(contract.Unavailable, "this executable's path could not be determined", "Pass --binary with the installed operator or node binary.")
		}
		// Operator and node targets are the same eregion binary.
		path, _ = filepath.EvalSymlinks(executable)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", contract.NewFault(contract.ValidationFailed, "target binary path is invalid", "Pass the installed binary path with --binary.")
	}
	return absolute, nil
}

func (a *app) upgradeManifest(cmd *cobra.Command) (upgrade.Manifest, ed25519.PublicKey, error) {
	source := upgradeFlag(cmd, "manifest")
	if source == "" {
		return upgrade.Manifest{}, nil, contract.NewFault(contract.InputRequired, "--manifest is required", "Pass the signed manifest path or its direct HTTPS URL.")
	}
	public, err := a.upgradePublicKey(cmd)
	if err != nil {
		return upgrade.Manifest{}, nil, err
	}
	var raw []byte
	if strings.HasPrefix(source, "https://") {
		raw, err = upgrade.NewDownloader(upgradeHosts(os.Getenv("EREGION_UPGRADE_ALLOWED_HOSTS"))).FetchManifest(cmd.Context(), source)
	} else {
		if strings.Contains(source, "://") {
			return upgrade.Manifest{}, nil, contract.NewFault(contract.ValidationFailed, "manifest URL must use HTTPS", "Pass a local file path or a direct HTTPS URL; plaintext and redirecting URLs are refused.")
		}
		info, statErr := os.Lstat(source)
		if statErr != nil || !info.Mode().IsRegular() {
			return upgrade.Manifest{}, nil, contract.NewFault(contract.NotFound, "signed manifest file could not be read", "Pass a regular manifest file or its direct HTTPS URL.")
		}
		if info.Size() > upgrade.MaxManifestBytes {
			return upgrade.Manifest{}, nil, contract.NewFault(contract.PreconditionFailed, "signed manifest exceeds the size bound", "Refuse the file; it is larger than a release manifest.")
		}
		raw, err = os.ReadFile(source) //nolint:gosec // G304: operator-selected manifest path.
	}
	if err != nil {
		return upgrade.Manifest{}, nil, err
	}
	var manifest upgrade.Manifest
	manifest, err = upgrade.ParseManifest(raw)
	if err != nil {
		return upgrade.Manifest{}, nil, err
	}
	if err := manifest.Verify(public); err != nil {
		return upgrade.Manifest{}, nil, err
	}
	return manifest, public, nil
}

func (a *app) upgradePublicKey(cmd *cobra.Command) (ed25519.PublicKey, error) {
	path := upgradeFlag(cmd, "public-key-file")
	if path == "" {
		path = os.Getenv("EREGION_UPGRADE_PUBLIC_KEY_FILE")
	}
	if path != "" {
		return upgrade.LoadPublicKey(path)
	}
	inline := os.Getenv("EREGION_UPGRADE_PUBLIC_KEY")
	if inline == "" {
		return nil, contract.NewFault(contract.InputRequired, "trusted release public key is not configured", "Set EREGION_UPGRADE_PUBLIC_KEY_FILE (preferred) or EREGION_UPGRADE_PUBLIC_KEY.")
	}
	return upgrade.ParsePublicKey(inline)
}

func (a *app) upgradeCheck(cmd *cobra.Command, _ []string) (result, error) {
	manifest, _, err := a.upgradeManifest(cmd)
	if err != nil {
		return result{}, err
	}
	options, err := a.upgradeOptions(cmd, manifest)
	if err != nil {
		return result{}, err
	}
	plan, err := options.manager.Prepare(manifest)
	if err != nil {
		return result{}, err
	}
	return result{data: plan, plain: renderUpgradePlan(plan)}, nil
}

func (a *app) upgradeVerify(cmd *cobra.Command, _ []string) (result, error) {
	manifest, _, err := a.upgradeManifest(cmd)
	if err != nil {
		return result{}, err
	}
	type artifact struct {
		Name   string         `json:"name"`
		Target upgrade.Target `json:"target"`
		OS     string         `json:"os"`
		Arch   string         `json:"arch"`
		Size   int64          `json:"size"`
		SHA256 string         `json:"sha256"`
	}
	verified := struct {
		Schema        string     `json:"schema"`
		Version       string     `json:"version"`
		Channel       string     `json:"channel"`
		PublishedAt   string     `json:"published_at"`
		Contract      string     `json:"contract"`
		NodeProtocol  int        `json:"node_protocol"`
		SchemaVersion int        `json:"schema_version"`
		UpgradeFrom   []string   `json:"upgrade_from,omitempty"`
		Artifacts     []artifact `json:"artifacts"`
	}{
		Schema: upgrade.ManifestSchema, Version: manifest.Version, Channel: manifest.Channel,
		PublishedAt: manifest.PublishedAt, Contract: manifest.Contract,
		NodeProtocol: manifest.NodeProtocol, SchemaVersion: manifest.SchemaVersion,
		UpgradeFrom: manifest.UpgradeFrom, Artifacts: make([]artifact, 0, len(manifest.Artifacts)),
	}
	for _, item := range manifest.Artifacts {
		verified.Artifacts = append(verified.Artifacts, artifact{
			Name: item.Name, Target: item.Target, OS: item.OS, Arch: item.Arch, Size: item.Size, SHA256: item.SHA256,
		})
	}
	var plain strings.Builder
	fmt.Fprintf(&plain, "Verified signed release %s (%s)\ncontract: %s\nnode protocol: %d\ndatabase schema: %d\npublished: %s\nartifacts:\n", manifest.Version, manifest.Channel, manifest.Contract, manifest.NodeProtocol, manifest.SchemaVersion, manifest.PublishedAt)
	for _, item := range verified.Artifacts {
		fmt.Fprintf(&plain, "  %s %s/%s %s %s\n", item.Target, item.OS, item.Arch, upgradeSize(item.Size), item.SHA256)
	}
	return result{data: verified, plain: plain.String()}, nil
}

func (a *app) upgradePlan(cmd *cobra.Command, _ []string) (result, error) {
	manifest, _, err := a.upgradeManifest(cmd)
	if err != nil {
		return result{}, err
	}
	options, err := a.upgradeOptions(cmd, manifest)
	if err != nil {
		return result{}, err
	}
	if options.target != upgrade.TargetOperator {
		return result{}, contract.NewFault(contract.ValidationFailed, "coordinated rollout planning applies to the operator target", "Use --target operator; node artifacts are rolled last in the same plan.")
	}
	inventory, err := a.upgradeInventory(upgradeFlag(cmd, "inventory"))
	if err != nil {
		return result{}, err
	}
	plan, err := upgrade.BuildRolloutPlan(manifest, options.manager.Env, options.database, inventory)
	if err != nil {
		return result{}, err
	}
	return result{data: plan, plain: upgrade.RenderRolloutPlan(plan)}, nil
}

func (a *app) upgradeInventory(path string) ([]upgrade.ControlPlaneInstance, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return nil, contract.NewFault(contract.NotFound, "control-plane inventory file could not be read", "Pass a regular JSON file or omit --inventory.")
	}
	if info.Size() > upgrade.MaxManifestBytes {
		return nil, contract.NewFault(contract.PreconditionFailed, "control-plane inventory file is too large", "Keep the inventory under 1 MiB.")
	}
	raw, err := os.ReadFile(path) //nolint:gosec // G304: operator-selected inventory path.
	if err != nil {
		return nil, contract.NewFault(contract.NotFound, "control-plane inventory file could not be read", "Pass a regular JSON file or omit --inventory.")
	}
	var inventory []upgrade.ControlPlaneInstance
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&inventory); err != nil {
		return nil, contract.NewFault(contract.ValidationFailed, "control-plane inventory is not a strict JSON array", "Use id, version, contract, node_protocol, and schema_version fields only.")
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return nil, contract.NewFault(contract.ValidationFailed, "control-plane inventory has trailing JSON", "Use exactly one JSON array.")
	}
	if err := upgrade.ValidateRolloutInventory(inventory); err != nil {
		return nil, err
	}
	return inventory, nil
}

func (a *app) upgradeApply(cmd *cobra.Command, _ []string) (result, error) {
	manifest, _, err := a.upgradeManifest(cmd)
	if err != nil {
		return result{}, err
	}
	options, err := a.upgradeOptions(cmd, manifest)
	if err != nil {
		return result{}, err
	}
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	if dryRun {
		plan, err := options.manager.Prepare(manifest)
		if err != nil {
			return result{}, err
		}
		return result{data: plan, plain: renderUpgradePlan(plan)}, nil
	}
	healthCheck, _ := cmd.Flags().GetBool("health-check")
	if healthCheck {
		timeout, _ := cmd.Flags().GetDuration("health-timeout")
		options.manager.HealthProbe = upgrade.NewHealthProbe(upgrade.ProbeConfig{
			Target: options.target, Manifest: manifest,
			DatabaseURL: os.Getenv("EREGION_DATABASE_URL"), Timeout: timeout,
		})
		options.manager.HealthTimeout = timeout
	}
	confirm, _ := cmd.Flags().GetString("confirm")
	applied, err := options.manager.Apply(cmd.Context(), manifest, confirm)
	if err != nil {
		return result{}, err
	}
	journal := applied.Journal
	plain := fmt.Sprintf("Installed %s %s at %s.\nprevious binary: %s\nbackup: %s\njournal: %s\n", options.target, journal.ToVersion, journal.BinaryPath, journal.FromVersion, journal.BackupPath, journal.OperationID)
	if applied.HealthCheck != "" {
		plain += "health: " + applied.HealthCheck + "\n"
	} else {
		plain += "health: not checked (--health-check=false)\n"
	}
	plain += "Restart the service to use the new binary; rollback is explicit."
	return result{data: applied, plain: plain}, nil
}

func (a *app) upgradeRollback(cmd *cobra.Command, _ []string) (result, error) {
	options, err := a.upgradeOptions(cmd, upgrade.Manifest{})
	if err != nil {
		return result{}, err
	}
	backup, _ := cmd.Flags().GetString("backup")
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	if dryRun {
		plan, err := options.manager.PrepareRollback(backup)
		if err != nil {
			return result{}, err
		}
		return result{data: plan, plain: renderRollbackPlan(plan)}, nil
	}
	confirm, _ := cmd.Flags().GetString("confirm")
	applied, err := options.manager.ApplyRollback(cmd.Context(), backup, confirm)
	if err != nil {
		return result{}, err
	}
	journal := applied.Journal
	plain := fmt.Sprintf("Rolled back %s from %s to %s.\nbackup used: %s\nsafety copy: %s\njournal: %s\nRestart the service to use the restored binary.", options.target, journal.FromVersion, journal.ToVersion, journal.SourcePath, journal.BackupPath, journal.OperationID)
	return result{data: applied, plain: plain}, nil
}

func (a *app) upgradeStatus(cmd *cobra.Command, _ []string) (result, error) {
	target, err := upgrade.ParseTarget(upgradeFlag(cmd, "target"))
	if err != nil {
		return result{}, err
	}
	stateDir := upgradeFlag(cmd, "state-dir")
	if stateDir == "" {
		stateDir = upgrade.DefaultStateDir()
	}
	stateDir, err = filepath.Abs(stateDir)
	if err != nil {
		return result{}, contract.NewFault(contract.ValidationFailed, "upgrade state directory is invalid", "Pass an absolute path with --state-dir.")
	}
	binary, err := upgradeBinaryPath(cmd, target)
	if err != nil {
		return result{}, err
	}
	info := buildinfo.Current()
	manager := &upgrade.Manager{
		StateDir: stateDir, Target: target, BinaryPath: binary,
		Env: upgrade.Environment{Target: target, OS: runtime.GOOS, Arch: runtime.GOARCH, Version: info.Version, Contract: contract.Schema, NodeProtocol: node.ProtocolVersion},
	}
	status, err := manager.Status(cmd.Context())
	if err != nil {
		return result{}, err
	}
	var plain strings.Builder
	version := status.Version
	if version == "" {
		version = "unknown (no matching journal entry)"
	}
	fmt.Fprintf(&plain, "%s binary: %s\nversion: %s\nsha256: %s\nstate: %s\n", target, status.BinaryPath, version, status.SHA256, status.StateDir)
	if status.ProcessVersion != "" && status.ProcessVersion != status.Version {
		fmt.Fprintf(&plain, "running this command: %s\n", status.ProcessVersion)
	}
	if status.Journal != nil {
		fmt.Fprintf(&plain, "journal: %s (%s)\n", status.Journal.Status, status.Journal.OperationID)
	}
	if status.Interrupted {
		fmt.Fprintf(&plain, "interrupted: %s\n", status.RecoveryDetail)
	}
	fmt.Fprintf(&plain, "backups: %d\n", len(status.Backups))
	for _, backup := range status.Backups {
		fmt.Fprintf(&plain, "  %s  %s  %s\n", backup.Version, backup.SHA256, backup.Path)
	}
	return result{data: status, plain: plain.String()}, nil
}

func (a *app) upgradeGuide(cmd *cobra.Command, _ []string) (result, error) {
	format, _ := cmd.Flags().GetString("format")
	if format != "markdown" && format != "json" {
		return result{}, contract.NewFault(contract.ValidationFailed, "--format must be markdown or json", "Use --format markdown or --format json.")
	}
	guide := a.currentUpgradeGuide()
	if format == "json" {
		raw, err := json.MarshalIndent(guide, "", "  ")
		if err != nil {
			return result{}, contract.NewFault(contract.Internal, "agent guide could not be rendered", "Report the command and binary version.")
		}
		return result{data: guide, plain: string(raw)}, nil
	}
	return result{data: guide, plain: upgrade.RenderMarkdown(guide)}, nil
}

func (a *app) currentUpgradeGuide() upgrade.Guide {
	catalog := a.meta.catalog(a.root)
	commands := make([]upgrade.GuideCommand, 0, len(catalog))
	for _, command := range catalog {
		codes := make([]string, 0, len(command.ErrorCodes))
		for _, code := range command.ErrorCodes {
			codes = append(codes, string(code))
		}
		commands = append(commands, upgrade.GuideCommand{
			Path: command.Path, Summary: command.Summary, Mutating: command.Mutating,
			Destructive: command.Destructive, DryRun: command.DryRun, Prompts: command.Prompts,
			Remote: command.Remote, InputFlags: command.InputFlags, Examples: command.Examples,
			Next: command.Next, ErrorCodes: codes,
		})
	}
	return upgrade.NewGuide(buildinfo.Current().Version, contract.Schema, node.ProtocolVersion, commands)
}

func renderUpgradePlan(plan upgrade.Plan) string {
	var out strings.Builder
	fmt.Fprintf(&out, "Upgrade plan (%s)\n\ntarget: %s\nbinary: %s\nstate: %s\n", plan.Direction, plan.Target, plan.BinaryPath, plan.StateDir)
	fmt.Fprintf(&out, "current: %s (%s)\nnext: %s\nartifact: %s (%s, sha256 %s)\n", plan.CurrentVersion, plan.CurrentSHA256, plan.NextVersion, plan.ArtifactName, upgradeSize(plan.ArtifactSize), plan.ArtifactSHA256)
	fmt.Fprintf(&out, "artifact source: %s\n\nChanges:\n", plan.ArtifactSource)
	for _, change := range plan.Changes {
		fmt.Fprintf(&out, "  - %s\n", change)
	}
	out.WriteString("\nCompatibility:\n")
	for _, check := range plan.Compatibility.Checks {
		fmt.Fprintf(&out, "  [%s] %s: %s\n", upgradeVerdict(check.OK), check.Name, check.Detail)
	}
	fmt.Fprintf(&out, "\nconfirmation: %s\n\nApply with:\n  eregion upgrade apply --manifest <manifest> --target %s --confirm %s\n", plan.Confirmation, plan.Target, plan.Confirmation)
	return out.String()
}

func renderRollbackPlan(plan upgrade.RollbackPlan) string {
	var out strings.Builder
	fmt.Fprintf(&out, "Rollback plan (%s)\n\ntarget: %s\nbinary: %s\nstate: %s\n", plan.Direction, plan.Target, plan.BinaryPath, plan.StateDir)
	fmt.Fprintf(&out, "current: %s (%s)\nrestore: %s (%s) from %s\nsafety copy: %s\n\nChanges:\n", plan.CurrentVersion, plan.CurrentSHA256, plan.RestoreVersion, plan.RestoreSHA256, plan.RestorePath, plan.SafetyBackup)
	for _, change := range plan.Changes {
		fmt.Fprintf(&out, "  - %s\n", change)
	}
	fmt.Fprintf(&out, "\nconfirmation: %s\n\nApply with:\n  eregion upgrade rollback --target %s --confirm %s\n", plan.Confirmation, plan.Target, plan.Confirmation)
	return out.String()
}

func upgradeVerdict(ok bool) string {
	if ok {
		return "ok"
	}
	return "FAIL"
}

func upgradeSize(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%d B", size)
	}
	div, exp := int64(unit), 0
	for n := size / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(size)/float64(div), "KMGTPE"[exp])
}

func upgradeHosts(value string) []string {
	var hosts []string
	for _, field := range strings.Split(value, ",") {
		if field = strings.TrimSpace(field); field != "" {
			hosts = append(hosts, field)
		}
	}
	return hosts
}

func upgradeFlag(cmd *cobra.Command, name string) string {
	value, _ := cmd.Flags().GetString(name)
	return value
}
