package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/spf13/cobra"
)

type diagnosisBackend interface {
	Diagnose(context.Context, contract.OperationID, uint64, int) (json.RawMessage, error)
}

type diagnosticExecutionBackend interface {
	DiagnosticExecution(context.Context, contract.OperationID, []string, string, string, bool) (json.RawMessage, error)
}

func (a *app) diagnoseSpecs() []spec {
	meta := followingMeta(operationMeta("Inspect an operation or explicitly run an isolated diagnostic", "eregion diagnose <operation_id> --json"))
	meta.MutatingFlags, meta.DestructiveFlags = []string{"execute"}, []string{"execute"}
	meta.DryRun, meta.Confirmation = true, "target"
	meta.ErrorCodes = append(meta.ErrorCodes, contract.ConfirmationRequired)
	meta.WhenToUse = []string{"Read bounded recorded evidence by default. Node auto-upgrade is never performed.", "Only --execute submits a separate isolated diagnostic against the original deployment's pinned local image. Supply exact --confirm and non-secret argv after --; command output is withheld. Use --dry-run for a read-only plan.", "Use --after with a returned cursor to continue a partial event page. Missing observations are not negative findings."}
	meta.Examples = append(meta.Examples, "eregion diagnose <operation_id> --after 1000 --limit 1000 --json", "eregion diagnose <operation_id> --execute --confirm <operation_id> --dry-run -- /app/check")
	meta.Next = []string{"eregion operations get <operation_id> --json"}
	return []spec{{use: "diagnose <operation_id> [-- /absolute/executable args...]", meta: meta, args: cobra.ArbitraryArgs, flags: func(cmd *cobra.Command) {
		cmd.Flags().Uint64("after", 0, "Resume after this event cursor")
		cmd.Flags().Int("limit", 1000, "Maximum events (1–20000; also bounded by bytes)")
		cmd.Flags().Bool("execute", false, "Submit an isolated diagnostic using the original operation's pinned image")
		cmd.Flags().String("confirm", "", "Exact original operation ID; required for execution and preview")
		addDeliveryFlags(cmd)
	}, run: a.diagnose}}
}

