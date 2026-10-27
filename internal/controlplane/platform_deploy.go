package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/vault"
)

// ----------------------------------------------------------------- builds

func (w *platformWorker) runRelease(ctx context.Context, id contract.OperationID, attempt uint32, normalized platform.Normalized) error {
	input := normalized.Input.(*platform.ReleaseCreateInput)
	env, err := w.service.environment(ctx, input.App, input.Environment)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	if err := crossNodeBuildUnsupported(env, input.Source.Type); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	nodeName := env.Node
	role, capability := platform.RoleApplication, platform.CapabilityDocker
	if input.Source.Type != "image" {
		nodeName = env.BuildNodeName()
		role, capability = platform.RoleBuilder, platform.CapabilityBuildpacks
	}
	n, err := w.service.node(ctx, nodeName)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	if err := w.service.checkPlacement(n, role, capability); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	tr, err := w.service.resolver.TransportFor(ctx, n.Name)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	release, err := w.service.releaseForOperation(ctx, id)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	buildSecrets := map[string]string{}
	if input.Source.Type == "git" && input.Source.CredentialSecretName != "" {
		value, _, resolveErr := w.resolveCredentialSecret(ctx, env, input.Source.CredentialSecretName)
		if resolveErr != nil {
			return w.settle(ctx, id, attempt, resolveErr)
		}
		buildSecrets[input.Source.CredentialSecretName] = value
	}
	if input.Source.Type == "image" {
		release, err = w.verifyReleaseImage(ctx, id, attempt, release, tr, input.Pull)
	} else {
		release, err = w.buildRelease(ctx, id, attempt, env, release, tr, buildSecrets)
	}
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	if err := w.service.finishPlatformOperation(ctx, id, attempt, func(tx pgx.Tx, op *contract.Operation) error {
		if err := appendEvent(ctx, tx, op, contract.Event{
			Kind: "log", Stage: "release", ReleaseID: string(release.ID),
			Message: "Release " + string(release.ID) + " is ready with image " + release.ImageID + ".",
		}); err != nil {
			return err
		}
		return insertPlatformChange(ctx, tx, op, "release.ready", release.App, release.Environment,
			"Release "+string(release.ID)+" is ready from "+release.Source.Type+" source.",
			map[string]any{"release_id": release.ID, "image": release.Image},
			map[string]any{"release_id": release.ID, "image": release.Image, "image_id": release.ImageID, "commit": release.CommitSHA})
	}); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	return nil
}

func (w *platformWorker) runBuildRetry(ctx context.Context, id contract.OperationID, attempt uint32, normalized platform.Normalized) error {
	input := normalized.Input.(*platform.BuildRetryInput)
	old, err := w.service.buildByID(ctx, input.BuildID)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	env, err := w.service.environment(ctx, old.App, old.Environment)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	nodeName := env.Node
	role, capability := platform.RoleApplication, platform.CapabilityDocker
	if old.Source.Type != "image" {
		nodeName = env.BuildNodeName()
		role, capability = platform.RoleBuilder, platform.CapabilityBuildpacks
	}
	n, err := w.service.node(ctx, nodeName)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	if err := w.service.checkPlacement(n, role, capability); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	tr, err := w.service.resolver.TransportFor(ctx, n.Name)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	release, err := w.service.releaseForOperation(ctx, id)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	buildSecrets := map[string]string{}
	if old.Source.CredentialSecretName != "" {
		value, _, err := w.resolveCredentialSecret(ctx, env, old.Source.CredentialSecretName)
		if err != nil {
			return w.settle(ctx, id, attempt, err)
		}
		buildSecrets[old.Source.CredentialSecretName] = value
	}
	if old.Source.Type == "image" {
		release, err = w.verifyReleaseImage(ctx, id, attempt, release, tr, false)
	} else {
		release, err = w.buildRelease(ctx, id, attempt, env, release, tr, buildSecrets)
	}
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	if err := w.service.finishPlatformOperation(ctx, id, attempt, func(tx pgx.Tx, op *contract.Operation) error {
		return insertPlatformChange(ctx, tx, op, "build.succeeded", env.App, env.Name,
			"Retried build "+string(old.ID)+" at commit "+release.CommitSHA+".",
			map[string]any{"build_id": old.ID, "commit": old.CommitSHA},
			map[string]any{"release_id": release.ID, "image_id": release.ImageID})
	}); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	return nil
}

func (w *platformWorker) runBuildStop(ctx context.Context, id contract.OperationID, attempt uint32, normalized platform.Normalized) error {
	input := normalized.Input.(*platform.BuildStopInput)
	build, err := w.service.buildByID(ctx, input.BuildID)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	if build.State == platform.BuildSucceeded {
		return w.settle(ctx, id, attempt, contract.NewFault(contract.Conflict, "build already succeeded", "Retry with a new build after changing the source if another artifact is needed."))
	}
	env, err := w.service.environment(ctx, build.App, build.Environment)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	// Mark cancelled before signalling the worker so a pack failure is recorded
	// as cancellation rather than a new failure.
	if err := w.service.cancelBuild(ctx, build); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	if err := w.service.requestCancelOperation(ctx, build.OperationID, "Build stop requested; cancelling the pack process."); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	n, err := w.service.node(ctx, env.BuildNodeName())
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	tr, err := w.service.resolver.TransportFor(ctx, n.Name)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	if _, err := w.runStepOn(ctx, tr, id, attempt, deploymentBuildStopStep, platform.BuildStopPayload{
		Step: deploymentBuildStopStep, OperationID: string(build.OperationID), PackStep: deploymentPackStep,
	}, nil); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	if err := w.service.finishPlatformOperation(ctx, id, attempt, func(tx pgx.Tx, op *contract.Operation) error {
		return insertPlatformChange(ctx, tx, op, "build.stopped", build.App, build.Environment,
			"Stopped build "+string(build.ID)+".", map[string]any{"build_id": build.ID}, nil)
	}); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	return nil
}

