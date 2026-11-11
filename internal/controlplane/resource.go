package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/vault"
)

const resourceColumns = `r.id,a.name,e.name,r.name,r.node_name,r.spec,r.state,r.image,r.image_id,r.image_digest,
    r.container_name,r.network_name,r.volumes,r.health_state,r.health_detail,r.health_checked_at,
    coalesce(r.operation_id,''),r.error_code,r.error,r.created_at,r.updated_at,r.destroyed_at`

func scanResourceRow(row pgx.Row) (platform.Resource, error) {
	var resource platform.Resource
	var rawSpec, rawVolumes []byte
	var state, health string
	var operationID, errorCode string
	var destroyedAt *time.Time
	err := row.Scan(&resource.ID, &resource.App, &resource.Environment, &resource.Name, &resource.Node,
		&rawSpec, &state, &resource.Image, &resource.ImageID, &resource.ImageDigest, &resource.ContainerName,
		&resource.NetworkName, &rawVolumes, &health, &resource.HealthDetail, &resource.HealthChecked,
		&operationID, &errorCode, &resource.Error, &resource.CreatedAt, &resource.UpdatedAt, &destroyedAt)
	if err != nil {
		return resource, err
	}
	resource.State = platform.ResourceState(state)
	resource.HealthState = platform.HealthState(health)
	resource.OperationID = contract.OperationID(operationID)
	resource.ErrorCode = contract.Code(errorCode)
	resource.DestroyedAt = destroyedAt
	if len(rawSpec) > 0 {
		if err := json.Unmarshal(rawSpec, &resource.Spec); err != nil {
			return resource, err
		}
	}
	if len(rawVolumes) > 0 {
		if err := json.Unmarshal(rawVolumes, &resource.Volumes); err != nil {
			return resource, err
		}
	}
	return resource, nil
}

