package cli

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/viewer"
	"github.com/spf13/cobra"
)

// resourceSpecs is the generic managed-resource surface. Every declaration is
// environment-owned, private to the environment network and image based; it is
// not a database-engine catalog.
func (a *app) resourceSpecs() []spec {
	parent := platformMeta("Manage environment-owned generic Docker resources", "eregion resource list --json")
	apply := followingMeta(platformMutationMeta(
		"Provision or update one generic Docker resource from an explicit image",
		"eregion resource apply api production db --image postgres:17 --health-command pg_isready --env POSTGRES_DB=app --secret-env POSTGRES_PASSWORD=DATABASE_PASSWORD --mount data:/var/lib/postgresql/data"))
	apply.WhenToUse = []string{
		"Declare one image-based dependency for an environment. The container joins the environment's private bridge network and never publishes a host port.",
		"Secret references are existing environment vault names mapped to the image's environment variables; no value is accepted on the command line.",
		"An apply is explicit. It is not read from a file and never runs implicitly during a deploy.",
	}
	apply.Next = []string{"eregion resource list --json", "eregion operations follow <id> --json"}
	destroy := followingMeta(platformDestructiveMeta(
		"Remove a resource container and optionally delete its named data volumes",
		"eregion resource destroy api production db --confirm api/production/db"))
	destroy.Confirmation = "target"
	destroy.WhenToUse = []string{
		"Remove the container for one exact resource. By default its named volumes and durable resource identity are retained so the same declaration can be applied again.",
		"--delete-data additionally deletes only the resource's own named volumes after the container is gone; it is irreversible and refused for volumes without this resource's ownership labels.",
		"A dry run needs no confirmation; a real destroy needs --confirm app/environment/name.",
	}
	destroy.Next = []string{"eregion resource list --json"}
	destroy.ErrorCodes = append(destroy.ErrorCodes, contract.ConfirmationRequired)
	attach := followingMeta(platformMutationMeta(
		"Attach an existing managed resource to the environment's desired revision",
		"eregion resource attach api production db --json"))
	attach.WhenToUse = []string{
		"Record the resource as a dependency of the environment's next desired-state revision. Nothing is provisioned and deployment stays blocked until the resource reports ready.",
	}
	attach.Next = []string{"eregion resource status api production db --json", "eregion deploy api production --json"}
	detach := followingMeta(platformMutationMeta(
		"Detach a managed resource from the environment's desired revision",
		"eregion resource detach api production db --json"))
	detach.WhenToUse = []string{
		"Remove the dependency from the environment's next desired-state revision. The resource container and named volumes are retained; destruction is a separate explicit command.",
	}
	detach.Next = []string{"eregion resource list --json"}
	changeFlags := func(cmd *cobra.Command) {
		cmd.Flags().Bool("dry-run", false, "Validate and return a plan without accepting an operation")
		cmd.Flags().String("idempotency-key", "", "Stable submission key; generated when omitted")
		cmd.Flags().Bool("detach", false, "Return after acceptance instead of following the operation")
		cmd.Flags().Duration("poll-interval", viewer.DefaultPollInterval, "Follow poll interval")
	}
	applyFlags := func(cmd *cobra.Command) {
		changeFlags(cmd)
		cmd.Flags().String("image", "", "Image reference, resolved to an immutable image ID at apply time")
		cmd.Flags().Int("port", 0, "Private container listening port recorded for the app's private endpoint")
		cmd.Flags().StringArray("env", nil, "Plain non-secret environment setting KEY=VALUE (repeatable)")
		cmd.Flags().StringArray("secret-env", nil, "Secret environment mapping KEY=SECRET_NAME (repeatable)")
		cmd.Flags().StringArray("mount", nil, "Named-volume mount NAME:DESTINATION[:ro] (repeatable)")
		cmd.Flags().StringArray("health-command", nil, "Exec health-check argv entry (repeatable; required)")
		cmd.Flags().StringArray("command-arg", nil, "Optional image command argv entry (repeatable)")
		cmd.Flags().Int("health-timeout", 30, "Health-check deadline in seconds (1–300)")
		cmd.Flags().Int("health-interval", 500, "Health-check retry interval in milliseconds (100–5000)")
	}
	return []spec{{
		use:  "resource",
		meta: parent,
		run:  func(cmd *cobra.Command, _ []string) (result, error) { return a.help(cmd), nil },
		children: []spec{
			{use: "apply <app> <environment> <name>", meta: apply, args: platformThreeNames("app", "environment", "resource"), flags: applyFlags, run: a.resourceApply},
			{use: "attach <app> <environment> <name>", meta: attach, args: platformThreeNames("app", "environment", "resource"), flags: changeFlags, run: a.resourceAttach},
			{use: "detach <app> <environment> <name>", meta: detach, args: platformThreeNames("app", "environment", "resource"), flags: changeFlags, run: a.resourceDetach},
			{use: "list", meta: platformMeta("List desired and observed managed resources", "eregion resource list --app api --environment production --json"), args: platformReadArgs, flags: func(cmd *cobra.Command) {
				cmd.Flags().String("app", "", "Filter by app name")
				cmd.Flags().String("environment", "", "Filter by environment name")
				cmd.Flags().String("state", "", "Filter by lifecycle state")
				cmd.Flags().Int("limit", 100, "Maximum items (1–200)")
			}, run: a.resourceList},
			{use: "status <app> <environment> <name>", meta: platformMeta("Show one managed resource's desired and observed state", "eregion resource status api production db --json"), args: platformThreeNames("app", "environment", "resource"), run: a.resourceStatus},
			{use: "destroy <app> <environment> <name>", meta: destroy, args: platformThreeNames("app", "environment", "resource"), flags: func(cmd *cobra.Command) {
				changeFlags(cmd)
				cmd.Flags().String("confirm", "", "Target-naming confirmation: app/environment/name")
				cmd.Flags().Bool("delete-data", false, "Also permanently delete the resource's named volumes after removing the container")
			}, run: a.resourceDestroy},
		},
	}}
}

