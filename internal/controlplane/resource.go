package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/vault"
)

func validateStoredResource(resource platform.Resource) error {
	if !resource.State.Valid() || (resource.HealthState != platform.HealthUnknown && resource.HealthState != platform.HealthHealthy && resource.HealthState != platform.HealthUnhealthy) || resource.ID.Validate() != nil || resource.Volumes == nil {
		return pointerIntegrityFault()
	}
	spec, err := platform.NormalizeResourceSpec(resource.Spec)
	if err != nil {
		return pointerIntegrityFault()
	}
	prefix := "er-res-" + strings.TrimPrefix(string(resource.ID), "res_") + "-"
	seen := map[string]bool{}
	for _, volume := range resource.Volumes {
		if !platform.ResourceVolumeNameValid(volume.Name) || !strings.HasPrefix(volume.Name, prefix) || seen[volume.Name] {
			return pointerIntegrityFault()
		}
		seen[volume.Name] = true
		check := spec
		check.Mounts = []platform.ResourceMount{{Name: strings.TrimPrefix(volume.Name, prefix), Destination: volume.Destination, ReadOnly: volume.ReadOnly}}
		if _, err := platform.NormalizeResourceSpec(check); err != nil {
			return pointerIntegrityFault()
		}
	}
	return nil
}

func resourceByNameTx(ctx context.Context, tx *entities.StoreTx, app, environment, name string, lock bool) (platform.Resource, error) {
	resource, found, err := tx.Resources().GetByName(ctx, app, environment, name, lock)
	if err != nil {
		return resource, unavailable(err)
	}
	if !found {
		return resource, contract.NewFault(contract.NotFound, "resource not found", "Apply the resource declaration first; inventory lists resource identities.")
	}
	if err := validateStoredResource(resource); err != nil {
		return resource, err
	}
	return resource, nil
}

func (s *Service) resourceByName(ctx context.Context, app, environment, name string) (platform.Resource, error) {
	resource, found, err := s.entities.Resources().GetByName(ctx, app, environment, name, false)
	if err != nil {
		return resource, unavailable(err)
	}
	if !found {
		return resource, contract.NewFault(contract.NotFound, "resource not found", "Apply the resource declaration first; inventory lists resource identities.")
	}
	if err := validateStoredResource(resource); err != nil {
		return resource, err
	}
	return resource, nil
}

func (s *Service) prepareResourceApply(ctx context.Context, tx *entities.StoreTx, id contract.OperationID, input *platform.ResourceApplyInput) error {
	env, err := environmentByNameTx(ctx, tx, input.App, input.Environment)
	if err != nil {
		return err
	}
	if err := s.checkPlacementByTx(ctx, tx, env.Node); err != nil {
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
		if err := s.validateResourceSecrets(ctx, tx, input); err != nil {
			return err
		}
	}
	existing, found, err := resourceByNameTxOptional(ctx, tx, input.App, input.Environment, input.Name)
	if err != nil {
		return err
	}
	if found {
		return resetResourceForApply(ctx, tx, id, input, env, existing)
	}
	resourceID, err := platform.NewResourceID()
	if err != nil {
		return unavailable()
	}
	if _, err := tx.Resources().Insert(ctx, entities.ResourceInsert{
		ID: resourceID, AppID: envAppID(ctx, tx, input.App), EnvironmentID: env.ID,
		Name: input.Name, Node: env.Node, Spec: input.ResourceSpec, Image: input.Image, OperationID: id,
	}); err != nil {
		return platformWriteError(err, "resource name already exists in this environment")
	}
	return nil
}

func (s *Service) validateResourceSecrets(ctx context.Context, tx *entities.StoreTx, input *platform.ResourceApplyInput) error {
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
		metadata, err := s.vault.Store().GetForUpdateTx(ctx, tx.Secrets(), namespace)
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
	if len(missing) == 0 {
		return nil
	}
	fault := contract.NewFault(contract.PreconditionFailed, "resource requires active secrets: "+strings.Join(missing, ", "), "Set the missing or archived secrets in this environment, then apply the resource again.")
	fault.Details = map[string]any{"reason": "missing_secret_values", "names": missing}
	return fault
}