// buildRelease executes Buildpacks for a pending release and records the exact
// commit, digest and build lifecycle. Its log stream is this operation's events.
func (w *platformWorker) buildRelease(ctx context.Context, id contract.OperationID, attempt uint32, env platform.Environment, release platform.Release, tr node.Transport, secretValues map[string]string) (platform.Release, error) {
	if release.Status == platform.ReleaseReady && release.ImageID != "" {
		return release, nil
	}
	build, has, err := w.service.buildByOperation(ctx, id)
	if err != nil {
		return release, err
	}
	if !has && release.BuildID != "" {
		build, err = w.service.buildByID(ctx, release.BuildID)
		if err != nil {
			return release, err
		}
		has = true
	}
	if !has {
		return release, contract.NewFault(contract.PreconditionFailed, "release has no build record", "Start the build from an environment revision or retry an existing build.")
	}
	if err := w.service.markBuildRunning(ctx, build.ID); err != nil {
		return release, err
	}
	if cancelled, _ := w.service.buildCancelled(ctx, build.ID); cancelled {
		return release, errPlatformCancelled
	}
	buildInfo := platform.Build{Builder: platform.DefaultBuilder}
	if release.Build != nil {
		buildInfo = *release.Build
	}
	if buildInfo.Builder == "" {
		buildInfo.Builder = platform.DefaultBuilder
	}
	gitRef := build.CommitSHA
	if gitRef == "" {
		gitRef = build.Source.Ref
	}
	payload := platform.PackPayload{
		Step: deploymentPackStep, Image: release.Image,
		Builder: buildInfo.Builder, Env: buildInfo.Env, TimeoutSeconds: 900,
	}
	switch build.Source.Type {
	case "git":
		payload.GitURL = build.Source.URL
		payload.GitRef = gitRef
		payload.GitCredentialSecretKey = build.Source.CredentialSecretName
	case "directory":
		payload.Path = build.Source.Path
	default:
		return release, contract.NewFault(contract.PreconditionFailed, "build source is not supported", "Use an HTTPS git source or an image.")
	}
	buildNode, err := w.service.node(ctx, env.BuildNodeName())
	if err != nil {
		return release, err
	}
	limit := buildLimit(buildNode)
	releaseSlot, err := w.service.acquireBuildSlot(ctx, buildNode.Name, limit, func() {
		w.service.appendWorkerEvent(ctx, id, attempt, deploymentBuildStageLabel,
			fmt.Sprintf("build_queued on node %s; all %d build slot(s) are busy", buildNode.Name, limit))
	})
	if err != nil {
		return release, err
	}
	defer releaseSlot()
	if cancelled, _ := w.service.buildCancelled(ctx, build.ID); cancelled {
		return release, errPlatformCancelled
	}
	w.service.appendWorkerEvent(ctx, id, attempt, deploymentBuildStageLabel,
		fmt.Sprintf("build_running on node %s; %d of %d build slot(s) in use; %s", buildNode.Name, 1, limit, build.Source.Type))
	buildSecrets := map[string]string{}
	if build.Source.CredentialSecretName != "" {
		value := secretValues[build.Source.CredentialSecretName]
		if value == "" {
			err := contract.NewFault(contract.PreconditionFailed, "git read credential is unavailable", "Set the revision's credential secret in the environment vault before building.")
			_ = w.service.markBuildFailed(ctx, build.ID, err, release.ID)
			return release, err
		}
		buildSecrets[build.Source.CredentialSecretName] = value
	}
	result, buildErr := w.runStepOn(ctx, tr, id, attempt, deploymentPackStep, payload, buildSecrets)
	if buildErr != nil {
		if state, _, stateErr := w.service.operationState(ctx, id); stateErr == nil && state == contract.Cancelling {
			_ = w.service.markBuildCancelled(ctx, build.ID)
			return release, errPlatformCancelled
		}
		if cancelled, _ := w.service.buildCancelled(ctx, build.ID); cancelled {
			return release, errPlatformCancelled
		}
		_ = w.service.markBuildFailed(ctx, build.ID, buildErr, release.ID)
		return release, buildErr
	}
	if cancelled, _ := w.service.buildCancelled(ctx, build.ID); cancelled {
		return release, errPlatformCancelled
	}
	commit := stepResultString(result, "commit")
	if build.Source.Type == "git" && commit == "" {
		fault := contract.NewFault(contract.PreconditionFailed, "build did not record an exact commit", "The git ref must resolve to a commit before the release can be deployed.")
		_ = w.service.markBuildFailed(ctx, build.ID, fault, release.ID)
		return release, fault
	}
	imageResult, err := w.runStepOn(ctx, tr, id, attempt, deploymentImageStep, platform.ArgvPayload{
		Step:           deploymentImageStep,
		Argv:           []string{"docker", "image", "inspect", "--format", "{{.Id}}", release.Image},
		TimeoutSeconds: 30,
	}, nil)
	if err != nil {
		_ = w.service.markBuildFailed(ctx, build.ID, err, release.ID)
		return release, err
	}
	imageID := stepStdout(imageResult)
	if !strings.HasPrefix(imageID, "sha256:") || len(imageID) != 71 {
		fault := contract.NewFault(contract.PreconditionFailed, "image inspection did not return an immutable image ID", "Inspect the node image and build operation before retrying.")
		_ = w.service.markBuildFailed(ctx, build.ID, fault, release.ID)
		return release, fault
	}
	if cancelled, _ := w.service.buildCancelled(ctx, build.ID); cancelled {
		return release, errPlatformCancelled
	}
	if err := w.service.markBuildSucceeded(ctx, build.ID, release.ID, commit, imageID); err != nil {
		return release, err
	}
	updated, err := w.service.releaseByID(ctx, release.ID)
	if err != nil {
		return release, err
	}
	if updated.Status != platform.ReleaseReady {
		return release, errPlatformCancelled
	}
	return updated, nil
}

func (w *platformWorker) verifyReleaseImage(ctx context.Context, id contract.OperationID, attempt uint32, release platform.Release, tr node.Transport, pull bool) (platform.Release, error) {
	if pull {
		if _, err := w.runStepOn(ctx, tr, id, attempt, deploymentPullStep, platform.ArgvPayload{
			Step: deploymentPullStep, Argv: []string{"docker", "pull", release.Image}, TimeoutSeconds: 600,
		}, nil); err != nil {
			return release, err
		}
	}
	result, err := w.runStepOn(ctx, tr, id, attempt, deploymentImageStep, platform.ArgvPayload{
		Step:           deploymentImageStep,
		Argv:           []string{"docker", "image", "inspect", "--format", "{{.Id}}", release.Image},
		TimeoutSeconds: 30,
	}, nil)
	if err != nil {
		return release, err
	}
	imageID := stepStdout(result)
	if !strings.HasPrefix(imageID, "sha256:") || len(imageID) != 71 {
		return release, contract.NewFault(contract.PreconditionFailed, "image inspection did not return an immutable image ID", "Inspect the node image and release operation before retrying.")
	}
	if _, err := w.service.pool.Exec(ctx, `UPDATE eregion_platform_releases
        SET status='ready',image_id=$2,completed_at=clock_timestamp(),error='',error_code=''
        WHERE id=$1`, release.ID, imageID); err != nil {
		return release, unavailable()
	}
	release.Status = platform.ReleaseReady
	release.ImageID = imageID
	return release, nil
}

