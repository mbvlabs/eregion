package cli

import (
	"strings"

	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/spf13/cobra"
)

func (a *app) operationsListSpec() spec {
	meta := operationMeta("List durable operations with app, environment and state filters",
		"eregion operations list --app api --environment production --state failed --limit 20 --json")
	meta.WhenToUse = []string{
		"Find accepted work and its durable state. Operation history is ordered newest first. Without --app it is scoped to the calling operator; --app/--environment also include the environment's builds and CI/webhook-submitted operations.",
		"Use operations get, events, follow or cancel with the returned operation ID.",
	}
	meta.Examples = []string{
		"eregion operations list --limit 20 --json",
		"eregion operations list --app api --json",
		"eregion operations list --app api --environment production --state running --json",
	}
	meta.Next = []string{"eregion operations get <operation_id> --json", "eregion operations follow <operation_id> --json"}
	return spec{
		use:  "list",
		meta: meta,
		args: platformReadArgs,
		flags: func(cmd *cobra.Command) {
			cmd.Flags().String("app", "", "Filter by app name")
			cmd.Flags().String("environment", "", "Filter by environment name; requires --app")
			cmd.Flags().String("state", "", "Filter by queued, running, cancelling, succeeded, failed, or cancelled")
			cmd.Flags().Int("limit", 50, "Maximum operations (1–200)")
		},
		run: a.operationsList,
	}
}

func (a *app) operationsList(cmd *cobra.Command, _ []string) (result, error) {
	app, _ := cmd.Flags().GetString("app")
	environment, _ := cmd.Flags().GetString("environment")
	state, _ := cmd.Flags().GetString("state")
	limit, _ := cmd.Flags().GetInt("limit")
	query := operations.ListQuery{App: app, Environment: environment, State: state, Limit: limit}
	if err := query.Validate(); err != nil {
		return result{}, err
	}
	client, err := a.client(cmd)
	if err != nil {
		return result{}, err
	}
	page, err := client.ListOperations(cmd.Context(), query)
	if err != nil {
		return result{}, err
	}
	var plain strings.Builder
	if len(page.Items) == 0 {
		plain.WriteString("No operations matched.\n")
	} else {
		plain.WriteString(writeOperationTable(page.Items))
	}
	return result{data: page, plain: plain.String()}, nil
}
