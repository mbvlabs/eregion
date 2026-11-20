package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/vault"
	"github.com/riverqueue/river"
)

// ----------------------------------------------------------------- builds

func (w *platformWorker) runBuildRetry(
	ctx context.Context,
	id contract.OperationID,
	attempt uint32,
	normalized platform.Normalized,
) error {
	input, err := typedOperationInput[*platform.BuildRetryInput](normalized.Input)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
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
		value, _, err := w.service.resolveCredentialSecret(
			ctx,
			env,
			old.Source.CredentialSecretName,
		)
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
	if err := w.service.finishPlatformOperation(
		ctx,
		id,
		attempt,
		func(tx *entities.StoreTx, op *contract.Operation) error {
			return insertPlatformChange(
				ctx,
				tx,
				op,
				"build.succeeded",
				env.App,
				env.Name,
				"Retried build "+string(old.ID)+" at commit "+release.CommitSHA+".",
				map[string]any{"build_id": old.ID, "commit": old.CommitSHA},
				map[string]any{
					"release_id": release.ID,
					"image":      release.Image,
					"image_id":   release.ImageID,
				},
			)
		},
	); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	return nil
}

func (w *platformWorker) runBuildStop(
	ctx context.Context,
	id contract.OperationID,
	attempt uint32,
	normalized platform.Normalized,
) error {
	input, err := typedOperationInput[*platform.BuildStopInput](normalized.Input)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	build, err := w.service.buildByID(ctx, input.BuildID)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	handled, err := w.service.stopPendingBuildBeforeDispatch(ctx, id, attempt, build)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	if handled {
		return nil
	}
	tr, dispatch, err := w.service.prepareBuildStopDispatch(ctx, id, build)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	// Mark cancelled before signalling the worker so a pack failure is recorded
	// as cancellation rather than a new failure.
	if err := w.service.cancelBuild(ctx, build); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	if err := w.service.requestCancelOperation(
		ctx,
		build.OperationID,
		"Build stop requested; cancelling the pack process.",
	); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	if _, err := w.runStepOnWithBuild(
		ctx,
		tr,
		id,
		attempt,
		deploymentBuildStopStep,
		platform.BuildStopPayload{
			Step:        deploymentBuildStopStep,
			OperationID: string(build.OperationID),
			PackStep:    deploymentPackStep,
		},
		nil,
		"", "", "", "", "", dispatch,
	); err != nil {
		if errors.Is(err, errPlatformCancelled) {
			return w.service.cancelRecoveredBuildStop(ctx, id, attempt)
		}
		return w.settle(ctx, id, attempt, err)
	}
	if err := w.service.finishPlatformOperation(
		ctx,
		id,
		attempt,
		func(tx *entities.StoreTx, op *contract.Operation) error {
			return insertPlatformChange(ctx, tx, op, "build.stopped", build.App, build.Environment,
				"Stopped build "+string(build.ID)+".", map[string]any{"build_id": build.ID}, nil)
		},
	); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	return nil
}

// buildRelease executes Buildpacks for a pending release and records the exact
// commit, digest and build lifecycle. Its log stream is this operation's events.
func (w *platformWorker) buildRelease(
	ctx context.Context,
	id contract.OperationID,
	attempt uint32,
	env platform.Environment,
	release platform.Release,
	tr node.Transport,
	secretValues map[string]string,
) (platform.Release, error) {
	if release.Status == platform.ReleaseReady && release.ImageID != "" {
		return release, nil
	}
	build, err := w.prepareBuildRecord(ctx, id, release)
	if err != nil {
		return release, err
	}
	gitRef := build.CommitSHA
	if gitRef == "" {
		gitRef = build.Source.Ref
	}
	registryEndpoint := strings.TrimSpace(w.service.registryEndpoint)
	if registryEndpoint == "" {
		fault := contract.NewFault(
			contract.PreconditionFailed,
			"the build node has no configured registry endpoint",
			"Apply the node's recorded host intent and start the worker with its configured loopback registry endpoint before building.",
		)
		_ = w.service.markBuildFailed(ctx, build.ID, fault, release.ID)
		return release, fault
	}
	payload, err := packPayloadForBuild(build, release, gitRef, registryEndpoint)
	if err != nil {
		return release, err
	}
	result, err := w.executeBuildPack(ctx, id, attempt, env, release, build, tr, payload, secretValues)
	if err != nil {
		return release, err
	}
	return w.finishBuildRelease(ctx, id, attempt, release, build, tr, result)
}

func (w *platformWorker) prepareBuildRecord(ctx context.Context, id contract.OperationID, release platform.Release) (platform.BuildRecord, error) {
	build, has, err := w.service.buildByOperation(ctx, id)
	if err != nil {
		return build, err
	}
	if !has && release.BuildID != "" {
		build, err = w.service.buildByID(ctx, release.BuildID)
		if err != nil {
			return build, err
		}
		has = true
	}
	if !has {
		return build, contract.NewFault(
			contract.PreconditionFailed,
			"release has no build record",
			"Start the build from an environment revision or retry an existing build.",
		)
	}
	if build.OperationID != id || build.ReleaseID != release.ID || (release.BuildID != "" && release.BuildID != build.ID) {
		return build, contract.NewFault(contract.PreconditionFailed, "build is not owned by this release operation",
			"Inspect the original build and release before dispatching Pack.")
	}
	if build.State == platform.BuildCancelled {
		return build, errPlatformCancelled
	}
	return build, nil
}

func packPayloadForBuild(build platform.BuildRecord, release platform.Release, gitRef, registryEndpoint string) (platform.PackPayload, error) {
	buildInfo := platform.Build{}
	if release.Build != nil {
		buildInfo = *release.Build
	}
	payload := platform.PackPayload{
		Step:           deploymentPackStep,
		Image:          release.Image,
		Registry:       registryEndpoint,
		Builder:        buildInfo.Builder,
		Env:            buildInfo.Env,
		GoTargets:      buildInfo.GoTargets,
		Buildpacks:     buildInfo.Buildpacks,
		TimeoutSeconds: 900,
		Cache:          platform.BuildpackCacheVolume(release.App, release.Environment),
		LaunchCache:    platform.BuildpackLaunchCacheVolume(release.App, release.Environment),
	}
	switch build.Source.Type {
	case "git":
		payload.GitURL = build.Source.URL
		payload.GitRef = gitRef
		payload.GitContextPath = build.Source.Path
		payload.GitCredentialSecretKey = build.Source.CredentialSecretName
	case "directory":
		payload.Path = build.Source.Path
	default:
		return platform.PackPayload{}, contract.NewFault(
			contract.PreconditionFailed,
			"build source is not supported",
			"Use an HTTPS git source or an image.",
		)
	}
	return payload, nil
}

func (w *platformWorker) executeBuildPack(ctx context.Context, id contract.OperationID, attempt uint32, env platform.Environment, release platform.Release, build platform.BuildRecord, tr node.Transport, payload platform.PackPayload, secretValues map[string]string) (json.RawMessage, error) {
	buildNode, err := w.service.node(ctx, env.BuildNodeName())
	if err != nil {
		return nil, err
	}
	if tr == nil || w.service.resolver == nil {
		return nil, contract.NewFault(contract.Unavailable, "node transport is not configured", "Run this work in a worker configured with a node transport.")
	}
	tr, identity, err := w.service.buildTransportFor(ctx, buildNode.Name)
	if err != nil {
		return nil, err
	}
	if err := w.service.adoptBuildCapacity(ctx, id, attempt, buildNode.Name, identity); err != nil {
		return nil, err
	}
	if cancelled, _ := w.service.buildCancelled(ctx, build.ID); cancelled {
		return nil, errPlatformCancelled
	}
	if err := w.service.checkCancelled(ctx, id); err != nil {
		return nil, err
	}
	buildSecrets, err := buildCredentialSecrets(ctx, w, build, release, secretValues)
	if err != nil {
		return nil, err
	}
	result, buildErr := w.runStepOnBuild(ctx, tr, id, attempt, payload, buildSecrets, buildNode.Name, identity, build.Source.Type, build.ID, release.ID)
	if buildErr != nil {
		var snooze *river.JobSnoozeError
		if errors.As(buildErr, &snooze) {
			return nil, buildErr
		}
		return nil, w.handleBuildPackFailure(ctx, id, release, build, buildErr)
	}
	// A successful node effect has a durable node record which prevents a
	// delayed older Run from starting another build. Failed or uncertain
	// effects deliberately keep their reservation.
	if err := w.service.releaseBuildCapacity(ctx, id, attempt, buildNode.Name); err != nil {
		return nil, err
	}
	if cancelled, _ := w.service.buildCancelled(ctx, build.ID); cancelled {
		return nil, errPlatformCancelled
	}
	if err := w.service.checkCancelled(ctx, id); err != nil {
		return nil, err
	}
	return result, nil
}

func buildCredentialSecrets(ctx context.Context, w *platformWorker, build platform.BuildRecord, release platform.Release, secretValues map[string]string) (map[string]string, error) {
	buildSecrets := map[string]string{}
	if build.Source.CredentialSecretName == "" {
		return buildSecrets, nil
	}
	value := secretValues[build.Source.CredentialSecretName]
	if value == "" {
		err := contract.NewFault(
			contract.PreconditionFailed,
			"git read credential is unavailable",
			"Set the revision's credential secret in the environment vault before building.",
		)
		_ = w.service.markBuildFailed(ctx, build.ID, err, release.ID)
		return nil, err
	}
	buildSecrets[build.Source.CredentialSecretName] = value
	return buildSecrets, nil
}

func (w *platformWorker) handleBuildPackFailure(ctx context.Context, id contract.OperationID, release platform.Release, build platform.BuildRecord, buildErr error) error {
	if errors.Is(buildErr, errNodeEffectOutcomeUnknown) {
		// The node may still be building. Do not turn a lost SSH response into
		// cancellation or a failed build; retain unknown ownership and let the
		// next attempt reconcile status before any new dispatch.
		return buildErr
	}
	if state, _, stateErr := w.service.operationState(ctx, id); stateErr == nil && state == contract.Cancelling {
		_ = w.service.markBuildCancelled(ctx, build.ID)
		return errPlatformCancelled
	}
	if errors.Is(buildErr, errPlatformCancelled) {
		_ = w.service.markBuildCancelled(ctx, build.ID)
		return errPlatformCancelled
	}
	if cancelled, _ := w.service.buildCancelled(ctx, build.ID); cancelled {
		_ = w.service.markBuildCancelled(ctx, build.ID)
		return errPlatformCancelled
	}
	_ = w.service.markBuildFailed(ctx, build.ID, buildErr, release.ID)
	return buildErr
}