// ------------------------------------------------------------ deployment

func (w *platformWorker) runDeployment(ctx context.Context, id contract.OperationID, attempt uint32, normalized platform.Normalized) error {
	dep, env, release, _, err := w.service.deploymentPlan(ctx, id)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	tr, err := w.service.resolver.TransportFor(ctx, env.Node)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	secretValues, credentialKey, _, secretRefs, err := w.resolveDeploymentSecrets(ctx, env, dep)
	if err != nil {
		return w.finishDeploymentFailure(ctx, tr, id, attempt, env, dep, nil, err)
	}
	needsBuild := release.Status != platform.ReleaseReady || release.ImageID == ""
	credentialValue := ""
	if needsBuild && credentialKey != "" && len(dep.SecretRevisions) == 0 {
		value, ref, resolveErr := w.resolveCredentialSecret(ctx, env, credentialKey)
		if resolveErr != nil {
			return w.finishDeploymentFailure(ctx, tr, id, attempt, env, dep, nil, resolveErr)
		}
		credentialValue = value
		secretRefs[credentialKey] = ref
	}
	if len(dep.SecretRevisions) == 0 {
		if err := w.service.recordDeploymentSecretSnapshot(ctx, id, env.RevisionID, secretRefs); err != nil {
			return w.settle(ctx, id, attempt, err)
		}
	}
	if err := w.service.checkCancelled(ctx, id); err != nil {
		return w.finishDeploymentFailure(ctx, tr, id, attempt, env, dep, nil, err)
	}

	// A deploy from the current revision records a pending release and build.
	if needsBuild && release.Source.Type != "image" {
		builder, err := w.service.node(ctx, env.BuildNodeName())
		if err != nil {
			return w.finishDeploymentFailure(ctx, tr, id, attempt, env, dep, nil, err)
		}
		if err := w.service.checkPlacement(builder, platform.RoleBuilder, platform.CapabilityBuildpacks); err != nil {
			return w.finishDeploymentFailure(ctx, tr, id, attempt, env, dep, nil, err)
		}
	}
	if needsBuild {
		buildSecrets := cloneMap(secretValues)
		if credentialValue != "" {
			buildSecrets[credentialKey] = credentialValue
		}
		if release.BuildID != "" || (release.Source.Type != "image" && release.Source.Type != "") {
			release, err = w.buildRelease(ctx, id, attempt, env, release, tr, buildSecrets)
		} else {
			// An image source may live only in the node's loopback registry.
			// Pull it by its recorded reference before inspecting the image ID.
			release, err = w.verifyReleaseImage(ctx, id, attempt, release, tr, release.Source.Type == "image")
		}
		if err != nil {
			return w.finishDeploymentFailure(ctx, tr, id, attempt, env, dep, nil, err)
		}
	}
	if release.Status != platform.ReleaseReady || release.ImageID == "" {
		return w.finishDeploymentFailure(ctx, tr, id, attempt, env, dep, nil,
			contract.NewFault(contract.PreconditionFailed, "release has no immutable image", "Build or inspect the release before deploying."))
	}
	if env.Revision == nil {
		return w.finishDeploymentFailure(ctx, tr, id, attempt, env, dep, nil,
			contract.NewFault(contract.PreconditionFailed, "deployment has no environment revision", "Deploy with an environment that has a current revision."))
	}

	// Preflight verifies the immutable image is present on the application node.
	if _, err := w.runStepOn(ctx, tr, id, attempt, deploymentPreflightStep, platform.ArgvPayload{
		Step:           deploymentPreflightStep,
		Argv:           []string{"docker", "image", "inspect", "--format", "{{.Id}}", release.ImageID},
		TimeoutSeconds: 30,
	}, nil); err != nil {
		return w.finishDeploymentFailure(ctx, tr, id, attempt, env, dep, nil, err)
	}

	network := platform.EnvironmentNetwork(env.ID)
	if _, err := w.runStepOn(ctx, tr, id, attempt, deploymentNetworkStep, platform.ArgvPayload{
		Step: deploymentNetworkStep,
		Argv: []string{"docker", "network", "create", "--driver", "bridge",
			"--label", "io.eregion.app=" + env.App, "--label", "io.eregion.environment=" + env.Name, network},
		CheckArgv:      []string{"docker", "network", "inspect", "--format", "{{.Name}}", network},
		TimeoutSeconds: 30,
	}, nil); err != nil {
		return w.finishDeploymentFailure(ctx, tr, id, attempt, env, dep, nil, err)
	}
	containerEnv := cloneMap(env.Revision.Config)
	if credentialKey != "" && !contains(env.Revision.SecretNames, credentialKey) {
		delete(secretValues, credentialKey)
	}
	releaseCommand := env.Revision.Runtime.ReleaseCommand
	if releaseCommand == nil {
		releaseCommand = release.Command
	}
	if releaseCommand != nil {
		commandArgs := []string{"docker", "run", "--rm", "--name", workloadCommandName(env, dep.ID), "--pull", "never", "--network", network}
		for _, key := range sortedPlatformKeys(containerEnv) {
			commandArgs = append(commandArgs, "-e", key)
		}
		for _, key := range sortedPlatformKeys(secretValues) {
			commandArgs = append(commandArgs, "-e", key)
		}
		commandArgs = append(commandArgs, release.ImageID)
		commandArgs = append(commandArgs, releaseCommand.Argv...)
		if _, err := w.runStepOn(ctx, tr, id, attempt, deploymentCommandStep, platform.ArgvPayload{
			Step: deploymentCommandStep, Argv: commandArgs,
			Env: containerEnv, SecretEnvKeys: sortedPlatformKeys(secretValues),
			TimeoutSeconds: releaseCommand.TimeoutSeconds,
		}, secretValues); err != nil {
			return w.finishDeploymentFailure(ctx, tr, id, attempt, env, dep, nil, err)
		}
	}
	if err := w.service.checkCancelled(ctx, id); err != nil {
		return w.finishDeploymentFailure(ctx, tr, id, attempt, env, dep, nil, err)
	}

	plan, err := platform.PlanDeployment(env.App, env.Name, env.Revision.RevisionState, release.ID, dep.ID)
	if err != nil {
		return w.finishDeploymentFailure(ctx, tr, id, attempt, env, dep, nil, err)
	}
	instances := make([]platform.DeploymentInstance, 0, len(plan))
	allCandidates := make([]platform.DeploymentInstance, 0, len(plan))
	for _, planned := range plan {
		allCandidates = append(allCandidates, platform.DeploymentInstance{Name: planned.Name, Process: planned.Process.Name, Kind: planned.Process.Kind, Replica: planned.Replica})
	}
	failWithCandidates := func(cause error) error {
		return w.finishDeploymentFailure(ctx, tr, id, attempt, env, dep, allCandidates, cause)
	}
	for _, planned := range plan {
		processEnv := cloneMap(containerEnv)
		if planned.Publish {
			processEnv["PORT"] = fmt.Sprint(planned.ContainerPort)
		} else {
			delete(processEnv, "PORT")
		}
		result, err := w.runStepOn(ctx, tr, id, attempt, containerStepID(planned.Process.Name, planned.Replica), platform.ContainerPayload{
			Step:          containerStepID(planned.Process.Name, planned.Replica),
			Name:          planned.Name,
			Image:         release.ImageID,
			ContainerPort: planned.ContainerPort,
			Publish:       planned.Publish,
			Bind:          "127.0.0.1",
			Env:           processEnv,
			SecretEnvKeys: sortedPlatformKeys(secretValues),
			Command:       planned.Command,
			Labels:        planned.Labels,
			Restart:       "unless-stopped",
			Network:       network,
			NetworkAlias:  networkAlias(planned.Process),
		}, secretValues)
		if err != nil {
			return failWithCandidates(err)
		}
		instance := platform.DeploymentInstance{
			Name: planned.Name, Process: planned.Process.Name, Kind: planned.Process.Kind,
			Replica: planned.Replica, ReleaseID: release.ID, State: "running",
		}
		instance.HostPort = stepResultInt(result, "host_port")
		instance.ContainerID = stepResultString(result, "container_id")
		if planned.Publish && instance.HostPort == 0 {
			portStep := portStepID(planned.Name)
			portResult, portErr := w.runStepOn(ctx, tr, id, attempt, portStep, platform.ArgvPayload{
				Step: portStep, Argv: []string{"docker", "port", planned.Name, fmt.Sprintf("%d/tcp", planned.ContainerPort)},
				TimeoutSeconds: 30,
			}, nil)
			if portErr != nil {
				return failWithCandidates(portErr)
			}
			instance.HostPort, portErr = platform.ParsePublishedPort(stepStdout(portResult), planned.ContainerPort)
			if portErr != nil {
				return failWithCandidates(portErr)
			}
		}
		instances = append(instances, instance)
	}
	if err := w.service.saveDeploymentInstances(ctx, id, instances); err != nil {
		return failWithCandidates(err)
	}

	for _, instance := range instances {
		if instance.Kind != "web" {
			continue
		}
		target := deploymentHealthTarget(env, instance.HostPort)
		healthResult, healthErr := w.runStepOn(ctx, tr, id, attempt, healthStepID(instance.Process, instance.Replica), platform.HealthPayload{
			Step:           healthStepID(instance.Process, instance.Replica),
			Type:           env.Revision.Runtime.HealthType,
			Target:         target,
			TimeoutSeconds: env.Revision.Runtime.HealthTimeoutSeconds,
			IntervalMS:     500,
		}, nil)
		w.observeHealth(ctx, id, attempt, healthStepID(instance.Process, instance.Replica), target, env.Health, healthResult, healthErr)
		if healthErr != nil {
			return failWithCandidates(healthErr)
		}
	}
	if err := w.service.checkCancelled(ctx, id); err != nil {
		return failWithCandidates(err)
	}

	candidateUpstreams := platform.WebUpstreams(instances)
	if len(candidateUpstreams) == 0 {
		return failWithCandidates(contract.NewFault(contract.PreconditionFailed, "deployment has no healthy web upstream", "Add a web process with replicas to the environment revision."))
	}
	switched, err := w.service.runControlStep(ctx, id, attempt, deploymentRouteStep, map[string]any{
		"environment": env.ID, "upstreams": candidateUpstreams, "release": release.ID,
	}, func() error {
		if w.service.routeSwitch != nil {
			return w.service.routeSwitch(ctx, env, candidateUpstreams)
		}
		return w.service.switchRoutesForDeployment(ctx, env, candidateUpstreams)
	})
	if err != nil {
		_ = w.service.syncRoutes(context.WithoutCancel(ctx))
		return failWithCandidates(err)
	}
	if switched {
		w.service.appendWorkerEventWith(ctx, id, attempt, contract.Event{
			Kind: "log", Stage: deploymentRouteStep, Step: deploymentRouteStep,
			ReleaseID: string(release.ID), ServingReleaseID: string(release.ID),
			Message: "Caddy now load balances release " + string(release.ID) + " across " + fmt.Sprint(len(candidateUpstreams)) + " web replica(s).",
		})
	}
	if _, err := w.service.runControlStep(ctx, id, attempt, deploymentPublicStep, map[string]any{
		"domain": env.PrimaryDomain, "path": env.Revision.Runtime.HealthPath,
	}, func() error {
		if w.service.publicVerify != nil {
			return w.service.publicVerify(ctx, env, env.Revision.Runtime.HealthPath)
		}
		return w.service.verifyPublicRoute(ctx, env, env.Revision.Runtime.HealthPath)
	}); err != nil {
		_ = w.service.syncRoutes(context.WithoutCancel(ctx))
		_, _ = w.service.runControlStep(context.WithoutCancel(ctx), id, attempt, deploymentRestoreRouteStep, map[string]any{
			"environment": env.ID, "release": dep.PreviousReleaseID,
		}, func() error { return w.service.syncRoutes(context.WithoutCancel(ctx)) })
		return failWithCandidates(fmt.Errorf("public verification failed after the Caddy switch: %w", err))
	}

	// Record the new release as serving before the previous containers go.
	// Every route writer renders from the recorded serving deployment, so the
	// periodic route maintenance can no longer point Caddy back at containers
	// this deploy is about to remove.
	if err := w.service.recordServing(ctx, env, release, dep, instances); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	resync := w.service.syncRoutes
	if w.service.routeSwitch != nil {
		// Tests replace Caddy; the switch seam renders the same upstreams.
		resync = func(ctx context.Context) error { return w.service.routeSwitch(ctx, env, candidateUpstreams) }
	}
	if err := resync(ctx); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	// The previous release is no longer routed. Removing its containers is
	// cleanup, not part of the success decision.
	w.service.removePreviousDeploymentContainers(ctx, tr, env, id, attempt, dep)
	if err := w.service.finishPlatformOperation(ctx, id, attempt, func(tx pgx.Tx, op *contract.Operation) error {
		now := time.Now().UTC()
		if _, err := tx.Exec(ctx, `UPDATE eregion_platform_deployments
            SET state='succeeded',completed_at=$2,updated_at=$2,message=$3,error_code=''
            WHERE operation_id=$1 AND state IN ('pending','running')`, id, now,
			"Serving release "+string(release.ID)+" passed candidate health and public verification."); err != nil {
			return err
		}
		if err := appendEvent(ctx, tx, op, contract.Event{
			Kind: "log", Stage: "deploy", ReleaseID: string(release.ID), ServingReleaseID: string(release.ID),
			Message: "Deployment succeeded; release " + string(release.ID) + " serves " + env.App + "/" + env.Name + ".",
		}); err != nil {
			return err
		}
		return insertPlatformChange(ctx, tx, op, string(dep.Action)+".succeeded", env.App, env.Name,
			"Serving release "+string(release.ID)+" for "+env.App+"/"+env.Name+".",
			map[string]any{"release_id": release.ID, "revision_id": env.RevisionID, "image": release.ImageID},
			map[string]any{"serving_release_id": release.ID, "serving_deployment_id": dep.ID, "health": "healthy", "instances": len(instances)})
	}); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	return nil
}

