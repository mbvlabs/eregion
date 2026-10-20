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
	"os"
	"sort"
	"strings"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/spf13/cobra"
)

// platformMeta is the common metadata for platform client commands. Platform
// commands are portable clients of the authenticated API; they never open the
// database, enqueue a River job themselves, or run SSH.
func platformMeta(summary, example string) CommandMeta {
	meta := operationMeta(summary, example)
	meta.WhenToUse = []string{summary + ". Reads the authenticated platform API; mutations are durable operations with explicit idempotency."}
	meta.Environment = []string{"EREGION_API_URL", "EREGION_API_TOKEN"}
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
				cmd.PersistentFlags().String("api-url", "", "API origin URL (defaults to EREGION_API_URL or loopback)")
			},
			run: func(cmd *cobra.Command, _ []string) (result, error) { return a.help(cmd), nil },
			children: []spec{
				{
					use:  "submit",
					meta: platformMutationMeta("Submit any registered platform action by kind and canonical target", "eregion platform submit --kind platform.node.check --target node/main --input-file check.json --idempotency-key ci:check-1 --json"),
					args: platformReadArgs,
					flags: func(cmd *cobra.Command) {
						addSubmissionFlags(cmd)
						cmd.Flags().String("kind", "", "Registered action kind, such as platform.deploy")
						cmd.Flags().String("target", "", "Canonical target: platform, app/<app>, env/<app>/<env>, or node/<node>")
						cmd.Flags().String("input-file", "", "Protected JSON file containing one action input object")
					},
					run: a.submitPlatformAction,
				},
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
				a.platformNodesSpec(),
				a.platformAppsSpec(),
				a.platformEnvironmentsSpec(),
				a.platformReleasesSpec(),
				a.platformDeploymentsSpec(),
				a.platformFleetSpec(),
			},
		},
	}
}

func (a *app) platformNodesSpec() spec {
	return spec{
		use:  "nodes",
		meta: platformMeta("Inspect registered nodes and request node checks", "eregion platform nodes list --json"),
		run:  func(cmd *cobra.Command, _ []string) (result, error) { return a.help(cmd), nil },
		children: []spec{
			{
				use:  "list",
				meta: platformMeta("List registered nodes and their declared placement facts", "eregion platform nodes list --state ready --json"),
				args: platformReadArgs,
				flags: func(cmd *cobra.Command) {
					addInventoryFlags(cmd)
				},
				run: func(cmd *cobra.Command, _ []string) (result, error) {
					return a.platformInventoryCollection(cmd, "nodes")
				},
			},
			{
				use:  "status <node>",
				meta: platformMeta("Show the current inventory observation for one node", "eregion platform nodes status main --json"),
				args: platformOneName("node"),
				run: func(cmd *cobra.Command, args []string) (result, error) {
					return a.platformInventoryItem(cmd, "nodes", "node", args[0])
				},
			},
			{
				use:  "check <node>",
				meta: platformMutationMeta("Submit a read-only node Docker/buildpacks availability check", "eregion platform nodes check main --idempotency-key ci:node-check-1 --json"),
				args: platformOneName("node"),
				flags: func(cmd *cobra.Command) {
					addSubmissionFlags(cmd)
				},
				run: func(cmd *cobra.Command, args []string) (result, error) {
					target, err := platform.NewNodeTarget(args[0])
					if err != nil {
						return result{}, err
					}
					return a.submitPlatform(cmd, platform.KindNodeCheck, target.String(), platform.NodeCheckInput{})
				},
			},
			{
				use:  "enroll <node>",
				meta: platformMutationMeta("Register node identity, roles, capabilities, and address facts", "eregion platform nodes enroll main --role application --role builder --capability docker --capability buildpacks --address 10.77.0.1:22 --idempotency-key ci:enroll-main --json"),
				args: platformOneName("node"),
				flags: func(cmd *cobra.Command) {
					addSubmissionFlags(cmd)
					cmd.Flags().StringArray("role", nil, "Node role (repeatable): application, builder, edge, registry, telemetry, control-plane")
					cmd.Flags().StringArray("capability", nil, "Node capability (repeatable), such as docker, buildpacks, registry, http-ingress")
					cmd.Flags().String("address", "", "Reachable host:port; a hostname is never authorization")
				},
				run: func(cmd *cobra.Command, args []string) (result, error) {
					roles, _ := cmd.Flags().GetStringArray("role")
					capabilities, _ := cmd.Flags().GetStringArray("capability")
					address, _ := cmd.Flags().GetString("address")
					return a.submitPlatform(cmd, platform.KindNodeRegister, platform.PlatformTarget().String(), platform.NodeRegisterInput{
						Name: args[0], Roles: roles, Capabilities: capabilities, Address: address,
					})
				},
			},
		},
	}
}

