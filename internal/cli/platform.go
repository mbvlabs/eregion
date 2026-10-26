package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"sort"
	"strings"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/spf13/cobra"
)

// platformMeta is the common metadata for node-mode application commands. They
// never open the database or enqueue a River job themselves.
func platformMeta(summary, example string) CommandMeta {
	meta := operationMeta(summary, example)
	meta.WhenToUse = []string{summary + ". Runs on the node through SSH inside WireGuard; mutations are durable operations with explicit idempotency."}
	meta.Environment = []string{"EREGION_CONTEXT_FILE"}
	return meta
}

func platformMutationMeta(summary, example string) CommandMeta {
	meta := platformMeta(summary, example)
	meta.Mutating, meta.DryRun = true, true
	return meta
}

// platformDestructiveMeta is the shared destructive-command metadata. The
// default is a plan-bound token; target-confirmed commands override
// Confirmation with "target".
func platformDestructiveMeta(summary, example string) CommandMeta {
	meta := platformMutationMeta(summary, example)
	meta.Destructive = true
	meta.Confirmation = "plan-bound"
	return meta
}

func platformReadArgs(_ *cobra.Command, args []string) error {
	if len(args) != 0 {
		return contract.NewFault(contract.Usage, "this command accepts no positional arguments", "Inspect the command with --help --json.")
	}
	return nil
}

func platformOneName(label string) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) != 1 {
			return contract.NewFault(contract.InputRequired, label+" is required", "Supply exactly one "+label+".")
		}
		return platform.ValidateName(args[0])
	}
}

func platformTwoNames(first, second string) cobra.PositionalArgs {
	return func(_ *cobra.Command, args []string) error {
		if len(args) != 2 {
			return contract.NewFault(contract.InputRequired, first+" and "+second+" are required", "Supply exactly two positional names.")
		}
		if err := platform.ValidateName(args[0]); err != nil {
			return err
		}
		return platform.ValidateName(args[1])
	}
}

func addInventoryFlags(cmd *cobra.Command) {
	cmd.Flags().String("app", "", "Filter by app name")
	cmd.Flags().String("environment", "", "Filter by environment name")
	cmd.Flags().String("node", "", "Filter by node name")
	cmd.Flags().String("state", "", "Filter by collection state")
	cmd.Flags().Int("limit", 50, "Maximum items (1–200)")
}

func (a *app) platformReadInventory(cmd *cobra.Command, collection string) (json.RawMessage, error) {
	return a.platformReadInventoryWith(cmd, collection, platformInventoryFilters(cmd))
}

func (a *app) platformReadInventoryWith(cmd *cobra.Command, collection string, filters url.Values) (json.RawMessage, error) {
	client, err := a.client(cmd)
	if err != nil {
		return nil, err
	}
	return client.PlatformInventory(cmd.Context(), collection, filters)
}

func platformInventoryFilters(cmd *cobra.Command) url.Values {
	filters := url.Values{}
	for _, name := range []string{"app", "environment", "node", "state"} {
		value, _ := cmd.Flags().GetString(name)
		if value != "" {
			filters.Set(name, value)
		}
	}
	if flag := cmd.Flags().Lookup("limit"); flag != nil {
		limit, _ := cmd.Flags().GetInt("limit")
		filters.Set("limit", fmt.Sprintf("%d", limit))
	}
	return filters
}

func platformOperationResult(raw json.RawMessage, dryRun bool) result {
	var operation contract.Operation
	if !dryRun && json.Unmarshal(raw, &operation) == nil && operation.Validate() == nil {
		plain := fmt.Sprintf("Accepted %s on %s\noperation: %s\nstate: %s\nnext: eregion operations events %s --json", operation.Kind, operation.Target, operation.ID, operation.State, operation.ID)
		return result{data: raw, plain: plain}
	}
	var preview platform.Preview
	if dryRun && json.Unmarshal(raw, &preview) == nil && preview.DryRun {
		var plain strings.Builder
		fmt.Fprintf(&plain, "Dry run: %s on %s\n", preview.Kind, preview.Target)
		if preview.Summary != "" {
			plain.WriteString(preview.Summary + "\n")
		}
		for _, step := range preview.Steps {
			fmt.Fprintf(&plain, "  step: %s\n", step)
		}
		for _, warning := range preview.Warnings {
			fmt.Fprintf(&plain, "  warning: %s\n", warning)
		}
		return result{data: raw, plain: plain.String()}
	}
	return apiResult(raw)
}

func platformSelectItem(raw json.RawMessage, name string) (json.RawMessage, error) {
	var page struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, contract.NewFault(contract.Unavailable, "API returned an invalid inventory page", "Check binary compatibility before assuming the item is absent.")
	}
	for _, item := range page.Items {
		var identity struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		}
		if json.Unmarshal(item, &identity) == nil && (identity.Name == name || identity.ID == name) {
			return item, nil
		}
	}
	return nil, contract.NewFault(contract.NotFound, "platform item not found", "List the collection to choose an existing name.")
}

// platformConfirmationToken binds a domain mutation to a dry-run plan. It is
// used by domain remove, which still takes a plan-bound token rather than a
// target-name confirmation.
func platformConfirmationToken(kind, target string, input json.RawMessage) string {
	digest := sha256.New()
	_, _ = io.WriteString(digest, kind)
	_, _ = io.WriteString(digest, "\x00")
	_, _ = io.WriteString(digest, target)
	_, _ = io.WriteString(digest, "\x00")
	_, _ = digest.Write(input)
	return hex.EncodeToString(digest.Sum(nil))
}

func platformWithConfirmation(data json.RawMessage, token string) (json.RawMessage, string) {
	var preview any
	_ = json.Unmarshal(data, &preview)
	wrapped := map[string]any{"preview": preview, "confirmation_token": token}
	encoded, _ := json.Marshal(wrapped)
	return encoded, platformPretty(encoded) + "\nconfirmation_token: " + token
}

func platformPretty(raw json.RawMessage) string {
	var pretty bytes.Buffer
	_ = json.Indent(&pretty, raw, "", "  ")
	return pretty.String()
}

func mustJSON(value any) json.RawMessage {
	raw, _ := json.Marshal(value)
	return raw
}

func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