func resetResourceForApply(ctx context.Context, tx *entities.StoreTx, id contract.OperationID, input *platform.ResourceApplyInput, env platform.Environment, existing platform.Resource) error {
	if existing.Node != env.Node {
		return contract.NewFault(contract.PreconditionFailed, "resource is bound to node "+existing.Node, "A resource with persistent volumes is never moved implicitly. Destroy it on "+existing.Node+" first; data volumes stay there, and a new resource identity must be declared for another node.")
	}
	if existing.State == platform.ResourceProvisioning || existing.State == platform.ResourceDestroying {
		return contract.NewFault(contract.Conflict, "resource is already changing", "Wait for the active resource operation to finish or cancel it explicitly.")
	}
	if _, err := tx.Resources().ResetForApply(ctx, entities.ResourceReset{
		ID: existing.ID, Node: env.Node, Spec: input.ResourceSpec, Image: input.Image, OperationID: id,
	}); err != nil {
		return unavailable(err)
	}
	return nil
}

func (s *Service) prepareResourceDestroy(ctx context.Context, tx *entities.StoreTx, id contract.OperationID, input *platform.ResourceDestroyInput) error {
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
	if err := resourceDestroyBlocked(ctx, tx.Platform().Environments(), tx.Platform().Revisions(), tx.Platform().Deployments(), env, input.Name); err != nil {
		return err
	}
	if err := ensureNoActiveParentDeleteTx(ctx, tx, id, input.App, input.Environment); err != nil {
		return err
	}
	if err := ensureNoActiveResourceOperationTx(ctx, tx, id, input.Target().String()); err != nil {
		return err
	}
	if _, err := tx.Resources().MarkDestroying(ctx, resource.ID, id); err != nil {
		return unavailable(err)
	}
	return nil
}

// resourceByNameTxOptional distinguishes an absent declaration from a database
// failure during apply, where the first declaration has no row yet.
func resourceByNameTxOptional(ctx context.Context, tx *entities.StoreTx, app, environment, name string) (platform.Resource, bool, error) {
	resource, err := resourceByNameTx(ctx, tx, app, environment, name, true)
	if err == nil {
		return resource, true, nil
	}
	if contract.Classify(err).Code == contract.NotFound {
		return platform.Resource{}, false, nil
	}
	return platform.Resource{}, false, err
}