func (a *app) resourceApply(cmd *cobra.Command, args []string) (result, error) {
	input, err := resourceApplyInput(cmd, args)
	if err != nil {
		return result{}, err
	}
	target, err := platform.NewResourceTarget(input.App, input.Environment, input.Name)
	if err != nil {
		return result{}, err
	}
	normalized, err := platform.Default().Normalize(platform.KindResourceApply, target.String(), mustJSON(input))
	if err != nil {
		return result{}, err
	}
	input = *normalized.Input.(*platform.ResourceApplyInput)
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	if dryRun {
		preview, err := a.previewAction(cmd, platform.KindResourceApply, target.String(), input)
		if err != nil {
			return result{}, err
		}
		return result{data: preview, plain: previewText(preview)}, nil
	}
	follow := followsByDefault(cmd) || streamsNDJSON(cmd)
	if err := validateFollowRequest(cmd, follow); err != nil {
		return result{}, err
	}
	op, err := a.submitAccepted(cmd, platform.KindResourceApply, target.String(), input, "")
	if err != nil {
		return result{}, err
	}
	if !follow {
		return platformOperationResult(mustJSON(op), false), nil
	}
	client, err := a.client(cmd)
	if err != nil {
		return result{}, err
	}
	return a.followAccepted(cmd, client, op)
}

func (a *app) resourceAttach(cmd *cobra.Command, args []string) (result, error) {
	return a.resourceDependencyChange(cmd, args, platform.KindEnvironmentResourceAttach)
}

func (a *app) resourceDetach(cmd *cobra.Command, args []string) (result, error) {
	return a.resourceDependencyChange(cmd, args, platform.KindEnvironmentResourceDetach)
}

func (a *app) resourceDependencyChange(cmd *cobra.Command, args []string, kind string) (result, error) {
	input := platform.EnvironmentResourceInput{App: args[0], Environment: args[1], Resource: args[2]}
	target, err := platform.NewEnvironmentTarget(input.App, input.Environment)
	if err != nil {
		return result{}, err
	}
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	if dryRun {
		preview, err := a.previewAction(cmd, kind, target.String(), input)
		if err != nil {
			return result{}, err
		}
		return result{data: preview, plain: previewText(preview)}, nil
	}
	follow := followsByDefault(cmd) || streamsNDJSON(cmd)
	if err := validateFollowRequest(cmd, follow); err != nil {
		return result{}, err
	}
	op, err := a.submitAccepted(cmd, kind, target.String(), input, "")
	if err != nil {
		return result{}, err
	}
	if !follow {
		return platformOperationResult(mustJSON(op), false), nil
	}
	client, err := a.client(cmd)
	if err != nil {
		return result{}, err
	}
	return a.followAccepted(cmd, client, op)
}