// recordServing makes the verified release the environment's serving release
// and stores its instances on the deployment, the state route writers render.
func (s *Service) recordServing(ctx context.Context, env platform.Environment, release platform.Release, dep platform.Deployment, instances []platform.DeploymentInstance) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return unavailable()
	}
	defer rollback(tx)
	now := time.Now().UTC()
	if _, err := tx.Exec(ctx, `UPDATE eregion_platform_deployments SET instances=$2,updated_at=$3 WHERE id=$1`,
		dep.ID, mustJSON(instances), now); err != nil {
		return unavailable()
	}
	if _, err := tx.Exec(ctx, `UPDATE eregion_platform_environments
        SET serving_release_id=$2,serving_deployment_id=$3,applied_release_id=$2,health_state='healthy',
            health_detail='serving release passed candidate health and public verification',
            health_checked_at=$4,updated_at=$4
        WHERE id=$1`, env.ID, release.ID, dep.ID, now); err != nil {
		return unavailable()
	}
	if err := tx.Commit(ctx); err != nil {
		return unavailable()
	}
	return nil
}

// finishDeploymentFailure removes candidate containers (if any) and optionally
// restores the previous Caddy upstreams. It records failure without touching the
// previous serving release.
func (w *platformWorker) finishDeploymentFailure(ctx context.Context, tr node.Transport, id contract.OperationID, attempt uint32, env platform.Environment, dep platform.Deployment, candidates []platform.DeploymentInstance, cause error) error {
	if cause == nil {
		return nil
	}
	if len(candidates) > 0 {
		cleanupCtx := context.WithoutCancel(ctx)
		if err := w.service.removeDeploymentContainers(cleanupCtx, tr, id, attempt, candidates); err != nil {
			w.service.appendWorkerEvent(ctx, id, attempt, "cleanup", "Candidate cleanup did not complete: "+contract.Classify(err).Message)
			if contract.Classify(err).Code == contract.Unavailable {
				return w.settle(ctx, id, attempt, err)
			}
		}
	}
	return w.settle(ctx, id, attempt, cause)
}