func (a *app) diagnose(cmd *cobra.Command, args []string) (result, error) {
	if boolFlag(cmd, "auto-upgrade-nodes") {
		return result{}, contract.NewFault(contract.Usage, "diagnosis cannot upgrade nodes", "Upgrade separately before reading diagnosis.")
	}
	if len(args) == 0 {
		return result{}, contract.NewFault(contract.Usage, "diagnosis needs an operation ID", "Use eregion diagnose <operation_id>.")
	}
	id := contract.OperationID(args[0])
	if err := id.Validate(); err != nil {
		return result{}, err
	}
	if boolFlag(cmd, "execute") {
		return a.diagnoseExecute(cmd, id, args)
	}
	if len(args) != 1 || cmd.ArgsLenAtDash() >= 0 {
		return result{}, contract.NewFault(contract.Usage, "diagnostic command arguments require --execute", "Use --execute --confirm <operation_id> -- /absolute/executable args.")
	}
	for _, name := range []string{"confirm", "dry-run", "idempotency-key", "detach", "poll-interval"} {
		if cmd.Flags().Changed(name) {
			return result{}, contract.NewFault(contract.Usage, "execution flags require --execute", "Omit execution flags for read-only diagnosis.")
		}
	}
	if streamsNDJSON(cmd) {
		return result{}, contract.NewFault(contract.Usage, "read-only diagnosis does not stream NDJSON", "Use --json for a bounded diagnosis page.")
	}
	after, _ := cmd.Flags().GetUint64("after")
	limit, _ := cmd.Flags().GetInt("limit")
	if limit < 1 || limit > 20000 {
		return result{}, contract.NewFault(contract.ValidationFailed, "diagnosis event limit out of range", "Use a limit from 1 to 20000.")
	}
	client, err := a.client(cmd)
	if err != nil {
		return result{}, err
	}
	// A workspace may have auto-upgrade enabled even without the flag. Reads
	// must never install a binary as a side effect of diagnosis.
	if node, ok := client.(*nodeClient); ok {
		node.autoUpgrade = false
	}
	backend, ok := client.(diagnosisBackend)
	if !ok {
		return result{}, contract.NewFault(contract.Unavailable, "diagnosis is not configured", "Use a node with diagnosis support.")
	}
	raw, err := backend.Diagnose(cmd.Context(), id, after, limit)
	if err != nil {
		return result{}, err
	}
	var report platform.Diagnosis
	if json.Unmarshal(raw, &report) != nil || report.Schema != "eregion/diagnosis/v1" || report.Operation.ID != id {
		return result{}, invalidNodeData("diagnosis")
	}
	var plain strings.Builder
	fmt.Fprintf(&plain, "Operation %s: %s (%s)\n", id, report.Operation.State, report.Operation.Kind)
	if report.Operation.ErrorCode != "" {
		fmt.Fprintf(&plain, "Error code: %s\n", report.Operation.ErrorCode)
	}
	if report.Build != nil {
		fmt.Fprintf(&plain, "Build: %s, commit %s, %s\n", report.Build.ID, report.Build.CommitSHA, report.Build.State)
	}
	if report.SourceBuild != nil {
		fmt.Fprintf(&plain, "Release source build: %s, operation %s, commit %s\n", report.SourceBuild.ID, report.SourceBuild.OperationID, report.SourceBuild.CommitSHA)
	}
	if report.Release != nil {
		fmt.Fprintf(&plain, "Release: %s, image reference %s, local image ID %s, pruned=%t\n", report.Release.ID, report.Release.ImageDigest, report.Release.ImageID, report.Release.Pruned)
	}
	if report.Deployment != nil {
		fmt.Fprintf(&plain, "Deployment: %s, revision %s, %s\n", report.Deployment.ID, report.Deployment.RevisionID, report.Deployment.State)
	}
	if report.Failure != nil {
		fmt.Fprintf(&plain, "Observed failure: %s\n", report.Failure.Message)
		if report.Failure.Stage != "" {
			fmt.Fprintf(&plain, "Observed failed stage: %s (%s): %s\n", report.Failure.Stage, report.Failure.StageSource, report.Failure.StepMessage)
		}
	}
	writeDiagnosisRevision(&plain, "Historical operation revision", report.Historical)
	writeDiagnosisRevision(&plain, "Build revision", report.BuildRevision)
	writeDiagnosisRevision(&plain, "Current desired revision", report.Current)
	if report.RecordedBuild != nil {
		plain.WriteString("Recorded release build settings:\n")
		writeDiagnosisBuild(&plain, *report.RecordedBuild)
	}
	if report.Image != nil {
		fmt.Fprintf(&plain, "Image observation: %s; metadata labels %d; declared layers %d\n", report.Image.ImageID, report.Image.MetadataLabelsPresent, report.Image.DeclaredLayerCount)
		for _, bp := range report.Image.Buildpacks {
			fmt.Fprintf(&plain, "  buildpack %s %s\n", bp.ID, bp.Version)
		}
		for _, layer := range report.Image.Layers {
			fmt.Fprintf(&plain, "  declared layer %s/%s\n", layer.Buildpack, layer.Name)
		}
		fmt.Fprintf(&plain, "  process types: %s\n  %s\n", strings.Join(report.Image.ProcessTypes, ", "), report.Image.Note)
	}
	if report.LiveCandidate != nil {
		fmt.Fprintf(&plain, "Live candidate (truncated=%t unavailable=%t):\n", report.LiveCandidate.Truncated, report.LiveCandidate.Unavailable)
		for _, line := range report.LiveCandidate.Lines {
			fmt.Fprintf(&plain, "  %s/%s %s: %s\n", line.Process, line.Replica, line.Timestamp.Format("2006-01-02T15:04:05Z07:00"), line.Message)
		}
	}
	for _, event := range report.Events {
		if event.Message != "" {
			if event.CandidateDiagnostic != nil {
				fmt.Fprintf(&plain, "captured candidate %s/%s [%s]: %s\n", event.CandidateDiagnostic.Process, event.CandidateDiagnostic.Replica, event.CandidateDiagnostic.Status, event.Message)
			} else {
				fmt.Fprintf(&plain, "%s %s: %s\n", event.Kind, event.Stage, event.Message)
			}
		}
	}
	fmt.Fprintf(&plain, "Event cursor: %d; partial: %t\n", report.Cursor.After, report.Partial)
	if report.Partial {
		fmt.Fprintf(&plain, "Continue: eregion diagnose %s --after %d\n", id, report.Cursor.After)
	}
	for _, reason := range report.Unavailable {
		fmt.Fprintf(&plain, "Unavailable: %s\n", reason)
	}
	return result{data: report, plain: plain.String()}, nil
}

