package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/appdef"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/spf13/cobra"
)

// definitionBackend is deliberately separate from Backend so other operator
// backends need not implement definition planning and admission.
type definitionBackend interface {
	PlanDefinition(context.Context, api.DefinitionPlanInput) (api.DefinitionPlan, error)
	ApplyDefinition(context.Context, api.DefinitionApplyInput) (operations.SubmissionResult, error)
}

func definitionPlanFlags(cmd *cobra.Command) {
	cmd.Flags().String("from", "", "Read a TOML definition for an existing app/environment")
	cmd.Flags().String("app", "", "Existing app name for the definition")
	cmd.Flags().String("environment", "", "Existing environment name for the definition")
}

func rejectDefinitionFlagsWithoutFrom(cmd *cobra.Command) error {
	for _, name := range []string{"from", "app", "environment", "plan-nonce", "confirm-plan", "idempotency-key", "confirm", "yes", "detach", "interactive", "ascii", "reduced-motion", "poll-interval"} {
		if f := cmd.Flags().Lookup(name); f != nil && f.Changed {
			return contract.NewFault(contract.Usage, "--"+name+" requires --from", "Use definition flags with --from, or omit them for host mode.")
		}
	}
	return nil
}

func definitionInputForTarget(cmd *cobra.Command, app, env string) (api.DefinitionPlanInput, error) {
	path := flagString(cmd, "from")
	if path == "" || strings.TrimSpace(app) == "" || strings.TrimSpace(env) == "" {
		return api.DefinitionPlanInput{}, contract.NewFault(contract.InputRequired, "definition path, app and environment are required", "Supply --from, --app and --environment.")
	}
	if err := platform.ValidateName(app); err != nil {
		return api.DefinitionPlanInput{}, err
	}
	if err := platform.ValidateName(env); err != nil {
		return api.DefinitionPlanInput{}, err
	}
	if boolFlag(cmd, "auto-upgrade-nodes") {
		return api.DefinitionPlanInput{}, contract.NewFault(contract.Usage, "node auto-upgrade cannot be combined with definition planning or apply", "Upgrade separately before planning.")
	}
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return api.DefinitionPlanInput{}, contract.NewFault(contract.InputRequired, "cannot read the app definition", "Pass --from with a readable regular file.")
	}
	f, err := os.Open(path) //nolint:gosec // The operator explicitly selects this input.
	if err != nil {
		return api.DefinitionPlanInput{}, contract.NewFault(contract.InputRequired, "cannot read the app definition", "Pass --from with a readable regular file.")
	}
	defer f.Close()
	info, err = f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return api.DefinitionPlanInput{}, contract.NewFault(contract.InputRequired, "the app definition is not a regular file", "Pass --from with a readable regular file.")
	}
	raw, err := io.ReadAll(io.LimitReader(f, appdef.MaxDefinitionBytes+1))
	if err != nil || len(raw) > appdef.MaxDefinitionBytes {
		return api.DefinitionPlanInput{}, contract.NewFault(contract.ValidationFailed, "cannot read the app definition within the size limit", "Keep the definition at or below 128 KiB.")
	}
	return api.DefinitionPlanInput{App: app, Environment: env, Definition: string(raw)}, nil
}

func (a *app) definitionClient(cmd *cobra.Command) (Backend, definitionBackend, error) {
	client, err := a.client(cmd)
	if err != nil {
		return nil, nil, err
	}
	// A workspace's automatic node upgrade is a mutation and must not happen
	// as a side effect of planning or applying an approved definition.
	if node, ok := client.(*nodeClient); ok {
		node.autoUpgrade = false
	}
	definition, ok := client.(definitionBackend)
	if !ok {
		return nil, nil, contract.NewFault(contract.Unavailable, "definition service is unavailable", "Use a node that supports definition planning and apply.")
	}
	return client, definition, nil
}

type definitionPlanResult struct {
	api.DefinitionPlan
	NextCommands []string `json:"next_commands"`
}

func definitionQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func definitionNext(path string, plan api.DefinitionPlan) string {
	command := "eregion apply --from " + definitionQuote(path) + " --app " + definitionQuote(plan.App) + " --environment " + definitionQuote(plan.Environment) + " --plan-nonce " + definitionQuote(plan.PlanNonce) + " --confirm-plan " + definitionQuote(plan.Digest) + " --idempotency-key KEY"
	confirm := plan.ConfirmTarget
	if confirm == "" {
		for _, domain := range plan.Domains {
			if domain.Action == "remove" {
				confirm = plan.App + "/" + plan.Environment
				break
			}
		}
	}
	if confirm != "" {
		command += " --confirm " + definitionQuote(confirm)
	}
	return command
}

func definitionPlanOutput(path string, plan api.DefinitionPlan) result {
	next := []string{}
	if plan.CanApply {
		next = append(next, definitionNext(path, plan))
	}
	data := definitionPlanResult{DefinitionPlan: plan, NextCommands: next}
	var out strings.Builder
	fmt.Fprintf(&out, "Definition plan for %s/%s\ncan apply: %t (eligibility, not readiness)\ndigest: %s\nplan nonce: %s\n", plan.App, plan.Environment, plan.CanApply, plan.Digest, plan.PlanNonce)
	if plan.ConfirmTarget != "" {
		fmt.Fprintf(&out, "required removal confirmation: %s\n", plan.ConfirmTarget)
	}
	for _, line := range plan.Diff {
		fmt.Fprintf(&out, "  %s\n", line)
	}
	for _, d := range plan.Domains {
		fmt.Fprintf(&out, "domain %s: %s\n", d.Action, d.Name)
	}
	for _, dns := range plan.DNS {
		fmt.Fprintf(&out, "DNS %s:\n", dns.Host)
		for _, r := range dns.Before {
			fmt.Fprintf(&out, "  before: %s %s (%s)\n", r.Type, r.Content, r.Ownership)
		}
		for _, r := range dns.Desired {
			fmt.Fprintf(&out, "  desired: %s %s\n", r.Type, r.Content)
		}
		for _, r := range dns.ExternalSatisfaction {
			fmt.Fprintf(&out, "  unchanged external, unowned: %s %s\n", r.Type, r.Content)
		}
		for _, change := range dns.Changes {
			before, after := "-", "-"
			if change.Before != nil {
				before = change.Before.Type + " " + change.Before.Content
			}
			if change.After != nil {
				after = change.After.Type + " " + change.After.Content
			}
			fmt.Fprintf(&out, "  %s: %s -> %s\n", change.Action, before, after)
		}
		for _, w := range dns.Warnings {
			fmt.Fprintf(&out, "  warning: %s\n", w)
		}
	}
	for _, dns := range plan.DNSCleanup {
		fmt.Fprintf(&out, "DNS cleanup %s:\n", dns.Host)
		for _, change := range dns.Changes {
			if change.Before != nil {
				fmt.Fprintf(&out, "  %s: %s %s -> -\n", change.Action, change.Before.Type, change.Before.Content)
			}
		}
		for _, w := range dns.Warnings {
			fmt.Fprintf(&out, "  warning: %s\n", w)
		}
		for _, c := range dns.Conflicts {
			fmt.Fprintf(&out, "  conflict: %s\n", c)
		}
	}
	for _, w := range plan.Warnings {
		fmt.Fprintf(&out, "warning: %s\n", w)
	}
	for _, b := range plan.Blockers {
		fmt.Fprintf(&out, "blocker: %s\n", b)
	}
	for _, n := range next {
		fmt.Fprintf(&out, "next: %s\n", n)
	}
	return result{data: data, plain: out.String(), next: next}
}

func (a *app) definitionPlan(cmd *cobra.Command) (result, error) {
	return a.definitionPlanFor(cmd, flagString(cmd, "app"), flagString(cmd, "environment"))
}