func (s *Service) deploymentPlan(ctx context.Context, operationID contract.OperationID) (platform.Deployment, platform.Environment, platform.Release, platform.Release, error) {
	dep, err := scanDeploymentRow(s.pool.QueryRow(ctx, deploymentSelect+` WHERE d.operation_id=$1`, operationID))
	if errors.Is(err, pgx.ErrNoRows) {
		return dep, platform.Environment{}, platform.Release{}, platform.Release{}, contract.NewFault(contract.NotFound, "deployment record is missing", "Do not retry an operation that has no durable deployment row.")
	}
	if err != nil {
		return dep, platform.Environment{}, platform.Release{}, platform.Release{}, unavailable()
	}
	env, err := s.environment(ctx, dep.App, dep.Environment)
	if err != nil {
		return dep, env, platform.Release{}, platform.Release{}, err
	}
	if dep.RevisionID != "" && (env.Revision == nil || env.Revision.ID != dep.RevisionID) {
		revision, err := s.revisionByID(ctx, dep.RevisionID)
		if err != nil {
			return dep, env, platform.Release{}, platform.Release{}, err
		}
		env.RevisionID, env.Revision = revision.ID, &revision
		env.Processes = revision.Processes
		env.Env = revision.Config
		if revision.Runtime.Node != "" {
			env.Node = revision.Runtime.Node
		}
		if revision.Runtime.HealthPath != "" {
			env.Health = platform.HealthConfig{Type: revision.Runtime.HealthType, Path: revision.Runtime.HealthPath, TimeoutSeconds: revision.Runtime.HealthTimeoutSeconds}
		}
	}
	if _, err := s.pool.Exec(ctx, `UPDATE eregion_platform_deployments SET previous_release_id=$2,previous_deployment_id=$3
        WHERE operation_id=$1 AND NOT EXISTS (SELECT 1 FROM eregion_operation_steps WHERE operation_id=$1)`,
		operationID, nullableReleaseID(env.ServingRelease), nullableDeploymentID(env.ServingDeploymentID)); err != nil {
		return dep, env, platform.Release{}, platform.Release{}, unavailable(err)
	}
	var previousID, previousDeploymentID string
	if err := s.pool.QueryRow(ctx, `SELECT coalesce(previous_release_id,''),coalesce(previous_deployment_id,'') FROM eregion_platform_deployments WHERE operation_id=$1`, operationID).Scan(&previousID, &previousDeploymentID); err != nil {
		return dep, env, platform.Release{}, platform.Release{}, unavailable()
	}
	dep.PreviousReleaseID = platform.ReleaseID(previousID)
	dep.PreviousDeploymentID = platform.DeploymentID(previousDeploymentID)
	release, err := s.releaseByID(ctx, dep.ReleaseID)
	if err != nil {
		return dep, env, release, platform.Release{}, err
	}
	if release.App != env.App || release.Environment != env.Name {
		return dep, env, release, platform.Release{}, contract.NewFault(contract.PreconditionFailed, "release is not ready for this environment", "Wait for the release operation to succeed, then retry the deployment.")
	}
	if release.Status == platform.ReleaseFailed && release.OperationID != operationID {
		return dep, env, release, platform.Release{}, contract.NewFault(contract.PreconditionFailed, "release failed and cannot be deployed", "Create or retry the build before deploying.")
	}
	n, err := s.node(ctx, env.Node)
	if err != nil {
		return dep, env, release, platform.Release{}, err
	}
	if err := s.checkPlacement(n, platform.RoleApplication, platform.CapabilityDocker); err != nil {
		return dep, env, release, platform.Release{}, err
	}
	var previous platform.Release
	if dep.PreviousReleaseID != "" {
		previous, err = s.releaseByID(ctx, dep.PreviousReleaseID)
		if err != nil {
			return dep, env, release, platform.Release{}, err
		}
	}
	return dep, env, release, previous, nil
}

