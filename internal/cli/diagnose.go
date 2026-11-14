package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/spf13/cobra"
)

type diagnosisBackend interface {
	Diagnose(context.Context, contract.OperationID, uint64, int) (json.RawMessage, error)
}

func (a *app) diagnoseSpecs() []spec {
	meta := operationMeta("Inspect recorded operation evidence", "eregion diagnose <operation_id> --json")
	meta.WhenToUse = []string{"Read bounded recorded evidence. Node auto-upgrade is never performed.", "Use --after with a returned cursor to continue a partial event page. Missing observations are not negative findings."}
	meta.Examples = append(meta.Examples, "eregion diagnose <operation_id> --after 1000 --limit 1000 --json")
	meta.Next = []string{"eregion operations get <operation_id> --json"}
	return []spec{{use: "diagnose <operation_id>", meta: meta, args: cobra.ArbitraryArgs, flags: func(cmd *cobra.Command) {
		cmd.Flags().Uint64("after", 0, "Resume after this event cursor")
		cmd.Flags().Int("limit", 1000, "Maximum events (1–20000; also bounded by bytes)")
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
	if len(args) != 1 || cmd.ArgsLenAtDash() >= 0 {
		return result{}, contract.NewFault(contract.Usage, "diagnosis accepts only one operation ID", "Use eregion diagnose <operation_id>.")
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
		for _, observation := range report.LiveCandidate.Observations {
			fmt.Fprintf(&plain, "  %s/%s state=%s running=%t oom=%t health=%s observed=%s", observation.Process, observation.Replica, observation.State, observation.Running, observation.OOMKilled, observation.Health, observation.ObservedAt.Format(time.RFC3339))
			if observation.ExitCode != nil {
				fmt.Fprintf(&plain, " exit=%d", *observation.ExitCode)
			}
			fmt.Fprintln(&plain)
		}
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
	fmt.Fprintf(&plain, "Event cursor: %d; more history: %t; history unavailable: %t\n", report.Cursor.After, report.HasMore, report.HistoryUnavailable)
	if report.HasMore {
		fmt.Fprintf(&plain, "Continue: eregion diagnose %s --after %d\n", id, report.Cursor.After)
	}
	for _, reason := range report.Unavailable {
		fmt.Fprintf(&plain, "Unavailable: %s\n", reason)
	}
	return result{data: report, plain: plain.String()}, nil
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
