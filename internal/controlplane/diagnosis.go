package controlplane

import (
	"context"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/buildpacks"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/secrets"
)

var diagnosticDigest = regexp.MustCompile(`^[a-zA-Z0-9._:/-]+@sha256:[a-f0-9]{64}$`)
var diagnosticImageID = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// Diagnose uses operation-owned rows and exact foreign keys, never recency.
func (s *Service) Diagnose(ctx context.Context, authority operations.Authority, id contract.OperationID, after uint64, limit int, localNode string) (platform.Diagnosis, error) {
	report := platform.Diagnosis{Schema: "eregion/diagnosis/v1", Events: []contract.Event{}, Cursor: contract.Cursor{OperationID: id, After: after}, Unavailable: []string{}}
	if err := id.Validate(); err != nil {
		return report, err
	}
	op, err := s.Get(ctx, authority, id)
	if err != nil {
		return report, err
	}
	report.Operation = op
	if limit < 1 || limit > 20000 {
		return report, contract.NewFault(contract.ValidationFailed, "diagnosis event limit out of range", "Use a limit from 1 to 20000.")
	}
	if after > op.LastSequence {
		return report, contract.NewFault(contract.PreconditionFailed, "event cursor is ahead of the operation", "Use a cursor returned by this operation.")
	}
	build, buildFound, err := s.diagnoseBuild(ctx, id, &report)
	if err != nil {
		return report, err
	}
	dep, depFound, err := s.diagnoseDeployment(ctx, id, &report)
	if err != nil {
		return report, err
	}
	releaseID, err := s.diagnoseReleaseID(ctx, id, build, buildFound, dep, depFound)
	if err != nil {
		return report, err
	}
	observedImage, err := s.diagnoseReleaseArtifact(ctx, releaseID, &report)
	if err != nil {
		return report, err
	}
	s.diagnoseBuildRevisionFallback(ctx, build, buildFound, &report)
	historical, appName, envName := s.diagnoseHistoricalRevision(ctx, build, buildFound, dep, depFound, &report)
	s.diagnoseCurrentRevision(ctx, appName, envName, &report)
	runtimeID := s.diagnoseImageObservation(ctx, observedImage, historical, localNode, &report)
	s.diagnoseLiveCandidate(ctx, dep, historical, localNode, runtimeID, &report)
	// Capture may append events after terminalization, so the last event is not
	// necessarily the terminal state. Read the most recent failed state within
	// the same immutable snapshot and current attempt independently of paging.
	if err := s.diagnoseFailureState(ctx, id, op, &report); err != nil {
		return report, err
	}
	if err := s.diagnoseEvents(ctx, authority, op, limit, &report); err != nil {
		return report, err
	}
	if err := ctx.Err(); err != nil {
		return report, err
	}
	report.Unavailable = append(report.Unavailable, "child stderr is available only if forwarded to Docker")
	return report, nil
}

func (s *Service) diagnoseBuild(ctx context.Context, id contract.OperationID, report *platform.Diagnosis) (platform.BuildRecord, bool, error) {
	build, found, err := s.buildByOperation(ctx, id)
	if err != nil {
		return build, false, err
	}
	if found {
		report.Build = diagnosisBuild(build)
	} else {
		report.Unavailable = append(report.Unavailable, "build: no build is recorded for this operation")
	}
	return build, found, nil
}

func (s *Service) diagnoseDeployment(ctx context.Context, id contract.OperationID, report *platform.Diagnosis) (platform.Deployment, bool, error) {
	dep, found, err := s.entities.Platform().Deployments().GetByOperation(ctx, id)
	if err != nil {
		return dep, false, unavailable(err)
	}
	if found {
		report.Deployment = &platform.DiagnosisDeployment{ID: dep.ID, ReleaseID: dep.ReleaseID, RevisionID: dep.RevisionID, State: dep.State}
	} else {
		report.Unavailable = append(report.Unavailable, "deployment: no deployment is recorded for this operation")
	}
	return dep, found, nil
}

