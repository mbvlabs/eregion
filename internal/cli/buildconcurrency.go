package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/output"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/rpc"
	"github.com/spf13/cobra"
)

func (a *app) buildConcurrencySpec() spec {
	group := operationMeta("Inspect and set main-node build capacity", "eregion server status --json")
	group.WhenToUse = []string{"Inspect capacity with server status; set it for the selected workspace's verified main node."}
	set := operationMeta("Set the main node's build concurrency", "eregion server build-concurrency set 2 --dry-run --json")
	set.Mutating, set.DryRun = true, true
	set.WhenToUse = []string{
		"Set the verified main node's build capacity from 1 to 16. --dry-run previews the change without writing it.",
		"Higher capacity uses more CPU, memory, disk and cache. Lowering capacity does not stop running builds or restart workers.",
	}
	set.Examples = []string{"eregion server build-concurrency set 2 --dry-run --json", "eregion server build-concurrency set 2 --json"}
	set.Next = []string{"eregion server status --json"}
	return spec{use: "build-concurrency", meta: group,
		run: func(cmd *cobra.Command, _ []string) (result, error) { return a.help(cmd), nil },
		children: []spec{{
			use: "set <N>", meta: set, args: buildConcurrencyArgs,
			flags: func(cmd *cobra.Command) {
				cmd.Flags().Bool("dry-run", false, "Preview without changing build capacity")
			},
			run: a.buildConcurrencySet,
		}},
	}
}

func buildConcurrencyArgs(_ *cobra.Command, args []string) error {
	if len(args) != 1 {
		return contract.NewFault(contract.InputRequired, "build concurrency needs one integer", "Pass a value from 1 to 16.")
	}
	value, err := strconv.Atoi(args[0])
	if err != nil || value < 1 || value > platform.MaxBuildConcurrency {
		return contract.NewFault(contract.ValidationFailed, "invalid build concurrency", "Choose an integer from 1 to 16.")
	}
	return nil
}

func (a *app) buildConcurrencySet(cmd *cobra.Command, args []string) (result, error) {
	value, _ := strconv.Atoi(args[0])
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	client, err := a.nodeClient(cmd)
	if err != nil {
		return result{}, err
	}
	change, err := client.SetBuildConcurrency(cmd.Context(), value, dryRun)
	if err != nil {
		return result{}, err
	}
	var out strings.Builder
	if change.DryRun {
		fmt.Fprintf(&out, "Build capacity preview for %s: %d -> %d (no change made)", change.Node, change.Previous, change.Effective)
	} else {
		fmt.Fprintf(&out, "Build capacity for %s: %d -> %d", change.Node, change.Previous, change.Effective)
	}
	for _, warning := range change.Warnings {
		fmt.Fprintf(&out, "\nwarning: %s", output.SafeText(warning))
	}
	return result{data: change, plain: out.String()}, nil
}

type rpcBuildConcurrencyParams struct {
	Value int `json:"value"`
}

func (n *nodeClient) GetBuildConcurrency(ctx context.Context) (api.BuildConcurrencyResult, error) {
	raw, err := n.call(ctx, methodBuildConcurrencyGet, struct{}{}, rpcCallOptions{})
	return decodeBuildConcurrencyResult(raw, err, false, 0)
}

func (n *nodeClient) SetBuildConcurrency(ctx context.Context, value int, dryRun bool) (api.BuildConcurrencyResult, error) {
	raw, err := n.call(ctx, methodBuildConcurrencySet, rpcBuildConcurrencyParams{Value: value}, rpcCallOptions{DryRun: dryRun})
	return decodeBuildConcurrencyResult(raw, err, dryRun, value)
}

func decodeBuildConcurrencyResult(raw json.RawMessage, err error, dryRun bool, requested int) (api.BuildConcurrencyResult, error) {
	if err != nil {
		return api.BuildConcurrencyResult{}, err
	}
	var result api.BuildConcurrencyResult
	if json.Unmarshal(raw, &result) != nil || platform.ValidateName(result.Node) != nil || result.Previous < 1 || result.Previous > platform.MaxBuildConcurrency ||
		result.Effective < 1 || result.Effective > platform.MaxBuildConcurrency || result.DryRun != dryRun ||
		(requested != 0 && result.Effective != requested) || (requested == 0 && result.Effective != result.Previous) {
		return api.BuildConcurrencyResult{}, invalidNodeData("build capacity")
	}
	return result, nil
}

func dispatchBuildConcurrency(ctx context.Context, app api.AppService, actor contract.Actor, request rpc.Request) (any, error) {
	if request.Stream || len(request.Secrets) != 0 || request.Confirm != "" || request.IdempotencyKey != "" {
		return nil, contract.NewFault(contract.ValidationFailed, "build capacity request contains unsupported fields", "Use only the documented setting and dry-run flag.")
	}
	if !bytes.HasPrefix(bytes.TrimSpace(request.Params), []byte("{")) {
		return nil, contract.NewFault(contract.ValidationFailed, "invalid build capacity request", "Use the documented parameters.")
	}
	if request.Method == methodBuildConcurrencyGet {
		if request.DryRun {
			return nil, contract.NewFault(contract.ValidationFailed, "build capacity inspection does not accept dry-run", "Read server status without --dry-run.")
		}
		var params struct{}
		decoder := json.NewDecoder(bytes.NewReader(request.Params))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&params) != nil || decoder.Decode(new(any)) != io.EOF {
			return nil, contract.NewFault(contract.ValidationFailed, "invalid build capacity request", "Use server status.")
		}
		return app.GetBuildConcurrency(ctx, actor)
	}
	var params rpcBuildConcurrencyParams
	decoder := json.NewDecoder(bytes.NewReader(request.Params))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&params) != nil || decoder.Decode(new(any)) != io.EOF || params.Value < 1 || params.Value > platform.MaxBuildConcurrency {
		return nil, contract.NewFault(contract.ValidationFailed, "invalid build concurrency", "Choose an integer from 1 to 16.")
	}
	return app.SetBuildConcurrency(ctx, actor, api.BuildConcurrencyInput{Value: params.Value, DryRun: request.DryRun})
}
