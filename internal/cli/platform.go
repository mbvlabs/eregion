package cli

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
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

// platformMeta is the common metadata for platform client commands. Platform
// commands run on the node through the saved context; they never open the
// database or enqueue a River job themselves.
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

func addSubmissionFlags(cmd *cobra.Command) {
	cmd.Flags().Bool("dry-run", false, "Validate with the API and return a plan without accepting an operation")
	cmd.Flags().String("idempotency-key", "", "Stable submission key; required unless --dry-run")
}

func addInventoryFlags(cmd *cobra.Command) {
	cmd.Flags().String("app", "", "Filter by app name")
	cmd.Flags().String("environment", "", "Filter by environment name")
	cmd.Flags().String("node", "", "Filter by node name")
	cmd.Flags().String("state", "", "Filter by collection state")
	cmd.Flags().Int("limit", 50, "Maximum items (1–200)")
}

func (a *app) platformSpecs() []spec {
	return []spec{
		{
			use:  "platform",
			meta: platformMeta("Inspect and change the application platform through the authenticated API", "eregion platform inventory --collection summary --json"),
			flags: func(cmd *cobra.Command) {
			},
			run: func(cmd *cobra.Command, _ []string) (result, error) { return a.help(cmd), nil },
			children: []spec{
				{
					use:  "inventory",
					meta: platformMeta("Read one bounded platform inventory collection", "eregion platform inventory --collection environments --app api --limit 50 --json"),
					args: platformReadArgs,
					flags: func(cmd *cobra.Command) {
						addInventoryFlags(cmd)
						cmd.Flags().String("collection", "", "Collection: summary, nodes, apps, environments, releases, deployments, changes, or an extension collection")
					},
					run: a.platformInventory,
				},
				a.platformReleasesSpec(),
				a.platformDeploymentsSpec(),
			},
		},
	}
}

func (a *app) platformReleasesSpec() spec {
	return spec{
		use:  "releases",
		meta: platformMeta("Inspect and create releases from images or build sources", "eregion platform releases create api production --image ghcr.io/acme/api:2026-10-20 --idempotency-key ci:release-api-1 --json"),
		run:  func(cmd *cobra.Command, _ []string) (result, error) { return a.help(cmd), nil },
		children: []spec{
			{
				use:  "create <app> <environment>",
				meta: platformMutationMeta("Record an image release or build a release from an absolute directory or HTTPS git source", "eregion platform releases create api production --directory /srv/src/api --idempotency-key ci:release-api-1 --json"),
				args: platformTwoNames("app", "environment"),
				flags: func(cmd *cobra.Command) {
					addSubmissionFlags(cmd)
					cmd.Flags().String("image", "", "OCI image reference; use exactly one source flag")
					cmd.Flags().String("directory", "", "Absolute source directory available on the target node")
					cmd.Flags().String("git-url", "", "HTTPS git repository URL without credentials")
					cmd.Flags().String("git-ref", "", "Optional branch or tag for --git-url")
					cmd.Flags().Bool("pull", false, "Pull the image on the node even if present")
					cmd.Flags().String("builder", "", "Buildpacks builder image for directory/git sources")
					cmd.Flags().StringArray("build-env", nil, "Non-secret Buildpacks KEY=VALUE (repeatable)")
				},
				run: func(cmd *cobra.Command, args []string) (result, error) {
					image, _ := cmd.Flags().GetString("image")
					directory, _ := cmd.Flags().GetString("directory")
					gitURL, _ := cmd.Flags().GetString("git-url")
					gitRef, _ := cmd.Flags().GetString("git-ref")
					pull, _ := cmd.Flags().GetBool("pull")
					builder, _ := cmd.Flags().GetString("builder")
					buildEnvValues, _ := cmd.Flags().GetStringArray("build-env")
					source, build, err := platformReleaseSource(image, directory, gitURL, gitRef, builder, buildEnvValues, pull)
					if err != nil {
						return result{}, err
					}
					target, err := platform.NewEnvironmentTarget(args[0], args[1])
					if err != nil {
						return result{}, err
					}
					input := platform.ReleaseCreateInput{App: args[0], Environment: args[1], Source: source, Build: build, Pull: pull}
					return a.submitPlatform(cmd, platform.KindReleaseCreate, target.String(), input)
				},
			},
			{
				use:  "list",
				meta: platformMeta("List releases and their build/readiness status", "eregion platform releases list --app api --environment production --json"),
				args: platformReadArgs,
				flags: func(cmd *cobra.Command) {
					addInventoryFlags(cmd)
				},
				run: func(cmd *cobra.Command, _ []string) (result, error) {
					return a.platformInventoryCollection(cmd, "releases")
				},
			},
		},
	}
}