func (s *Service) diagnoseReleaseArtifact(ctx context.Context, releaseID platform.ReleaseID, report *platform.Diagnosis) (string, error) {
	if releaseID == "" {
		report.Unavailable = append(report.Unavailable, "release: no artifact relationship is recorded")
		return "", nil
	}
	return s.diagnoseRelease(ctx, releaseID, report)
}

func (s *Service) diagnoseLiveCandidate(ctx context.Context, dep platform.Deployment, historical *platform.EnvironmentRevision, localNode, runtimeID string, report *platform.Diagnosis) {
	if report.Deployment == nil {
		report.Unavailable = append(report.Unavailable, "live candidate: no deployment recorded")
		return
	}
	live, reason := s.diagnosisCandidate(ctx, dep, historical, localNode, runtimeID)
	report.LiveCandidate = live
	if reason != "" {
		report.Unavailable = append(report.Unavailable, "live candidate: "+reason)
	}
}

func (s *Service) diagnoseReleaseID(ctx context.Context, id contract.OperationID, build platform.BuildRecord, buildFound bool, dep platform.Deployment, depFound bool) (platform.ReleaseID, error) {
	var releaseID platform.ReleaseID
	if depFound {
		releaseID = dep.ReleaseID
	}
	if buildFound && build.ReleaseID != "" {
		if releaseID != "" && releaseID != build.ReleaseID {
			return "", contract.NewFault(contract.PreconditionFailed, "operation artifact relationships disagree", "Inspect operation inventory before proceeding.")
		}
		releaseID = build.ReleaseID
	}
	if releaseID != "" {
		return releaseID, nil
	}
	release, ok, err := s.entities.Platform().Releases().GetByOperation(ctx, id)
	if err != nil {
		return "", unavailable(err)
	}
	if ok {
		releaseID = release.ID
	}
	return releaseID, nil
}

func (s *Service) diagnoseRelease(ctx context.Context, releaseID platform.ReleaseID, report *platform.Diagnosis) (string, error) {
	release, err := s.releaseByID(ctx, releaseID)
	if err != nil {
		return "", err
	}
	r := &platform.DiagnosisRelease{ID: release.ID, BuildID: release.BuildID, CommitSHA: release.CommitSHA, Status: release.Status, Pruned: release.PrunedAt != nil}
	if diagnosticImageID.MatchString(release.ImageID) {
		// The recorded image_id is the authenticated immutable config
		// identity. The local Docker runtime ID is observed separately
		// below and must never replace this field.
		r.ImageID = release.ImageID
	}
	if parsed, parseErr := platform.ParseFullyQualifiedImage(release.Image); parseErr == nil && parsed.Digest != "" {
		r.ImageDigest = parsed.Digest
	} else if diagnosticDigest.MatchString(release.Image) {
		r.ImageDigest = release.Image
	} else {
		report.Unavailable = append(report.Unavailable, "image digest: exact image reference not recorded")
	}
	report.Release = r
	if release.Build != nil {
		b := release.Build
		settings := diagnosisBuildSettings(b.Builder, release.Source.Path, b.Env, b.GoTargets, b.Buildpacks)
		report.RecordedBuild = &settings
	} else {
		report.Unavailable = append(report.Unavailable, "release build settings: no build snapshot recorded")
	}
	if release.BuildID != "" {
		s.diagnoseSourceBuild(ctx, release.BuildID, report)
	}
	return release.Image, nil
}

func (s *Service) diagnoseSourceBuild(ctx context.Context, buildID platform.BuildID, report *platform.Diagnosis) {
	linked, err := s.buildByID(ctx, buildID)
	if err != nil {
		report.Unavailable = append(report.Unavailable, "release source build: referenced build unavailable")
		return
	}
	report.SourceBuild = diagnosisBuild(linked)
	if linked.RevisionID == "" {
		report.Unavailable = append(report.Unavailable, "build revision: build did not record a revision")
		return
	}
	br, err := s.revisionByID(ctx, linked.RevisionID)
	if err != nil || br.App != linked.App || br.Environment != linked.Environment {
		report.Unavailable = append(report.Unavailable, "build revision: recorded source revision unavailable or inconsistent")
		return
	}
	projected := diagnosisRevision(br)
	report.BuildRevision = &projected
}