func (a *app) definitionPlanFor(cmd *cobra.Command, app, environment string) (result, error) {
	input, err := definitionInputForTarget(cmd, app, environment)
	if err != nil {
		return result{}, err
	}
	input.PlanNonce = flagString(cmd, "plan-nonce")
	_, backend, err := a.definitionClient(cmd)
	if err != nil {
		return result{}, err
	}
	plan, err := backend.PlanDefinition(cmd.Context(), input)
	if err != nil {
		return result{}, err
	}
	if !plan.CanApply {
		return result{}, definitionBlocked(flagString(cmd, "from"), plan)
	}
	return definitionPlanOutput(flagString(cmd, "from"), plan), nil
}

func definitionBlocked(path string, plan api.DefinitionPlan) error {
	output := definitionPlanOutput(path, plan)
	return &contract.Fault{Code: contract.PreconditionFailed, Message: "definition plan cannot be applied", Hint: output.plain, Details: map[string]any{"plan": output.data}}
}

func (a *app) definitionApply(cmd *cobra.Command) (result, error) {
	return a.definitionApplyFor(cmd, flagString(cmd, "app"), flagString(cmd, "environment"))
}

func (a *app) definitionApplyFor(cmd *cobra.Command, app, environment string) (result, error) {
	if boolFlag(cmd, "dry-run") {
		return a.definitionPlanFor(cmd, app, environment)
	}
	nonce, digest, key := flagString(cmd, "plan-nonce"), flagString(cmd, "confirm-plan"), flagString(cmd, "idempotency-key")
	if strings.TrimSpace(nonce) == "" || strings.TrimSpace(digest) == "" || strings.TrimSpace(key) == "" {
		return result{}, contract.NewFault(contract.InputRequired, "definition apply requires explicit plan approval and an idempotency key", "Run eregion plan --from first, then supply its --plan-nonce and --confirm-plan along with --idempotency-key; --yes is not approval.")
	}
	if err := contract.ValidateIdempotencyKey(key); err != nil {
		return result{}, err
	}
	follow := !boolFlag(cmd, "detach") && (humanInteractive(cmd) || streamsNDJSON(cmd))
	if streamsNDJSON(cmd) && boolFlag(cmd, "detach") {
		return result{}, contract.NewFault(contract.Usage, "--detach cannot be combined with NDJSON", "Omit --detach to follow, or use JSON for acceptance.")
	}
	if err := validateFollowRequest(cmd, follow); err != nil {
		return result{}, err
	}
	input, err := definitionInputForTarget(cmd, app, environment)
	if err != nil {
		return result{}, err
	}
	input.PlanNonce = nonce
	client, backend, err := a.definitionClient(cmd)
	if err != nil {
		return result{}, err
	}
	receipt, err := backend.ApplyDefinition(cmd.Context(), api.DefinitionApplyInput{Plan: input, ApprovedDigest: digest, IdempotencyKey: key, Confirm: flagString(cmd, "confirm")})
	if err != nil {
		// Do not preflight before admission: an authoritative idempotent replay
		// must remain possible even after the original plan changes. On a
		// precondition failure only, enrich the error with current blockers.
		if contract.Classify(err).Code == contract.PreconditionFailed {
			if plan, planErr := backend.PlanDefinition(cmd.Context(), input); planErr == nil && !plan.CanApply {
				return result{}, definitionBlocked(flagString(cmd, "from"), plan)
			}
		}
		return result{}, err
	}
	if follow {
		return a.followAccepted(cmd, client, receipt.Operation)
	}
	id := string(receipt.Operation.ID)
	next := []string{"eregion operations get " + id + " --json", "eregion operations follow " + id + " --output ndjson"}
	return result{data: receipt, plain: fmt.Sprintf("Definition operation accepted (not completed)\noperation: %s\nstate: %s\nnext: %s\nnext: %s\n", id, receipt.Operation.State, next[0], next[1]), next: next}, nil
}