func (a *app) platformAppsSpec() spec {
	return spec{
		use:  "apps",
		meta: platformMeta("Inspect and create applications", "eregion platform apps list --json"),
		run:  func(cmd *cobra.Command, _ []string) (result, error) { return a.help(cmd), nil },
		children: []spec{
			{
				use:  "create <app>",
				meta: platformMutationMeta("Create an application record", "eregion platform apps create api --idempotency-key ci:app-api --json"),
				args: platformOneName("app"),
				flags: func(cmd *cobra.Command) {
					addSubmissionFlags(cmd)
				},
				run: func(cmd *cobra.Command, args []string) (result, error) {
					return a.submitPlatform(cmd, platform.KindAppCreate, platform.PlatformTarget().String(), platform.AppCreateInput{Name: args[0]})
				},
			},
			{
				use:  "list",
				meta: platformMeta("List application records", "eregion platform apps list --json"),
				args: platformReadArgs,
				flags: func(cmd *cobra.Command) {
					addInventoryFlags(cmd)
				},
				run: func(cmd *cobra.Command, _ []string) (result, error) {
					return a.platformInventoryCollection(cmd, "apps")
				},
			},
			{
				use:  "status <app>",
				meta: platformMeta("Show one application and its inventory environments", "eregion platform apps status api --json"),
				args: platformOneName("app"),
				run: func(cmd *cobra.Command, args []string) (result, error) {
					return a.platformAppStatus(cmd, args[0])
				},
			},
		},
	}
}