func (s *Service) diagnoseBuildRevisionFallback(ctx context.Context, build platform.BuildRecord, buildFound bool, report *platform.Diagnosis) {
	if report.BuildRevision != nil || !buildFound || build.RevisionID == "" {
		return
	}
	br, err := s.revisionByID(ctx, build.RevisionID)
	if err != nil || br.App != build.App || br.Environment != build.Environment {
		report.Unavailable = append(report.Unavailable, "build revision: recorded revision unavailable or inconsistent")
		return
	}
	projected := diagnosisRevision(br)
	report.BuildRevision = &projected
}

func (s *Service) diagnoseHistoricalRevision(ctx context.Context, build platform.BuildRecord, buildFound bool, dep platform.Deployment, depFound bool, report *platform.Diagnosis) (*platform.EnvironmentRevision, string, string) {
	var revisionID platform.RevisionID
	var appName, envName string
	if buildFound {
		revisionID, appName, envName = build.RevisionID, build.App, build.Environment
	}
	if depFound {
		revisionID, appName, envName = dep.RevisionID, dep.App, dep.Environment
	}
	if revisionID == "" {
		report.Unavailable = append(report.Unavailable, "historical revision: operation did not record a revision")
		return nil, appName, envName
	}
	r, err := s.revisionByID(ctx, revisionID)
	if err != nil || r.App != appName || r.Environment != envName {
		report.Unavailable = append(report.Unavailable, "historical revision: recorded revision is unavailable or inconsistent")
		return nil, appName, envName
	}
	projected := diagnosisRevision(r)
	report.Historical = &projected
	return &r, appName, envName
}

func (s *Service) diagnoseCurrentRevision(ctx context.Context, appName, envName string, report *platform.Diagnosis) {
	if appName == "" {
		return
	}
	env, err := s.environment(ctx, appName, envName)
	if err != nil || env.Revision == nil {
		report.Unavailable = append(report.Unavailable, "current desired revision: environment or revision unavailable")
		return
	}
	projected := diagnosisRevision(*env.Revision)
	report.Current = &projected
}

func (s *Service) diagnoseImageObservation(ctx context.Context, observedImage string, historical *platform.EnvironmentRevision, localNode string, report *platform.Diagnosis) string {
	if report.Release == nil || report.Release.ImageDigest == "" {
		report.Unavailable = append(report.Unavailable, "image observation: canonical image reference not recorded")
		return ""
	}
	if report.Deployment == nil || historical == nil || historical.Runtime.Node == "" || localNode == "" {
		report.Unavailable = append(report.Unavailable, "image observation: historical node placement is unknown")
		return ""
	}
	if historical.Runtime.Node != localNode {
		report.Unavailable = append(report.Unavailable, "image observation: historical placement is another node; remote observation unsupported")
		return ""
	}
	// Resolve the canonical manifest to the Docker runtime image ID before
	// observing labels or comparing candidate container .Image values.
	resolveRuntimeID := platform.ResolveDiagnosisImageID
	if s.diagnosisImageResolver != nil {
		resolveRuntimeID = s.diagnosisImageResolver
	}
	observeImage := platform.ObserveDiagnosisImage
	if s.diagnosisImageObserver != nil {
		observeImage = s.diagnosisImageObserver
	}
	resolvedID, reason := resolveRuntimeID(ctx, observedImage)
	if reason != "" {
		report.Unavailable = append(report.Unavailable, "image observation: "+reason)
		return resolvedID
	}
	image, reason := observeImage(ctx, resolvedID)
	report.Image = image
	if reason != "" {
		report.Unavailable = append(report.Unavailable, "image observation: "+reason)
	}
	return resolvedID
}