func (a *app) platformDeploymentsSpec() spec {
	return spec{
		use:  "deployments",
		meta: platformMeta("Plan, deploy, inspect, and roll back environment releases", "eregion platform deployments list --app api --json"),
		run:  func(cmd *cobra.Command, _ []string) (result, error) { return a.help(cmd), nil },
		children: []spec{
			{
				use:  "plan <app> <environment>",
				meta: platformMeta("Preview a deployment without accepting work", "eregion platform deployments plan api production --release-id rel_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa --json"),
				args: platformTwoNames("app", "environment"),
				flags: func(cmd *cobra.Command) {
					cmd.Flags().String("release-id", "", "Release to deploy")
					cmd.Flags().String("idempotency-key", "", "Unused for a preview; accepted for command symmetry")
				},
				run: func(cmd *cobra.Command, args []string) (result, error) {
					release, _ := cmd.Flags().GetString("release-id")
					if release == "" {
						return result{}, contract.NewFault(contract.InputRequired, "--release-id is required", "List releases with platform releases list.")
					}
					target, err := platform.NewEnvironmentTarget(args[0], args[1])
					if err != nil {
						return result{}, err
					}
					return a.submitPlatformDryRun(cmd, platform.KindDeploy, target.String(), platform.DeployInput{ReleaseID: platform.ReleaseID(release)}, "")
				},
			},
			{
				use:  "deploy <app> <environment>",
				meta: platformMutationMeta("Deploy a release with health-first candidate switching", "eregion platform deployments deploy api production --release-id rel_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa --idempotency-key ci:deploy-1 --json"),
				args: platformTwoNames("app", "environment"),
				flags: func(cmd *cobra.Command) {
					addSubmissionFlags(cmd)
					cmd.Flags().String("release-id", "", "Release to deploy")
				},
				run: func(cmd *cobra.Command, args []string) (result, error) {
					release, _ := cmd.Flags().GetString("release-id")
					if release == "" {
						return result{}, contract.NewFault(contract.InputRequired, "--release-id is required", "Create a release first with platform releases create.")
					}
					target, err := platform.NewEnvironmentTarget(args[0], args[1])
					if err != nil {
						return result{}, err
					}
					return a.submitPlatform(cmd, platform.KindDeploy, target.String(), platform.DeployInput{ReleaseID: platform.ReleaseID(release)})
				},
			},
			{
				use:  "list",
				meta: platformMeta("List deployments and their terminal state", "eregion platform deployments list --app api --environment production --json"),
				args: platformReadArgs,
				flags: func(cmd *cobra.Command) {
					addInventoryFlags(cmd)
				},
				run: func(cmd *cobra.Command, _ []string) (result, error) {
					return a.platformInventoryCollection(cmd, "deployments")
				},
			},
			{
				use:  "retry",
				meta: platformMutationMeta("Re-submit a terminally failed deployment through the same operation service, preserving its target and release", "eregion platform deployments retry --deployment dep_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa --idempotency-key ci:retry-deploy-1 --json"),
				args: platformReadArgs,
				flags: func(cmd *cobra.Command) {
					addSubmissionFlags(cmd)
					cmd.Flags().String("deployment", "", "Failed deployment ID (dep_...) or its operation ID (op_...)")
				},
				run: a.retryDeployment,
			},
			{
				use:  "rollback <app> <environment>",
				meta: platformDestructiveMeta("Deploy a previous release, by default the environment's last applied release", "eregion platform deployments rollback api production --dry-run --json"),
				args: platformTwoNames("app", "environment"),
				flags: func(cmd *cobra.Command) {
					addSubmissionFlags(cmd)
					cmd.Flags().String("to-release-id", "", "Release to restore; omit the last applied release")
					cmd.Flags().String("confirm", "", "Confirmation token from rollback --dry-run")
				},
				run: func(cmd *cobra.Command, args []string) (result, error) {
					toRelease, _ := cmd.Flags().GetString("to-release-id")
					target, err := platform.NewEnvironmentTarget(args[0], args[1])
					if err != nil {
						return result{}, err
					}
					input := platform.RollbackInput{ToReleaseID: platform.ReleaseID(toRelease)}
					return a.submitPlatformDestructive(cmd, platform.KindRollback, target.String(), input)
				},
			},
		},
	}
}

