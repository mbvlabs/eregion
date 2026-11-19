package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/output"
	"github.com/mbvlabs/eregion/internal/rpc"
	"github.com/spf13/cobra"
)

func (a *app) buildSlotsSpec() spec {
	group := deliveryReadMeta("Inspect build capacity reservations", "eregion build slots list --json")
	group.WhenToUse = []string{"Inspect reservations or retry cleanup of a terminal operation on its pinned builder."}
	list := operationMeta("List build capacity reservations", "eregion build slots list --json")
	clear := operationMeta("Retry safe cleanup of a terminal build reservation", "eregion build slots clear op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa --dry-run --json")
	clear.Mutating, clear.DryRun = true, true
	clear.WhenToUse = []string{"Preview without contacting the builder or writing. Use --legacy-local only for a legacy unverified local claim; cleanup never forces release."}
	return spec{use: "slots", meta: group,
		run: func(cmd *cobra.Command, _ []string) (result, error) { return a.help(cmd), nil },
		children: []spec{
			{use: "list", meta: list, args: cobra.NoArgs, run: a.buildSlotsList},
			{use: "clear <operation-id>", meta: clear, args: func(_ *cobra.Command, args []string) error {
				if len(args) != 1 {
					return contract.NewFault(contract.InputRequired, "one operation ID is required", "Pass the operation ID from build slots list.")
				}
				return contract.OperationID(args[0]).Validate()
			}, flags: func(cmd *cobra.Command) {
				cmd.Flags().Bool("dry-run", false, "Preview without contacting the builder or changing anything")
				cmd.Flags().Bool("legacy-local", false, "Prove retirement of a legacy unverified local claim")
			}, run: a.buildSlotsClear},
		}}
}

func (a *app) buildSlotsList(cmd *cobra.Command, _ []string) (result, error) {
	client, err := a.nodeClient(cmd)
	if err != nil {
		return result{}, err
	}
	raw, err := client.call(cmd.Context(), methodBuildSlotsList, struct{}{}, rpcCallOptions{})
	if err != nil {
		return result{}, err
	}
	var slots []api.BuildSlot
	if json.Unmarshal(raw, &slots) != nil || slots == nil {
		return result{}, invalidNodeData("build slots")
	}
	var out strings.Builder
	for _, slot := range slots {
		fmt.Fprintf(&out, "%s  %s  age=%ds  state=%s", output.SafeText(string(slot.OperationID)), output.SafeText(slot.Builder), slot.AgeSeconds, output.SafeText(slot.State))
		if slot.Reason != "" {
			fmt.Fprintf(&out, "  reason=%s", output.SafeText(slot.Reason))
		}
		out.WriteByte('\n')
	}
	if len(slots) == 0 {
		out.WriteString("No build slots reserved.\n")
	}
	return result{data: slots, plain: out.String()}, nil
}

func (a *app) buildSlotsClear(cmd *cobra.Command, args []string) (result, error) {
	client, err := a.nodeClient(cmd)
	if err != nil {
		return result{}, err
	}
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	legacyLocal, _ := cmd.Flags().GetBool("legacy-local")
	raw, err := client.call(cmd.Context(), methodBuildSlotsClear, struct {
		OperationID contract.OperationID `json:"operation_id"`
		LegacyLocal bool                 `json:"legacy_local,omitempty"`
	}{contract.OperationID(args[0]), legacyLocal}, rpcCallOptions{DryRun: dryRun})
	if err != nil {
		return result{}, err
	}
	var view api.BuildSlotClearResult
	if json.Unmarshal(raw, &view) != nil || view.Slot.OperationID != contract.OperationID(args[0]) || view.DryRun != dryRun || (dryRun && view.Cleared) {
		return result{}, invalidNodeData("build slot cleanup")
	}
	message := "Build slot retained"
	if dryRun {
		message = "Build slot cleanup preview"
	} else if view.Cleared {
		message = "Build slot cleared"
	}
	return result{data: view, plain: fmt.Sprintf("%s: %s (%s) %s", message, output.SafeText(args[0]), output.SafeText(view.Slot.Builder), output.SafeText(view.Reason))}, nil
}

func dispatchBuildSlots(ctx context.Context, app api.AppService, actor contract.Actor, request rpc.Request) (any, error) {
	if request.Stream || len(request.Secrets) != 0 || request.Confirm != "" || request.IdempotencyKey != "" || !bytes.HasPrefix(bytes.TrimSpace(request.Params), []byte("{")) {
		return nil, contract.NewFault(contract.ValidationFailed, "invalid build slots request", "Use the documented parameters.")
	}
	decoder := json.NewDecoder(bytes.NewReader(request.Params))
	decoder.DisallowUnknownFields()
	if request.Method == methodBuildSlotsList {
		var params struct{}
		if request.DryRun || decoder.Decode(&params) != nil || decoder.Decode(new(any)) != io.EOF {
			return nil, contract.NewFault(contract.ValidationFailed, "invalid build slots request", "Use build slots list without options.")
		}
		return app.ListBuildSlots(ctx, actor)
	}
	var params struct {
		OperationID contract.OperationID `json:"operation_id"`
		LegacyLocal bool                 `json:"legacy_local,omitempty"`
	}
	if decoder.Decode(&params) != nil || decoder.Decode(new(any)) != io.EOF {
		return nil, contract.NewFault(contract.ValidationFailed, "invalid build slot cleanup request", "Pass one operation ID.")
	}
	return app.ClearBuildSlot(ctx, actor, params.OperationID, request.DryRun, params.LegacyLocal)
}