// ----------------------------------------------------------- secret snapshot

func (w *platformWorker) resolveDeploymentSecrets(ctx context.Context, env platform.Environment, dep platform.Deployment) (map[string]string, string, string, map[string]platform.SecretRevisionRef, error) {
	credentialKey := ""
	names := []string{}
	if env.Revision != nil {
		names = append(names, env.Revision.SecretNames...)
		credentialKey = env.Revision.Source.CredentialSecretName
	}
	if len(names) == 0 && len(env.SecretRefs) == 0 && len(dep.SecretRevisions) == 0 {
		return map[string]string{}, credentialKey, "", map[string]platform.SecretRevisionRef{}, nil
	}
	if w.service.secrets == nil {
		return nil, credentialKey, "", nil, contract.NewFault(contract.PreconditionFailed, "secret injection is not configured", "Set EREGION_VAULT_KEY_FILE on every control-plane worker and declare secret names before deploying.")
	}
	snapshot, ok := w.service.secrets.(EnvironmentSecretSnapshotResolver)
	if !ok {
		return nil, credentialKey, "", nil, contract.NewFault(contract.PreconditionFailed, "secret name resolution is not configured", "Use a worker whose secret resolver supports environment revision secret names.")
	}
	var values map[string]string
	var resolved map[string]vault.RevisionRef
	var err error
	if len(dep.SecretRevisions) > 0 {
		pins := map[string]string{}
		for name, ref := range dep.SecretRevisions {
			pins[name] = fmt.Sprintf("vault:%s/%s/%s@%d", env.App, env.Name, name, ref.Revision)
		}
		values, resolved, err = snapshot.ResolveEnvironmentSnapshot(ctx, env.App, env.Name, nil, pins)
	} else {
		values, resolved, err = snapshot.ResolveEnvironmentSnapshot(ctx, env.App, env.Name, names, env.SecretRefs)
	}
	if err != nil {
		return nil, credentialKey, "", nil, contract.NewFault(contract.Unavailable, "environment secrets could not be resolved", "Check the vault agent and secret names, then retry the deployment.")
	}
	container := map[string]string{}
	for key, value := range values {
		if value == "" {
			return nil, credentialKey, "", nil, contract.NewFault(contract.PreconditionFailed, "a declared environment secret is missing", "Resolve every configured secret name before deploying.")
		}
		if env.Revision == nil || contains(env.Revision.SecretNames, key) || (env.Revision != nil && len(env.Revision.SecretNames) == 0 && len(env.SecretRefs) > 0) {
			container[key] = value
		}
	}
	refs := make(map[string]platform.SecretRevisionRef, len(resolved))
	for name, ref := range resolved {
		refs[name] = platform.SecretRevisionRef{Revision: ref.Revision, Hash: ref.Hash, KeyID: ref.KeyID, CreatedAt: ref.CreatedAt}
	}
	return container, credentialKey, values[credentialKey], refs, nil
}

func (w *platformWorker) resolveCredentialSecret(ctx context.Context, env platform.Environment, name string) (string, platform.SecretRevisionRef, error) {
	var empty platform.SecretRevisionRef
	if w.service.secrets == nil {
		return "", empty, contract.NewFault(contract.PreconditionFailed, "secret injection is not configured", "Set EREGION_VAULT_KEY_FILE on every control-plane worker.")
	}
	snapshot, ok := w.service.secrets.(EnvironmentSecretSnapshotResolver)
	if !ok {
		return "", empty, contract.NewFault(contract.PreconditionFailed, "secret name resolution is not configured", "Use a worker whose secret resolver supports environment revision secret names.")
	}
	values, resolved, err := snapshot.ResolveEnvironmentSnapshot(ctx, env.App, env.Name, []string{name}, nil)
	if err != nil {
		return "", empty, contract.NewFault(contract.Unavailable, "git read credential could not be resolved", "Check the vault agent and the revision's credential secret name.")
	}
	if values[name] == "" {
		return "", empty, contract.NewFault(contract.PreconditionFailed, "git read credential is missing", "Set the revision's credential secret in the environment vault before building.")
	}
	ref := resolved[name]
	return values[name], platform.SecretRevisionRef{Revision: ref.Revision, Hash: ref.Hash, KeyID: ref.KeyID, CreatedAt: ref.CreatedAt}, nil
}