func (s *Service) diagnoseFailureState(ctx context.Context, id contract.OperationID, op contract.Operation, report *platform.Diagnosis) error {
	if op.State != contract.Failed || op.LastSequence == 0 {
		return nil
	}
	event, found, err := s.entities.Operations().FailedStateEvent(ctx, id, op.LastSequence, op.Attempt)
	if err != nil {
		return unavailable(err)
	}
	if !found || event.Validate() != nil || event.OperationID != id || event.Attempt != op.Attempt || event.State != contract.Failed {
		return nil
	}
	report.Failure = &platform.DiagnosisFailure{Message: event.Message, Stage: event.Stage, Code: event.ErrorCode}
	if event.Stage != "" {
		report.Failure.StageSource = "failed state event"
	}
	return nil
}

func (s *Service) diagnoseEvents(ctx context.Context, authority operations.Authority, op contract.Operation, limit int, report *platform.Diagnosis) error {
	bytes := 0
	capturedSeen := false
	for len(report.Events) < limit && report.Cursor.After < op.LastSequence {
		batch := limit - len(report.Events)
		if batch > 200 {
			batch = 200
		}
		page, err := s.Events(ctx, authority, report.Cursor, batch)
		stop, err := handleDiagnosisPage(ctx, report, page, err)
		if err != nil {
			return err
		}
		if stop {
			break
		}
		captured, err := appendDiagnosisPage(report, page, op, &bytes)
		if err != nil {
			return err
		}
		capturedSeen = capturedSeen || captured
		if report.Partial {
			break
		}
	}
	if report.Cursor.After < op.LastSequence {
		report.Partial = true
		report.HasMore = true
	}
	if op.State == contract.Failed && (report.Failure == nil || report.Failure.Message == "") {
		report.Unavailable = append(report.Unavailable, "failure detail: failed state event not in this event page")
	}
	if !capturedSeen {
		report.Unavailable = append(report.Unavailable, "captured candidate diagnostics: no capture evidence in this event page")
	}
	return nil
}

func handleDiagnosisPage(ctx context.Context, report *platform.Diagnosis, page operations.EventPage, pageErr error) (bool, error) {
	if pageErr != nil {
		if ctx.Err() != nil {
			return true, ctx.Err()
		}
		report.Partial = true
		report.HistoryUnavailable = true
		report.Unavailable = append(report.Unavailable, "events: history unavailable or has a gap")
		return true, nil
	}
	if len(page.Events) == 0 || page.Cursor.After <= report.Cursor.After {
		report.Partial = true
		report.HistoryUnavailable = true
		report.Unavailable = append(report.Unavailable, "events: history ended before snapshot cursor")
		return true, nil
	}
	return false, nil
}

func appendDiagnosisPage(report *platform.Diagnosis, page operations.EventPage, op contract.Operation, bytes *int) (bool, error) {
	capturedSeen := false
	for _, event := range page.Events {
		if event.Sequence > op.LastSequence {
			break
		}
		raw, err := json.Marshal(event)
		if err != nil {
			return capturedSeen, err
		}
		if *bytes+len(raw) > 2*1024*1024 {
			report.Partial = true
			break
		}
		*bytes += len(raw)
		report.Events = append(report.Events, event)
		report.Cursor.After = event.Sequence
		if event.Stage == platform.CandidateDiagnosticStage && event.CandidateDiagnostic != nil {
			capturedSeen = true
		}
		observeDiagnosisFailure(report, op, event)
	}
	return capturedSeen, nil
}