func (w *platformWorker) finishBuildRelease(ctx context.Context, id contract.OperationID, attempt uint32, release platform.Release, build platform.BuildRecord, tr node.Transport, result json.RawMessage) (platform.Release, error) {
	commit := stepResultString(result, "commit")
	if build.Source.Type == "git" && commit == "" {
		fault := contract.NewFault(
			contract.PreconditionFailed,
			"build did not record an exact commit",
			"The git ref must resolve to a commit before the release can be deployed.",
		)
		_ = w.service.markBuildFailed(ctx, build.ID, fault, release.ID)
		return release, fault
	}
	canonicalImage, configDigest, runtimeID, identityErr := builtReleaseIdentity(result)
	if identityErr != nil {
		_ = w.service.markBuildFailed(ctx, build.ID, identityErr, release.ID)
		return release, identityErr
	}
	imageResult, err := w.runStepOn(ctx, tr, id, attempt, deploymentImageStep, platform.ArgvPayload{
		Step: deploymentImageStep,
		Argv: []string{
			"docker",
			"image",
			"inspect",
			"--format",
			"{{.Id}}",
			canonicalImage,
		},
		IsolatedDocker: true,
		TimeoutSeconds: 30,
	}, nil)
	if err != nil {
		_ = w.service.markBuildFailed(ctx, build.ID, err, release.ID)
		return release, err
	}
	imageID := stepStdout(imageResult)
	if !strings.HasPrefix(imageID, "sha256:") || len(imageID) != 71 {
		fault := contract.NewFault(
			contract.PreconditionFailed,
			"image inspection did not return an immutable image ID",
			"Inspect the node image and build operation before retrying.",
		)
		_ = w.service.markBuildFailed(ctx, build.ID, fault, release.ID)
		return release, fault
	}
	if imageID != runtimeID {
		fault := contract.NewFault(
			contract.PreconditionFailed,
			"the local completion tag does not match the built registry image",
			"Do not accept a mismatched image; retry the build and inspect the registry publication.",
		)
		_ = w.service.markBuildFailed(ctx, build.ID, fault, release.ID)
		return release, fault
	}
	if cancelled, _ := w.service.buildCancelled(ctx, build.ID); cancelled {
		return release, errPlatformCancelled
	}
	if err := w.service.markBuildSucceeded(
		ctx,
		build.ID,
		release.ID,
		canonicalImage,
		commit,
		configDigest,
	); err != nil {
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

// readyReleaseImagePayload is the bounded ready-release materialization
// contract for every immutable release, Git-built or artifact-admitted. A ready
// release must already name its canonical registry manifest digest and recorded
// config digest, with no fallback to a historical local tag.
func readyReleaseImagePayload(release platform.Release) (platform.ReadyImagePayload, error) {
	if release.Source.Type == "" && release.Image == "" {
		return platform.ReadyImagePayload{}, nil
	}
	if !platform.IsImageDigestReference(release.Image) {
		return platform.ReadyImagePayload{}, contract.NewFault(
			contract.PreconditionFailed,
			"ready release has no canonical registry digest reference",
			"This historical local-only release cannot be materialized safely; deploy it again with an explicit artifact or a Git build.",
		)
	}
	if release.ImageID == "" {
		return platform.ReadyImagePayload{}, contract.NewFault(contract.PreconditionFailed,
			"ready release has no recorded image config digest",
			"Do not accept image presence alone; inspect or redeploy the release.")
	}
	return platform.ReadyImagePayload{
		Step: deploymentReadyImageStep, Image: release.Image, ImageID: release.ImageID,
	}, nil
}

// materializeReadyReleaseImage ensures a ready Git-built release exists on the
// application node. Present images stay untouched. A missing image is pulled by
// the recorded canonical digest, and the resulting config ID must equal the
// recorded image_id; the database is never rewritten to accept a mismatch.
func (w *platformWorker) materializeReadyReleaseImage(
	ctx context.Context,
	id contract.OperationID,
	attempt uint32,
	release platform.Release,
	tr node.Transport,
) error {
	payload, err := readyReleaseImagePayload(release)
	if err != nil {
		return err
	}
	if payload.Image == "" {
		return nil
	}
	// The reuse gate runs before any pull, release command, candidate or traffic
	// effect. It authenticates the canonical manifest and rejects a stored
	// image_id that is not its config digest. Registry unavailability fails this
	// deployment closed; it never weakens route restoration or serving recovery
	// of already-running containers.
	if err := w.verifyReadyReleaseIdentity(ctx, release); err != nil {
		return err
	}
	_, err = w.runStepOn(ctx, tr, id, attempt, deploymentReadyImageStep, payload, nil)
	return err
}

func (w *platformWorker) verifyReadyReleaseIdentity(
	ctx context.Context,
	release platform.Release,
) error {
	configDigest, err := w.service.canonicalImageIdentity(ctx, release.Image)
	if err != nil {
		return err
	}
	if configDigest != release.ImageID {
		return contract.NewFault(
			contract.PreconditionFailed,
			"historical release image_id does not match the authenticated manifest config digest",
			"This release was recorded under the old identity contract and is refused before any pull, release command or traffic change. Running containers are untouched; build or explicitly admit a corrected release.",
		)
	}
	return nil
}

// canonicalImageIdentity authenticates a canonical manifest reference and
// returns its verified config digest. Tests that seed releases without a live
// registry may preload the exact pair; production always resolves through the
// registry.
func (s *Service) canonicalImageIdentity(
	ctx context.Context,
	image string,
) (string, error) {
	if pair, ok := s.imageIdentityOverrides[image]; ok {
		return pair[1], nil
	}
	resolution, err := platform.ResolveCanonicalImageIdentity(ctx, image, runtime.GOARCH)
	if err != nil {
		return "", err
	}
	return resolution.ConfigDigest, nil
}

func (w *platformWorker) verifyReleaseImage(
	ctx context.Context,
	id contract.OperationID,
	attempt uint32,
	release platform.Release,
	tr node.Transport,
	pull bool,
) (platform.Release, error) {
	expectedConfig := release.ImageID
	if expectedConfig == "" || !strings.HasPrefix(expectedConfig, "sha256:") ||
		len(expectedConfig) != 71 {
		return release, contract.NewFault(contract.PreconditionFailed,
			"release has no expected image config digest",
			"Artifact admission must record the immutable config identity before deploying.")
	}
	// The manifest reference is authenticated again before any pull. The local
	// Docker image ID is a separate runtime observation, never the image_id.
	configDigest, err := w.service.canonicalImageIdentity(ctx, release.Image)
	if err != nil {
		return release, err
	}
	if configDigest != expectedConfig {
		return release, contract.NewFault(
			contract.PreconditionFailed,
			"the release image_id does not match the authenticated manifest config digest",
			"Do not deploy a release whose recorded identity differs from the registry. Running containers are untouched; rebuild or re-admit the artifact.",
		)
	}
	if pull {
		if _, err := w.runStepOn(ctx, tr, id, attempt, deploymentPullStep, platform.ArgvPayload{
			Step: deploymentPullStep,
			Argv: []string{
				"docker",
				"pull",
				"--platform",
				"linux/" + runtime.GOARCH,
				release.Image,
			},
			IsolatedDocker: true,
			TimeoutSeconds: 600,
		}, nil); err != nil {
			return release, err
		}
	}
	if err := w.service.checkCancelled(ctx, id); err != nil {
		return release, err
	}
	result, err := w.runStepOn(ctx, tr, id, attempt, deploymentImageStep, platform.ArgvPayload{
		Step: deploymentImageStep,
		Argv: []string{
			"docker",
			"image",
			"inspect",
			"--format",
			"{{.Id}}|{{.Os}}|{{.Architecture}}|{{json .RepoDigests}}",
			release.Image,
		},
		IsolatedDocker: true,
		TimeoutSeconds: 30,
	}, nil)
	if err != nil {
		return release, err
	}
	runtimeID, err := platform.VerifyMaterializedImageInspection(
		stepStdout(result),
		release.Image,
		runtime.GOARCH,
	)
	if err != nil {
		return release, contract.NewFault(
			contract.PreconditionFailed,
			"the pulled artifact does not match the admitted immutable image",
			"The registry artifact changed, the platform differs, or the node image does not carry the pinned repository digest; do not deploy it.",
		)
	}
	if _, err := w.service.entities.Platform().Releases().MarkReadyWithImageID(ctx, release.ID, configDigest); err != nil {
		return release, unavailable(err)
	}
	release.Status = platform.ReleaseReady
	release.ImageID = configDigest
	_ = runtimeID
	return release, nil
}

// ------------------------------------------------------------ deployment

type deploymentRun struct {
	id            contract.OperationID
	attempt       uint32
	normalized    platform.Normalized
	dep           platform.Deployment
	env           platform.Environment
	owner         deploymentOwner
	release       platform.Release
	tr            node.Transport
	secretValues  map[string]string
	credentialKey string
	credentialVal string
}

func (w *platformWorker) runDeployment(
	ctx context.Context,
	id contract.OperationID,
	attempt uint32,
	normalized platform.Normalized,
) error {
	run, done, err := w.startDeploymentRun(ctx, id, attempt, normalized)
	if err != nil || done {
		return err
	}
	if finished, err := w.ensureDeploymentRuntimeFacts(ctx, run); err != nil || finished {
		return err
	}
	network, containerEnv, finished, err := w.prepareDeploymentNetwork(ctx, run)
	if err != nil || finished {
		return err
	}
	plan, err := platform.PlanDeployment(
		run.env.App,
		run.env.Name,
		run.env.Revision.RevisionState,
		run.release.ID,
		run.dep.ID,
	)
	if err != nil {
		return w.finishDeploymentFailure(ctx, id, attempt, run.env, run.dep, nil, err)
	}
	instances, allCandidates, finished, err := w.startDeploymentContainers(ctx, run, plan, containerEnv, network)
	if err != nil || finished {
		return err
	}
	failWithCandidates := func(cause error) error {
		return w.finishDeploymentFailure(ctx, id, attempt, run.env, run.dep, allCandidates, cause)
	}
	if err := w.checkDeploymentHealth(ctx, run, instances); err != nil {
		return failWithCandidates(err)
	}
	if err := w.service.checkCancelled(ctx, id); err != nil {
		return failWithCandidates(err)
	}
	upstreams := platform.WebUpstreams(instances)
	if len(upstreams) == 0 {
		return failWithCandidates(
			contract.NewFault(
				contract.PreconditionFailed,
				"deployment has no healthy web upstream",
				"Add a web process with replicas to the environment revision.",
			),
		)
	}
	owner, finished, err := w.switchDeploymentTraffic(ctx, run, upstreams, allCandidates)
	if err != nil || finished {
		return err
	}
	return w.finalizeDeploymentRun(ctx, run, instances, upstreams, allCandidates, owner)
}

func (w *platformWorker) ensureDeploymentRuntimeFacts(ctx context.Context, run *deploymentRun) (bool, error) {
	if run.env.Revision != nil && len(run.env.Revision.Resources) > 0 {
		missing, err := w.service.resourceDeployability(ctx, run.env)
		if err != nil {
			return true, w.finishDeploymentFailure(ctx, run.id, run.attempt, run.env, run.dep, nil, err)
		}
		if len(missing) > 0 {
			return true, w.finishDeploymentFailure(ctx, run.id, run.attempt, run.env, run.dep, nil,
				contract.NewFault(
					contract.PreconditionFailed,
					"environment dependencies are not ready: "+strings.Join(missing, ", "),
					"Apply each attached resource and wait for its exec health check before deploying; resource status reports the same state.",
				))
		}
	}
	if finished, err := w.prepareDeploymentSecrets(ctx, run); err != nil || finished {
		return finished, err
	}
	if finished, err := w.prepareDeploymentArtifact(ctx, run); err != nil || finished {
		return finished, err
	}
	if run.env.Revision == nil {
		return true, w.finishDeploymentFailure(
			ctx,
			run.id,
			run.attempt,
			run.env,
			run.dep,
			nil,
			contract.NewFault(
				contract.PreconditionFailed,
				"deployment has no environment revision",
				"Deploy with an environment that has a current revision.",
			),
		)
	}
	if err := w.service.checkCancelled(ctx, run.id); err != nil {
		return true, w.finishDeploymentFailure(ctx, run.id, run.attempt, run.env, run.dep, nil, err)
	}
	return false, nil
}

func (w *platformWorker) prepareDeploymentNetwork(ctx context.Context, run *deploymentRun) (string, map[string]string, bool, error) {
	tr := run.tr
	// Preflight verifies the immutable image is present on the application
	// node. Identity was authenticated by the build completion, artifact
	// admission or the ready-release reuse gate above; the local Docker ID is a
	// separate observation and is never compared to image_id here.
	if _, err := w.runStepOn(ctx, tr, run.id, run.attempt, deploymentPreflightStep, platform.ArgvPayload{
		Step: deploymentPreflightStep,
		Argv: []string{
			"docker",
			"image",
			"inspect",
			"--format",
			"{{.Id}}",
			run.release.Image,
		},
		IsolatedDocker: true,
		TimeoutSeconds: 30,
	}, nil); err != nil {
		return "", nil, true, w.finishDeploymentFailure(ctx, run.id, run.attempt, run.env, run.dep, nil, err)
	}
	if err := w.service.checkCancelled(ctx, run.id); err != nil {
		return "", nil, true, w.finishDeploymentFailure(ctx, run.id, run.attempt, run.env, run.dep, nil, err)
	}
	network := platform.EnvironmentNetwork(run.env.ID)
	if _, err := w.runStepOn(ctx, tr, run.id, run.attempt, deploymentNetworkStep, platform.ArgvPayload{
		Step: deploymentNetworkStep,
		Argv: []string{
			"docker",
			"network",
			"create",
			"--driver",
			"bridge",
			"--label",
			"io.eregion.app=" + run.env.App,
			"--label",
			"io.eregion.environment=" + run.env.Name,
			network,
		},
		CheckArgv:      []string{"docker", "network", "inspect", "--format", "{{.Name}}", network},
		TimeoutSeconds: 30,
	}, nil); err != nil {
		return "", nil, true, w.finishDeploymentFailure(ctx, run.id, run.attempt, run.env, run.dep, nil, err)
	}
	containerEnv := cloneMap(run.env.Revision.Config)
	if run.credentialKey != "" && !contains(run.env.Revision.SecretNames, run.credentialKey) {
		delete(run.secretValues, run.credentialKey)
	}
	if releaseCommand := deploymentReleaseCommand(run.env, run.release); releaseCommand != nil {
		if finished, err := w.runDeploymentCommand(ctx, run, network, containerEnv, releaseCommand); err != nil || finished {
			return "", nil, finished, err
		}
	}
	if err := w.service.checkCancelled(ctx, run.id); err != nil {
		return "", nil, true, w.finishDeploymentFailure(ctx, run.id, run.attempt, run.env, run.dep, nil, err)
	}
	return network, containerEnv, false, nil
}

// switchDeploymentTraffic records candidate intent, switches Caddy, verifies
// the public route and moves the owner to serving.
func (w *platformWorker) switchDeploymentTraffic(ctx context.Context, run *deploymentRun, upstreams []string, candidates []platform.DeploymentInstance) (deploymentOwner, bool, error) {
	// Record the candidate route intent before the step row can be visible to a
	// concurrent route writer. The running step is evidence of work, not proof
	// that the apply happened; route writers read this owner field only.
	owner, err := w.service.setDeploymentOwnerRoute(
		ctx,
		run.owner,
		run.dep.ID,
		deploymentOwnerSwitching,
		run.attempt,
	)
	if err != nil {
		return owner, true, w.finishDeploymentFailure(ctx, run.id, run.attempt, run.env, run.dep, candidates, err)
	}
	if err := w.service.checkCancelled(ctx, run.id); err != nil {
		if errors.Is(err, errPlatformCancelled) {
			return owner, true, w.cancelOwnedDeployment(ctx, run.id, run.attempt)
		}
		return owner, true, w.finishDeploymentFailure(ctx, run.id, run.attempt, run.env, run.dep, candidates, err)
	}
	owner, switched, routeFinished, err := w.switchDeploymentRoute(ctx, run, owner, upstreams, candidates)
	_ = switched
	if err != nil || routeFinished {
		return owner, true, err
	}
	if err := w.verifyDeploymentPublic(ctx, run, owner, candidates); err != nil {
		return owner, true, err
	}
	// HEAD checked cancellation again after public verification and returned
	// directly from the deployment path. Verify may already have restored and
	// terminalized a cancellation; a terminal state must not fall through into
	// serving the candidate.
	if err := w.service.checkCancelled(ctx, run.id); err != nil {
		if errors.Is(err, errPlatformCancelled) {
			return owner, true, w.cancelOwnedDeployment(ctx, run.id, run.attempt)
		}
		return owner, true, w.finishDeploymentFailure(ctx, run.id, run.attempt, run.env, run.dep, candidates, err)
	}
	owner, err = w.service.setDeploymentOwnerRoute(
		ctx,
		owner,
		run.dep.ID,
		deploymentOwnerServing,
		run.attempt,
	)
	if err != nil {
		return owner, true, w.finishDeploymentFailure(ctx, run.id, run.attempt, run.env, run.dep, candidates, err)
	}
	return owner, false, nil
}

func (w *platformWorker) finalizeDeploymentRun(ctx context.Context, run *deploymentRun, instances []platform.DeploymentInstance, upstreams []string, candidates []platform.DeploymentInstance, owner deploymentOwner) error {
	// Record the new release as serving before the previous containers go.
	// Every route writer renders from the recorded serving deployment, so the
	// periodic route maintenance can no longer point Caddy back at containers
	// this deploy is about to remove.
	if err := w.service.recordServing(ctx, run.env, run.release, run.dep, instances); err != nil {
		return w.finishDeploymentFailure(ctx, run.id, run.attempt, run.env, run.dep, candidates, err)
	}
	if err := w.service.checkCancelled(ctx, run.id); err != nil {
		if errors.Is(err, errPlatformCancelled) {
			return w.cancelOwnedDeployment(ctx, run.id, run.attempt)
		}
		return w.finishDeploymentFailure(ctx, run.id, run.attempt, run.env, run.dep, candidates, err)
	}
	resync := w.service.syncRoutes
	if w.service.routeSwitch != nil {
		// Tests replace Caddy; the switch seam renders the same upstreams.
		resync = func(ctx context.Context) error { return w.service.routeSwitch(ctx, run.env, upstreams) }
	}
	if err := resync(ctx); err != nil {
		settled, handleErr := w.service.handlePostSwitchRouteFailure(ctx,
			contract.Operation{ID: run.id, Attempt: run.attempt}, run.dep, run.env, owner, err)
		if settled {
			return nil
		}
		return w.settle(ctx, run.id, run.attempt, handleErr)
	}
	if err := w.service.checkCancelled(ctx, run.id); err != nil {
		if errors.Is(err, errPlatformCancelled) {
			return w.cancelOwnedDeployment(ctx, run.id, run.attempt)
		}
		return w.finishDeploymentFailure(ctx, run.id, run.attempt, run.env, run.dep, candidates, err)
	}
	// Record previous-container cleanup before finalization so a crash after
	// the success commit cannot lose the cleanup work. The containers are still
	// present if a cancellation wins the finalizer transaction.
	if err := w.service.ensurePreviousDeploymentCleanup(ctx, run.env, run.dep); err != nil {
		w.service.appendCleanupWarning(
			ctx,
			run.id,
			run.attempt,
			"Previous release cleanup could not be pre-recorded; ownership is retained: "+contract.Classify(
				err,
			).Message,
		)
		return w.settle(ctx, run.id, run.attempt, contract.NewFault(
			contract.Unavailable,
			"previous release cleanup could not be recorded durably",
			"Keep the deployment owner and retry after the control database and previous deployment record are readable.",
		))
	}
	if w.service.beforeDeploymentFinalize != nil {
		w.service.beforeDeploymentFinalize()
	}
	if err := w.finishDeploymentOperation(ctx, run, instances); err != nil {
		if errors.Is(err, errDeploymentCancelledAtFinalize) {
			return w.cancelOwnedDeployment(ctx, run.id, run.attempt)
		}
		return w.settle(ctx, run.id, run.attempt, err)
	}
	// Success is committed. Previous-container cleanup was pre-recorded before
	// finalization; this best-effort pass removes it immediately and leaves the
	// durable rows for maintenance if the node is unavailable.
	w.service.removePreviousDeploymentCleanupNow(ctx, run.env, run.dep.PreviousDeploymentID)
	return nil
}

func (w *platformWorker) finishDeploymentOperation(ctx context.Context, run *deploymentRun, instances []platform.DeploymentInstance) error {
	env, dep, release := run.env, run.dep, run.release
	return w.service.finishPlatformOperation(
		ctx,
		run.id,
		run.attempt,
		func(tx *entities.StoreTx, op *contract.Operation) error {
			if op.State == contract.Cancelling {
				return errDeploymentCancelledAtFinalize
			}
			if err := lockDeploymentOperationParentsTx(
				ctx,
				tx,
				run.id,
				env.ID,
				dep.ID,
				release.ID,
			); err != nil {
				return err
			}
			now := time.Now().UTC()
			if _, err := tx.Platform().Deployments().MarkSucceededByOperation(ctx, run.id, now,
				"Serving release "+string(release.ID)+" passed candidate health and public verification.",
			); err != nil {
				return err
			}
			if err := appendEvent(ctx, tx, op, contract.Event{
				Kind:             "log",
				Stage:            "deploy",
				ReleaseID:        string(release.ID),
				ServingReleaseID: string(release.ID),
				Message: deploymentWord(
					op.Kind,
				) + " succeeded; release " + string(
					release.ID,
				) + " serves " + env.App + "/" + env.Name + ".",
			}); err != nil {
				return err
			}
			desired := map[string]any{
				"release_id":  release.ID,
				"revision_id": env.RevisionID,
				"image":       release.Image,
				"image_id":    release.ImageID,
			}
			if input, ok := run.normalized.Input.(*platform.PromoteInput); ok {
				desired["promoted_from_environment"] = input.SourceEnvironment
			}
			if err := insertPlatformChange(
				ctx,
				tx,
				op,
				deploymentChangeKind(op.Kind, dep.Action, "succeeded"),
				env.App,
				env.Name,
				"Serving release "+string(release.ID)+" for "+env.App+"/"+env.Name+".",
				desired,
				map[string]any{
					"serving_release_id":    release.ID,
					"serving_deployment_id": dep.ID,
					"health":                "healthy",
					"instances":             len(instances),
				},
			); err != nil {
				return err
			}
			return releaseServedDeploymentOwnerTx(ctx, tx, run.id)
		},
	)
}

// startDeploymentRun acquires the deployment facts, owner and immutable
// release. A started owner is returned with done=false; done=true means another
// owner settled this operation and no further work belongs to this attempt.
// acquireDeploymentRunOwner acquires or renews the owner for a live run. A
// contended owner snoozes without consuming an attempt; done reports that a
// settled owner already ended the operation.
func (w *platformWorker) acquireDeploymentRunOwner(ctx context.Context, id contract.OperationID, dep platform.Deployment, env platform.Environment, attempt uint32) (deploymentOwner, platform.Environment, bool, error) {
	owner, acquired, err := w.service.acquireDeploymentOwner(ctx, dep, env, attempt)
	if err != nil {
		return owner, env, true, w.settle(ctx, id, attempt, err)
	}
	if !acquired {
		return w.settleContendedDeploymentOwner(ctx, id, dep, env, owner, attempt)
	}
	return owner, env, false, nil
}

func (w *platformWorker) settleContendedDeploymentOwner(ctx context.Context, id contract.OperationID, dep platform.Deployment, env platform.Environment, owner deploymentOwner, attempt uint32) (deploymentOwner, platform.Environment, bool, error) {
	settled, recoverErr := w.service.recoverStaleDeploymentOwner(ctx, owner, env)
	if recoverErr != nil {
		w.service.appendWorkerEventWith(ctx, id, attempt, contract.Event{
			Kind:  "warning",
			Stage: "owner",
			Message: "Another deployment owner could not be settled yet; this operation is deferring without consuming an attempt: " + contract.Classify(
				recoverErr,
			).Message,
		})
		return owner, env, false, river.JobSnooze(deploymentOwnerDefer)
	}
	if !settled {
		return owner, env, false, river.JobSnooze(deploymentOwnerDefer)
	}
	if state, _, stateErr := w.service.operationState(ctx, id); stateErr == nil && state.Terminal() {
		return owner, env, true, nil
	}
	// The settled owner may have restored a different serving deployment.
	// Re-read before freezing this owner's predecessor.
	refreshedEnv, err := w.service.environment(ctx, dep.App, dep.Environment)
	if err != nil {
		return owner, env, true, w.finishDeploymentFailure(ctx, id, attempt, env, dep, nil, err)
	}
	env = refreshedEnv
	owner, acquired, err := w.service.acquireDeploymentOwner(ctx, dep, env, attempt)
	if err != nil {
		return owner, env, true, w.settle(ctx, id, attempt, err)
	}
	if !acquired {
		return owner, env, false, river.JobSnooze(deploymentOwnerDefer)
	}
	return owner, env, false, nil
}

func (w *platformWorker) startDeploymentRun(ctx context.Context, id contract.OperationID, attempt uint32, normalized platform.Normalized) (*deploymentRun, bool, error) {
	dep, has, err := w.service.deploymentByOperation(ctx, id)
	if err != nil {
		return nil, true, w.settle(ctx, id, attempt, err)
	}
	if !has {
		return nil, true, w.settle(
			ctx,
			id,
			attempt,
			contract.NewFault(
				contract.NotFound,
				"deployment record is missing",
				"Do not retry an operation that has no durable deployment row.",
			),
		)
	}
	env, err := w.service.environment(ctx, dep.App, dep.Environment)
	if err != nil {
		return nil, true, w.settle(ctx, id, attempt, err)
	}
	owner, env, done, err := w.acquireDeploymentRunOwner(ctx, id, dep, env, attempt)
	if err != nil || done {
		return nil, done, err
	}
	refreshed, refreshedHas, err := w.service.deploymentByOperation(ctx, id)
	if err != nil {
		return nil, true, w.finishDeploymentFailure(ctx, id, attempt, env, dep, nil, err)
	}
	if !refreshedHas {
		return nil, true, w.finishDeploymentFailure(
			ctx,
			id,
			attempt,
			env,
			dep,
			nil,
			contract.NewFault(
				contract.NotFound,
				"deployment record is missing",
				"Do not retry an operation that has no durable deployment row.",
			),
		)
	}
	dep = refreshed
	op, _, err := w.service.operationSnapshot(ctx, id)
	if err != nil {
		return nil, true, w.finishDeploymentFailure(ctx, id, attempt, env, dep, nil, err)
	}
	settled, err := w.service.recoverOwnedDeployment(ctx, op, dep, env, owner, true)
	if err != nil {
		return nil, true, w.finishDeploymentFailure(ctx, id, attempt, env, dep, nil, err)
	}
	if settled {
		return nil, true, nil
	}
	cleanupDep, cleanupEnv := dep, env
	dep, env, release, _, err := w.service.deploymentPlan(ctx, id)
	if err != nil {
		return nil, true, w.finishDeploymentFailure(ctx, id, attempt, cleanupEnv, cleanupDep, nil, err)
	}
	if input, ok := normalized.Input.(*platform.PromoteInput); ok {
		w.service.appendWorkerEvent(
			ctx,
			id,
			attempt,
			"promote",
			"Promoting "+input.App+"/"+input.SourceEnvironment+" artifact "+release.Image+" (config "+release.ImageID+") as production release "+string(
				release.ID,
			)+
				"; production's revision, config, secrets, domains and release command apply, and no build is requested.",
		)
	}
	if input, ok := normalized.Input.(*platform.RestartInput); ok && !repairRestartStillApplies(*input, env) {
		return nil, true, w.finishDeploymentFailure(ctx, id, attempt, env, dep, nil,
			contract.NewFault(contract.PreconditionFailed,
				"serving deployment changed since the repair was queued",
				"The next maintenance run will re-observe the serving workload."))
	}
	tr, err := w.service.resolver.TransportFor(ctx, env.Node)
	if err != nil {
		return nil, true, w.finishDeploymentFailure(ctx, id, attempt, env, dep, nil, err)
	}
	return &deploymentRun{
		id: id, attempt: attempt, normalized: normalized,
		dep: dep, env: env, owner: owner, release: release, tr: tr,
	}, false, nil
}
func (w *platformWorker) prepareDeploymentSecrets(ctx context.Context, run *deploymentRun) (bool, error) {
	secretValues, credentialKey, pinnedCredential, secretRefs, err := w.resolveDeploymentSecrets(ctx, run.env, run.dep)
	if err != nil {
		return true, w.finishDeploymentFailure(ctx, run.id, run.attempt, run.env, run.dep, nil, err)
	}
	gitBuild := requiresGitBuild(run.release)
	// The git read credential is not an application secret, but it is part of
	// the same snapshot: the first attempt pins its revision and every later
	// attempt reuses that pinned revision. A retry whose snapshot already exists
	// therefore reads the credential from resolveDeploymentSecrets, not only
	// from the first-attempt branch below. An explicit image artifact never
	// resolves or pins a Git credential merely because the revision has a Git
	// recipe.
	credentialValue, err := w.pinOrResolveDeploymentCredential(ctx, run, gitBuild, credentialKey, pinnedCredential, secretRefs)
	if err != nil {
		return true, w.finishDeploymentFailure(ctx, run.id, run.attempt, run.env, run.dep, nil, err)
	}
	if len(run.dep.SecretRevisions) == 0 {
		if err := w.service.recordDeploymentSecretSnapshot(
			ctx,
			run.id,
			run.env.RevisionID,
			secretRefs,
		); err != nil {
			return true, w.finishDeploymentFailure(ctx, run.id, run.attempt, run.env, run.dep, nil, err)
		}
	}
	if err := w.service.checkCancelled(ctx, run.id); err != nil {
		return true, w.finishDeploymentFailure(ctx, run.id, run.attempt, run.env, run.dep, nil, err)
	}
	run.secretValues, run.credentialKey, run.credentialVal = secretValues, credentialKey, credentialValue
	return false, nil
}

func (w *platformWorker) pinOrResolveDeploymentCredential(ctx context.Context, run *deploymentRun, gitBuild bool, credentialKey, pinnedCredential string, secretRefs map[string]platform.SecretRevisionRef) (string, error) {
	if !gitBuild || credentialKey == "" {
		return "", nil
	}
	if pinnedCredential != "" {
		return pinnedCredential, nil
	}
	// No snapshot pinned this credential (first attempt, or a snapshot written
	// before the credential was pinned), so resolve the active revision now. A
	// legacy snapshot keeps its application pins and gains the credential pin,
	// so a rotation between attempts cannot change the credential inside one
	// deployment.
	value, ref, err := w.service.resolveCredentialSecret(ctx, run.env, credentialKey)
	if err != nil {
		return "", err
	}
	if len(run.dep.SecretRevisions) == 0 {
		secretRefs[credentialKey] = ref
		return value, nil
	}
	if _, pinned := run.dep.SecretRevisions[credentialKey]; !pinned {
		if err := w.service.pinDeploymentCredentialSecret(ctx, run.id, run.attempt, credentialKey, ref); err != nil {
			return "", err
		}
	}
	return value, nil
}

func (w *platformWorker) prepareDeploymentArtifact(ctx context.Context, run *deploymentRun) (bool, error) {
	release := run.release
	needsBuild := release.Status != platform.ReleaseReady || release.ImageID == ""
	gitBuild := requiresGitBuild(release)
	// A deploy from the current revision records a pending release and build.
	if gitBuild {
		builder, err := w.service.node(ctx, run.env.BuildNodeName())
		if err != nil {
			return true, w.finishDeploymentFailure(ctx, run.id, run.attempt, run.env, run.dep, nil, err)
		}
		if err := w.service.checkPlacement(
			builder,
			platform.RoleBuilder,
			platform.CapabilityBuildpacks,
		); err != nil {
			return true, w.finishDeploymentFailure(ctx, run.id, run.attempt, run.env, run.dep, nil, err)
		}
	}
	if needsBuild {
		buildSecrets := cloneMap(run.secretValues)
		if run.credentialVal != "" {
			buildSecrets[run.credentialKey] = run.credentialVal
		}
		var err error
		if gitBuild {
			release, err = w.buildRelease(ctx, run.id, run.attempt, run.env, release, run.tr, buildSecrets)
		} else {
			// An image source may live only in the node's loopback registry.
			// Pull it by its recorded reference before inspecting the image ID.
			release, err = w.verifyReleaseImage(
				ctx,
				run.id,
				run.attempt,
				release,
				run.tr,
				release.Source.Type == "image",
			)
		}
		if err != nil {
			return true, w.finishDeploymentFailure(ctx, run.id, run.attempt, run.env, run.dep, nil, err)
		}
	}
	if release.Status != platform.ReleaseReady || release.ImageID == "" {
		return true, w.finishDeploymentFailure(
			ctx,
			run.id,
			run.attempt,
			run.env,
			run.dep,
			nil,
			contract.NewFault(
				contract.PreconditionFailed,
				"release has no immutable image",
				"Build or inspect the release before deploying.",
			),
		)
	}
	if !needsBuild {
		if err := w.materializeReadyReleaseImage(ctx, run.id, run.attempt, release, run.tr); err != nil {
			return true, w.finishDeploymentFailure(ctx, run.id, run.attempt, run.env, run.dep, nil, err)
		}
	}
	run.release = release
	return false, nil
}

func (w *platformWorker) runDeploymentCommand(ctx context.Context, run *deploymentRun, network string, containerEnv map[string]string, command *platform.CommandSpec) (bool, error) {
	digest := platform.ReleaseCommandDigest(*command)
	row, err := w.service.releaseCommandRun(ctx, run.release.ID, digest)
	if err != nil {
		return true, w.finishDeploymentFailure(ctx, run.id, run.attempt, run.env, run.dep, nil, err)
	}
	decision := releaseCommandAction(row, run.dep.Action, run.id)
	if decision == releaseCommandRecover {
		decision, err = w.recoverReleaseCommandRun(ctx, run.release.ID, digest, row, run.id)
		if err != nil {
			return true, w.finishDeploymentFailure(ctx, run.id, run.attempt, run.env, run.dep, nil, err)
		}
	}
	switch decision {
	case releaseCommandSkip:
		w.service.appendWorkerEvent(
			ctx,
			run.id,
			run.attempt,
			deploymentCommandStep,
			"release command already succeeded for this release",
		)
		return false, nil
	case releaseCommandRefuse:
		return true, w.refuseReleaseCommand(ctx, run)
	case releaseCommandRunNow, releaseCommandRecover:
		fallthrough
	default:
		if err := w.executeReleaseCommand(ctx, run, network, containerEnv, command, digest); err != nil {
			return true, err
		}
		return false, nil
	}
}

func (w *platformWorker) refuseReleaseCommand(ctx context.Context, run *deploymentRun) error {
	if run.dep.Action == platform.ActionDeploy {
		return w.finishDeploymentFailure(
			ctx,
			run.id,
			run.attempt,
			run.env,
			run.dep,
			nil,
			contract.NewFault(
				contract.Conflict,
				"the release command is already running for this release",
				"Wait for the other deployment to finish, then retry.",
			),
		)
	}
	return w.finishDeploymentFailure(
		ctx,
		run.id,
		run.attempt,
		run.env,
		run.dep,
		nil,
		contract.NewFault(
			contract.PreconditionFailed,
			"release command has not succeeded for this release",
			"Deploy the release first; restart, scale and rollback never run the release command.",
		),
	)
}

func (w *platformWorker) executeReleaseCommand(ctx context.Context, run *deploymentRun, network string, containerEnv map[string]string, command *platform.CommandSpec, digest string) error {
	if err := w.service.checkCancelled(ctx, run.id); err != nil {
		return w.finishDeploymentFailure(ctx, run.id, run.attempt, run.env, run.dep, nil, err)
	}
	if err := w.service.startReleaseCommandRun(ctx, run.release.ID, digest, run.id); err != nil {
		return w.finishDeploymentFailure(ctx, run.id, run.attempt, run.env, run.dep, nil, err)
	}
	_, err := w.runStepOn(
		ctx,
		run.tr,
		run.id,
		run.attempt,
		deploymentCommandStep,
		platform.ReleaseCommandPayload{
			Step: deploymentCommandStep, Name: workloadCommandName(run.env, run.dep.ID),
			Image: run.release.Image, Network: network, Command: command.Argv,
			Labels: platform.ReleaseCommandLabels(run.env.App, run.env.Name, run.release.ID, run.dep.ID),
			Env:    containerEnv, SecretEnvKeys: sortedPlatformKeys(run.secretValues),
			TimeoutSeconds: command.TimeoutSeconds,
		},
		run.secretValues,
	)
	if err != nil {
		if !releaseCommandErrorIsTerminal(err) {
			// The command may have run. Leave the row owned by this operation
			// and retry; the next attempt asks node status before it dispatches
			// the command again.
			return w.finishDeploymentFailure(ctx, run.id, run.attempt, run.env, run.dep, nil, err)
		}
		if markErr := w.service.finishReleaseCommandRun(
			ctx,
			run.release.ID,
			digest,
			run.id,
			err,
		); markErr != nil {
			return w.finishDeploymentFailure(ctx, run.id, run.attempt, run.env, run.dep, nil, markErr)
		}
		return w.finishDeploymentFailure(ctx, run.id, run.attempt, run.env, run.dep, nil, err)
	}
	if err := w.service.finishReleaseCommandRun(
		ctx,
		run.release.ID,
		digest,
		run.id,
		nil,
	); err != nil {
		return w.finishDeploymentFailure(ctx, run.id, run.attempt, run.env, run.dep, nil, err)
	}
	return nil
}

func (w *platformWorker) startDeploymentContainers(ctx context.Context, run *deploymentRun, plan []platform.PlannedContainer, containerEnv map[string]string, network string) ([]platform.DeploymentInstance, []platform.DeploymentInstance, bool, error) {
	allCandidates := make([]platform.DeploymentInstance, 0, len(plan))
	for _, planned := range plan {
		allCandidates = append(allCandidates, platform.DeploymentInstance{
			Name:    planned.Name,
			Process: planned.Process.Name,
			Kind:    planned.Process.Kind,
			Replica: planned.Replica,
		})
	}
	instances := make([]platform.DeploymentInstance, 0, len(plan))
	for _, planned := range plan {
		instance, err := w.startDeploymentContainer(ctx, run, planned, containerEnv, network)
		if err != nil {
			return nil, nil, true, w.finishDeploymentFailure(ctx, run.id, run.attempt, run.env, run.dep, allCandidates, err)
		}
		instances = append(instances, instance)
	}
	if err := w.service.saveDeploymentInstances(ctx, run.id, instances); err != nil {
		return nil, nil, true, w.finishDeploymentFailure(ctx, run.id, run.attempt, run.env, run.dep, allCandidates, err)
	}
	return instances, allCandidates, false, nil
}

func (w *platformWorker) startDeploymentContainer(ctx context.Context, run *deploymentRun, planned platform.PlannedContainer, containerEnv map[string]string, network string) (platform.DeploymentInstance, error) {
	if err := w.service.checkCancelled(ctx, run.id); err != nil {
		return platform.DeploymentInstance{}, err
	}
	processEnv := cloneMap(containerEnv)
	if planned.Publish {
		processEnv["PORT"] = fmt.Sprint(planned.ContainerPort)
	} else {
		delete(processEnv, "PORT")
	}
	result, err := w.runStepOn(
		ctx,
		run.tr,
		run.id,
		run.attempt,
		containerStepID(planned.Process.Name, planned.Replica),
		platform.ContainerPayload{
			Step:          containerStepID(planned.Process.Name, planned.Replica),
			Name:          planned.Name,
			Image:         run.release.Image,
			ContainerPort: planned.ContainerPort,
			Publish:       planned.Publish,
			Bind:          "127.0.0.1",
			Env:           processEnv,
			SecretEnvKeys: sortedPlatformKeys(run.secretValues),
			Command:       planned.Command,
			Labels:        planned.Labels,
			Restart:       "unless-stopped",
			Network:       network,
			NetworkAlias:  networkAlias(planned.Process),
		},
		run.secretValues,
	)
	if err != nil {
		return platform.DeploymentInstance{}, err
	}
	instance := platform.DeploymentInstance{
		Name: planned.Name, Process: planned.Process.Name, Kind: planned.Process.Kind,
		Replica: planned.Replica, ReleaseID: run.release.ID, State: "running",
	}
	instance.HostPort = stepResultInt(result, "host_port")
	instance.ContainerID = stepResultString(result, "container_id")
	if !planned.Publish || instance.HostPort != 0 {
		return instance, nil
	}
	if err := w.service.checkCancelled(ctx, run.id); err != nil {
		return platform.DeploymentInstance{}, err
	}
	portStep := portStepID(planned.Name)
	portResult, err := w.runStepOn(ctx, run.tr, run.id, run.attempt, portStep, platform.ArgvPayload{
		Step: portStep,
		Argv: []string{
			"docker",
			"port",
			planned.Name,
			fmt.Sprintf("%d/tcp", planned.ContainerPort),
		},
		TimeoutSeconds: 30,
	}, nil)
	if err != nil {
		return platform.DeploymentInstance{}, err
	}
	instance.HostPort, err = platform.ParsePublishedPort(
		stepStdout(portResult),
		planned.ContainerPort,
	)
	return instance, err
}

func (w *platformWorker) checkDeploymentHealth(ctx context.Context, run *deploymentRun, instances []platform.DeploymentInstance) error {
	for _, instance := range instances {
		if instance.Kind != "web" {
			continue
		}
		if err := w.service.checkCancelled(ctx, run.id); err != nil {
			return err
		}
		target := deploymentHealthTarget(run.env, instance.HostPort)
		healthResult, healthErr := w.runStepOn(
			ctx,
			run.tr,
			run.id,
			run.attempt,
			healthStepID(instance.Process, instance.Replica),
			platform.HealthPayload{
				Step:           healthStepID(instance.Process, instance.Replica),
				Type:           run.env.Revision.Runtime.HealthType,
				Target:         target,
				TimeoutSeconds: run.env.Revision.Runtime.HealthTimeoutSeconds,
				IntervalMS:     500,
			},
			nil,
		)
		w.observeHealth(
			ctx,
			run.id,
			run.attempt,
			healthStepID(instance.Process, instance.Replica),
			target,
			run.env.Health,
			healthResult,
			healthErr,
		)
		if healthErr != nil {
			return healthErr
		}
	}
	return w.service.checkCancelled(ctx, run.id)
}

func (w *platformWorker) handleRouteSwitchFailure(ctx context.Context, run *deploymentRun, owner deploymentOwner, candidates []platform.DeploymentInstance, cause error) (deploymentOwner, error) {
	record, has, recordErr := w.service.stepRecord(ctx, run.id, deploymentRouteStep)
	if recordErr != nil {
		return owner, w.finishDeploymentFailure(ctx, run.id, run.attempt, run.env, run.dep, candidates, recordErr)
	}
	// The apply may have succeeded before the error (including an
	// acknowledgement whose commit response was lost). Move the owner to the
	// exact frozen restore target first, then restore it before removing any
	// candidate container.
	restoreErr := w.service.restoreDeploymentRoute(
		context.WithoutCancel(ctx),
		run.id,
		run.attempt,
		run.env,
		owner,
	)
	if restoreErr != nil {
		w.service.appendWorkerEventWith(ctx, run.id, run.attempt, contract.Event{
			Kind:    "warning",
			Stage:   "reconcile",
			Message: "The route switch did not complete cleanly and the recorded serving route could not be restored; candidate containers and ownership were retained for retry.",
		})
		return owner, w.finishDeploymentFailure(ctx, run.id, run.attempt, run.env, run.dep, candidates,
			contract.NewFault(
				contract.Unavailable,
				"deployment route state is uncertain and the previous route could not be restored",
				"Keep the candidate containers; retry after the route writer is available.",
			))
	}
	if !has || record.State != "failed" {
		if w.service.workerInterrupted(ctx, run.id, cause) {
			return owner, cause
		}
		return owner, w.failDeploymentAfterRouteRestore(
			ctx,
			run.id,
			run.attempt,
			run.env,
			run.dep,
			candidates,
			contract.Unavailable,
			"The route switch was not acknowledged; the recorded serving route was restored, so the deployment was not finalized.",
		)
	}
	return owner, w.finishDeploymentFailure(ctx, run.id, run.attempt, run.env, run.dep, candidates, cause)
}

func (w *platformWorker) switchDeploymentRoute(ctx context.Context, run *deploymentRun, owner deploymentOwner, upstreams []string, candidates []platform.DeploymentInstance) (deploymentOwner, bool, bool, error) {
	switched, err := w.service.runControlStep(ctx, run.id, run.attempt, deploymentRouteStep, map[string]any{
		"environment": run.env.ID, "upstreams": upstreams, "release": run.release.ID,
	}, func() error {
		if w.service.routeSwitch != nil {
			return w.service.routeSwitch(ctx, run.env, upstreams)
		}
		return w.service.switchRoutesForDeployment(ctx, run.env, upstreams)
	})
	if err != nil {
		owner, err := w.handleRouteSwitchFailure(ctx, run, owner, candidates, err)
		return owner, false, true, err
	}
	if !switched {
		return owner, false, false, nil
	}
	owner, err = w.service.setDeploymentOwnerRoute(
		ctx,
		owner,
		run.dep.ID,
		deploymentOwnerVerifying,
		run.attempt,
	)
	if err != nil {
		return owner, false, true, w.finishDeploymentFailure(ctx, run.id, run.attempt, run.env, run.dep, candidates, err)
	}
	w.service.appendWorkerEventWith(ctx, run.id, run.attempt, contract.Event{
		Kind:             "log",
		Stage:            deploymentRouteStep,
		Step:             deploymentRouteStep,
		ReleaseID:        string(run.release.ID),
		ServingReleaseID: string(run.release.ID),
		Message: "Caddy now load balances release " + string(
			run.release.ID,
		) + " across " + fmt.Sprint(
			len(upstreams),
		) + " web replica(s).",
	})
	return owner, true, false, nil
}

func (w *platformWorker) verifyDeploymentPublic(ctx context.Context, run *deploymentRun, owner deploymentOwner, candidates []platform.DeploymentInstance) error {
	if err := w.service.checkCancelled(ctx, run.id); err != nil {
		if errors.Is(err, errPlatformCancelled) {
			return w.cancelOwnedDeployment(ctx, run.id, run.attempt)
		}
		return w.finishDeploymentFailure(ctx, run.id, run.attempt, run.env, run.dep, candidates, err)
	}
	publicPath := w.service.primaryCheckPath(ctx, run.env)
	if _, err := w.service.runControlStep(ctx, run.id, run.attempt, deploymentPublicStep, map[string]any{
		"domain": run.env.PrimaryDomain, "path": publicPath,
	}, func() error {
		if w.service.publicVerify != nil {
			return w.service.publicVerify(ctx, run.env, publicPath)
		}
		return w.service.verifyPublicRoute(ctx, run.id, run.attempt, run.env, publicPath)
	}); err != nil {
		return w.handlePublicVerificationFailure(ctx, run, owner, candidates, err)
	}
	if err := w.service.checkCancelled(ctx, run.id); err != nil {
		if errors.Is(err, errPlatformCancelled) {
			return w.cancelOwnedDeployment(ctx, run.id, run.attempt)
		}
		return w.finishDeploymentFailure(ctx, run.id, run.attempt, run.env, run.dep, candidates, err)
	}
	return nil
}

func (w *platformWorker) handlePublicVerificationFailure(ctx context.Context, run *deploymentRun, owner deploymentOwner, candidates []platform.DeploymentInstance, cause error) error {
	restoreErr := w.service.restoreDeploymentRoute(
		context.WithoutCancel(ctx),
		run.id,
		run.attempt,
		run.env,
		owner,
	)
	if restoreErr != nil {
		w.service.appendWorkerEventWith(ctx, run.id, run.attempt, contract.Event{
			Kind:    "warning",
			Stage:   "reconcile",
			Message: "Public verification failed and the recorded serving route could not be restored; candidate containers and ownership were retained for retry.",
		})
		return w.finishDeploymentFailure(ctx, run.id, run.attempt, run.env, run.dep, candidates,
			contract.NewFault(
				contract.Unavailable,
				"public verification failed and the previous serving route could not be restored",
				"Keep the candidate containers; retry after the route writer is available.",
			))
	}
	fault := contract.Classify(cause)
	if w.service.workerInterrupted(ctx, run.id, cause) {
		return cause
	}
	return w.failDeploymentAfterRouteRestore(
		ctx,
		run.id,
		run.attempt,
		run.env,
		run.dep,
		candidates,
		fault.Code,
		"Public verification failed after the Caddy switch; the recorded serving route was restored: "+fault.Message,
	)
}

// recordServing makes the verified release the environment's serving release
// and stores its instances on the deployment, the state route writers render.
func (s *Service) recordServing(
	ctx context.Context,
	env platform.Environment,
	release platform.Release,
	dep platform.Deployment,
	instances []platform.DeploymentInstance,
) error {
	err := s.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		if err := lockServingPointersTx(ctx, tx, env.ID, release.ID, dep.ID); err != nil {
			return err
		}
		now := time.Now().UTC()
		if _, err := tx.Platform().Deployments().SetInstances(ctx, dep.ID, mustJSON(instances), now); err != nil {
			return unavailable(err)
		}
		if _, err := tx.Platform().Environments().SetServing(ctx, entities.EnvironmentServing{
			ID: env.ID, ReleaseID: release.ID, DeploymentID: dep.ID,
			HealthState:  platform.HealthHealthy,
			HealthDetail: "serving release passed candidate health and public verification",
			CheckedAt:    now,
		}); err != nil {
			return unavailable(err)
		}
		return nil
	})
	if err != nil {
		if entities.IsBeginFailure(err) || entities.IsCommitFailure(err) {
			return unavailable(err)
		}
		return err
	}
	return nil
}