// resourceInUse reports whether the current revision or the serving deployment
// still attaches the named resource. The caller must hold the environment row
// lock (submission) or the environment target lock (worker), so a revision or
// serving switch cannot pass between this read and the action it guards.
func resourceInUse(ctx context.Context, environments *entities.EnvironmentStore, revisions *entities.RevisionStore, app, environment, name string) (bool, string, error) {
	pointers, found, err := environments.RevisionPointers(ctx, app, environment)
	if err != nil {
		return false, "", unavailable(err)
	}
	if !found {
		return false, "", contract.NewFault(contract.NotFound, "environment not found", "The resource's environment no longer exists.")
	}
	for _, revision := range []platform.RevisionID{pointers.Current, pointers.Serving} {
		if revision == "" {
			continue
		}
		attached, err := revisions.UsesResource(ctx, revision, name)
		if err != nil {
			return false, "", unavailable(err)
		}
		if attached {
			detail := "Detach the resource from the current environment revision and deploy the detached revision, or stop the environment, before destroying it; its container and volumes are not touched by detach."
			if revision != pointers.Current {
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
func resourceDeploymentInUse(ctx context.Context, deployments *entities.DeploymentStore, environmentID platform.EnvironmentID, name string) (bool, error) {
	dependencies, err := deployments.ResourceDependencies(ctx, environmentID)
	if err != nil {
		return false, unavailable(err)
	}
	for _, dependency := range dependencies {
		mapped := resourceDeploymentDependency{
			RevisionID: string(dependency.RevisionID), Resources: dependency.Resources,
			PreviousResources: dependency.PreviousResources,
		}
		if mapped.UsesResource(name) {
			return true, nil
		}
	}
	return false, nil
}

// resourceDestroyBlocked is the shared refusal for submission and execution.
// The submission holds the environment row lock; the worker holds the
// environment target lock. Together with the resource lifecycle lock this
// prevents a destroy from racing a deployment switch or removing a dependency
// that a queued/running deployment still expects.
func resourceDestroyBlocked(ctx context.Context, environments *entities.EnvironmentStore, revisions *entities.RevisionStore, deployments *entities.DeploymentStore, env platform.Environment, name string) error {
	if inUse, detail, err := resourceInUse(ctx, environments, revisions, env.App, env.Name, name); err != nil {
		return err
	} else if inUse {
		return contract.NewFault(contract.PreconditionFailed, "resource is still referenced by an environment revision", detail)
	}
	if inUse, err := resourceDeploymentInUse(ctx, deployments, env.ID, name); err != nil {
		return err
	} else if inUse {
		return contract.NewFault(contract.Conflict, "a deployment that uses this resource is queued or running", "Wait for the deployment to finish or cancel it explicitly, then destroy the resource.")
	}
	return nil
}

func ensureNoActiveParentDeleteTx(ctx context.Context, tx *entities.StoreTx, id contract.OperationID, app, environment string) error {
	active, err := tx.Operations().CountActiveParentDeletes(ctx, entities.ParentDeleteGuard{
		ExcludeID: id, AppTarget: "app/" + app, EnvironmentTarget: "env/" + app + "/" + environment,
		AppDeleteKind: platform.KindAppDelete, EnvironmentDeleteKind: platform.KindEnvironmentDelete,
	})
	if err != nil {
		return unavailable(err)
	}
	if active > 0 {
		return contract.NewFault(contract.Conflict, "an application or environment deletion is already active", "Wait for it to finish before changing a managed resource.")
	}
	return nil
}

func ensureNoActiveResourceOperationTx(ctx context.Context, tx *entities.StoreTx, id contract.OperationID, target string) error {
	active, err := tx.Operations().CountActiveResourceOperations(ctx, id, target)
	if err != nil {
		return unavailable(err)
	}
	if active > 0 {
		return contract.NewFault(contract.Conflict, "another operation is active for this resource", "Wait for it to finish or cancel it explicitly.")
	}
	return nil
}

type deletionResourceBlocker struct {
	environment string
	name        string
	state       string
}

// liveDeletionResources is shared by the read-only preview and locked
// admission. Destruction state, not attachment or health, defines the guard.
func liveDeletionResources(ctx context.Context, resources *entities.ResourceStore, appID platform.AppID, envID platform.EnvironmentID) ([]deletionResourceBlocker, error) {
	var (
		records []entities.ResourceBlocker
		err     error
	)
	if envID != "" {
		records, err = resources.LiveForEnvironment(ctx, envID)
	} else {
		records, err = resources.LiveForApp(ctx, appID)
	}
	if err != nil {
		return nil, unavailable(err)
	}
	blockers := make([]deletionResourceBlocker, 0, len(records))
	for _, record := range records {
		blockers = append(blockers, deletionResourceBlocker{
			environment: record.Environment, name: record.Name, state: record.State,
		})
	}
	return blockers, nil
}

// ensureNoLiveResourcesTx rechecks the preview predicate under admission locks.
func (s *Service) ensureNoLiveResourcesTx(ctx context.Context, tx *entities.StoreTx, appID string, envID string) error {
	blockers, err := liveDeletionResources(ctx, tx.Resources(), platform.AppID(appID), platform.EnvironmentID(envID))
	if err != nil {
		return err
	}
	names := make([]string, 0, len(blockers))
	for _, blocker := range blockers {
		names = append(names, blocker.name)
	}
	if len(names) > 0 {
		return contract.NewFault(contract.PreconditionFailed,
			"resources still exist for this target: "+strings.Join(names, ", "),
			"Destroy each named resource first; named data volumes are retained and application or environment deletion never removes them.")
	}
	return nil
}

func (s *Service) resourceTargets(ctx context.Context, app, environment string) ([]string, error) {
	targets, err := s.entities.Resources().Targets(ctx, app, environment)
	if err != nil {
		return nil, unavailable(err)
	}
	if targets == nil {
		targets = []string{}
	}
	return targets, nil
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

func (w *platformWorker) prepareResourceImage(ctx context.Context, tr node.Transport, id contract.OperationID, attempt uint32, plan platform.ResourcePlan) (string, string, error) {
	networkStep := platform.ResourceNetworkStepID(plan)
	if _, err := w.runStepOn(ctx, tr, id, attempt, networkStep, platform.ArgvPayload{
		Step: networkStep,
		Argv: []string{"docker", "network", "create", "--driver", "bridge",
			"--label", "io.eregion.app=" + plan.App,
			"--label", "io.eregion.environment=" + plan.Environment, plan.Network},
		CheckArgv:      []string{"docker", "network", "inspect", "--format", "{{.Name}}", plan.Network},
		TimeoutSeconds: 30,
	}, nil); err != nil {
		return "", "", err
	}
	if err := w.service.checkCancelled(ctx, id); err != nil {
		return "", "", err
	}
	pullStep := platform.ResourcePullStepID(plan)
	if _, err := w.runStepOn(ctx, tr, id, attempt, pullStep, platform.ArgvPayload{
		Step: pullStep, Argv: []string{"docker", "pull", plan.Spec.Image}, TimeoutSeconds: 600,
	}, nil); err != nil {
		return "", "", err
	}
	if err := w.service.checkCancelled(ctx, id); err != nil {
		return "", "", err
	}
	imageStep := platform.ResourceImageStepID(plan)
	imageRaw, err := w.runStepOn(ctx, tr, id, attempt, imageStep, platform.ArgvPayload{
		Step:           imageStep,
		Argv:           []string{"docker", "image", "inspect", "--format", platform.ResourceImageInspectFormat, plan.Spec.Image},
		TimeoutSeconds: 30,
	}, nil)
	if err != nil {
		return "", "", err
	}
	if err := w.service.checkCancelled(ctx, id); err != nil {
		return "", "", err
	}
	imageID, repoDigests, err := platform.ParseResourceImageInspect(stepStdout(imageRaw))
	if err != nil {
		return "", "", contract.NewFault(contract.PreconditionFailed, "the resource image has no immutable identity", err.Error())
	}
	return imageID, platform.ResourceImageDigest(plan.Spec.Image, repoDigests), nil
}

func (w *platformWorker) createResourceVolumes(ctx context.Context, tr node.Transport, id contract.OperationID, attempt uint32, plan platform.ResourcePlan, env platform.Environment, resource platform.Resource) error {
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
	return nil
}

type resourceApplyRun struct {
	resource          platform.Resource
	env               platform.Environment
	plan              platform.ResourcePlan
	tr                node.Transport
	secretValues      map[string]string
	secretFingerprint string
}

func (w *platformWorker) prepareResourceApplyRun(ctx context.Context, id contract.OperationID, input *platform.ResourceApplyInput) (*resourceApplyRun, error) {
	resource, err := w.service.resourceByName(ctx, input.App, input.Environment, input.Name)
	if err != nil {
		return nil, err
	}
	if resource.OperationID != id {
		return nil, contract.NewFault(contract.PreconditionFailed, "resource operation was superseded", "Refresh resource inventory and retry from its current operation.")
	}
	env, err := w.service.environment(ctx, input.App, input.Environment)
	if err != nil {
		return nil, err
	}
	if env.ID == "" {
		return nil, contract.NewFault(contract.NotFound, "environment not found", "Create the environment before applying a resource.")
	}
	if err := w.service.checkCancelled(ctx, id); err != nil {
		return nil, err
	}
	plan, err := platform.PlanResource(env.ID, resource.Node, resource.ID, env.App, env.Name, resource.Name, resource.Spec)
	if err != nil {
		return nil, err
	}
	secretValues, secretFingerprint, err := w.service.resolveResourceSecrets(ctx, env, resource.Spec)
	if err != nil {
		return nil, err
	}
	tr, err := w.service.resolver.TransportFor(ctx, resource.Node)
	if err != nil {
		return nil, err
	}
	return &resourceApplyRun{
		resource: resource, env: env, plan: plan, tr: tr,
		secretValues: secretValues, secretFingerprint: secretFingerprint,
	}, nil
}

func (w *platformWorker) applyResource(ctx context.Context, id contract.OperationID, attempt uint32, input *platform.ResourceApplyInput) error {
	run, err := w.prepareResourceApplyRun(ctx, id, input)
	if err != nil {
		return err
	}
	resource, env, plan, tr := run.resource, run.env, run.plan, run.tr
	secretValues, secretFingerprint := run.secretValues, run.secretFingerprint
	secretKeys := platform.SortedResourceSecretEnvKeys(secretValues)

	imageID, imageDigest, err := w.prepareResourceImage(ctx, tr, id, attempt, plan)
	if err != nil {
		return err
	}

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
	if err := w.createResourceVolumes(ctx, tr, id, attempt, plan, env, resource); err != nil {
		return err
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

	if err := w.service.checkCancelled(ctx, id); err != nil {
		return err
	}
	return w.service.finishResourceOperation(ctx, id, attempt, func(tx *entities.StoreTx, op *contract.Operation) error {
		rows, err := tx.Resources().MarkReady(ctx, entities.ResourceReady{
			ID: resource.ID, OperationID: id, Image: plan.Spec.Image, ImageID: imageID,
			ImageDigest: imageDigest, ContainerName: plan.Container, NetworkName: plan.Network,
			Volumes: ownedVolumes,
		})
		if err != nil {
			return unavailable(err)
		}
		if rows == 0 {
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

func (w *platformWorker) deleteResourceVolumes(ctx context.Context, tr node.Transport, id contract.OperationID, attempt uint32, deleteData bool, plan platform.ResourcePlan, env platform.Environment, resource platform.Resource) ([]platform.ResourceVolumeStatus, error) {
	if !deleteData {
		return []platform.ResourceVolumeStatus{}, nil
	}
	if err := w.service.checkCancelled(ctx, id); err != nil {
		return nil, err
	}
	deleteVolumes := platform.ResourceVolumesForDestroy(resource.Volumes, plan)
	for index, volume := range deleteVolumes {
		removeStep := platform.ResourceVolumeRemoveStepID(plan, index)
		if _, err := w.runStepOn(ctx, tr, id, attempt, removeStep, platform.ResourceVolumeRemovePayload{
			Step: removeStep, Volume: volume.Name, App: env.App, Environment: env.Name,
			Resource: resource.Name, ResourceID: string(resource.ID),
		}, nil); err != nil {
			return nil, err
		}
		if err := w.service.checkCancelled(ctx, id); err != nil {
			return nil, err
		}
	}
	return deleteVolumes, nil
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
	if err := resourceDestroyBlocked(ctx, w.service.entities.Platform().Environments(), w.service.entities.Platform().Revisions(), w.service.entities.Platform().Deployments(), env, resource.Name); err != nil {
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
	deleteVolumes, err := w.deleteResourceVolumes(ctx, tr, id, attempt, input.DeleteData, plan, env, resource)
	if err != nil {
		return err
	}
	if err := w.service.checkCancelled(ctx, id); err != nil {
		return err
	}
	return w.service.finishResourceOperation(ctx, id, attempt, func(tx *entities.StoreTx, op *contract.Operation) error {
		return finishDestroyedResource(ctx, tx, op, resource, id, input.DeleteData, ownedVolumes, deleteVolumes)
	})
}

func finishDestroyedResource(ctx context.Context, tx *entities.StoreTx, op *contract.Operation, resource platform.Resource, id contract.OperationID, deleteData bool, ownedVolumes, deleteVolumes []platform.ResourceVolumeStatus) error {
	detail := "Container removed; named volumes were retained."
	destroyVolumes := ownedVolumes
	if deleteData {
		destroyVolumes = []platform.ResourceVolumeStatus{}
		if len(deleteVolumes) == 0 {
			detail = "Container removed; no owned named volumes were recorded to delete."
		} else {
			detail = "Container and named volumes removed; resource data was deleted."
		}
	}
	rows, err := tx.Resources().MarkDestroyed(ctx, entities.ResourceDestroyed{
		ID: resource.ID, OperationID: id, Volumes: destroyVolumes, Detail: detail,
	})
	if err != nil {
		return unavailable(err)
	}
	if rows == 0 {
		return contract.NewFault(contract.PreconditionFailed, "resource changed while it was being destroyed", "Refresh resource inventory before retrying.")
	}
	return appendEvent(ctx, tx, op, contract.Event{
		Kind: "log", Stage: "resource", Message: "Resource " + resource.Name + ": " + detail,
	})
}

// finishResourceOperation finalizes an apply or destroy under the operation row
// lock. A cancellation accepted after the last step-boundary check but before
// this transaction must not be converted into success: return the normal
// cancellation error so the resource settles to degraded through the normal
// cancellation path. This is deliberately scoped to resources; other operation
// completion policy is unchanged.
func (s *Service) finishResourceOperation(ctx context.Context, id contract.OperationID, attempt uint32, finalize func(*entities.StoreTx, *contract.Operation) error) error {
	if s.beforeResourceFinalize != nil {
		s.beforeResourceFinalize()
	}
	_, owned, err := s.mutateOwned(ctx, id, attempt, func(tx *entities.StoreTx, op *contract.Operation) error {
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
	if _, err := s.entities.Resources().SetStateForTarget(ctx, id, target, state); err != nil {
		return unavailable(err)
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
	_, _ = s.entities.Resources().RecordFailure(ctx, entities.ResourceFailure{
		App: app, Environment: environment, Name: name, OperationID: id,
		Code: fault.Code, Message: message,
	})
}

// settleResourceForTerminalOperationTx moves a resource out of a transient
// provisioning/destroying state when its owning operation has become cancelled
// or failed. It is fenced by operation_id, so a newer resource operation is
// never clobbered. The container identity, image and cumulative volume ledger
// are intentionally preserved: a later apply reuses them, while a later
// destroy can still find every owned volume and the possibly existing container.
func settleResourceForTerminalOperationTx(ctx context.Context, tx *entities.StoreTx, id contract.OperationID, kind string, code contract.Code) error {
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
	if _, err := tx.Resources().SettleTerminal(ctx, id, errorCode, detail); err != nil {
		return unavailable(err)
	}
	return nil
}

// reconcileTerminalResourceStates repairs rows left in a transient state by an
// operation that ended cancelled or failed before recording its resource state.
// It runs on the normal maintenance reconciliation path, so an already-stuck
// operation from an earlier crash converges without a repair command or a manual
// database edit. The operation_id predicate is the concurrency fence.
func (s *Service) reconcileTerminalResourceStates(ctx context.Context) error {
	if _, err := s.entities.Resources().ReconcileTerminal(ctx); err != nil {
		return unavailable(err)
	}
	return nil
}

// persistOwnedResourceVolumes merges newly planned volume identities into the
// row's cumulative ledger before any Docker volume is created. The operation ID
// fence rejects a worker that was superseded while preparing the effect.
func (s *Service) persistOwnedResourceVolumes(ctx context.Context, id contract.OperationID, resourceID platform.ResourceID, planned []platform.ResourceVolumeStatus) ([]platform.ResourceVolumeStatus, error) {
	var volumes []platform.ResourceVolumeStatus
	err := s.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		raw, found, err := tx.Resources().LockVolumes(ctx, resourceID, id)
		if err != nil {
			return unavailable(err)
		}
		if !found {
			return contract.NewFault(contract.PreconditionFailed, "resource operation was superseded", "Refresh resource inventory and retry from its current operation.")
		}
		existing := []platform.ResourceVolumeStatus{}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &existing); err != nil {
				return unavailable(err)
			}
		}
		volumes = platform.MergeResourceVolumes(existing, planned)
		encoded, err := json.Marshal(volumes)
		if err != nil {
			return unavailable(err)
		}
		if _, err := tx.Resources().SetVolumes(ctx, resourceID, id, encoded); err != nil {
			return unavailable(err)
		}
		return nil
	})
	if err != nil {
		if entities.IsBeginFailure(err) || entities.IsCommitFailure(err) {
			return nil, unavailable(err)
		}
		return nil, err
	}
	return volumes, nil
}

func (s *Service) recordResourceContainer(ctx context.Context, id contract.OperationID, resourceID platform.ResourceID, plan platform.ResourcePlan, imageID, imageDigest string, volumes []platform.ResourceVolumeStatus) error {
	if _, err := s.entities.Resources().RecordContainer(ctx, entities.ResourceContainer{
		ID: resourceID, OperationID: id, Image: plan.Spec.Image, ImageID: imageID,
		ImageDigest: imageDigest, ContainerName: plan.Container, NetworkName: plan.Network,
		Volumes: volumes,
	}); err != nil {
		return unavailable(err)
	}
	return nil
}

func (s *Service) resourceDeployability(ctx context.Context, env platform.Environment) ([]string, error) {
	if env.Revision == nil || len(env.Revision.Resources) == 0 {
		return nil, nil
	}
	missing := []string{}
	for _, name := range env.Revision.Resources {
		state, found, err := s.entities.Resources().State(ctx, env.ID, name)
		if err != nil {
			return nil, unavailable(err)
		}
		if !found || state.State != platform.ResourceReady || state.Health != platform.HealthHealthy {
			missing = append(missing, "resource:"+name)
		}
	}
	return missing, nil
}

func (s *Service) resourceDeployabilityTx(ctx context.Context, tx *entities.StoreTx, env platform.Environment) ([]string, error) {
	if env.Revision == nil || len(env.Revision.Resources) == 0 {
		return nil, nil
	}
	missing := []string{}
	for _, name := range env.Revision.Resources {
		state, found, err := tx.Resources().State(ctx, env.ID, name)
		if err != nil {
			return nil, unavailable(err)
		}
		if !found || state.State != platform.ResourceReady || state.Health != platform.HealthHealthy {
			missing = append(missing, "resource:"+name)
		}
	}
	return missing, nil
}

func (s *Service) inventoryResources(ctx context.Context, query platform.InventoryQuery) ([]platform.Resource, error) {
	items, err := s.entities.Resources().List(ctx, entities.ResourceFilter{
		App: query.App, Environment: query.Environment, State: query.State,
		Node: query.Node, Limit: query.Limit,
	})
	if err != nil {
		return nil, unavailable(err)
	}
	for _, resource := range items {
		if err := validateStoredResource(resource); err != nil {
			return nil, err
		}
	}
	if items == nil {
		items = []platform.Resource{}
	}
	return items, nil
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