// resolveEnvironmentSecrets is the environment-only form used by tests and by
// callers that have no deployment snapshot yet.
func (s *Service) resolveEnvironmentSecrets(ctx context.Context, env platform.Environment) (map[string]string, map[string]platform.SecretRevisionRef, error) {
	worker := &platformWorker{service: s}
	values, _, _, refs, err := worker.resolveDeploymentSecrets(ctx, env, platform.Deployment{})
	return values, refs, err
}

func (s *Service) recordDeploymentSecretSnapshot(ctx context.Context, id contract.OperationID, revisionID platform.RevisionID, refs map[string]platform.SecretRevisionRef) error {
	raw, err := json.Marshal(refs)
	if err != nil {
		return unavailable(err)
	}
	if _, err := s.pool.Exec(ctx, `UPDATE eregion_platform_deployments
        SET revision_id=coalesce(revision_id,$2),
            secret_revisions=CASE WHEN secret_revisions='{}'::jsonb THEN $3 ELSE secret_revisions END,
            updated_at=clock_timestamp()
        WHERE operation_id=$1`, id, nullableRevisionID(revisionID), raw); err != nil {
		return unavailable()
	}
	return nil
}

// --------------------------------------------------------------- build state

func (s *Service) markBuildRunning(ctx context.Context, id platform.BuildID) error {
	_, err := s.pool.Exec(ctx, `UPDATE eregion_platform_builds
        SET state='running',started_at=coalesce(started_at,clock_timestamp()),error='',error_code=''
        WHERE id=$1 AND state IN ('pending','running')`, id)
	if err != nil {
		return unavailable()
	}
	return nil
}

func (s *Service) markBuildSucceeded(ctx context.Context, id platform.BuildID, releaseID platform.ReleaseID, commit, imageID string) error {
	if _, err := s.pool.Exec(ctx, `UPDATE eregion_platform_builds
        SET state='succeeded',commit_sha=$2,completed_at=clock_timestamp(),error='',error_code=''
        WHERE id=$1 AND state<>'cancelled'`, id, commit); err != nil {
		return unavailable()
	}
	if _, err := s.pool.Exec(ctx, `UPDATE eregion_platform_releases
        SET status='ready',image_id=$2,commit_sha=$3,completed_at=clock_timestamp(),error='',error_code=''
        WHERE id=$1 AND status='pending'`, releaseID, imageID, commit); err != nil {
		return unavailable()
	}
	return nil
}

func (s *Service) markBuildFailed(ctx context.Context, id platform.BuildID, cause error, releaseID platform.ReleaseID) error {
	code := contract.Classify(cause).Code
	message := contract.Classify(cause).Message
	if _, err := s.pool.Exec(ctx, `UPDATE eregion_platform_builds
        SET state='failed',error_code=$2,error=$3,completed_at=clock_timestamp()
        WHERE id=$1 AND state NOT IN ('succeeded','cancelled')`, id, string(code), message); err != nil {
		return unavailable()
	}
	if releaseID != "" {
		if _, err := s.pool.Exec(ctx, `UPDATE eregion_platform_releases
            SET status='failed',error_code=$2,error=$3,completed_at=clock_timestamp()
            WHERE id=$1 AND status='pending'`, releaseID, string(code), message); err != nil {
			return unavailable()
		}
	}
	return nil
}

func (s *Service) markBuildCancelled(ctx context.Context, id platform.BuildID) error {
	_, err := s.pool.Exec(ctx, `UPDATE eregion_platform_builds
        SET state='cancelled',error_code=$2,error=$3,completed_at=clock_timestamp()
        WHERE id=$1 AND state NOT IN ('succeeded','failed')`, id, string(contract.Cancelled), "Build stopped by operator.")
	if err != nil {
		return unavailable()
	}
	return nil
}

func (s *Service) buildCancelled(ctx context.Context, id platform.BuildID) (bool, error) {
	var state platform.BuildState
	if err := s.pool.QueryRow(ctx, `SELECT state FROM eregion_platform_builds WHERE id=$1`, id).Scan(&state); err != nil {
		return false, unavailable()
	}
	return state == platform.BuildCancelled, nil
}

func (s *Service) cancelBuild(ctx context.Context, build platform.BuildRecord) error {
	if err := s.markBuildCancelled(ctx, build.ID); err != nil {
		return err
	}
	if build.ReleaseID != "" {
		if _, err := s.pool.Exec(ctx, `UPDATE eregion_platform_releases
            SET status='failed',error_code=$2,error=$3,completed_at=clock_timestamp()
            WHERE id=$1 AND status='pending'`, build.ReleaseID, string(contract.Cancelled), "Build stopped by operator."); err != nil {
			return unavailable()
		}
	}
	return nil
}

func (s *Service) requestCancelOperation(ctx context.Context, id contract.OperationID, message string) error {
	if id == "" {
		return nil
	}
	_, err := s.mutate(ctx, id, func(tx pgx.Tx, op *contract.Operation) error {
		if op.State.Terminal() || op.State == contract.Cancelling {
			return nil
		}
		return transition(ctx, tx, op, contract.Cancelling, "", message)
	})
	return err
}

// ------------------------------------------------------------ deployment data

func (s *Service) saveDeploymentInstances(ctx context.Context, id contract.OperationID, instances []platform.DeploymentInstance) error {
	if _, err := s.pool.Exec(ctx, `UPDATE eregion_platform_deployments SET instances=$2,updated_at=clock_timestamp() WHERE operation_id=$1`, id, mustJSON(instances)); err != nil {
		return unavailable()
	}
	return nil
}

// removeDeploymentContainers removes each recorded container through a durable
// per-container cleanup step. Missing containers are not an error.
func (s *Service) removeDeploymentContainers(ctx context.Context, tr node.Transport, operationID contract.OperationID, attempt uint32, instances []platform.DeploymentInstance) error {
	worker := &platformWorker{service: s}
	for _, instance := range instances {
		if instance.Name == "" {
			continue
		}
		step := cleanupStepID(instance.Name)
		if _, err := worker.runStepOn(ctx, tr, operationID, attempt, step, platform.ArgvPayload{
			Step: step, Argv: []string{"docker", "rm", "-f", instance.Name},
			AllowFailure: true, TimeoutSeconds: 30,
		}, nil); err != nil {
			return err
		}
	}
	return nil
}