// failDeploymentAfterRouteRestore removes candidates only after the recorded
// serving route has been restored, then ends the operation with the given code.
// The serving guard keeps a deployment that the environment still records as
// serving from being removed even if the route writer is inconsistent.
func (w *platformWorker) failDeploymentAfterRouteRestore(
	ctx context.Context,
	id contract.OperationID,
	attempt uint32,
	env platform.Environment,
	dep platform.Deployment,
	candidates []platform.DeploymentInstance,
	code contract.Code,
	message string,
) error {
	if ctx.Err() != nil && w.service.workerInterrupted(ctx, id, ctx.Err()) {
		return ctx.Err()
	}
	cleanupCtx := context.WithoutCancel(ctx)
	if _, guardErr := w.service.deploymentCleanupProtected(
		cleanupCtx,
		env,
		dep.ID,
	); guardErr != nil {
		return deploymentCleanupIntentFault(guardErr)
	}
	// Record intent even when the candidate is temporarily protected: the
	// terminal transaction releases ownership, and maintenance needs a durable
	// row if the immediate removal cannot run yet.
	if len(candidates) > 0 {
		if err := w.service.persistDeploymentCleanupInstances(
			cleanupCtx,
			env,
			dep,
			candidates,
			attempt,
		); err != nil {
			return deploymentCleanupIntentFault(err)
		}
	}
	if code == contract.Cancelled {
		if err := w.service.cancelPlatform(cleanupCtx, id, attempt, message); err != nil {
			return err
		}
		w.service.removeDeploymentCleanupRowsNow(cleanupCtx, env, dep.ID)
		return nil
	}
	if err := w.service.checkCancelled(cleanupCtx, id); err != nil {
		return w.settle(cleanupCtx, id, attempt, err)
	}
	if err := w.service.failPlatformOperation(cleanupCtx, id, attempt, code, message); err != nil {
		return err
	}
	w.service.removeDeploymentCleanupRowsNow(cleanupCtx, env, dep.ID)
	return nil
}