func (a *app) resourceDestroy(cmd *cobra.Command, args []string) (result, error) {
	input := platform.ResourceDestroyInput{App: args[0], Environment: args[1], Name: args[2]}
	input.Confirm, _ = cmd.Flags().GetString("confirm")
	input.DeleteData, _ = cmd.Flags().GetBool("delete-data")
	target, err := platform.NewResourceTarget(input.App, input.Environment, input.Name)
	if err != nil {
		return result{}, err
	}
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	if dryRun {
		preview, err := a.previewAction(cmd, platform.KindResourceDestroy, target.String(), input)
		if err != nil {
			return result{}, err
		}
		return result{data: preview, plain: previewText(preview)}, nil
	}
	if err := platform.ValidateConfirmation(platform.KindResourceDestroy, target.String(), input.Confirm); err != nil {
		return result{}, err
	}
	follow := followsByDefault(cmd) || streamsNDJSON(cmd)
	if err := validateFollowRequest(cmd, follow); err != nil {
		return result{}, err
	}
	op, err := a.submitAccepted(cmd, platform.KindResourceDestroy, target.String(), input, input.Confirm)
	if err != nil {
		return result{}, err
	}
	if !follow {
		return platformOperationResult(mustJSON(op), false), nil
	}
	client, err := a.client(cmd)
	if err != nil {
		return result{}, err
	}
	return a.followAccepted(cmd, client, op)
}

func (a *app) resourceList(cmd *cobra.Command, _ []string) (result, error) {
	_, items, raw, err := a.resourceInventory(cmd, "", "")
	if err != nil {
		return result{}, err
	}
	rows := make([][]string, 0, len(items))
	for _, resource := range items {
		rows = append(rows, []string{
			resource.Name, resource.App, resource.Environment, string(resource.State),
			string(resource.HealthState), resource.Image, resource.Node,
		})
	}
	plain := "(no managed resources)\n"
	if len(rows) > 0 {
		plain = writeTable([]string{"NAME", "APP", "ENVIRONMENT", "STATE", "HEALTH", "IMAGE", "NODE"}, rows)
	}
	return result{data: raw, plain: plain}, nil
}

func (a *app) resourceStatus(cmd *cobra.Command, args []string) (result, error) {
	_, items, _, err := a.resourceInventory(cmd, args[0], args[1])
	if err != nil {
		return result{}, err
	}
	for _, resource := range items {
		if resource.Name != args[2] {
			continue
		}
		raw, err := json.Marshal(resource)
		if err != nil {
			return result{}, contract.NewFault(contract.Internal, "could not encode resource status", "Report the command and binary version.")
		}
		return result{data: json.RawMessage(raw), plain: resourceStatusText(resource)}, nil
	}
	return result{}, contract.NewFault(contract.NotFound, "resource not found", "List resources for this app and environment, or apply the declaration first.")
}