func resourceByNameTx(ctx context.Context, tx pgx.Tx, app, environment, name string, lock bool) (platform.Resource, error) {
	statement := `SELECT ` + resourceColumns + `
        FROM eregion_platform_resources r
        JOIN eregion_platform_apps a ON a.id=r.app_id
        JOIN eregion_platform_environments e ON e.id=r.environment_id
        WHERE a.name=$1 AND e.name=$2 AND r.name=$3`
	if lock {
		statement += ` FOR UPDATE OF r`
	}
	resource, err := scanResourceRow(tx.QueryRow(ctx, statement, app, environment, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return resource, contract.NewFault(contract.NotFound, "resource not found", "Apply the resource declaration first; inventory lists resource identities.")
	}
	if err != nil {
		return resource, unavailable()
	}
	return resource, nil
}

func (s *Service) resourceByName(ctx context.Context, app, environment, name string) (platform.Resource, error) {
	resource, err := scanResourceRow(s.pool.QueryRow(ctx, `SELECT `+resourceColumns+`
        FROM eregion_platform_resources r
        JOIN eregion_platform_apps a ON a.id=r.app_id
        JOIN eregion_platform_environments e ON e.id=r.environment_id
        WHERE a.name=$1 AND e.name=$2 AND r.name=$3`, app, environment, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return resource, contract.NewFault(contract.NotFound, "resource not found", "Apply the resource declaration first; inventory lists resource identities.")
	}
	if err != nil {
		return resource, unavailable()
	}
	return resource, nil
}

func (s *Service) prepareResourceApply(ctx context.Context, tx pgx.Tx, id contract.OperationID, input *platform.ResourceApplyInput) error {
	env, err := environmentByNameTx(ctx, tx, input.App, input.Environment)
	if err != nil {
		return err
	}
	if err := s.checkPlacementByTx(ctx, tx, env.Node, platform.RoleApplication, platform.CapabilityDocker); err != nil {
		return err
	}
	if err := ensureNoActiveParentDeleteTx(ctx, tx, id, input.App, input.Environment); err != nil {
		return err
	}
	target := input.Target().String()
	if err := ensureNoActiveResourceOperationTx(ctx, tx, id, target); err != nil {
		return err
	}
	if len(input.SecretEnv) > 0 {
		if s.vault == nil {
			return contract.NewFault(contract.Unavailable, "secret storage is not configured", "Configure the vault before applying a resource with secret references.")
		}
		names := make([]string, 0, len(input.SecretEnv))
		seen := make(map[string]bool, len(input.SecretEnv))
		for _, name := range input.SecretEnv {
			if !seen[name] {
				seen[name] = true
				names = append(names, name)
			}
		}
		sort.Strings(names)
		var missing []string
		for _, name := range names {
			namespace, err := vault.NewNamespace(input.App, input.Environment, name)
			if err != nil {
				return err
			}
			metadata, err := s.vault.Store().GetForUpdateTx(ctx, tx, namespace)
			if err != nil {
				if contract.Classify(err).Code == contract.NotFound {
					missing = append(missing, name)
					continue
				}
				return err
			}
			if metadata.State != vault.StateActive {
				missing = append(missing, name)
			}
		}
		if len(missing) > 0 {
			fault := contract.NewFault(contract.PreconditionFailed, "resource requires active secrets: "+strings.Join(missing, ", "), "Set the missing or archived secrets in this environment, then apply the resource again.")
			fault.Details = map[string]any{"reason": "missing_secret_values", "names": missing}
			return fault
		}
	}
	existing, found, err := resourceByNameTxOptional(ctx, tx, input.App, input.Environment, input.Name)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(input.ResourceSpec)
	if err != nil {
		return unavailable()
	}
	if found {
		if existing.Node != env.Node {
			return contract.NewFault(contract.PreconditionFailed, "resource is bound to node "+existing.Node, "A resource with persistent volumes is never moved implicitly. Destroy it on "+existing.Node+" first; data volumes stay there, and a new resource identity must be declared for another node.")
		}
		if existing.State == platform.ResourceProvisioning || existing.State == platform.ResourceDestroying {
			return contract.NewFault(contract.Conflict, "resource is already changing", "Wait for the active resource operation to finish or cancel it explicitly.")
		}
		if _, err := tx.Exec(ctx, `UPDATE eregion_platform_resources
            SET node_name=$2, spec=$3, image=$4, state='pending', operation_id=$5,
                health_state='unknown', health_detail='', health_checked_at=NULL,
                error_code='', error='', destroyed_at=NULL, updated_at=clock_timestamp()
            WHERE id=$1`, existing.ID, env.Node, raw, input.Image, id); err != nil {
			return unavailable()
		}
		return nil
	}
	resourceID, err := platform.NewResourceID()
	if err != nil {
		return unavailable()
	}
	if _, err := tx.Exec(ctx, `INSERT INTO eregion_platform_resources
        (id,app_id,environment_id,name,node_name,spec,state,image,operation_id)
        VALUES($1,$2,$3,$4,$5,$6,'pending',$7,$8)`,
		resourceID, envAppID(ctx, tx, input.App), env.ID, input.Name, env.Node, raw, input.Image, id); err != nil {
		return platformWriteError(err, "resource name already exists in this environment")
	}
	return nil
}

func (s *Service) prepareResourceDestroy(ctx context.Context, tx pgx.Tx, id contract.OperationID, input *platform.ResourceDestroyInput) error {
	env, err := environmentByNameTx(ctx, tx, input.App, input.Environment)
	if err != nil {
		return err
	}
	resource, err := resourceByNameTx(ctx, tx, input.App, input.Environment, input.Name, true)
	if err != nil {
		return err
	}
	if err := platform.ValidateTargetConfirmation(input.Target(), input.Confirm); err != nil {
		return err
	}
	if resource.State == platform.ResourceDestroyed && !input.DeleteData {
		return contract.NewFault(contract.PreconditionFailed, "resource is already destroyed", "Apply the resource again to reactivate the same identity and retained volumes, or pass --delete-data to delete the retained data explicitly.")
	}
	if resource.State == platform.ResourceProvisioning {
		return contract.NewFault(contract.Conflict, "resource is still provisioning", "Wait for the apply to finish before destroying the resource.")
	}
	if err := resourceDestroyBlocked(ctx, tx, env, input.Name); err != nil {
		return err
	}
	if err := ensureNoActiveParentDeleteTx(ctx, tx, id, input.App, input.Environment); err != nil {
		return err
	}
	if err := ensureNoActiveResourceOperationTx(ctx, tx, id, input.Target().String()); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE eregion_platform_resources
        SET state='destroying', operation_id=$2, error_code='', error='', updated_at=clock_timestamp()
        WHERE id=$1`, resource.ID, id); err != nil {
		return unavailable()
	}
	return nil
}

// resourceByNameTxOptional distinguishes an absent declaration from a database
// failure during apply, where the first declaration has no row yet.
func resourceByNameTxOptional(ctx context.Context, tx pgx.Tx, app, environment, name string) (platform.Resource, bool, error) {
	resource, err := resourceByNameTx(ctx, tx, app, environment, name, true)
	if err == nil {
		return resource, true, nil
	}
	if contract.Classify(err).Code == contract.NotFound {
		return platform.Resource{}, false, nil
	}
	return platform.Resource{}, false, err
}

// resourceStateQueryer is the read surface shared by a submission transaction
// and the worker's pooled checks.
type resourceStateQueryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

// resourceInUse reports whether the current revision or the serving deployment
// still attaches the named resource. The caller must hold the environment row
// lock (submission) or the environment target lock (worker), so a revision or
// serving switch cannot pass between this read and the action it guards.
func resourceInUse(ctx context.Context, q resourceStateQueryer, app, environment, name string) (bool, string, error) {
	var currentRevision, servingRevision string
	err := q.QueryRow(ctx, `SELECT coalesce(e.current_revision_id,''), coalesce(d.revision_id,'')
        FROM eregion_platform_environments e
        JOIN eregion_platform_apps a ON a.id=e.app_id
        LEFT JOIN eregion_platform_deployments d ON d.id=e.serving_deployment_id
        WHERE a.name=$1 AND e.name=$2`, app, environment).Scan(&currentRevision, &servingRevision)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, "", contract.NewFault(contract.NotFound, "environment not found", "The resource's environment no longer exists.")
	}
	if err != nil {
		return false, "", unavailable()
	}
	for _, revision := range []string{currentRevision, servingRevision} {
		if revision == "" {
			continue
		}
		var attached bool
		if err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM eregion_platform_environment_revisions WHERE id=$1 AND $2=ANY(resources))`, revision, name).Scan(&attached); err != nil {
			return false, "", unavailable()
		}
		if attached {
			detail := "Detach the resource from the current environment revision and deploy the detached revision, or stop the environment, before destroying it; its container and volumes are not touched by detach."
			if revision != currentRevision {
				detail = "The serving deployment still uses a revision that references this resource. Deploy a revision without it (or stop the environment) before destroying the resource."
			}
			return true, detail, nil
		}
	}
	return false, "", nil
}