// deploymentFailureNeedsCleanup mirrors settle classification: terminal and
// cancellation failures cleanup candidates, while retryable failures retain
// them so completed container steps can resume safely.
func deploymentFailureNeedsCleanup(err error) bool {
	if err == nil || errors.Is(err, errNotOwned) || errors.Is(err, errOperationTerminal) {
		return false
	}
	if errors.Is(err, errPlatformCancelled) || errors.Is(err, engine.ErrPermanent) {
		return true
	}
	switch contract.Classify(err).Code {
	case contract.Cancelled, contract.ValidationFailed, contract.PreconditionFailed,
		contract.NotFound, contract.Forbidden, contract.Conflict:
		return true
	case contract.Usage, contract.InvalidOutputMode, contract.InputRequired, contract.MissingTool,
		contract.Unauthenticated, contract.DoctorFailed, contract.ConfirmationRequired,
		contract.NodeVersionMismatch, contract.RemoteUnreachable, contract.Unavailable, contract.Internal:
		return false
	default:
		return false
	}
}

// finishDeploymentFailure cleans candidate containers only for terminal or
// cancellation failures. Retryable failures and worker shutdown retain
// containers and their completed steps so a retry does not skip reconstruction
// of missing containers.
func (w *platformWorker) finishDeploymentFailure(
	ctx context.Context,
	id contract.OperationID,
	attempt uint32,
	env platform.Environment,
	dep platform.Deployment,
	candidates []platform.DeploymentInstance,
	cause error,
) error {
	if cause == nil {
		return nil
	}
	var snooze *river.JobSnoozeError
	if errors.As(cause, &snooze) {
		return cause
	}
	if w.service.workerInterrupted(ctx, id, cause) {
		return cause
	}
	if deploymentFailureNeedsCleanup(cause) {
		// A terminal or cancellation settlement is not safe while a detached
		// node effect may still be live. This includes a lost SSH response
		// during a build whose operation received cancellation meanwhile.
		if err := w.ensureNoOutstandingDeploymentEffect(
			ctx,
			id,
			attempt,
			dep,
			env,
			"deployment-failure",
		); err != nil {
			return err
		}
		cleanupCtx := context.WithoutCancel(ctx)
		if err := w.service.persistDeploymentCleanupInstances(
			cleanupCtx,
			env,
			dep,
			candidates,
			attempt,
		); err != nil {
			return deploymentCleanupIntentFault(err)
		}
		if err := w.settle(ctx, id, attempt, cause); err != nil {
			return err
		}
		// The terminal settlement releases the owner; cleanup now uses the
		// maintenance identity because a terminal operation cannot own steps.
		w.service.removeDeploymentCleanupRowsNow(cleanupCtx, env, dep.ID)
		return nil
	}
	return w.settle(ctx, id, attempt, cause)
}

