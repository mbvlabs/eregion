package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
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
	build, found, err := s.buildByOperation(ctx, id)
	if err != nil {
		return report, err
	}
	if found {
		report.Build = diagnosisBuild(build)
	} else {
		report.Unavailable = append(report.Unavailable, "build: no build is recorded for this operation")
	}
	dep, err := scanDeploymentRow(s.pool.QueryRow(ctx, deploymentSelect+" WHERE d.operation_id=$1", id))
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return report, unavailable()
	}
	var releaseID platform.ReleaseID
	if err == nil {
		report.Deployment = &platform.DiagnosisDeployment{ID: dep.ID, ReleaseID: dep.ReleaseID, RevisionID: dep.RevisionID, State: dep.State}
		releaseID = dep.ReleaseID
	} else {
		report.Unavailable = append(report.Unavailable, "deployment: no deployment is recorded for this operation")
	}
	if found && build.ReleaseID != "" {
		if releaseID != "" && releaseID != build.ReleaseID {
			return report, contract.NewFault(contract.PreconditionFailed, "operation artifact relationships disagree", "Inspect operation inventory before proceeding.")
		}
		releaseID = build.ReleaseID
	}
	if releaseID == "" {
		release, e := scanReleaseRow(s.pool.QueryRow(ctx, releaseSelect+" WHERE r.operation_id=$1", id))
		if e == nil {
			releaseID = release.ID
		} else if !errors.Is(e, pgx.ErrNoRows) {
			return report, unavailable()
		}
	}
	if releaseID != "" {
		release, e := s.releaseByID(ctx, releaseID)
		if e != nil {
			return report, e
		}
		r := &platform.DiagnosisRelease{ID: release.ID, BuildID: release.BuildID, CommitSHA: release.CommitSHA, Status: release.Status, Pruned: release.PrunedAt != nil}
		if diagnosticImageID.MatchString(release.ImageID) {
			r.ImageID = release.ImageID
		}
		if diagnosticDigest.MatchString(release.ImageID) {
			r.ImageDigest = release.ImageID
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
			linked, e := s.buildByID(ctx, release.BuildID)
			if e != nil {
				report.Unavailable = append(report.Unavailable, "release source build: referenced build unavailable")
			} else {
				report.SourceBuild = diagnosisBuild(linked)
				if linked.RevisionID != "" {
					br, e := s.revisionByID(ctx, linked.RevisionID)
					if e == nil && br.App == linked.App && br.Environment == linked.Environment {
						projected := diagnosisRevision(br)
						report.BuildRevision = &projected
					} else {
						report.Unavailable = append(report.Unavailable, "build revision: recorded source revision unavailable or inconsistent")
					}
				} else {
					report.Unavailable = append(report.Unavailable, "build revision: build did not record a revision")
				}
			}
		}
	} else {
		report.Unavailable = append(report.Unavailable, "release: no artifact relationship is recorded")
	}
	if report.BuildRevision == nil && found && build.RevisionID != "" {
		br, e := s.revisionByID(ctx, build.RevisionID)
		if e == nil && br.App == build.App && br.Environment == build.Environment {
			projected := diagnosisRevision(br)
			report.BuildRevision = &projected
		} else {
			report.Unavailable = append(report.Unavailable, "build revision: recorded revision unavailable or inconsistent")
		}
	}
	var revisionID platform.RevisionID
	var appName, envName string
	if found {
		revisionID, appName, envName = build.RevisionID, build.App, build.Environment
	}
	if report.Deployment != nil {
		revisionID, appName, envName = dep.RevisionID, dep.App, dep.Environment
	}
	var historical *platform.EnvironmentRevision
	if revisionID != "" {
		r, e := s.revisionByID(ctx, revisionID)
		if e == nil && r.App == appName && r.Environment == envName {
			historical = &r
			projected := diagnosisRevision(r)
			report.Historical = &projected
		} else {
			report.Unavailable = append(report.Unavailable, "historical revision: recorded revision is unavailable or inconsistent")
		}
	} else {
		report.Unavailable = append(report.Unavailable, "historical revision: operation did not record a revision")
	}
	if appName != "" {
		env, e := s.environment(ctx, appName, envName)
		if e == nil && env.Revision != nil {
			projected := diagnosisRevision(*env.Revision)
			report.Current = &projected
		} else {
			report.Unavailable = append(report.Unavailable, "current desired revision: environment or revision unavailable")
		}
	}
	if report.Release == nil || report.Release.ImageID == "" {
		report.Unavailable = append(report.Unavailable, "image observation: immutable local image ID not recorded")
	} else if report.Deployment == nil || historical == nil || historical.Runtime.Node == "" || localNode == "" {
		report.Unavailable = append(report.Unavailable, "image observation: historical node placement is unknown")
	} else if historical.Runtime.Node != localNode {
		report.Unavailable = append(report.Unavailable, "image observation: historical placement is another node; remote observation unsupported")
	} else {
		image, reason := platform.ObserveDiagnosisImage(ctx, report.Release.ImageID)
		report.Image = image
		if reason != "" {
			report.Unavailable = append(report.Unavailable, "image observation: "+reason)
		}
	}
	if report.Deployment != nil {
		imageID := ""
		if report.Release != nil {
			imageID = report.Release.ImageID
		}
		live, reason := s.diagnosisCandidate(ctx, dep, historical, localNode, imageID)
		report.LiveCandidate = live
		if reason != "" {
			report.Unavailable = append(report.Unavailable, "live candidate: "+reason)
		}
	} else {
		report.Unavailable = append(report.Unavailable, "live candidate: no deployment recorded")
	}
	// Capture may append events after terminalization, so the last event is not
	// necessarily the terminal state. Read the most recent failed state within
	// the same immutable snapshot and current attempt independently of paging.
	if op.State == contract.Failed && op.LastSequence > 0 {
		var raw []byte
		e := s.pool.QueryRow(ctx, `SELECT event FROM eregion_operation_events WHERE operation_id=$1 AND sequence<=$2 AND event->>'kind'='state' AND event->>'state'='failed' AND event->>'attempt'=$3 ORDER BY sequence DESC LIMIT 1`, id, int64(op.LastSequence), fmt.Sprint(op.Attempt)).Scan(&raw)
		var event contract.Event
		if e == nil && json.Unmarshal(raw, &event) == nil && event.Validate() == nil && event.OperationID == id && event.Attempt == op.Attempt && event.State == contract.Failed {
			report.Failure = &platform.DiagnosisFailure{Message: event.Message, Stage: event.Stage, Code: event.ErrorCode}
			if event.Stage != "" {
				report.Failure.StageSource = "failed state event"
			}
		}
	}
	bytes := 0
	capturedSeen := false
	for len(report.Events) < limit && report.Cursor.After < op.LastSequence {
		batch := limit - len(report.Events)
		if batch > 200 {
			batch = 200
		}
		page, e := s.Events(ctx, authority, report.Cursor, batch)
		if e != nil {
			if ctx.Err() != nil {
				return report, ctx.Err()
			}
			report.Partial = true
			report.HistoryUnavailable = true
			report.Unavailable = append(report.Unavailable, "events: history unavailable or has a gap")
			break
		}
		if len(page.Events) == 0 || page.Cursor.After <= report.Cursor.After {
			report.Partial = true
			report.HistoryUnavailable = true
			report.Unavailable = append(report.Unavailable, "events: history ended before snapshot cursor")
			break
		}
		for _, event := range page.Events {
			if event.Sequence > op.LastSequence {
				break
			}
			raw, e := json.Marshal(event)
			if e != nil {
				return report, e
			}
			if bytes+len(raw) > 2*1024*1024 {
				report.Partial = true
				break
			}
			bytes += len(raw)
			report.Events = append(report.Events, event)
			report.Cursor.After = event.Sequence
			if event.Stage == platform.CandidateDiagnosticStage && event.CandidateDiagnostic != nil {
				capturedSeen = true
			}
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
			if event.Kind == "state" && event.State == contract.Failed {
				if event.Attempt != op.Attempt {
					continue
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
		}
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
	if err := ctx.Err(); err != nil {
		return report, err
	}
	report.Unavailable = append(report.Unavailable, "child stderr is available only if forwarded to Docker")
	return report, nil
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
	names := append([]string(nil), revision.SecretNames...)
	metadataOnly := false
	if len(dep.SecretRevisions) == 0 && len(env.SecretRefs) > 0 {
		// Older snapshots without pins cannot prove whether legacy secret refs
		// were used, even if today's refs differ.
		metadataOnly = true
	}
	for _, name := range names {
		if _, ok := dep.SecretRevisions[name]; !ok {
			metadataOnly = true
		}
	}
	worker := &platformWorker{service: s}
	values := map[string]string{}
	if !metadataOnly {
		// Never resolve today's names when no secret was pinned to the action.
		// The current environment can have unrelated later secret references.
		env.SecretRefs = nil
		var key, credential string
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
	registry := &secrets.Registry{}
	for _, value := range values {
		registry.Add(value)
	}
	plan, err := platform.PlanDeployment(dep.App, dep.Environment, revision.RevisionState, dep.ReleaseID, dep.ID)
	if err != nil || len(plan) == 0 {
		return nil, "candidate formation unavailable"
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