// resourceDeploymentDependency describes one queued or running deployment's
// relationship to a resource. The revision is the deployment's target; the
// previous revision is the one leaving service during a switch. A deployment
// without a recorded revision cannot prove independence, so it blocks too.
type resourceDeploymentDependency struct {
	RevisionID        string
	Resources         []string
	PreviousResources []string
}

func (d resourceDeploymentDependency) UsesResource(name string) bool {
	if d.RevisionID == "" {
		return true
	}
	for _, resource := range d.Resources {
		if resource == name {
			return true
		}
	}
	for _, resource := range d.PreviousResources {
		if resource == name {
			return true
		}
	}
	return false
}

// resourceDeploymentInUse reports a queued or running deployment that may still
// start, use or switch away from the named resource. It includes the previous
// deployment's revision because D17 keeps the old release serving until the
// switch completes; destroying a dependency during that window is a race.
func resourceDeploymentInUse(ctx context.Context, q resourceStateQueryer, environmentID platform.EnvironmentID, name string) (bool, error) {
	rows, err := q.Query(ctx, `SELECT coalesce(r.id,''),
            coalesce(r.resources,'{}'::text[]), coalesce(pr.resources,'{}'::text[])
        FROM eregion_platform_deployments d
        LEFT JOIN eregion_platform_environment_revisions r ON r.id=d.revision_id
        LEFT JOIN eregion_platform_deployments p ON p.id=d.previous_deployment_id
        LEFT JOIN eregion_platform_environment_revisions pr ON pr.id=p.revision_id
        WHERE d.environment_id=$1 AND d.state IN ('pending','running')`, environmentID)
	if err != nil {
		return false, unavailable()
	}
	defer rows.Close()
	for rows.Next() {
		var dependency resourceDeploymentDependency
		if err := rows.Scan(&dependency.RevisionID, &dependency.Resources, &dependency.PreviousResources); err != nil {
			return false, unavailable()
		}
		if dependency.UsesResource(name) {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, unavailable()
	}
	return false, nil
}

// resourceDestroyBlocked is the shared refusal for submission and execution.
// The submission holds the environment row lock; the worker holds the
// environment target lock. Together with the resource lifecycle lock this
// prevents a destroy from racing a deployment switch or removing a dependency
// that a queued/running deployment still expects.
func resourceDestroyBlocked(ctx context.Context, q resourceStateQueryer, env platform.Environment, name string) error {
	if inUse, detail, err := resourceInUse(ctx, q, env.App, env.Name, name); err != nil {
		return err
	} else if inUse {
		return contract.NewFault(contract.PreconditionFailed, "resource is still referenced by an environment revision", detail)
	}
	if inUse, err := resourceDeploymentInUse(ctx, q, env.ID, name); err != nil {
		return err
	} else if inUse {
		return contract.NewFault(contract.Conflict, "a deployment that uses this resource is queued or running", "Wait for the deployment to finish or cancel it explicitly, then destroy the resource.")
	}
	return nil
}

func ensureNoActiveParentDeleteTx(ctx context.Context, tx pgx.Tx, id contract.OperationID, app, environment string) error {
	var active int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM eregion_operations
        WHERE id<>$1 AND state IN ('queued','running','cancelling')
          AND ((kind=$4 AND target=$2) OR (kind=$5 AND target=$3))`, id, "app/"+app, "env/"+app+"/"+environment, platform.KindAppDelete, platform.KindEnvironmentDelete).Scan(&active); err != nil {
		return unavailable()
	}
	if active > 0 {
		return contract.NewFault(contract.Conflict, "an application or environment deletion is already active", "Wait for it to finish before changing a managed resource.")
	}
	return nil
}

func ensureNoActiveResourceOperationTx(ctx context.Context, tx pgx.Tx, id contract.OperationID, target string) error {
	var active int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM eregion_operations
        WHERE id<>$1 AND target=$2 AND state IN ('queued','running','cancelling')`, id, target).Scan(&active); err != nil {
		return unavailable()
	}
	if active > 0 {
		return contract.NewFault(contract.Conflict, "another operation is active for this resource", "Wait for it to finish or cancel it explicitly.")
	}
	return nil
}

// ensureNoLiveResourcesTx refuses an app or environment deletion while a
// resource container still exists. Data volumes are never removed by this
// guard, and the operator must destroy each named resource explicitly first.
func (s *Service) ensureNoLiveResourcesTx(ctx context.Context, tx pgx.Tx, app, environment string, appID, envID string) error {
	statement := `SELECT r.name FROM eregion_platform_resources r
        WHERE r.state<>'destroyed' AND `
	args := []any{}
	if environment != "" {
		statement += `r.environment_id=$1 ORDER BY r.name`
		args = append(args, envID)
	} else {
		statement += `r.app_id=$1 ORDER BY r.name`
		args = append(args, appID)
	}
	rows, err := tx.Query(ctx, statement, args...)
	if err != nil {
		return unavailable()
	}
	defer rows.Close()
	names := []string{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return unavailable()
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return unavailable()
	}
	if len(names) > 0 {
		return contract.NewFault(contract.PreconditionFailed,
			"resources still exist for this target: "+strings.Join(names, ", "),
			"Destroy each named resource first; named data volumes are retained and application or environment deletion never removes them.")
	}
	return nil
}