func (s *Service) deploymentPlan(
	ctx context.Context,
	operationID contract.OperationID,
) (platform.Deployment, platform.Environment, platform.Release, platform.Release, error) {
	dep, found, err := s.entities.Platform().Deployments().GetByOperation(ctx, operationID)
	if err != nil {
		return dep, platform.Environment{}, platform.Release{}, platform.Release{}, unavailable(err)
	}
	if !found {
		return dep, platform.Environment{}, platform.Release{}, platform.Release{}, contract.NewFault(
			contract.NotFound,
			"deployment record is missing",
			"Do not retry an operation that has no durable deployment row.",
		)
	}
	env, err := s.environment(ctx, dep.App, dep.Environment)
	if err != nil {
		return dep, env, platform.Release{}, platform.Release{}, err
	}
	env, err = s.pinnedDeploymentEnvironment(ctx, dep, env)
	if err != nil {
		return dep, env, platform.Release{}, platform.Release{}, err
	}
	dep, err = s.bindDeploymentPredecessorPlan(ctx, operationID, dep)
	if err != nil {
		return dep, env, platform.Release{}, platform.Release{}, err
	}
	release, err := s.releaseByID(ctx, dep.ReleaseID)
	if err != nil {
		return dep, env, release, platform.Release{}, err
	}
	if err := validateDeploymentRelease(release, env, operationID); err != nil {
		return dep, env, release, platform.Release{}, err
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

func (s *Service) pinnedDeploymentEnvironment(ctx context.Context, dep platform.Deployment, env platform.Environment) (platform.Environment, error) {
	if dep.RevisionID == "" || (env.Revision != nil && env.Revision.ID == dep.RevisionID) {
		return env, nil
	}
	revision, err := s.revisionByID(ctx, dep.RevisionID)
	if err != nil {
		return env, err
	}
	env.RevisionID, env.Revision = revision.ID, &revision
	env.Processes = revision.Processes
	env.Env = revision.Config
	if revision.Runtime.Node != "" {
		env.Node = revision.Runtime.Node
	}
	if revision.Runtime.HealthPath != "" {
		env.Health = platform.HealthConfig{
			Type:           revision.Runtime.HealthType,
			Path:           revision.Runtime.HealthPath,
			TimeoutSeconds: revision.Runtime.HealthTimeoutSeconds,
		}
	}
	return env, nil
}

func (s *Service) bindDeploymentPredecessorPlan(ctx context.Context, operationID contract.OperationID, dep platform.Deployment) (platform.Deployment, error) {
	// The owner already bound the frozen predecessor into this deployment
	// transactionally at acquisition/renewal. Read it back; never rebind it from
	// the live serving snapshot after ownership exists.
	predecessor, found, err := s.entities.Platform().Deployments().Predecessor(ctx, operationID)
	if err != nil {
		return dep, unavailable(err)
	}
	if found {
		dep.PreviousReleaseID = predecessor.ReleaseID
		dep.PreviousDeploymentID = predecessor.DeploymentID
	}
	return dep, nil
}

func validateDeploymentRelease(release platform.Release, env platform.Environment, operationID contract.OperationID) error {
	if release.App != env.App || release.Environment != env.Name {
		return contract.NewFault(
			contract.PreconditionFailed,
			"release is not ready for this environment",
			"Wait for the release operation to succeed, then retry the deployment.",
		)
	}
	if release.Status == platform.ReleaseFailed && release.OperationID != operationID {
		return contract.NewFault(
			contract.PreconditionFailed,
			"release failed and cannot be deployed",
			"Create or retry the build before deploying.",
		)
	}
	return nil
}

func requiresGitBuild(release platform.Release) bool {
	return (release.Status != platform.ReleaseReady || release.ImageID == "") &&
		release.Source.Type == "git"
}

// ----------------------------------------------------------- secret snapshot

func (w *platformWorker) resolveDeploymentSecrets(
	ctx context.Context,
	env platform.Environment,
	dep platform.Deployment,
) (map[string]string, string, string, map[string]platform.SecretRevisionRef, error) {
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
		return nil, credentialKey, "", nil, contract.NewFault(
			contract.PreconditionFailed,
			"secret injection is not configured",
			"Set EREGION_VAULT_KEY_FILE on every control-plane worker and declare secret names before deploying.",
		)
	}
	snapshot, ok := w.service.secrets.(EnvironmentSecretSnapshotResolver)
	if !ok {
		return nil, credentialKey, "", nil, contract.NewFault(
			contract.PreconditionFailed,
			"secret name resolution is not configured",
			"Use a worker whose secret resolver supports environment revision secret names.",
		)
	}
	var values map[string]string
	var resolved map[string]vault.RevisionRef
	var err error
	if len(dep.SecretRevisions) > 0 {
		pins := map[string]string{}
		for name, ref := range dep.SecretRevisions {
			pins[name] = fmt.Sprintf("vault:%s/%s/%s@%d", env.App, env.Name, name, ref.Revision)
		}
		values, resolved, err = snapshot.ResolveEnvironmentSnapshot(
			ctx,
			env.App,
			env.Name,
			nil,
			pins,
		)
	} else {
		values, resolved, err = snapshot.ResolveEnvironmentSnapshot(
			ctx,
			env.App,
			env.Name,
			names,
			env.SecretRefs,
		)
	}
	if err != nil {
		return nil, credentialKey, "", nil, contract.NewFault(
			contract.Unavailable,
			"environment secrets could not be resolved",
			"Check the vault agent and secret names, then retry the deployment.",
		)
	}
	container := map[string]string{}
	for key, value := range values {
		if value == "" {
			return nil, credentialKey, "", nil, contract.NewFault(
				contract.PreconditionFailed,
				"a declared environment secret is missing",
				"Resolve every configured secret name before deploying.",
			)
		}
		if env.Revision == nil || contains(env.Revision.SecretNames, key) ||
			(env.Revision != nil && len(env.Revision.SecretNames) == 0 && len(env.SecretRefs) > 0) {
			container[key] = value
		}
	}
	refs := make(map[string]platform.SecretRevisionRef, len(resolved))
	for name, ref := range resolved {
		refs[name] = platform.SecretRevisionRef{
			Revision:  ref.Revision,
			Hash:      ref.Hash,
			KeyID:     ref.KeyID,
			CreatedAt: ref.CreatedAt,
		}
	}
	return container, credentialKey, values[credentialKey], refs, nil
}

func (s *Service) resolveCredentialSecret(
	ctx context.Context,
	env platform.Environment,
	name string,
) (string, platform.SecretRevisionRef, error) {
	var empty platform.SecretRevisionRef
	if s.secrets == nil {
		return "", empty, contract.NewFault(
			contract.PreconditionFailed,
			"secret injection is not configured",
			"Set EREGION_VAULT_KEY_FILE on every control-plane worker.",
		)
	}
	snapshot, ok := s.secrets.(EnvironmentSecretSnapshotResolver)
	if !ok {
		return "", empty, contract.NewFault(
			contract.PreconditionFailed,
			"secret name resolution is not configured",
			"Use a worker whose secret resolver supports environment revision secret names.",
		)
	}
	values, resolved, err := snapshot.ResolveEnvironmentSnapshot(
		ctx,
		env.App,
		env.Name,
		[]string{name},
		nil,
	)
	if err != nil {
		return "", empty, contract.NewFault(
			contract.Unavailable,
			"git read credential could not be resolved",
			"Check the vault agent and the revision's credential secret name.",
		)
	}
	if values[name] == "" {
		return "", empty, contract.NewFault(
			contract.PreconditionFailed,
			"git read credential is missing",
			"Set the revision's credential secret in the environment vault before building.",
		)
	}
	ref := resolved[name]
	return values[name], platform.SecretRevisionRef{
		Revision:  ref.Revision,
		Hash:      ref.Hash,
		KeyID:     ref.KeyID,
		CreatedAt: ref.CreatedAt,
	}, nil
}

// resolveEnvironmentSecrets is the environment-only form used by tests and by
// callers that have no deployment snapshot yet.
func (s *Service) resolveEnvironmentSecrets(
	ctx context.Context,
	env platform.Environment,
) (map[string]string, map[string]platform.SecretRevisionRef, error) {
	worker := &platformWorker{service: s}
	values, _, _, refs, err := worker.resolveDeploymentSecrets(ctx, env, platform.Deployment{})
	return values, refs, err
}

func (s *Service) recordDeploymentSecretSnapshot(
	ctx context.Context,
	id contract.OperationID,
	revisionID platform.RevisionID,
	refs map[string]platform.SecretRevisionRef,
) error {
	raw, err := json.Marshal(refs)
	if err != nil {
		return unavailable(err)
	}
	if _, err := s.entities.Platform().Deployments().SetRevisionAndSecretSnapshot(ctx, id, revisionID, raw); err != nil {
		return unavailable(err)
	}
	return nil
}

// pinDeploymentCredentialSecret merges a missing git read credential revision
// into an existing deployment secret snapshot. It is attempt-fenced, preserves
// the application pins already recorded, and never replaces a credential that
// another attempt pinned first.
func (s *Service) pinDeploymentCredentialSecret(
	ctx context.Context,
	id contract.OperationID,
	attempt uint32,
	name string,
	ref platform.SecretRevisionRef,
) error {
	raw, err := json.Marshal(ref)
	if err != nil {
		return unavailable(err)
	}
	_, owned, err := s.mutateOwned(ctx, id, attempt, func(tx *entities.StoreTx, _ *contract.Operation) error {
		if _, err := tx.Platform().Deployments().PinCredentialSecret(ctx, id, name, raw); err != nil {
			return unavailable(err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !owned {
		return errNotOwned
	}
	return nil
}

// --------------------------------------------------------------- build state

func (s *Service) markBuildSucceeded(
	ctx context.Context,
	id platform.BuildID,
	releaseID platform.ReleaseID,
	image, commit, imageID string,
) error {
	err := s.entities.WithinTx(ctx, func(tx *entities.StoreTx) error {
		if err := lockBuildParentsTx(ctx, tx, id, releaseID); err != nil {
			return err
		}
		buildRows, err := tx.Platform().Builds().MarkSucceeded(ctx, id, commit)
		if err != nil {
			return unavailable(err)
		}
		releaseRows, err := tx.Platform().Releases().MarkReadyFromBuild(ctx, releaseID, image, imageID, commit)
		if err != nil {
			return unavailable(err)
		}
		if buildRows == 0 || releaseRows == 0 {
			// A cancellation or another terminal writer won the fence. Rolling
			// back keeps the build and release rows consistent instead of
			// publishing a ready release after a stop was accepted.
			return errPlatformCancelled
		}
		return nil
	})
	if err != nil {
		if entities.IsBeginFailure(err) || entities.IsCommitFailure(err) {
			return unavailable(err)
		}
		return err
	}
	return nil
}

func (s *Service) markBuildFailed(
	ctx context.Context,
	id platform.BuildID,
	cause error,
	releaseID platform.ReleaseID,
) error {
	code := contract.Classify(cause).Code
	message := contract.Classify(cause).Message
	if _, err := s.entities.Platform().Builds().MarkFailed(ctx, id, code, message); err != nil {
		return unavailable(err)
	}
	if releaseID != "" {
		if _, err := s.entities.Platform().Releases().MarkFailedByID(ctx, releaseID, code, message); err != nil {
			return unavailable(err)
		}
	}
	return nil
}

func (s *Service) markBuildCancelled(ctx context.Context, id platform.BuildID) error {
	if _, err := s.entities.Platform().Builds().MarkCancelled(ctx, id, contract.Cancelled, "Build stopped by operator."); err != nil {
		return unavailable(err)
	}
	return nil
}

func (s *Service) buildCancelled(ctx context.Context, id platform.BuildID) (bool, error) {
	state, found, err := s.entities.Platform().Builds().State(ctx, id)
	if err != nil || !found {
		return false, unavailable(err)
	}
	return state == platform.BuildCancelled, nil
}

func (s *Service) cancelBuild(ctx context.Context, build platform.BuildRecord) error {
	if err := s.markBuildCancelled(ctx, build.ID); err != nil {
		return err
	}
	if build.ReleaseID != "" {
		if _, err := s.entities.Platform().Releases().MarkFailedByID(ctx, build.ReleaseID, contract.Cancelled, "Build stopped by operator."); err != nil {
			return unavailable(err)
		}
	}
	return nil
}

func (s *Service) requestCancelOperation(
	ctx context.Context,
	id contract.OperationID,
	message string,
) error {
	if id == "" {
		return nil
	}
	_, err := s.mutate(ctx, id, func(tx *entities.StoreTx, op *contract.Operation) error {
		if op.State.Terminal() || op.State == contract.Cancelling {
			return nil
		}
		return transition(ctx, tx, op, contract.Cancelling, "", message)
	})
	return err
}

// ------------------------------------------------------------ deployment data

func (s *Service) saveDeploymentInstances(
	ctx context.Context,
	id contract.OperationID,
	instances []platform.DeploymentInstance,
) error {
	if _, err := s.entities.Platform().Deployments().SetInstancesByOperation(ctx, id, mustJSON(instances)); err != nil {
		return unavailable(err)
	}
	return nil
}

// removeDeploymentContainers removes each recorded container through a durable
// per-container cleanup step. Missing containers are not an error; any other
// failure is recorded for maintenance retry and never changes the switch result.
func (s *Service) removeDeploymentContainers(
	ctx context.Context,
	tr node.Transport,
	operationID contract.OperationID,
	attempt uint32,
	env platform.Environment,
	deploymentID platform.DeploymentID,
	nodeName, reason string,
	instances []platform.DeploymentInstance,
) error {
	protected, err := s.deploymentCleanupProtected(ctx, env, deploymentID)
	if err != nil {
		return err
	}
	if protected {
		return s.deferDeploymentContainerCleanup(ctx, operationID, attempt, env, deploymentID, nodeName, reason, instances)
	}
	return s.removeDeploymentContainerSteps(ctx, tr, operationID, attempt, env, deploymentID, nodeName, reason, instances)
}

func (s *Service) deferDeploymentContainerCleanup(ctx context.Context, operationID contract.OperationID, attempt uint32, env platform.Environment, deploymentID platform.DeploymentID, nodeName, reason string, instances []platform.DeploymentInstance) error {
	cause := contract.NewFault(contract.Unavailable,
		"cleanup deferred while the deployment is serving or is the owner route target",
		"The guarded maintenance cleanup retries after the serving state changes.")
	var failures []error
	for _, instance := range instances {
		if instance.Name == "" {
			continue
		}
		if err := s.ensureContainerCleanup(
			ctx,
			instance.Name,
			env.ID,
			nodeName,
			deploymentID,
			reason,
			cause,
		); err != nil {
			failures = append(
				failures,
				fmt.Errorf("record deferred cleanup for %s: %w", instance.Name, err),
			)
		}
	}
	if len(failures) > 0 {
		s.appendWorkerEventWith(ctx, operationID, attempt, contract.Event{
			Kind:  "warning",
			Stage: "cleanup",
			Message: "Container cleanup was deferred but could not be recorded for retry: " + contract.Classify(
				errors.Join(failures...),
			).Message,
		})
		return errors.Join(failures...)
	}
	s.appendWorkerEventWith(ctx, operationID, attempt, contract.Event{
		Kind:    "warning",
		Stage:   "cleanup",
		Message: "Container cleanup was deferred because the deployment is the serving deployment or the deployment owner's current route target; it was recorded for guarded retry.",
	})
	return nil
}

func (s *Service) removeDeploymentContainerSteps(ctx context.Context, tr node.Transport, operationID contract.OperationID, attempt uint32, env platform.Environment, deploymentID platform.DeploymentID, nodeName, reason string, instances []platform.DeploymentInstance) error {
	worker := &platformWorker{service: s}
	var failures []error
	var failed []string
	var causes []string
	for _, instance := range instances {
		if instance.Name == "" {
			continue
		}
		step := cleanupStepID(instance.Name)
		_, err := worker.runStepOn(ctx, tr, operationID, attempt, step, platform.ArgvPayload{
			Step: step, Argv: []string{"docker", "rm", "-f", instance.Name},
			MissingOK: true, TimeoutSeconds: 30,
		}, nil)
		if err == nil {
			if _, deleteErr := s.entities.Platform().Cleanup().Delete(ctx, instance.Name); deleteErr != nil {
				failures = append(failures, unavailable(deleteErr))
			}
			continue
		}
		if cause := s.cleanupStepCause(err, step); cause != "" {
			causes = append(causes, cause)
		}
		if recordErr := s.recordContainerCleanup(
			ctx,
			instance.Name,
			env.ID,
			nodeName,
			deploymentID,
			reason,
			err,
		); recordErr != nil {
			failures = append(failures, recordErr)
		}
		failures = append(failures, err)
		failed = append(failed, instance.Name)
	}
	return containerCleanupError(failed, causes, failures)
}

// cleanupStepCause is the node's own reason for one failed removal. The step
// error wraps Docker's answer as "node step <id> failed: ..." or as a fault
// with the same detail; both reduce to the detail itself. Redaction runs on the
// raw text before the wrapper is trimmed, whitespace is normalized and the
// result is truncated: a secret can cross the truncation boundary or contain a
// newline that normalization would collapse.
func (s *Service) cleanupStepCause(err error, step string) string {
	message := s.redact(cleanupErrorText(err))
	for _, prefix := range []string{"node step " + step + " failed: ", "node step failed: "} {
		message = strings.TrimPrefix(message, prefix)
	}
	return boundedCleanupCause(message)
}

// containerCleanupError puts a fault naming the containers and the first
// removal reason ahead of the joined step errors, so classification reports
// what failed and why instead of an internal error.
func containerCleanupError(failed, causes []string, failures []error) error {
	if len(failed) == 0 {
		return errors.Join(failures...)
	}
	message := fmt.Sprintf(
		"%d container(s) could not be removed: %s",
		len(failed),
		strings.Join(failed, ", "),
	)
	if len(causes) > 0 {
		message += " (" + causes[0] + ")"
	}
	summary := contract.NewFault(contract.Unavailable, message,
		"Maintenance retries the removal; env status lists pending cleanup.")
	return errors.Join(append([]error{summary}, failures...)...)
}

func previousDeploymentInstances(
	ctx context.Context,
	s *Service,
	deployment platform.Deployment,
	fallbackNode string,
) ([]platform.DeploymentInstance, string, error) {
	if deployment.PreviousDeploymentID == "" {
		return nil, "", nil
	}
	dep, found, err := s.entities.Platform().Deployments().GetByID(ctx, deployment.PreviousDeploymentID)
	if err != nil {
		return nil, "", fmt.Errorf(
			"read previous deployment %s: %w",
			deployment.PreviousDeploymentID,
			unavailable(err),
		)
	}
	if !found {
		return nil, "", fmt.Errorf(
			"read previous deployment %s: %w",
			deployment.PreviousDeploymentID,
			contract.NewFault(contract.PreconditionFailed, "previous deployment is missing", "Inspect deployment history before cleaning up."),
		)
	}
	node, err := deploymentCleanupNode(ctx, s, dep, fallbackNode)
	if err != nil {
		return nil, "", err
	}
	return dep.Instances, node, nil
}

// deploymentCleanupNode resolves the node that actually ran a deployment. A
// pinned revision must resolve to a non-empty runtime node; only an unpinned
// legacy deployment may fall back to the current environment node.
func deploymentCleanupNode(
	ctx context.Context,
	s *Service,
	deployment platform.Deployment,
	fallbackNode string,
) (string, error) {
	if deployment.RevisionID == "" {
		return fallbackNode, nil
	}
	revision, err := s.revisionByID(ctx, deployment.RevisionID)
	if err != nil {
		return "", fmt.Errorf(
			"read pinned revision %s for deployment %s: %w",
			deployment.RevisionID,
			deployment.ID,
			err,
		)
	}
	if strings.TrimSpace(revision.Runtime.Node) == "" {
		return "", fmt.Errorf(
			"pinned revision %s for deployment %s has no runtime node",
			deployment.RevisionID,
			deployment.ID,
		)
	}
	return revision.Runtime.Node, nil
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

// ensurePreviousDeploymentCleanup records the previous deployment's containers
// before success finalization. A crash after the success commit leaves durable
// cleanup work, while a cancellation that wins finalization keeps the previous
// deployment serving and the guarded cleanup defers.
func (s *Service) ensurePreviousDeploymentCleanup(
	ctx context.Context,
	env platform.Environment,
	deployment platform.Deployment,
) error {
	instances, cleanupNode, err := previousDeploymentInstances(ctx, s, deployment, env.Node)
	if err != nil {
		return err
	}
	if len(instances) == 0 {
		return nil
	}
	if cleanupNode == "" {
		return fmt.Errorf(
			"previous deployment %s has no cleanup node",
			deployment.PreviousDeploymentID,
		)
	}
	cause := contract.NewFault(
		contract.Unavailable,
		"previous release cleanup is queued",
		"The guarded maintenance cleanup removes the old containers after the serving state is terminal.",
	)
	var failures []error
	for _, instance := range instances {
		if instance.Name == "" {
			continue
		}
		if err := s.ensureContainerCleanup(
			ctx,
			instance.Name,
			env.ID,
			cleanupNode,
			deployment.PreviousDeploymentID,
			"previous",
			cause,
		); err != nil {
			failures = append(
				failures,
				fmt.Errorf("record previous cleanup for %s: %w", instance.Name, err),
			)
		}
	}
	return errors.Join(failures...)
}

// --------------------------------------------------------------- control steps

// restoreDeploymentRoute sets the owner's exact frozen restore target before
// recording and applying the restore step. Generic route writers already honor
// the owner target; the explicit write makes the restoration happen even if no
// maintenance writer runs. This must complete before an uncertain candidate is
// cleaned up. A failed restore leaves the owner and candidates in place.
func (s *Service) restoreDeploymentRoute(
	ctx context.Context,
	id contract.OperationID,
	attempt uint32,
	env platform.Environment,
	owner deploymentOwner,
) error {
	owner, err := s.setDeploymentOwnerRoute(
		ctx,
		owner,
		owner.RestoreDeploymentID,
		deploymentOwnerRestoring,
		attempt,
	)
	if err != nil {
		return err
	}
	// The exact restore target release is the owner's frozen predecessor. There
	// is no compatibility hash for pre-cutover in-flight restore rows: migration
	// 026 refuses to upgrade while such work exists.
	applied, err := s.runControlStep(ctx, id, attempt, deploymentRestoreRouteStep, map[string]any{
		"environment": env.ID,
		"release":     owner.RestoreReleaseID,
	}, func() error { return s.writeRoutesForDeployment(ctx, env, owner.RestoreDeploymentID) })
	if err != nil {
		return err
	}
	if !applied {
		// A completed row is accepted, but the owner's selected target may be
		// newer than what that row actually applied. Reassert the exact frozen
		// target before recording it as serving.
		if err := s.writeRoutesForDeployment(ctx, env, owner.RestoreDeploymentID); err != nil {
			return err
		}
	}
	if err := s.recordDeploymentRestoreServing(
		ctx,
		env,
		owner.RestoreReleaseID,
		owner.RestoreDeploymentID,
	); err != nil {
		return err
	}
	_, err = s.setDeploymentOwnerRoute(
		ctx,
		owner,
		owner.RestoreDeploymentID,
		deploymentOwnerRestored,
		attempt,
	)
	return err
}

func (s *Service) runControlStep(
	ctx context.Context,
	id contract.OperationID,
	attempt uint32,
	step string,
	inputs any,
	apply func() error,
) (bool, error) {
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
		_ = s.failStep(
			ctx,
			id,
			attempt,
			step,
			contract.Classify(err).Code,
			contract.Classify(err).Message,
		)
		return false, err
	}
	if err := s.ackStep(
		ctx,
		id,
		attempt,
		step,
		inputsHash,
		"",
		nil,
		platformStepTotal,
	); err != nil {
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

// builtReleaseIdentity reads the complete identity a published build must
// return. A missing or mutable reference is a failed build, never a reason to
// fall back to a local tag.
func builtReleaseIdentity(raw json.RawMessage) (string, string, string, error) {
	image := stepResultString(raw, "reference")
	imageID := stepResultString(raw, "image_id")
	runtimeID := stepResultString(raw, "runtime_id")
	if !platform.IsImageDigestReference(image) || !strings.HasPrefix(imageID, "sha256:") ||
		len(imageID) != 71 ||
		!strings.HasPrefix(runtimeID, "sha256:") ||
		len(runtimeID) != 71 {
		return "", "", "", contract.NewFault(
			contract.PreconditionFailed,
			"build did not record a canonical registry digest, config digest and runtime image identity",
			"A Git build must publish to the node registry and return repo@sha256:<manifest>, its config digest, and the local runtime image ID before the release can be ready.",
		)
	}
	return image, imageID, runtimeID, nil
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