func (a *app) resourceInventory(cmd *cobra.Command, app, environment string) (string, []platform.Resource, json.RawMessage, error) {
	client, err := a.client(cmd)
	if err != nil {
		return "", nil, nil, err
	}
	filters := url.Values{}
	if app != "" {
		filters.Set("app", app)
	}
	if environment != "" {
		filters.Set("environment", environment)
	}
	for _, name := range []string{"app", "environment", "state"} {
		if flag := cmd.Flags().Lookup(name); flag != nil {
			if value, _ := cmd.Flags().GetString(name); value != "" {
				filters.Set(name, value)
			}
		}
	}
	if flag := cmd.Flags().Lookup("limit"); flag != nil {
		if value, _ := cmd.Flags().GetInt("limit"); value > 0 {
			filters.Set("limit", fmt.Sprint(value))
		}
	}
	if app != "" && environment != "" && filters.Get("limit") == "" {
		filters.Set("limit", "200")
	}
	raw, err := client.PlatformInventory(cmd.Context(), "resources", filters)
	if err != nil {
		return "", nil, nil, err
	}
	var page struct {
		Items []platform.Resource `json:"items"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return "", nil, nil, contract.NewFault(contract.Unavailable, "the node returned an invalid resource inventory", "Check binary compatibility before assuming the resource is absent.")
	}
	return "resources", page.Items, raw, nil
}

func resourceApplyInput(cmd *cobra.Command, args []string) (platform.ResourceApplyInput, error) {
	input := platform.ResourceApplyInput{
		App: args[0], Environment: args[1], Name: args[2],
		ResourceSpec: platform.ResourceSpec{
			Env:       map[string]string{},
			SecretEnv: map[string]string{},
		},
	}
	input.Image, _ = cmd.Flags().GetString("image")
	if strings.TrimSpace(input.Image) == "" {
		return input, contract.NewFault(contract.InputRequired, "resource image is required", "Pass --image, for example --image postgres:17.")
	}
	input.Port, _ = cmd.Flags().GetInt("port")
	if err := parseResourcePairs(cmd, "env", input.Env); err != nil {
		return input, err
	}
	if err := parseResourcePairs(cmd, "secret-env", input.SecretEnv); err != nil {
		return input, err
	}
	mounts, _ := cmd.Flags().GetStringArray("mount")
	for _, value := range mounts {
		parts := strings.Split(value, ":")
		if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || parts[1] == "" {
			return input, contract.NewFault(contract.ValidationFailed, "invalid resource mount", "Use NAME:DESTINATION or NAME:DESTINATION:ro.")
		}
		readOnly := false
		if len(parts) == 3 {
			if parts[2] != "ro" {
				return input, contract.NewFault(contract.ValidationFailed, "invalid resource mount mode", "Use :ro for read-only or omit the mode.")
			}
			readOnly = true
		}
		input.Mounts = append(input.Mounts, platform.ResourceMount{Name: parts[0], Destination: parts[1], ReadOnly: readOnly})
	}
	input.Health.Command, _ = cmd.Flags().GetStringArray("health-command")
	input.Command, _ = cmd.Flags().GetStringArray("command-arg")
	if len(input.Health.Command) == 0 {
		return input, contract.NewFault(contract.InputRequired, "resource health command is required", "Pass --health-command for the exec probe, for example --health-command pg_isready.")
	}
	input.Health.Type = "exec"
	input.Health.TimeoutSeconds, _ = cmd.Flags().GetInt("health-timeout")
	input.Health.IntervalMS, _ = cmd.Flags().GetInt("health-interval")
	return input, nil
}

func parseResourcePairs(cmd *cobra.Command, flag string, out map[string]string) error {
	values, _ := cmd.Flags().GetStringArray(flag)
	for _, value := range values {
		key, text, ok := strings.Cut(value, "=")
		if !ok || strings.TrimSpace(key) == "" {
			return contract.NewFault(contract.ValidationFailed, "invalid --"+flag+" value", "Use KEY=VALUE without a leading or trailing separator.")
		}
		if _, exists := out[key]; exists {
			return contract.NewFault(contract.ValidationFailed, "duplicate --"+flag+" key", "Use each environment key at most once.")
		}
		out[key] = text
	}
	return nil
}

func resourceStatusText(resource platform.Resource) string {
	var out strings.Builder
	fmt.Fprintf(&out, "Resource %s/%s/%s\n", resource.App, resource.Environment, resource.Name)
	fmt.Fprintf(&out, "  id: %s\n", resource.ID)
	fmt.Fprintf(&out, "  state: %s\n", resource.State)
	fmt.Fprintf(&out, "  health: %s\n", resource.HealthState)
	if resource.HealthDetail != "" {
		fmt.Fprintf(&out, "  health detail: %s\n", resource.HealthDetail)
	}
	fmt.Fprintf(&out, "  node: %s\n", resource.Node)
	fmt.Fprintf(&out, "  image: %s\n", resource.Image)
	if resource.ImageID != "" {
		fmt.Fprintf(&out, "  image id: %s\n", resource.ImageID)
	}
	if resource.ImageDigest != "" {
		fmt.Fprintf(&out, "  image digest: %s\n", resource.ImageDigest)
	}
	if resource.ContainerName != "" {
		fmt.Fprintf(&out, "  container: %s\n", resource.ContainerName)
		fmt.Fprintf(&out, "  private network: %s\n", resource.NetworkName)
		if resource.Spec.Port > 0 {
			fmt.Fprintf(&out, "  private endpoint: %s:%d (alias on the environment network)\n", resource.Name, resource.Spec.Port)
		}
	}
	if len(resource.Volumes) > 0 {
		out.WriteString("  owned volumes:\n")
		for _, volume := range resource.Volumes {
			mode := "rw"
			if volume.ReadOnly {
				mode = "ro"
			}
			fmt.Fprintf(&out, "    %s -> %s (%s)\n", volume.Name, volume.Destination, mode)
		}
	}
	if len(resource.Spec.SecretEnv) > 0 {
		out.WriteString("  secret names:\n")
		keys := make([]string, 0, len(resource.Spec.SecretEnv))
		for key := range resource.Spec.SecretEnv {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			fmt.Fprintf(&out, "    %s=%s\n", key, resource.Spec.SecretEnv[key])
		}
	}
	if resource.Error != "" {
		fmt.Fprintf(&out, "  error: %s\n", resource.Error)
	}
	return out.String()
}