func (s *Service) resourceTargets(ctx context.Context, app, environment string) ([]string, error) {
	statement := `SELECT 'resource/'||a.name||'/'||e.name||'/'||r.name
        FROM eregion_platform_resources r
        JOIN eregion_platform_apps a ON a.id=r.app_id
        JOIN eregion_platform_environments e ON e.id=r.environment_id
        WHERE a.name=$1`
	args := []any{app}
	if environment != "" {
		statement += ` AND e.name=$2`
		args = append(args, environment)
	}
	statement += ` ORDER BY r.name`
	rows, err := s.pool.Query(ctx, statement, args...)
	if err != nil {
		return nil, unavailable()
	}
	defer rows.Close()
	targets := []string{}
	for rows.Next() {
		var target string
		if err := rows.Scan(&target); err != nil {
			return nil, unavailable()
		}
		targets = append(targets, target)
	}
	return targets, rows.Err()
}

func (w *platformWorker) runResourceApply(ctx context.Context, id contract.OperationID, attempt uint32, input *platform.ResourceApplyInput) error {
	if err := w.service.markResourceState(ctx, id, input.Target().String(), platform.ResourceProvisioning); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	err := w.applyResource(ctx, id, attempt, input)
	if err == nil {
		return nil
	}
	w.service.recordResourceFailure(ctx, id, input.App, input.Environment, input.Name, err)
	return w.settle(ctx, id, attempt, err)
}