func previousDeploymentInstances(ctx context.Context, s *Service, deployment platform.Deployment) ([]platform.DeploymentInstance, string) {
	if deployment.PreviousDeploymentID == "" {
		return nil, ""
	}
	dep, err := scanDeploymentRow(s.pool.QueryRow(ctx, deploymentSelect+` WHERE d.id=$1`, deployment.PreviousDeploymentID))
	if err != nil {
		return nil, ""
	}
	return dep.Instances, s.deploymentNode(ctx, dep)
}

// deploymentNode is where a deployment's containers ran. The deployment's
// pinned revision records the runtime node; the environment's current node is
// only a fallback for a deployment without a revision. Instance rows do not
// carry a node name.
func (s *Service) deploymentNode(ctx context.Context, deployment platform.Deployment) string {
	if deployment.RevisionID != "" {
		revision, err := s.revisionByID(ctx, deployment.RevisionID)
		if err == nil && revision.Runtime.Node != "" {
			return revision.Runtime.Node
		}
	}
	return ""
}

// removePreviousDeploymentContainers cleans the previous release on the node
// that actually ran it, not only the environment's current node. Cleanup is
// best effort: an unreachable old node is reported and never turns a verified
// switch into a failed deployment.
func (s *Service) removePreviousDeploymentContainers(ctx context.Context, current node.Transport, env platform.Environment, id contract.OperationID, attempt uint32, deployment platform.Deployment) {
	instances, nodeName := previousDeploymentInstances(ctx, s, deployment)
	if len(instances) == 0 {
		return
	}
	tr := current
	if nodeName != "" && nodeName != env.Node {
		if s.resolver == nil {
			s.appendWorkerEvent(ctx, id, attempt, "cleanup", "Previous release cleanup was not attempted: the worker has no node resolver.")
			return
		}
		resolved, err := s.resolver.TransportFor(ctx, nodeName)
		if err != nil {
			s.appendWorkerEvent(ctx, id, attempt, "cleanup", "Previous release cleanup could not reach node "+nodeName+": "+contract.Classify(err).Message)
			return
		}
		tr = resolved
	}
	if err := s.removeDeploymentContainers(ctx, tr, id, attempt, instances); err != nil {
		s.appendWorkerEvent(ctx, id, attempt, "cleanup", "Previous release cleanup did not complete: "+contract.Classify(err).Message)
	}
}

// --------------------------------------------------------------- control steps

func (s *Service) runControlStep(ctx context.Context, id contract.OperationID, attempt uint32, step string, inputs any, apply func() error) (bool, error) {
	raw, err := json.Marshal(inputs)
	if err != nil {
		return false, err
	}
	inputsHash := engine.InputsHash(step, raw)
	// A control step has no node execution identity; only node steps carry one.
	already, err := s.beginStep(ctx, id, attempt, step, inputsHash, "")
	if err != nil {
		return false, err
	}
	if already {
		return false, nil
	}
	if err := apply(); err != nil {
		_ = s.failStep(ctx, id, attempt, step, contract.Classify(err).Code, contract.Classify(err).Message)
		return false, err
	}
	if _, err := s.ackStep(ctx, id, attempt, step, inputsHash, "", nil, platformStepTotal); err != nil {
		return false, err
	}
	return true, nil
}

// ---------------------------------------------------------------- step helpers

func containerStepID(process, replica string) string {
	return "platform-container-" + stepToken(process, replica) + "-v1"
}

func healthStepID(process, replica string) string {
	return "platform-health-" + stepToken(process, replica) + "-v1"
}

func cleanupStepID(name string) string {
	sum := sha256.Sum256([]byte(name))
	return "platform-cleanup-" + hex.EncodeToString(sum[:4]) + "-v1"
}

func portStepID(name string) string {
	sum := sha256.Sum256([]byte(name))
	return "platform-port-" + hex.EncodeToString(sum[:4]) + "-v1"
}

func stepToken(process, replica string) string {
	sum := sha256.Sum256([]byte(process + "\x00" + replica))
	safe := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			return r
		case r == '_':
			return '-'
		default:
			return -1
		}
	}, strings.ToLower(process))
	safe = strings.Trim(safe, "-")
	if safe == "" {
		safe = "process"
	}
	if len(safe) > 24 {
		safe = strings.TrimRight(safe[:24], "-")
	}
	return safe + "-" + replica + "-" + hex.EncodeToString(sum[:3])
}

func workloadCommandName(env platform.Environment, deployment platform.DeploymentID) string {
	name := "eregion-" + env.App + "-" + env.Name + "-release-" + strings.TrimPrefix(string(deployment), "dep_")[:12]
	if len(name) > 63 {
		name = name[:63]
	}
	return strings.TrimRight(name, "-")
}

func deploymentHealthTarget(env platform.Environment, port int) string {
	if env.Revision != nil && env.Revision.Runtime.HealthType == "tcp" {
		return fmt.Sprintf("127.0.0.1:%d", port)
	}
	path := "/"
	if env.Revision != nil && env.Revision.Runtime.HealthPath != "" {
		path = env.Revision.Runtime.HealthPath
	}
	return fmt.Sprintf("http://127.0.0.1:%d%s", port, path)
}

func stepResultString(raw json.RawMessage, key string) string {
	var value map[string]any
	if json.Unmarshal(raw, &value) != nil {
		return ""
	}
	text, _ := value[key].(string)
	return text
}

func stepResultInt(raw json.RawMessage, key string) int {
	var value map[string]any
	if json.Unmarshal(raw, &value) != nil {
		return 0
	}
	switch number := value[key].(type) {
	case float64:
		return int(number)
	case json.Number:
		parsed, _ := number.Int64()
		return int(parsed)
	case string:
		parsed, _ := strconv.Atoi(number)
		return parsed
	default:
		return 0
	}
}

func cloneMap(values map[string]string) map[string]string {
	out := make(map[string]string, len(values))
	for key, value := range values {
		out[key] = value
	}
	return out
}

func mustJSON(value any) []byte {
	raw, _ := json.Marshal(value)
	return raw
}

// networkAlias is the name a web or service process answers to on the
// environment network; a worker takes no inbound traffic and gets none.
func networkAlias(process platform.ProcessSpec) string {
	if process.Kind == "web" || process.Kind == "service" {
		return process.Name
	}
	return ""
}
