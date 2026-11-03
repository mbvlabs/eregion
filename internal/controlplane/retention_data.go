package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/retention"
)

// retentionEnvironmentRow is the platform state the retention planner needs
// for one environment, with the effective policy already merged.
type retentionEnvironmentRow struct {
	ID             platform.EnvironmentID
	App            string
	Name           string
	Node           string
	ServingRelease string
	ServingDeploy  string
	Override       retention.Override
	Policy         retention.Policy
}

// RetentionShow returns the server policy and, when app/environment are named,
// the environment's effective policy with the source of every field.
func (s *Service) RetentionShow(ctx context.Context, app, environment string) (retention.PolicyView, error) {
	server, err := s.ServerRetentionPolicy(ctx)
	if err != nil {
		return retention.PolicyView{}, err
	}
	view := retention.PolicyView{Scope: "server", Server: server, Effective: server, Sources: retentionServerSources()}
	if app == "" && environment == "" {
		return view, nil
	}
	if app == "" || environment == "" {
		return retention.PolicyView{}, contract.NewFault(contract.InputRequired, "app and environment are required together", "Pass --app <app> --env <environment>, or neither for the server policy.")
	}
	if err := platform.ValidateName(app); err != nil {
		return retention.PolicyView{}, err
	}
	if err := platform.ValidateName(environment); err != nil {
		return retention.PolicyView{}, err
	}
	override, err := s.environmentRetentionOverride(ctx, app, environment)
	if err != nil {
		return retention.PolicyView{}, err
	}
	effective, err := retention.Merge(server, override)
	if err != nil {
		return retention.PolicyView{}, err
	}
	view.Scope = "environment"
	view.App = app
	view.Environment = environment
	view.Override = override
	view.Effective = effective
	view.Sources = retentionEnvironmentSources(override)
	return view, nil
}

func retentionServerSources() map[string]string {
	return map[string]string{
		"keep_releases":               "server",
		"keep_newer_than_days":        "server",
		"artifact_grace_days":         "server",
		"buildpack_cache_max_bytes":   "server",
		"buildpack_cache_max_percent": "server",
	}
}

func retentionEnvironmentSources(override retention.Override) map[string]string {
	sources := retentionServerSources()
	if override.KeepReleases != nil {
		sources["keep_releases"] = "environment"
	}
	if override.KeepNewerThanDays != nil {
		sources["keep_newer_than_days"] = "environment"
	}
	if override.ArtifactGraceDays != nil {
		sources["artifact_grace_days"] = "environment"
	}
	return sources
}

func (s *Service) environmentRetentionOverride(ctx context.Context, app, environment string) (retention.Override, error) {
	var raw []byte
	err := s.pool.QueryRow(ctx, `SELECT coalesce(r.retention,'{}'::jsonb)
        FROM eregion_platform_environments e
        JOIN eregion_platform_apps a ON a.id=e.app_id
        LEFT JOIN eregion_platform_environment_revisions r ON r.id=e.current_revision_id
        WHERE a.name=$1 AND e.name=$2`, app, environment).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return retention.Override{}, contract.NewFault(contract.NotFound, "environment not found", "Create the environment under this app first.")
	}
	if err != nil {
		return retention.Override{}, unavailable(err)
	}
	override := retention.Override{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &override); err != nil {
			return retention.Override{}, unavailable(err)
		}
	}
	if err := override.Validate(); err != nil {
		return retention.Override{}, unavailable(err)
	}
	return override, nil
}

// RetentionPlan evaluates the one planner for one node without mutating
// anything. It is the machine-readable body of `eregion prune --dry-run`.
func (s *Service) RetentionPlan(ctx context.Context, nodeName, app, environment string) (retention.PlanResult, error) {
	resolved, err := s.retentionNodeName(nodeName)
	if err != nil {
		return retention.PlanResult{}, err
	}
	inventory, input, err := s.loadRetentionInput(ctx, resolved, time.Now().UTC())
	if err != nil {
		return retention.PlanResult{}, err
	}
	return retentionResultFromInput(resolved, app, environment, inventory, input)
}