func (w *platformWorker) applyResource(ctx context.Context, id contract.OperationID, attempt uint32, input *platform.ResourceApplyInput) error {
	resource, err := w.service.resourceByName(ctx, input.App, input.Environment, input.Name)
	if err != nil {
		return err
	}
	if resource.OperationID != id {
		return contract.NewFault(contract.PreconditionFailed, "resource operation was superseded", "Refresh resource inventory and retry from its current operation.")
	}
	env, err := w.service.environment(ctx, input.App, input.Environment)
	if err != nil {
		return err
	}
	if env.ID == "" {
		return contract.NewFault(contract.NotFound, "environment not found", "Create the environment before applying a resource.")
	}
	if err := w.service.checkCancelled(ctx, id); err != nil {
		return err
	}
	plan, err := platform.PlanResource(env.ID, resource.Node, resource.ID, env.App, env.Name, resource.Name, resource.Spec)
	if err != nil {
		return err
	}
	secretValues, secretFingerprint, err := w.service.resolveResourceSecrets(ctx, env, resource.Spec)
	if err != nil {
		return err
	}
	secretKeys := platform.SortedResourceSecretEnvKeys(secretValues)
	tr, err := w.service.resolver.TransportFor(ctx, resource.Node)
	if err != nil {
		return err
	}

	networkStep := platform.ResourceNetworkStepID(plan)
	if _, err := w.runStepOn(ctx, tr, id, attempt, networkStep, platform.ArgvPayload{
		Step: networkStep,
		Argv: []string{"docker", "network", "create", "--driver", "bridge",
			"--label", "io.eregion.app=" + env.App,
			"--label", "io.eregion.environment=" + env.Name, plan.Network},
		CheckArgv:      []string{"docker", "network", "inspect", "--format", "{{.Name}}", plan.Network},
		TimeoutSeconds: 30,
	}, nil); err != nil {
		return err
	}
	if err := w.service.checkCancelled(ctx, id); err != nil {
		return err
	}

	pullStep := platform.ResourcePullStepID(plan)
	if _, err := w.runStepOn(ctx, tr, id, attempt, pullStep, platform.ArgvPayload{
		Step: pullStep, Argv: []string{"docker", "pull", plan.Spec.Image}, TimeoutSeconds: 600,
	}, nil); err != nil {
		return err
	}
	if err := w.service.checkCancelled(ctx, id); err != nil {
		return err
	}
	imageStep := platform.ResourceImageStepID(plan)
	imageRaw, err := w.runStepOn(ctx, tr, id, attempt, imageStep, platform.ArgvPayload{
		Step:           imageStep,
		Argv:           []string{"docker", "image", "inspect", "--format", platform.ResourceImageInspectFormat, plan.Spec.Image},
		TimeoutSeconds: 30,
	}, nil)
	if err != nil {
		return err
	}
	if err := w.service.checkCancelled(ctx, id); err != nil {
		return err
	}
	imageID, repoDigests, err := platform.ParseResourceImageInspect(stepStdout(imageRaw))
	if err != nil {
		return contract.NewFault(contract.PreconditionFailed, "the resource image has no immutable identity", err.Error())
	}
	imageDigest := platform.ResourceImageDigest(plan.Spec.Image, repoDigests)

	// Record every volume identity before Docker creates any of them. A crash
	// between the create steps and the final resource update must not leave
	// data volumes that destroy --delete-data cannot find. The merge also keeps
	// identities whose mount a later declaration removed.
	ownedVolumes, err := w.service.persistOwnedResourceVolumes(ctx, id, resource.ID, platform.PlannedResourceVolumes(plan))
	if err != nil {
		return err
	}

	if err := w.service.checkCancelled(ctx, id); err != nil {
		return err
	}
	for index, volume := range plan.Volumes {
		step := platform.ResourceVolumeStepID(plan, index)
		if _, err := w.runStepOn(ctx, tr, id, attempt, step, platform.ResourceVolumePayload{
			Step: step, Volume: volume.Name, App: env.App, Environment: env.Name,
			Resource: resource.Name, ResourceID: string(resource.ID),
		}, nil); err != nil {
			return err
		}
		if err := w.service.checkCancelled(ctx, id); err != nil {
			return err
		}
	}

	configHash := platform.ResourceContainerConfigHash(plan, imageID, secretFingerprint, secretKeys)
	runStep := platform.ResourceRunStepID(plan)
	if _, err := w.runStepOn(ctx, tr, id, attempt, runStep, platform.ResourceRunPayload{
		Step: runStep, Name: plan.Container, ImageID: imageID, Network: plan.Network,
		Alias: plan.Alias, Env: plan.Spec.Env, SecretKeys: secretKeys, Command: plan.Spec.Command,
		Mounts: plan.Volumes, ConfigHash: configHash, App: env.App, Environment: env.Name,
		Resource: resource.Name, ResourceID: string(resource.ID), Restart: "unless-stopped",
	}, secretValues); err != nil {
		return err
	}
	if err := w.service.checkCancelled(ctx, id); err != nil {
		return err
	}
	if err := w.service.recordResourceContainer(ctx, id, resource.ID, plan, imageID, imageDigest, ownedVolumes); err != nil {
		return err
	}

	healthStep := platform.ResourceHealthStepID(plan)
	if _, err := w.runStepOn(ctx, tr, id, attempt, healthStep, platform.ResourceHealthPayload{
		Step: healthStep, Container: plan.Container, Command: plan.Spec.Health.Command,
		TimeoutSeconds: plan.Spec.Health.TimeoutSeconds, IntervalMS: plan.Spec.Health.IntervalMS,
	}, secretValues); err != nil {
		return err
	}
	if err := w.service.checkCancelled(ctx, id); err != nil {
		return err
	}

	rawVolumes, err := json.Marshal(ownedVolumes)
	if err != nil {
		return unavailable()
	}
	if err := w.service.checkCancelled(ctx, id); err != nil {
		return err
	}
	return w.service.finishResourceOperation(ctx, id, attempt, func(tx pgx.Tx, op *contract.Operation) error {
		tag, err := tx.Exec(ctx, `UPDATE eregion_platform_resources
            SET state='ready', image=$2, image_id=$3, image_digest=$4, container_name=$5, network_name=$6,
                volumes=$7, health_state='healthy', health_detail='',
                health_checked_at=clock_timestamp(), operation_id=$8, error_code='', error='',
                destroyed_at=NULL, updated_at=clock_timestamp()
            WHERE id=$1 AND operation_id=$8`, resource.ID, plan.Spec.Image, imageID, imageDigest, plan.Container,
			plan.Network, rawVolumes, id)
		if err != nil {
			return unavailable()
		}
		if tag.RowsAffected() == 0 {
			return contract.NewFault(contract.PreconditionFailed, "resource changed while it was being provisioned", "Refresh resource inventory and submit a new explicit apply if the declaration is still desired.")
		}
		return appendEvent(ctx, tx, op, contract.Event{
			Kind: "log", Stage: "resource", Message: "Resource " + resource.Name + " is healthy on private network " + plan.Network + "; no host port was published.",
		})
	})
}

func (w *platformWorker) runResourceDestroy(ctx context.Context, id contract.OperationID, attempt uint32, input *platform.ResourceDestroyInput) error {
	if err := w.service.markResourceState(ctx, id, input.Target().String(), platform.ResourceDestroying); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	err := w.destroyResource(ctx, id, attempt, input)
	if err == nil {
		return nil
	}
	w.service.recordResourceFailure(ctx, id, input.App, input.Environment, input.Name, err)
	return w.settle(ctx, id, attempt, err)
}