func observeDiagnosisFailure(report *platform.Diagnosis, op contract.Operation, event contract.Event) {
	if event.Attempt == op.Attempt && event.Kind == "warning" && event.Stage != "" && strings.HasPrefix(event.Message, "Node step failed: ") {
		if report.Failure == nil {
			report.Failure = &platform.DiagnosisFailure{}
		}
		if report.Failure.Stage == "" || report.Failure.StageSource != "failed state event" {
			report.Failure.Stage = event.Stage
			report.Failure.StageSource = "observed failed node step; not necessarily terminal cause"
		}
		report.Failure.StepMessage = event.Message
	}
	if event.Kind != "state" || event.State != contract.Failed || event.Attempt != op.Attempt {
		return
	}
	if report.Failure == nil {
		report.Failure = &platform.DiagnosisFailure{}
	}
	report.Failure.Message = event.Message
	report.Failure.Code = event.ErrorCode
	if event.Stage != "" {
		report.Failure.Stage = event.Stage
		report.Failure.StageSource = "failed state event"
	}
}

func diagnosisBuild(b platform.BuildRecord) *platform.DiagnosisBuild {
	return &platform.DiagnosisBuild{ID: b.ID, OperationID: b.OperationID, App: b.App, Environment: b.Environment, RevisionID: b.RevisionID, ReleaseID: b.ReleaseID, CommitSHA: b.CommitSHA, State: b.State}
}

func (s *Service) diagnosisCandidate(parent context.Context, dep platform.Deployment, revision *platform.EnvironmentRevision, localNode, imageID string) (*platform.CandidateDiagnosticResult, string) {
	if !diagnosticImageID.MatchString(imageID) {
		return nil, "release immutable image ID is unavailable; logs withheld"
	}
	if revision == nil || revision.Runtime.Node == "" || localNode == "" {
		return nil, "historical node placement is unknown"
	}
	if revision.Runtime.Node != localNode {
		return nil, "placement is another node; remote observation unsupported"
	}
	if dep.State == platform.DeploymentSucceeded {
		return nil, "deployment succeeded and is not a candidate"
	}
	ctx, cancel := context.WithTimeout(parent, 5*time.Second)
	defer cancel()
	env, err := s.environment(ctx, dep.App, dep.Environment)
	if err != nil || env.ServingDeploymentID == dep.ID {
		return nil, "environment unavailable or deployment is serving"
	}
	env.Revision = revision
	values, metadataOnly := s.candidateSecretValues(ctx, dep, env, revision)
	registry := &secrets.Registry{}
	for _, value := range values {
		registry.Add(value)
	}
	payload, truncated, ok := candidateDiagnosticPayload(ctx, dep, revision, metadataOnly)
	if !ok {
		return nil, "candidate formation unavailable"
	}
	result, err := platform.ObserveCandidateDiagnostics(ctx, payload, imageID, engine.Runtime{Secrets: registry, SecretValues: values})
	if err != nil {
		return nil, "candidate observation failed"
	}
	result.Truncated = result.Truncated || truncated
	if metadataOnly {
		return &result, "pinned secret redaction context unavailable or incomplete; logs withheld"
	}
	if result.ImageMismatch {
		return &result, "candidate image identity missing or differs from the recorded release; affected logs withheld"
	}
	if result.Unavailable {
		return &result, "some candidate evidence was unavailable or logs were withheld"
	}
	return &result, ""
}

func (s *Service) candidateSecretValues(ctx context.Context, dep platform.Deployment, env platform.Environment, revision *platform.EnvironmentRevision) (map[string]string, bool) {
	names := append([]string(nil), revision.SecretNames...)
	// Older snapshots without pins cannot prove whether legacy secret refs
	// were used, even if today's refs differ.
	metadataOnly := len(dep.SecretRevisions) == 0 && len(env.SecretRefs) > 0
	for _, name := range names {
		if _, ok := dep.SecretRevisions[name]; !ok {
			metadataOnly = true
		}
	}
	values := map[string]string{}
	if !metadataOnly {
		// Never resolve today's names when no secret was pinned to the action.
		// The current environment can have unrelated later secret references.
		env.SecretRefs = nil
		worker := &platformWorker{service: s}
		var key, credential string
		var err error
		values, key, credential, _, err = worker.resolveDeploymentSecrets(ctx, env, dep)
		if err != nil {
			metadataOnly = true
			values = map[string]string{}
		} else if credential != "" {
			values[key] = credential
		}
	}
	for name := range dep.SecretRevisions {
		if values[name] == "" {
			metadataOnly = true
		}
	}
	for _, name := range names {
		if values[name] == "" {
			metadataOnly = true
		}
	}
	return values, metadataOnly
}