// RetentionPlanWithInventory evaluates the planner with an inventory the node
// RPC already observed. The node-mode dry run uses it so planning never needs a
// loopback transport inside an insert-only RPC process.
func (s *Service) RetentionPlanWithInventory(ctx context.Context, nodeName, app, environment string, inventory retention.Inventory) (retention.PlanResult, error) {
	resolved, err := s.retentionNodeName(nodeName)
	if err != nil {
		return retention.PlanResult{}, err
	}
	now := time.Now().UTC()
	environments, err := s.retentionEnvironmentRows(ctx, resolved)
	if err != nil {
		return retention.PlanResult{}, err
	}
	server, err := s.ServerRetentionPolicy(ctx)
	if err != nil {
		return retention.PlanResult{}, err
	}
	state, err := s.retentionPlatformState(ctx, resolved, environments, server)
	if err != nil {
		return retention.PlanResult{}, err
	}
	input := retention.InputFromInventory(retention.Inventory{}, state, now)
	return retentionResultFromInput(resolved, app, environment, inventory, input)
}

func retentionResultFromInput(nodeName, app, environment string, inventory retention.Inventory, input retention.Input) (retention.PlanResult, error) {
	if app == "" && environment != "" {
		return retention.PlanResult{}, contract.NewFault(contract.InputRequired, "--app is required with --env", "Pass both names, or neither to plan every environment.")
	}
	if app != "" {
		if err := platform.ValidateName(app); err != nil {
			return retention.PlanResult{}, err
		}
		if environment != "" {
			if err := platform.ValidateName(environment); err != nil {
				return retention.PlanResult{}, err
			}
		}
		input = filterRetentionInput(input, app, environment)
	}
	result := retention.PlanInventory(inventory, retention.PlatformState{
		Environments: input.Environments,
		Releases:     input.Releases,
		Deployments:  input.Deployments,
		InFlight:     input.InFlight,
		ServerPolicy: input.ServerPolicy,
	}, input.Now)
	result.Node = nodeName
	return result, nil
}

func filterRetentionInput(input retention.Input, app, environment string) retention.Input {
	matches := func(candidateApp, candidateEnvironment string) bool {
		return candidateApp == app && (environment == "" || candidateEnvironment == environment)
	}
	environments := input.Environments[:0]
	for _, item := range input.Environments {
		if matches(item.App, item.Name) {
			environments = append(environments, item)
		}
	}
	releases := input.Releases[:0]
	for _, item := range input.Releases {
		if matches(item.App, item.Environment) {
			releases = append(releases, item)
		}
	}
	deployments := input.Deployments[:0]
	for _, item := range input.Deployments {
		if matches(item.App, item.Environment) {
			deployments = append(deployments, item)
		}
	}
	input.Environments, input.Releases, input.Deployments = environments, releases, deployments
	return input
}

// retentionNodeName resolves the node a prune targets. The worker and node RPC
// always know their local identity; an explicit name must be a valid name.
func (s *Service) retentionNodeName(nodeName string) (string, error) {
	nodeName = strings.TrimSpace(nodeName)
	if nodeName == "" {
		nodeName = strings.TrimSpace(s.localNode)
	}
	if nodeName == "" {
		return "", contract.NewFault(contract.Unavailable, "the node identity is not configured", "Start the worker with EREGION_NODE_NAME or pass the node from node.info.")
	}
	if err := platform.ValidateName(nodeName); err != nil {
		return "", err
	}
	return nodeName, nil
}