func (w *platformWorker) destroyResource(ctx context.Context, id contract.OperationID, attempt uint32, input *platform.ResourceDestroyInput) error {
	resource, err := w.service.resourceByName(ctx, input.App, input.Environment, input.Name)
	if err != nil {
		return err
	}
	if resource.OperationID != id {
		return contract.NewFault(contract.PreconditionFailed, "resource operation was superseded", "Refresh resource inventory and retry from its current operation.")
	}
	env, err := w.service.environment(ctx, input.App, input.Environment)
	if err != nil {
		return err
	}
	if err := resourceDestroyBlocked(ctx, w.service.pool, env, resource.Name); err != nil {
		return err
	}
	plan, err := platform.PlanResource(env.ID, resource.Node, resource.ID, env.App, env.Name, resource.Name, resource.Spec)
	if err != nil {
		return err
	}
	tr, err := w.service.resolver.TransportFor(ctx, resource.Node)
	if err != nil {
		return err
	}
	if err := w.service.checkCancelled(ctx, id); err != nil {
		return err
	}
	step := platform.ResourceDestroyStepID(resource.ID)
	if _, err := w.runStepOn(ctx, tr, id, attempt, step, platform.ResourceDestroyPayload{
		Step: step, Container: plan.Container, App: env.App, Environment: env.Name,
		Resource: resource.Name, ResourceID: string(resource.ID),
	}, nil); err != nil {
		return err
	}
	if err := w.service.checkCancelled(ctx, id); err != nil {
		return err
	}
	ownedVolumes := platform.MergeResourceVolumes(resource.Volumes, nil)
	deleteVolumes := []platform.ResourceVolumeStatus{}
	if input.DeleteData {
		if err := w.service.checkCancelled(ctx, id); err != nil {
			return err
		}
		deleteVolumes = platform.ResourceVolumesForDestroy(resource.Volumes, plan)
		for index, volume := range deleteVolumes {
			removeStep := platform.ResourceVolumeRemoveStepID(plan, index)
			if _, err := w.runStepOn(ctx, tr, id, attempt, removeStep, platform.ResourceVolumeRemovePayload{
				Step: removeStep, Volume: volume.Name, App: env.App, Environment: env.Name,
				Resource: resource.Name, ResourceID: string(resource.ID),
			}, nil); err != nil {
				return err
			}
			if err := w.service.checkCancelled(ctx, id); err != nil {
				return err
			}
		}
	}
	if err := w.service.checkCancelled(ctx, id); err != nil {
		return err
	}
	return w.service.finishResourceOperation(ctx, id, attempt, func(tx pgx.Tx, op *contract.Operation) error {
		volumes := []byte("[]")
		detail := "Container removed; named volumes were retained."
		if input.DeleteData {
			if len(deleteVolumes) == 0 {
				detail = "Container removed; no owned named volumes were recorded to delete."
			} else {
				detail = "Container and named volumes removed; resource data was deleted."
			}
		} else {
			raw, err := json.Marshal(ownedVolumes)
			if err != nil {
				return unavailable()
			}
			volumes = raw
		}
		tag, err := tx.Exec(ctx, `UPDATE eregion_platform_resources
            SET state='destroyed', container_name='', volumes=$3, health_state='unknown',
                health_detail=$4, health_checked_at=clock_timestamp(), operation_id=$2,
                error_code='', error='', destroyed_at=clock_timestamp(), updated_at=clock_timestamp()
            WHERE id=$1 AND operation_id=$2`, resource.ID, id, volumes, detail)
		if err != nil {
			return unavailable()
		}
		if tag.RowsAffected() == 0 {
			return contract.NewFault(contract.PreconditionFailed, "resource changed while it was being destroyed", "Refresh resource inventory before retrying.")
		}
		return appendEvent(ctx, tx, op, contract.Event{
			Kind: "log", Stage: "resource", Message: "Resource " + resource.Name + ": " + detail,
		})
	})
}

// finishResourceOperation finalizes an apply or destroy under the operation row
// lock. A cancellation accepted after the last step-boundary check but before
// this transaction must not be converted into success: return the normal
// cancellation error so the resource settles to degraded through the normal
// cancellation path. This is deliberately scoped to resources; other operation
// completion policy is unchanged.
func (s *Service) finishResourceOperation(ctx context.Context, id contract.OperationID, attempt uint32, finalize func(pgx.Tx, *contract.Operation) error) error {
	if s.beforeResourceFinalize != nil {
		s.beforeResourceFinalize()
	}
	_, owned, err := s.mutateOwned(ctx, id, attempt, func(tx pgx.Tx, op *contract.Operation) error {
		if op.State.Terminal() {
			return nil
		}
		if op.State == contract.Cancelling {
			return errPlatformCancelled
		}
		if err := finalize(tx, op); err != nil {
			return err
		}
		return transition(ctx, tx, op, contract.Succeeded, "", "Platform action succeeded; domain state and history are durable.")
	})
	if err != nil {
		return err
	}
	if !owned {
		return errNotOwned
	}
	return nil
}

func (s *Service) markResourceState(ctx context.Context, id contract.OperationID, target string, state platform.ResourceState) error {
	_, err := s.pool.Exec(ctx, `UPDATE eregion_platform_resources r
        SET state=$3, updated_at=clock_timestamp()
        FROM eregion_platform_apps a, eregion_platform_environments e
        WHERE r.app_id=a.id AND r.environment_id=e.id
          AND 'resource/'||a.name||'/'||e.name||'/'||r.name=$2 AND r.operation_id=$1`,
		id, target, string(state))
	if err != nil {
		return unavailable()
	}
	return nil
}

func (s *Service) recordResourceFailure(ctx context.Context, id contract.OperationID, app, environment, name string, cause error) {
	if cause == nil || errors.Is(cause, errPlatformCancelled) {
		return
	}
	fault := contract.Classify(cause)
	message := s.redact(fault.Message)
	if message == "" {
		message = "resource operation failed"
	}
	_, _ = s.pool.Exec(ctx, `UPDATE eregion_platform_resources r
        SET state='degraded', health_state='unhealthy', health_detail=$4,
            health_checked_at=clock_timestamp(), error_code=$5, error=$4,
            updated_at=clock_timestamp()
        FROM eregion_platform_apps a, eregion_platform_environments e
        WHERE r.app_id=a.id AND r.environment_id=e.id
          AND a.name=$1 AND e.name=$2 AND r.name=$3 AND r.operation_id=$6`,
		app, environment, name, message, string(fault.Code), id)
}