func candidateDiagnosticPayload(ctx context.Context, dep platform.Deployment, revision *platform.EnvironmentRevision, metadataOnly bool) (platform.CandidateDiagnosticPayload, bool, bool) {
	plan, err := platform.PlanDeployment(dep.App, dep.Environment, revision.RevisionState, dep.ReleaseID, dep.ID)
	if err != nil || len(plan) == 0 {
		return platform.CandidateDiagnosticPayload{}, false, false
	}
	truncated := len(plan) > 64
	if truncated {
		plan = plan[:64]
	}
	deadline, _ := ctx.Deadline()
	payload := platform.CandidateDiagnosticPayload{Step: platform.CandidateDiagnosticStep, App: dep.App, Environment: dep.Environment, ReleaseID: dep.ReleaseID, DeploymentID: dep.ID, Deadline: deadline, MetadataOnly: metadataOnly}
	for _, p := range plan {
		payload.Containers = append(payload.Containers, platform.CandidateDiagnosticContainer{Name: p.Name, Process: p.Process.Name, Kind: p.Process.Kind, Replica: p.Replica})
	}
	return payload, truncated, true
}

func diagnosisRevision(r platform.EnvironmentRevision) platform.DiagnosisRevision {
	out := platform.DiagnosisRevision{ID: r.ID, Number: r.Number, Node: r.Runtime.Node, Processes: []platform.DiagnosisProcess{}, Build: diagnosisBuildSettings(r.Source.Builder, r.Source.ContextPath, r.Source.BuildEnv, r.Source.GoTargets, r.Source.Buildpacks)}
	for _, p := range r.Processes {
		out.Processes = append(out.Processes, platform.DiagnosisProcess{Name: p.Name, Kind: p.Kind, Replicas: p.Replicas, ContainerPort: p.ContainerPort, CommandOmitted: p.Command != "" || len(p.Args) != 0})
	}
	return out
}

func diagnosisBuildSettings(builder, path string, rawEnv map[string]string, targets []buildpacks.GoTarget, settings *buildpacks.Settings) platform.DiagnosisBuildSettings {
	out := platform.DiagnosisBuildSettings{Builder: builder, ContextPath: path, Effective: map[string]string{}, RawBuildEnvPresent: len(rawEnv) > 0, OmissionReason: "raw build environment values and arbitrary Go build flags are omitted"}
	if settings != nil {
		out.Order = append([]string(nil), settings.Order...)
		if settings.Advanced != nil {
			out.GoBuildFlagsPresent = settings.Advanced.GoBuildFlags != ""
		}
	}
	if _, present := rawEnv["BP_GO_BUILD_FLAGS"]; present {
		out.GoBuildFlagsPresent = true
	}
	generated := buildpacks.Environment(settings, targets)
	delete(generated, "BP_GO_BUILD_FLAGS")
	for k, v := range generated {
		out.Effective[k] = v
	}
	for _, key := range []string{"BP_GO_TARGETS", "BP_KEEP_FILES", "BP_NPM_START_SCRIPT", "BP_NODE_RUN_SCRIPTS", "BP_NODE_PROJECT_PATH", "BP_GO_VERSION", "BP_NODE_VERSION", "BP_GO_BUILD_FLAGS"} {
		if _, ok := rawEnv[key]; ok {
			if key == "BP_GO_BUILD_FLAGS" && settings != nil && settings.Advanced != nil && settings.Advanced.GoBuildFlags != "" {
				continue
			}
			if _, typed := generated[key]; !typed {
				out.RawOverridesOmitted = append(out.RawOverridesOmitted, key)
			}
		}
	}
	sort.Strings(out.RawOverridesOmitted)
	return out
}