// retentionEnvironmentRows loads each environment the node serves and merges
// the server policy with the current revision's override.
func (s *Service) retentionEnvironmentRows(ctx context.Context, nodeName string) ([]retentionEnvironmentRow, error) {
	server, err := s.ServerRetentionPolicy(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT e.id,a.name,e.name,e.node_name,
        coalesce(e.serving_release_id,''),coalesce(e.serving_deployment_id,''),coalesce(r.retention,'{}'::jsonb)
        FROM eregion_platform_environments e
        JOIN eregion_platform_apps a ON a.id=e.app_id
        LEFT JOIN eregion_platform_environment_revisions r ON r.id=e.current_revision_id
        WHERE $1='' OR e.node_name=$1
        ORDER BY a.name,e.name`, nodeName)
	if err != nil {
		return nil, unavailable(err)
	}
	defer rows.Close()
	var environments []retentionEnvironmentRow
	for rows.Next() {
		var item retentionEnvironmentRow
		var raw []byte
		if err := rows.Scan(&item.ID, &item.App, &item.Name, &item.Node, &item.ServingRelease, &item.ServingDeploy, &raw); err != nil {
			return nil, unavailable(err)
		}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &item.Override); err != nil {
				return nil, unavailable(err)
			}
		}
		policy, err := retention.Merge(server, item.Override)
		if err != nil {
			return nil, unavailable(err)
		}
		item.Policy = policy
		environments = append(environments, item)
	}
	return environments, rows.Err()
}

// loadRetentionInput reads the platform rows and the node inventory, then maps
// them through the shared adapter. It is read-only; a caller that will execute
// effects must hold the environment locks first.
func (s *Service) loadRetentionInput(ctx context.Context, nodeName string, now time.Time) (retention.Inventory, retention.Input, error) {
	environments, err := s.retentionEnvironmentRows(ctx, nodeName)
	if err != nil {
		return retention.Inventory{}, retention.Input{}, err
	}
	server, err := s.ServerRetentionPolicy(ctx)
	if err != nil {
		return retention.Inventory{}, retention.Input{}, err
	}
	state, err := s.retentionPlatformState(ctx, nodeName, environments, server)
	if err != nil {
		return retention.Inventory{}, retention.Input{}, err
	}
	inventory, err := s.retentionInventory(ctx, nodeName)
	if err != nil {
		return retention.Inventory{}, retention.Input{}, err
	}
	input := retention.InputFromInventory(inventory, state, now)
	return inventory, input, nil
}

func (s *Service) retentionPlatformState(ctx context.Context, nodeName string, environments []retentionEnvironmentRow, server retention.Policy) (retention.PlatformState, error) {
	state := retention.PlatformState{ServerPolicy: server}
	for _, item := range environments {
		state.Environments = append(state.Environments, retention.Environment{
			App: item.App, Name: item.Name, ServingRelease: item.ServingRelease,
			ServingDeployment: item.ServingDeploy, Policy: item.Policy,
		})
	}

	releases, err := s.pool.Query(ctx, `SELECT r.id,a.name,e.name,r.image,coalesce(r.image_id,''),r.created_at,r.pruned_at
        FROM eregion_platform_releases r
        JOIN eregion_platform_apps a ON a.id=r.app_id
        JOIN eregion_platform_environments e ON e.id=r.environment_id
        WHERE $1='' OR e.node_name=$1
        ORDER BY r.created_at,r.id`, nodeName)
	if err != nil {
		return state, unavailable(err)
	}
	for releases.Next() {
		var item retention.Release
		if err := releases.Scan(&item.ID, &item.App, &item.Environment, &item.Image, &item.ImageID, &item.CreatedAt, &item.PrunedAt); err != nil {
			releases.Close()
			return state, unavailable(err)
		}
		state.Releases = append(state.Releases, item)
	}
	err = releases.Err()
	releases.Close()
	if err != nil {
		return state, err
	}

	deployments, err := s.pool.Query(ctx, `SELECT d.id,a.name,e.name,coalesce(d.release_id,''),coalesce(d.previous_release_id,''),d.state,d.created_at,d.completed_at
        FROM eregion_platform_deployments d
        JOIN eregion_platform_apps a ON a.id=d.app_id
        JOIN eregion_platform_environments e ON e.id=d.environment_id
        WHERE ($1='' OR e.node_name=$1) AND d.state IN ('succeeded','pending','running')
        ORDER BY d.created_at,d.id`, nodeName)
	if err != nil {
		return state, unavailable(err)
	}
	for deployments.Next() {
		var item retention.Deployment
		if err := deployments.Scan(&item.ID, &item.App, &item.Environment, &item.Release, &item.PreviousRelease, &item.State, &item.CreatedAt, &item.CompletedAt); err != nil {
			deployments.Close()
			return state, unavailable(err)
		}
		state.Deployments = append(state.Deployments, item)
		if item.State == "pending" || item.State == "running" {
			state.InFlight.Deployments = append(state.InFlight.Deployments, item.ID)
			if item.Release != "" {
				state.InFlight.Releases = append(state.InFlight.Releases, item.Release)
			}
			if item.PreviousRelease != "" {
				state.InFlight.Releases = append(state.InFlight.Releases, item.PreviousRelease)
			}
		}
	}
	err = deployments.Err()
	deployments.Close()
	if err != nil {
		return state, err
	}

	active, err := s.pool.Query(ctx, `SELECT r.id
        FROM eregion_platform_releases r
        JOIN eregion_operations o ON o.id=r.operation_id
        JOIN eregion_platform_environments e ON e.id=r.environment_id
        WHERE ($1='' OR e.node_name=$1) AND o.state IN ('queued','running','cancelling')`, nodeName)
	if err != nil {
		return state, unavailable(err)
	}
	for active.Next() {
		var releaseID string
		if err := active.Scan(&releaseID); err != nil {
			active.Close()
			return state, unavailable(err)
		}
		state.InFlight.Releases = append(state.InFlight.Releases, releaseID)
	}
	err = active.Err()
	active.Close()
	return state, err
}

func (s *Service) retentionInventory(ctx context.Context, nodeName string) (retention.Inventory, error) {
	if s.resolver == nil {
		return retention.Inventory{}, contract.NewFault(contract.Unavailable, "the worker has no node resolver", "Run retention in a worker configured with node transports.")
	}
	transport, err := s.resolver.TransportFor(ctx, nodeName)
	if err != nil {
		return retention.Inventory{}, err
	}
	return observeRetentionInventoryTransport(ctx, transport)
}

func observeRetentionInventoryTransport(ctx context.Context, transport node.Transport) (retention.Inventory, error) {
	response, err := transport.Call(ctx, node.Request{
		Method: node.MethodInventory, RequestID: newRequestID(),
	}, nil)
	if err != nil {
		return retention.Inventory{}, fmt.Errorf("node inventory %s: %w", transport.Name(), err)
	}
	if response.Status != engine.StatusCompleted {
		code := contract.Code(response.ErrorCode)
		if code == "" {
			code = contract.Unavailable
		}
		detail := response.Detail
		if detail == "" {
			detail = "the node did not complete the retention inventory"
		}
		return retention.Inventory{}, contract.NewFault(code, detail, "Inspect the node's Docker and registry state, then retry.")
	}
	var inventory retention.Inventory
	if err := json.Unmarshal(response.Result, &inventory); err != nil {
		return retention.Inventory{}, contract.NewFault(contract.Unavailable, "the node returned an unreadable retention inventory", "Check that the operator and node run the same build.")
	}
	return inventory, nil
}

// retentionRemovalItems drops registry_skipped diagnostics: they name a reason
// the planner left a repository alone and are never effects.
func retentionRemovalItems(items []retention.Item) []retention.Item {
	removals := make([]retention.Item, 0, len(items))
	for _, item := range items {
		if item.Kind == retention.ItemRegistrySkipped || item.Kind == retention.ItemImageSkipped || item.Kind == retention.ItemCacheSkipped || item.Kind == retention.ItemRegistryGC {
			continue
		}
		removals = append(removals, item)
	}
	return removals
}

func retentionPlanSteps(plan retention.PlanResult) []string {
	steps := make([]string, 0, len(plan.Items))
	for _, item := range plan.Items {
		steps = append(steps, fmt.Sprintf("%s %s (%d bytes): %s", item.Kind, item.Reference, item.Bytes, item.Reason))
	}
	return steps
}