func (a *app) platformEnvironmentsSpec() spec {
	return spec{
		use:  "environments",
		meta: platformMeta("Inspect and create application environments", "eregion platform environments list --app api --json"),
		run:  func(cmd *cobra.Command, _ []string) (result, error) { return a.help(cmd), nil },
		children: []spec{
			{
				use:  "create <app> <environment>",
				meta: platformMutationMeta("Create an environment with explicit node placement and non-secret configuration", "eregion platform environments create api production --node main --container-port 8080 --env APP_ENV=production --idempotency-key ci:env-api-production --json"),
				args: platformTwoNames("app", "environment"),
				flags: func(cmd *cobra.Command) {
					addSubmissionFlags(cmd)
					cmd.Flags().String("node", "", "Node that must have the application role and docker capability")
					cmd.Flags().Int("container-port", platform.DefaultContainerPort, "Container port")
					cmd.Flags().Int("host-port", 0, "Stable host port; 0 allocates one from 20000–29999")
					cmd.Flags().String("bind", platform.DefaultBind, "Literal bind address")
					cmd.Flags().String("health-type", "http", "Health check type: http or tcp")
					cmd.Flags().String("health-path", platform.DefaultHealthPath, "Loopback HTTP health path")
					cmd.Flags().Int("health-timeout", platform.DefaultHealthTimeout, "Health timeout in seconds (1–300)")
					cmd.Flags().StringArray("env", nil, "Non-secret configuration KEY=VALUE (repeatable)")
				},
				run: func(cmd *cobra.Command, args []string) (result, error) {
					node, _ := cmd.Flags().GetString("node")
					if node == "" {
						return result{}, contract.NewFault(contract.InputRequired, "--node is required", "Choose a registered node with the application role and docker capability.")
					}
					containerPort, _ := cmd.Flags().GetInt("container-port")
					hostPort, _ := cmd.Flags().GetInt("host-port")
					bind, _ := cmd.Flags().GetString("bind")
					healthType, _ := cmd.Flags().GetString("health-type")
					healthPath, _ := cmd.Flags().GetString("health-path")
					healthTimeout, _ := cmd.Flags().GetInt("health-timeout")
					envValues, _ := cmd.Flags().GetStringArray("env")
					env, err := parsePlatformEnv(envValues)
					if err != nil {
						return result{}, err
					}
					target, err := platform.NewAppTarget(args[0])
					if err != nil {
						return result{}, err
					}
					return a.submitPlatform(cmd, platform.KindEnvironmentCreate, target.String(), platform.EnvironmentCreateInput{
						App: args[0], Name: args[1], Node: node, ContainerPort: containerPort,
						HostPort: hostPort, Bind: bind,
						Health: platform.HealthInput{Type: healthType, Path: healthPath, TimeoutSeconds: healthTimeout},
						Env:    env,
					})
				},
			},
			{
				use:  "list",
				meta: platformMeta("List environments and their serving/health observations", "eregion platform environments list --app api --json"),
				args: platformReadArgs,
				flags: func(cmd *cobra.Command) {
					addInventoryFlags(cmd)
				},
				run: func(cmd *cobra.Command, _ []string) (result, error) {
					return a.platformInventoryCollection(cmd, "environments")
				},
			},
			{
				use:  "status <app> <environment>",
				meta: platformMeta("Show one environment inventory observation", "eregion platform environments status api production --json"),
				args: platformTwoNames("app", "environment"),
				run: func(cmd *cobra.Command, args []string) (result, error) {
					return a.platformEnvironmentStatus(cmd, args[0], args[1])
				},
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

// submitPlatformAction is the generic fallback for extension actions. It only
// presents the shared submission path; it does not keep a second queue or guess
// extension input fields.
func (a *app) submitPlatformAction(cmd *cobra.Command, _ []string) (result, error) {
	kind, _ := cmd.Flags().GetString("kind")
	targetText, _ := cmd.Flags().GetString("target")
	path, _ := cmd.Flags().GetString("input-file")
	if strings.TrimSpace(kind) == "" {
		return result{}, contract.NewFault(contract.InputRequired, "--kind is required", "Use a documented platform.<action> kind.")
	}
	if strings.TrimSpace(targetText) == "" {
		return result{}, contract.NewFault(contract.InputRequired, "--target is required", "Use platform, app/<app>, env/<app>/<env>, or node/<node>.")
	}
	if path == "" {
		return result{}, contract.NewFault(contract.InputRequired, "--input-file is required", "Write one JSON object to a protected local file.")
	}
	target, err := platform.ParseTarget(targetText)
	if err != nil {
		return result{}, err
	}
	raw, err := readPlatformInput(path)
	if err != nil {
		return result{}, err
	}
	// Built-in actions can be validated locally. Extension kinds stay opaque to
	// this binary; the API registry remains the authority for their fields.
	if _, ok := platform.Default().Lookup(kind); ok {
		normalized, err := platform.Default().Normalize(kind, target.String(), raw)
		if err != nil {
			return result{}, err
		}
		raw = normalized.Canonical
	}
	return a.submitPlatformRaw(cmd, kind, target.String(), raw)
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

func (a *app) platformInventoryItem(cmd *cobra.Command, collection, filter, name string) (result, error) {
	raw, err := a.platformReadInventoryWith(cmd, collection, url.Values{filter: {name}})
	if err != nil {
		return result{}, err
	}
	item, err := platformSelectItem(raw, name)
	if err != nil {
		return result{}, err
	}
	return platformItemResult(collection, name, item, raw), nil
}

func (a *app) platformAppStatus(cmd *cobra.Command, app string) (result, error) {
	apps, err := a.platformReadInventoryWith(cmd, "apps", url.Values{"app": {app}})
	if err != nil {
		return result{}, err
	}
	appItem, err := platformSelectItem(apps, app)
	if err != nil {
		return result{}, err
	}
	environments, err := a.platformReadInventoryWith(cmd, "environments", url.Values{"app": {app}})
	if err != nil {
		return result{}, err
	}
	data := map[string]json.RawMessage{"app": appItem, "environments": environments}
	encoded, _ := json.Marshal(data)
	var plain strings.Builder
	fmt.Fprintf(&plain, "App %s\n", app)
	plain.WriteString("Environments\n")
	plain.WriteString(platformTable(environments, []string{"name", "node", "serving_release_id", "health", "updated_at"}))
	return result{data: encoded, plain: plain.String()}, nil
}

func (a *app) platformEnvironmentStatus(cmd *cobra.Command, app, environment string) (result, error) {
	raw, err := a.platformReadInventoryWith(cmd, "environments", url.Values{"app": {app}, "environment": {environment}})
	if err != nil {
		return result{}, err
	}
	item, err := platformSelectItem(raw, environment)
	if err != nil {
		return result{}, err
	}
	return platformItemResult("environments", app+"/"+environment, item, raw), nil
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

func readPlatformInput(path string) (json.RawMessage, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, contract.NewFault(contract.ValidationFailed, "could not read platform input file", "Provide a readable local file containing one JSON object; file contents are not printed.")
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, platform.MaxInputBytes+1))
	if err != nil || len(raw) > platform.MaxInputBytes {
		return nil, contract.NewFault(contract.ValidationFailed, "platform input file is unreadable or too large", "Limit the JSON object to 64 KiB.")
	}
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 {
		return nil, contract.NewFault(contract.InputRequired, "platform input file is empty", "Provide one JSON object.")
	}
	var object map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&object); err != nil || object == nil {
		return nil, contract.NewFault(contract.ValidationFailed, "platform input is not one JSON object", "Provide a single JSON object; unknown fields are validated by the API.")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, contract.NewFault(contract.ValidationFailed, "platform input has trailing data", "Provide exactly one JSON object.")
	}
	return raw, nil
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

func platformItemResult(collection, name string, item, raw json.RawMessage) result {
	var pretty bytes.Buffer
	_ = json.Indent(&pretty, item, "", "  ")
	return result{data: item, plain: fmt.Sprintf("%s %s\n%s", strings.TrimSuffix(collection, "s"), name, pretty.String())}
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

func platformPretty(raw json.RawMessage) string {
	var pretty bytes.Buffer
	_ = json.Indent(&pretty, raw, "", "  ")
	return pretty.String()
}

// platformTable renders a compact read-only snapshot. It intentionally has no
// colour, cursor movement, or terminal capability probing. Untrusted values are
// sanitized by the output writer before they reach a terminal.
func platformTable(raw json.RawMessage, columns []string) string {
	var page struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(raw, &page); err != nil || len(page.Items) == 0 {
		return "(none)\n"
	}
	widths := make([]int, len(columns))
	for i, column := range columns {
		widths[i] = len(column)
	}
	rows := make([][]string, 0, len(page.Items))
	for _, item := range page.Items {
		row := make([]string, len(columns))
		for i, column := range columns {
			row[i] = platformCell(item[column])
			if len(row[i]) > widths[i] {
				widths[i] = len(row[i])
			}
		}
		rows = append(rows, row)
	}
	var out strings.Builder
	for i, column := range columns {
		fmt.Fprintf(&out, "%-*s", widths[i]+2, strings.ToUpper(column))
	}
	out.WriteString("\n")
	for _, row := range rows {
		for i, value := range row {
			fmt.Fprintf(&out, "%-*s", widths[i]+2, value)
		}
		out.WriteString("\n")
	}
	return out.String()
}

func platformCell(value any) string {
	switch typed := value.(type) {
	case nil:
		return "-"
	case string:
		if typed == "" {
			return "-"
		}
		return typed
	case []any:
		items := make([]string, 0, len(typed))
		for _, item := range typed {
			items = append(items, fmt.Sprint(item))
		}
		return strings.Join(items, ",")
	case map[string]any:
		return platformPretty(mustJSON(typed))
	default:
		return fmt.Sprint(typed)
	}
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