func (a *app) diagnoseExecute(cmd *cobra.Command, id contract.OperationID, args []string) (result, error) {
	if cmd.ArgsLenAtDash() != 1 || len(args) < 2 {
		return result{}, contract.NewFault(contract.Usage, "diagnostic execution needs argv after --", "Use diagnose <operation_id> --execute --confirm <operation_id> -- /absolute/executable args.")
	}
	if cmd.Flags().Changed("after") || cmd.Flags().Changed("limit") {
		return result{}, contract.NewFault(contract.Usage, "event paging flags are read-only", "Omit --after and --limit when executing a diagnostic.")
	}
	argv := args[1:]
	if err := platform.ValidateDiagnosticExecutionArgv(argv); err != nil {
		return result{}, err
	}
	confirm := stringFlag(cmd, "confirm")
	if confirm != string(id) {
		return result{}, contract.NewFault(contract.ConfirmationRequired, "diagnostic execution requires exact operation confirmation", "Pass --confirm with the original operation ID; --yes does not replace it.")
	}
	key := stringFlag(cmd, "idempotency-key")
	if key == "" {
		key = "cli:diagnostic:" + randomHex(8)
	}
	if err := contract.ValidateIdempotencyKey(key); err != nil {
		return result{}, err
	}
	dryRun := boolFlag(cmd, "dry-run")
	if dryRun && streamsNDJSON(cmd) {
		return result{}, contract.NewFault(contract.Usage, "diagnostic preview does not stream NDJSON", "Use --dry-run --json for one read-only plan.")
	}
	follow := !dryRun && !boolFlag(cmd, "detach") && (followsByDefault(cmd) || streamsNDJSON(cmd))
	if err := validateFollowRequest(cmd, follow); err != nil {
		return result{}, err
	}
	client, err := a.client(cmd)
	if err != nil {
		return result{}, err
	}
	if node, ok := client.(*nodeClient); ok {
		node.autoUpgrade = false
	}
	backend, ok := client.(diagnosticExecutionBackend)
	if !ok {
		return result{}, contract.NewFault(contract.Unavailable, "diagnostic execution is not configured", "Use a node with diagnostic execution support.")
	}
	raw, err := backend.DiagnosticExecution(cmd.Context(), id, argv, key, confirm, dryRun)
	if err != nil {
		return result{}, err
	}
	if dryRun {
		var preview platform.DiagnosticExecutionPreview
		if json.Unmarshal(raw, &preview) != nil || !preview.DryRun || preview.OriginalOperationID != id {
			return result{}, invalidNodeData("diagnostic preview")
		}
		plain := fmt.Sprintf("Dry run: diagnostic for %s\nnode: %s\nimage ID: %s\narguments: %d\n%s", id, preview.Node, preview.ImageID, preview.ArgCount, preview.Note)
		for _, constraint := range preview.Sandbox {
			plain += "\nsandbox: " + constraint
		}
		if preview.Replay != nil {
			plain += fmt.Sprintf("\nreplay operation: %s", preview.Replay.ID)
		}
		return result{data: preview, plain: plain}, nil
	}
	var receipt struct {
		Operation contract.Operation `json:"operation"`
		Replay    bool               `json:"replay"`
	}
	if json.Unmarshal(raw, &receipt) != nil || receipt.Operation.Validate() != nil || receipt.Operation.Kind != platform.KindDiagnosticExecution || receipt.Operation.Target != "operation/"+string(id) {
		return result{}, invalidNodeData("accepted diagnostic operation")
	}
	if !follow {
		return result{data: receipt.Operation, plain: fmt.Sprintf("Accepted diagnostic for %s\noperation: %s\nstate: %s\nCommand output is withheld.\nnext: eregion operations follow %s --output ndjson", id, receipt.Operation.ID, receipt.Operation.State, receipt.Operation.ID), next: []string{"eregion operations get " + string(receipt.Operation.ID) + " --json", "eregion operations follow " + string(receipt.Operation.ID) + " --output ndjson"}}, nil
	}
	if plainOutput(cmd) {
		if _, err := fmt.Fprintf(followConfig(cmd).out, "Accepted diagnostic operation: %s (output withheld)\n", receipt.Operation.ID); err != nil {
			return result{}, contract.NewFault(contract.Internal, "could not write command output", "Use operations follow with the accepted operation ID.")
		}
	}
	followed, err := a.followAccepted(cmd, client, receipt.Operation)
	followed.next = []string{"eregion operations get " + string(receipt.Operation.ID) + " --json", "eregion operations follow " + string(receipt.Operation.ID) + " --output ndjson"}
	return followed, err
}

func writeDiagnosisRevision(out *strings.Builder, label string, r *platform.DiagnosisRevision) {
	if r == nil {
		return
	}
	fmt.Fprintf(out, "%s: %s (#%d), node %s\n", label, r.ID, r.Number, r.Node)
	for _, p := range r.Processes {
		fmt.Fprintf(out, "  %s (%s) replicas=%d port=%d command_omitted=%t\n", p.Name, p.Kind, p.Replicas, p.ContainerPort, p.CommandOmitted)
	}
	writeDiagnosisBuild(out, r.Build)
}

func writeDiagnosisBuild(out *strings.Builder, b platform.DiagnosisBuildSettings) {
	fmt.Fprintf(out, "  builder=%s context=%s order=%s\n", b.Builder, b.ContextPath, strings.Join(b.Order, ", "))
	keys := make([]string, 0, len(b.Effective))
	for key := range b.Effective {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fmt.Fprintf(out, "  %s=%s (typed)\n", key, b.Effective[key])
	}
	fmt.Fprintf(out, "  raw build environment present=%t, Go flags present=%t; %s\n", b.RawBuildEnvPresent, b.GoBuildFlagsPresent, b.OmissionReason)
	if len(b.RawOverridesOmitted) > 0 {
		fmt.Fprintf(out, "  raw effective values omitted: %s\n", strings.Join(b.RawOverridesOmitted, ", "))
	}
}