// settleResourceForTerminalOperationTx moves a resource out of a transient
// provisioning/destroying state when its owning operation has become cancelled
// or failed. It is fenced by operation_id, so a newer resource operation is
// never clobbered. The container identity, image and cumulative volume ledger
// are intentionally preserved: a later apply reuses them, while a later
// destroy can still find every owned volume and the possibly existing container.
func settleResourceForTerminalOperationTx(ctx context.Context, tx pgx.Tx, id contract.OperationID, kind string, code contract.Code) error {
	if kind != platform.KindResourceApply && kind != platform.KindResourceDestroy {
		return nil
	}
	detail := "Resource apply was interrupted before a durable ready state; re-apply or destroy the resource explicitly."
	if kind == platform.KindResourceDestroy {
		detail = "Resource destroy was interrupted before a durable destroyed state; re-apply or re-destroy the resource explicitly."
	}
	errorCode := code
	if errorCode == "" {
		errorCode = contract.Unavailable
	}
	if _, err := tx.Exec(ctx, `UPDATE eregion_platform_resources
        SET state='degraded', health_state='unknown', health_detail=$2,
            health_checked_at=clock_timestamp(), error_code=$3, error=$2,
            updated_at=clock_timestamp()
        WHERE operation_id=$1 AND state IN ('provisioning','destroying')`,
		id, detail, string(errorCode)); err != nil {
		return unavailable()
	}
	return nil
}

// reconcileTerminalResourceStates repairs rows left in a transient state by an
// operation that ended cancelled or failed before recording its resource state.
// It runs on the normal maintenance reconciliation path, so an already-stuck
// operation from an earlier crash converges without a repair command or a manual
// database edit. The operation_id predicate is the concurrency fence.
func (s *Service) reconcileTerminalResourceStates(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, `UPDATE eregion_platform_resources r
        SET state='degraded', health_state='unknown',
            health_detail=CASE r.state
                WHEN 'destroying' THEN 'Resource destroy was interrupted before a durable destroyed state; re-apply or re-destroy the resource explicitly.'
                ELSE 'Resource apply was interrupted before a durable ready state; re-apply or destroy the resource explicitly.'
            END,
            health_checked_at=clock_timestamp(),
            error_code=CASE WHEN o.error_code<>'' THEN o.error_code ELSE 'cancelled' END,
            error=CASE r.state
                WHEN 'destroying' THEN 'Resource destroy was interrupted before a durable destroyed state; re-apply or re-destroy the resource explicitly.'
                ELSE 'Resource apply was interrupted before a durable ready state; re-apply or destroy the resource explicitly.'
            END,
            updated_at=clock_timestamp()
        FROM eregion_operations o
        WHERE r.operation_id=o.id AND o.state IN ('cancelled','failed')
          AND r.state IN ('provisioning','destroying')`); err != nil {
		return unavailable()
	}
	return nil
}