func (a *app) platformInventory(cmd *cobra.Command, _ []string) (result, error) {
	collection, _ := cmd.Flags().GetString("collection")
	if strings.TrimSpace(collection) == "" {
		return result{}, contract.NewFault(contract.InputRequired, "--collection is required", "Use summary, nodes, apps, environments, releases, deployments, changes, or an extension collection.")
	}
	raw, err := a.platformReadInventory(cmd, collection)
	if err != nil {
		return result{}, err
	}
	return platformInventoryResult(collection, raw), nil
}

func (a *app) platformInventoryCollection(cmd *cobra.Command, collection string) (result, error) {
	raw, err := a.platformReadInventory(cmd, collection)
	if err != nil {
		return result{}, err
	}
	return platformInventoryResult(collection, raw), nil
}

// retryDeployment re-submits a failed deployment through the ordinary action
// service. It preserves the frozen target, action, and release; it does not
// create a queue entry or reset the failed operation itself.
func (a *app) retryDeployment(cmd *cobra.Command, _ []string) (result, error) {
	deploymentID, _ := cmd.Flags().GetString("deployment")
	if strings.TrimSpace(deploymentID) == "" {
		return result{}, contract.NewFault(contract.InputRequired, "--deployment is required", "Use the deployment ID or the failed deployment's operation ID.")
	}
	raw, err := a.platformReadInventoryWith(cmd, "deployments", url.Values{"limit": {"200"}})
	if err != nil {
		return result{}, err
	}
	var page struct {
		Items []platform.Deployment `json:"items"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return result{}, contract.NewFault(contract.Unavailable, "API returned an invalid deployment inventory", "Check binary compatibility before assuming the deployment is absent.")
	}
	var found *platform.Deployment
	for index := range page.Items {
		item := &page.Items[index]
		if string(item.ID) == deploymentID || string(item.OperationID) == deploymentID {
			found = item
			break
		}
	}
	if found == nil {
		return result{}, contract.NewFault(contract.NotFound, "deployment not found", "List deployments with platform deployments list --json.")
	}
	if found.State != platform.DeploymentFailed {
		return result{}, contract.NewFault(contract.PreconditionFailed, "only a terminally failed deployment can be retried",
			fmt.Sprintf("Deployment %s is %s. Wait for it to finish, or submit a new deployment for a different release.", found.ID, found.State))
	}
	target, err := platform.NewEnvironmentTarget(found.App, found.Environment)
	if err != nil {
		return result{}, err
	}
	if found.Action == platform.ActionRollback {
		return a.submitPlatform(cmd, platform.KindRollback, target.String(), platform.RollbackInput{ToReleaseID: found.ReleaseID})
	}
	return a.submitPlatform(cmd, platform.KindDeploy, target.String(), platform.DeployInput{ReleaseID: found.ReleaseID})
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

func (a *app) submitPlatform(cmd *cobra.Command, kind, target string, input any) (result, error) {
	raw, err := json.Marshal(input)
	if err != nil {
		return result{}, contract.NewFault(contract.Internal, "could not encode platform input", "Report the command and binary version.")
	}
	return a.submitPlatformRaw(cmd, kind, target, raw)
}

func (a *app) submitPlatformDryRun(cmd *cobra.Command, kind, target string, input any, key string) (result, error) {
	raw, err := json.Marshal(input)
	if err != nil {
		return result{}, contract.NewFault(contract.Internal, "could not encode platform input", "Report the command and binary version.")
	}
	return a.submitPlatformRawMode(cmd, kind, target, raw, key, true)
}

func (a *app) submitPlatformRaw(cmd *cobra.Command, kind, target string, input json.RawMessage) (result, error) {
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	key, _ := cmd.Flags().GetString("idempotency-key")
	if !dryRun {
		if key == "" {
			return result{}, contract.NewFault(contract.InputRequired, "--idempotency-key is required", "Choose a stable key and reuse it if submission times out.")
		}
		if err := contract.ValidateIdempotencyKey(key); err != nil {
			return result{}, err
		}
	} else {
		key = ""
	}
	return a.submitPlatformRawMode(cmd, kind, target, input, key, dryRun)
}

func (a *app) submitPlatformRawMode(cmd *cobra.Command, kind, target string, input json.RawMessage, key string, dryRun bool) (result, error) {
	client, err := a.client(cmd)
	if err != nil {
		return result{}, err
	}
	raw, err := client.SubmitAction(cmd.Context(), kind, target, input, key, dryRun)
	if err != nil {
		return result{}, err
	}
	return platformOperationResult(raw, dryRun), nil
}

func (a *app) submitPlatformDestructive(cmd *cobra.Command, kind, target string, input any) (result, error) {
	raw, err := json.Marshal(input)
	if err != nil {
		return result{}, contract.NewFault(contract.Internal, "could not encode platform input", "Report the command and binary version.")
	}
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	token := platformConfirmationToken(kind, target, raw)
	if dryRun {
		preview, err := a.submitPlatformRawMode(cmd, kind, target, raw, "", true)
		if err != nil {
			return preview, err
		}
		previewRaw, _ := preview.data.(json.RawMessage)
		preview.data, preview.plain = platformWithConfirmation(previewRaw, token)
		return preview, nil
	}
	confirm, _ := cmd.Flags().GetString("confirm")
	if confirm == "" {
		return result{}, contract.NewFault(contract.ConfirmationRequired, "--confirm is required for this destructive action", "Run the command with --dry-run and pass the returned confirmation token.")
	}
	if subtle.ConstantTimeCompare([]byte(confirm), []byte(token)) != 1 {
		return result{}, contract.NewFault(contract.PreconditionFailed, "confirmation token does not match this plan", "Run with --dry-run and pass the confirmation token for the exact target and input.")
	}
	return a.submitPlatformRaw(cmd, kind, target, raw)
}

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

func platformReleaseSource(image, directory, gitURL, gitRef, builder string, buildEnv []string, pull bool) (platform.Source, *platform.Build, error) {
	selected := 0
	for _, value := range []string{image, directory, gitURL} {
		if value != "" {
			selected++
		}
	}
	if selected != 1 {
		return platform.Source{}, nil, contract.NewFault(contract.InputRequired, "exactly one release source is required", "Use one of --image, --directory, or --git-url.")
	}
	if gitRef != "" && gitURL == "" {
		return platform.Source{}, nil, contract.NewFault(contract.ValidationFailed, "--git-ref requires --git-url", "Use --git-ref only with an HTTPS git source.")
	}
	if image != "" && (builder != "" || len(buildEnv) > 0) {
		return platform.Source{}, nil, contract.NewFault(contract.ValidationFailed, "build flags require a directory or git source", "An image release does not run Buildpacks.")
	}
	if pull && image == "" {
		return platform.Source{}, nil, contract.NewFault(contract.ValidationFailed, "--pull requires --image", "Use --pull only with an image release.")
	}
	if (directory != "" || gitURL != "") && (builder != "" || len(buildEnv) > 0) {
		env, err := parsePlatformEnv(buildEnv)
		if err != nil {
			return platform.Source{}, nil, err
		}
		if builder == "" {
			builder = platform.DefaultBuilder
		}
		return sourceAndBuild(image, directory, gitURL, gitRef, pull), &platform.Build{Builder: builder, Env: env}, nil
	}
	return sourceAndBuild(image, directory, gitURL, gitRef, pull), nil, nil
}

func sourceAndBuild(image, directory, gitURL, gitRef string, pull bool) platform.Source {
	if image != "" {
		return platform.Source{Type: "image", Image: image}
	}
	if directory != "" {
		return platform.Source{Type: "directory", Path: directory}
	}
	return platform.Source{Type: "git", URL: gitURL, Ref: gitRef}
}

func parsePlatformEnv(values []string) (map[string]string, error) {
	env := make(map[string]string, len(values))
	for _, value := range values {
		key, item, ok := strings.Cut(value, "=")
		if !ok || key == "" || strings.ContainsAny(key, " \t\r\n") {
			return nil, contract.NewFault(contract.ValidationFailed, "invalid --env value", "Use KEY=VALUE with a nonempty key; configuration values are not secrets.")
		}
		if _, exists := env[key]; exists {
			return nil, contract.NewFault(contract.ValidationFailed, "duplicate --env key", "Set each configuration key once.")
		}
		env[key] = item
	}
	return env, nil
}

func platformInventoryResult(collection string, raw json.RawMessage) result {
	return apiResult(raw)
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

// platformPretty renders canonical JSON for a preview or error context.
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