// persistOwnedResourceVolumes merges newly planned volume identities into the
// row's cumulative ledger before any Docker volume is created. The operation ID
// fence rejects a worker that was superseded while preparing the effect.
func (s *Service) persistOwnedResourceVolumes(ctx context.Context, id contract.OperationID, resourceID platform.ResourceID, planned []platform.ResourceVolumeStatus) ([]platform.ResourceVolumeStatus, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, unavailable()
	}
	defer rollback(tx)
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT volumes FROM eregion_platform_resources
        WHERE id=$1 AND operation_id=$2 FOR UPDATE`, resourceID, id).Scan(&raw); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, contract.NewFault(contract.PreconditionFailed, "resource operation was superseded", "Refresh resource inventory and retry from its current operation.")
		}
		return nil, unavailable()
	}
	existing := []platform.ResourceVolumeStatus{}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &existing); err != nil {
			return nil, unavailable()
		}
	}
	volumes := platform.MergeResourceVolumes(existing, planned)
	encoded, err := json.Marshal(volumes)
	if err != nil {
		return nil, unavailable()
	}
	if _, err := tx.Exec(ctx, `UPDATE eregion_platform_resources
        SET volumes=$3, updated_at=clock_timestamp()
        WHERE id=$1 AND operation_id=$2`, resourceID, id, encoded); err != nil {
		return nil, unavailable()
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, unavailable()
	}
	return volumes, nil
}

func (s *Service) recordResourceContainer(ctx context.Context, id contract.OperationID, resourceID platform.ResourceID, plan platform.ResourcePlan, imageID, imageDigest string, volumes []platform.ResourceVolumeStatus) error {
	rawVolumes, err := json.Marshal(volumes)
	if err != nil {
		return unavailable()
	}
	_, err = s.pool.Exec(ctx, `UPDATE eregion_platform_resources
        SET image=$2, image_id=$3, image_digest=$4, container_name=$5, network_name=$6, volumes=$7,
            updated_at=clock_timestamp()
        WHERE id=$1 AND operation_id=$8`, resourceID, plan.Spec.Image, imageID, imageDigest, plan.Container,
		plan.Network, rawVolumes, id)
	if err != nil {
		return unavailable()
	}
	return nil
}

func (s *Service) resourceDeployability(ctx context.Context, env platform.Environment) ([]string, error) {
	if env.Revision == nil || len(env.Revision.Resources) == 0 {
		return nil, nil
	}
	missing := []string{}
	for _, name := range env.Revision.Resources {
		var state, health string
		err := s.pool.QueryRow(ctx, `SELECT state, health_state FROM eregion_platform_resources WHERE environment_id=$1 AND name=$2`, env.ID, name).Scan(&state, &health)
		if errors.Is(err, pgx.ErrNoRows) {
			missing = append(missing, "resource:"+name)
			continue
		}
		if err != nil {
			return nil, unavailable()
		}
		if state != string(platform.ResourceReady) || health != string(platform.HealthHealthy) {
			missing = append(missing, "resource:"+name)
		}
	}
	return missing, nil
}

func (s *Service) resourceDeployabilityTx(ctx context.Context, tx pgx.Tx, env platform.Environment) ([]string, error) {
	if env.Revision == nil || len(env.Revision.Resources) == 0 {
		return nil, nil
	}
	missing := []string{}
	for _, name := range env.Revision.Resources {
		var state, health string
		err := tx.QueryRow(ctx, `SELECT state, health_state FROM eregion_platform_resources WHERE environment_id=$1 AND name=$2`, env.ID, name).Scan(&state, &health)
		if errors.Is(err, pgx.ErrNoRows) {
			missing = append(missing, "resource:"+name)
			continue
		}
		if err != nil {
			return nil, unavailable()
		}
		if state != string(platform.ResourceReady) || health != string(platform.HealthHealthy) {
			missing = append(missing, "resource:"+name)
		}
	}
	return missing, nil
}

func (s *Service) inventoryResources(ctx context.Context, query platform.InventoryQuery) ([]platform.Resource, error) {
	statement := `SELECT ` + resourceColumns + `
        FROM eregion_platform_resources r
        JOIN eregion_platform_apps a ON a.id=r.app_id
        JOIN eregion_platform_environments e ON e.id=r.environment_id
        WHERE ($1='' OR a.name=$1) AND ($2='' OR e.name=$2)
          AND ($3='' OR r.state=$3) AND ($4='' OR r.node_name=$4)
        ORDER BY a.name,e.name,r.name LIMIT $5`
	rows, err := s.pool.Query(ctx, statement, query.App, query.Environment, query.State, query.Node, query.Limit)
	if err != nil {
		return nil, unavailable()
	}
	defer rows.Close()
	items := []platform.Resource{}
	for rows.Next() {
		resource, err := scanResourceRow(rows)
		if err != nil {
			return nil, unavailable()
		}
		items = append(items, resource)
	}
	return items, rows.Err()
}

func (s *Service) resolveResourceSecrets(ctx context.Context, env platform.Environment, spec platform.ResourceSpec) (map[string]string, string, error) {
	if len(spec.SecretEnv) == 0 {
		return map[string]string{}, "", nil
	}
	if s.secrets == nil {
		return nil, "", contract.NewFault(contract.PreconditionFailed, "secret injection is not configured", "Set EREGION_VAULT_KEY_FILE on every control-plane worker before applying a resource with secret references.")
	}
	snapshot, ok := s.secrets.(EnvironmentSecretSnapshotResolver)
	if !ok {
		return nil, "", contract.NewFault(contract.PreconditionFailed, "secret name resolution is not configured", "Use a worker whose secret resolver supports environment secret names.")
	}
	names := make([]string, 0, len(spec.SecretEnv))
	seen := map[string]bool{}
	for _, name := range spec.SecretEnv {
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	values, revisions, err := snapshot.ResolveEnvironmentSnapshot(ctx, env.App, env.Name, names, nil)
	if err != nil {
		return nil, "", contract.NewFault(contract.PreconditionFailed, "resource secret references could not be resolved", "Set every referenced secret name in this environment's vault, then apply the resource again.")
	}
	out := make(map[string]string, len(spec.SecretEnv))
	for key, name := range spec.SecretEnv {
		value := values[name]
		if value == "" {
			return nil, "", contract.NewFault(contract.PreconditionFailed, "resource secret "+name+" is missing", "Set the secret in this environment's vault with secret set, then apply the resource again.")
		}
		out[key] = value
	}
	return out, resourceSecretRevisionFingerprint(revisions), nil
}

// resourceSecretRevisionFingerprint hashes only non-secret revision metadata.
// It lets an explicit apply replace a container after secret rotation without
// writing any value into the payload, journal or database.
func resourceSecretRevisionFingerprint(revisions map[string]vault.RevisionRef) string {
	if len(revisions) == 0 {
		return ""
	}
	type entry struct {
		Name     string `json:"name"`
		Revision uint64 `json:"revision"`
		Hash     string `json:"hash"`
		KeyID    string `json:"key_id"`
	}
	entries := make([]entry, 0, len(revisions))
	for name, revision := range revisions {
		entries = append(entries, entry{Name: name, Revision: revision.Revision, Hash: revision.Hash, KeyID: revision.KeyID})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	raw, _ := json.Marshal(entries)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
