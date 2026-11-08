package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

// promotionPlan is the immutable artifact and the target state a promotion
// submission has validated under lock. The source release is never re-resolved
// later: its image digest, commit and source are written to the new production
// release in the same submission transaction.
type promotionPlan struct {
	SourceEnvironment platform.Environment
	SourceRelease     platform.Release
	TargetEnvironment platform.Environment
}

// preparePromotionTx records a new production release for the exact source
// serving artifact, then prepares the ordinary blue/green deployment against
// production's current revision. No build row is created. The new release
// deliberately has no command of its own: the deployment resolves production's
// current revision release command, so a staging command is never copied.
func (s *Service) preparePromotionTx(ctx context.Context, tx pgx.Tx, operationID contract.OperationID, input *platform.PromoteInput) error {
	plan, err := s.promotionPlanTx(ctx, tx, input)
	if err != nil {
		return err
	}
	releaseID, err := platform.NewReleaseID()
	if err != nil {
		return unavailable()
	}
	sourceJSON, err := json.Marshal(plan.SourceRelease.Source)
	if err != nil {
		return contract.NewFault(contract.Internal, "could not encode the promoted artifact source", "Report the command and binary version.")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO eregion_platform_releases
        (id,operation_id,app_id,environment_id,image,image_id,source,status,commit_sha,completed_at)
        VALUES($1,$2,$3,$4,$5,$6,$7,'ready',$8,clock_timestamp())`,
		releaseID, operationID, envAppID(ctx, tx, plan.TargetEnvironment.App), plan.TargetEnvironment.ID,
		plan.SourceRelease.Image, plan.SourceRelease.ImageID, sourceJSON, plan.SourceRelease.CommitSHA); err != nil {
		return platformWriteError(err, "promoted release could not be recorded")
	}
	if err := s.prepareDeploymentTx(ctx, tx, operationID, plan.TargetEnvironment, releaseID,
		platform.ActionDeploy, plan.TargetEnvironment.RevisionID,
		plan.TargetEnvironment.ServingDeploymentID, nil); err != nil {
		return err
	}
	return nil
}

// promotionPlanTx locks the source environment before the production target.
// A staging deploy submits under the same source row lock, so the source
// serving release cannot advance between validation and the release insert.
// The target row lock serializes this promotion with any other submission that
// would change production's serving state.
func (s *Service) promotionPlanTx(ctx context.Context, tx pgx.Tx, input *platform.PromoteInput) (promotionPlan, error) {
	source, err := environmentByNameTx(ctx, tx, input.App, input.SourceEnvironment)
	if err != nil {
		return promotionPlan{}, err
	}
	target, err := environmentByNameTx(ctx, tx, input.App, platform.PromotionTargetEnvironment)
	if err != nil {
		return promotionPlan{}, promotionTargetFault(err)
	}
	if source.ServingRelease == "" {
		return promotionPlan{}, promotionSourceServingFault(input)
	}
	release, err := releaseByIDTx(ctx, tx, source.ServingRelease)
	if err != nil {
		return promotionPlan{}, err
	}
	if err := validatePromotionState(input, source, target, release); err != nil {
		return promotionPlan{}, err
	}
	if err := s.checkDeployabilityTx(ctx, tx, target, true); err != nil {
		return promotionPlan{}, err
	}
	if err := s.checkPlacementByTx(ctx, tx, target.Node, platform.RoleApplication, platform.CapabilityDocker); err != nil {
		return promotionPlan{}, err
	}
	if err := promotionDeploymentConflict(ctx, tx, target, release); err != nil {
		return promotionPlan{}, err
	}
	return promotionPlan{SourceEnvironment: source, SourceRelease: release, TargetEnvironment: target}, nil
}

// previewPromotion resolves the same facts without mutation. A preview is
// advisory and takes no row lock; the submission repeats every check inside its
// transaction.
func (s *Service) previewPromotion(ctx context.Context, normalized platform.Normalized, input *platform.PromoteInput) (platform.Preview, error) {
	preview := platform.Preview{
		Kind: platform.KindPromote, Target: normalized.Target.String(), DryRun: true,
		Input: input.Preview(), Summary: normalized.Summary,
	}
	source, err := s.environment(ctx, input.App, input.SourceEnvironment)
	if err != nil {
		return preview, err
	}
	target, err := s.environment(ctx, input.App, platform.PromotionTargetEnvironment)
	if err != nil {
		return preview, promotionTargetFault(err)
	}
	if source.ServingRelease == "" {
		return preview, promotionSourceServingFault(input)
	}
	release, err := s.releaseByID(ctx, source.ServingRelease)
	if err != nil {
		return preview, err
	}
	if err := validatePromotionState(input, source, target, release); err != nil {
		return preview, err
	}
	if err := s.promotionDeployability(ctx, target); err != nil {
		return preview, err
	}
	if err := s.checkPreviewPlacement(ctx, target.Node, ""); err != nil {
		return preview, err
	}
	if err := promotionDeploymentConflict(ctx, s.pool, target, release); err != nil {
		return preview, err
	}
	preview.Steps = promotionSteps()
	preview.Summary = fmt.Sprintf("Promote %s/%s serving release %s to %s/%s without rebuilding; production's current revision, config, secrets, domains and release command apply.",
		input.App, input.SourceEnvironment, release.ID, input.App, platform.PromotionTargetEnvironment)
	if target.ServingRelease != "" {
		preview.Warnings = append(preview.Warnings, "Production's serving release stays until the candidate passes health and its primary-domain public check.")
	}
	preview.Warnings = append(preview.Warnings,
		"Nothing is copied from "+input.App+"/"+input.SourceEnvironment+": production keeps its own domain bindings, plain config, secret names and release command.",
		"The exact image digest is pinned as a new production release; no build is requested.")
	preview.Data = map[string]any{
		"source_environment":        input.SourceEnvironment,
		"source_release_id":         string(release.ID),
		"source_image":              release.Image,
		"source_image_id":           release.ImageID,
		"source_commit":             release.CommitSHA,
		"target_environment":        platform.PromotionTargetEnvironment,
		"target_revision_id":        string(target.RevisionID),
		"target_serving_release_id": string(target.ServingRelease),
	}
	return preview, nil
}

func (s *Service) promotionDeployability(ctx context.Context, env platform.Environment) error {
	report := platform.EvaluateDeployability(env.Revision, platform.DeployabilityState{
		ArtifactReady: true, ExplicitRelease: true, PrimaryDomain: env.PrimaryDomain, DNS: env.Deployability.DNS,
	})
	missing, err := s.resourceDeployability(ctx, env)
	if err != nil {
		return err
	}
	report.Missing = append(report.Missing, missing...)
	report.Deployable = len(report.Missing) == 0
	if report.Deployable {
		return nil
	}
	fault := contract.NewFault(contract.PreconditionFailed,
		"environment is not deployable: "+strings.Join(report.Missing, ", "),
		"Fix the listed desired state before promoting; eregion env status shows the same report.")
	fault.Details = map[string]any{"missing": report.Missing}
	return fault
}

// validatePromotionState is the database-independent safety gate shared by
// preview and submission. It proves the source artifact is a ready, unpruned
// release that is currently serving its own environment, and that the public
// target can be verified on the same node without cross-node artifact transfer.
func validatePromotionState(input *platform.PromoteInput, source, target platform.Environment, release platform.Release) error {
	if input.App != target.App || input.App != source.App {
		return contract.NewFault(contract.ValidationFailed, "promotion must stay inside one application", "Use environments from the same app; promotion never crosses app ownership.")
	}
	if target.Name != platform.PromotionTargetEnvironment {
		return contract.NewFault(contract.ValidationFailed, "promotion target must be production", "Use env promote; the production target is fixed.")
	}
	if source.Name == target.Name {
		return contract.NewFault(contract.ValidationFailed, "an environment cannot be promoted to itself", "Name a different source environment.")
	}
	if source.ServingRelease == "" {
		return promotionSourceServingFault(input)
	}
	if source.ServingRelease != release.ID {
		return contract.NewFault(contract.Conflict, "the source serving release changed during promotion", "Read the source environment again and retry; no production release was recorded.")
	}
	if release.App != source.App || release.Environment != source.Name {
		return contract.NewFault(contract.Conflict, "the source release does not belong to the source environment", "Use the serving release of the named environment.")
	}
	if err := refusePrunedRelease(release); err != nil {
		return err
	}
	if release.Status != platform.ReleaseReady {
		return contract.NewFault(contract.PreconditionFailed, "the source serving release is not ready", "Wait for the source release to become ready before promoting it.")
	}
	if release.ImageID == "" {
		return contract.NewFault(contract.PreconditionFailed, "the source serving release has no immutable image", "Deploy a ready release with a verified image digest before promoting it.")
	}
	if source.Node != target.Node {
		return contract.NewFault(contract.PreconditionFailed, "the source and production environments are on different nodes", "Promotion reuses the exact image on the production node; cross-node artifact transfer is not implemented. Use the same node, or deploy from production's own source.")
	}
	return nil
}

func promotionSourceServingFault(input *platform.PromoteInput) error {
	return contract.NewFault(contract.PreconditionFailed,
		"environment "+input.App+"/"+input.SourceEnvironment+" has no serving release",
		"Deploy the source environment and wait for its public verification before promoting it.")
}

func promotionTargetFault(err error) error {
	if contract.Classify(err).Code != contract.NotFound {
		return err
	}
	return contract.NewFault(contract.PreconditionFailed,
		"application has no production environment to promote to",
		"Create or restore the application's production environment before promoting a release.")
}

// promotionDeploymentConflict refuses a promotion while production has any
// active deployment, not only one carrying the same image. The next deployment
// must have an unambiguous previous deployment; admitting a promotion behind a
// queued different-artifact deploy would pin the wrong previous release at
// admission and make cleanup/rollback history depend on a later race. Ordinary
// deploys may still be queued after a promotion; the execution-time rebind in
// deploymentPlan keeps whichever one runs next pointed at the actual previous
// serving deployment.
func promotionDeploymentConflict(ctx context.Context, q rowQueryer, target platform.Environment, release platform.Release) error {
	if release.ImageID == "" {
		return nil
	}
	if target.ServingRelease != "" {
		serving, err := scanReleaseRow(q.QueryRow(ctx, releaseSelect+" WHERE r.id=$1", target.ServingRelease))
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return unavailable()
		}
		if err == nil && serving.ImageID == release.ImageID {
			return contract.NewFault(contract.Conflict,
				"production already serves the exact source artifact "+release.ImageID,
				"Use a new source release for a new promotion; apply a revision change with restart.")
		}
	}
	var activeReleaseID string
	err := q.QueryRow(ctx, `SELECT d.release_id
        FROM eregion_platform_deployments d
        JOIN eregion_operations o ON o.id=d.operation_id
        WHERE d.environment_id=$1
          AND d.state IN ('pending','running')
          AND o.state IN ('queued','running','cancelling')
        ORDER BY d.created_at, d.id LIMIT 1`, target.ID).Scan(&activeReleaseID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return unavailable()
	}
	return contract.NewFault(contract.Conflict,
		"production has an active deployment ("+activeReleaseID+"); promotion cannot be admitted until it finishes",
		"Follow or wait for the active operation, then retry the promotion; a later promotion never rewrites that deployment's previous-release cleanup history.")
}

func promotionSteps() []string {
	return []string{
		"read the source environment's serving release",
		"record its exact image digest as a new production release without building",
		"preflight the exact image on the production node",
		"run production's release command once for the new production release",
		"candidate production containers per process replica on dynamic loopback ports",
		"candidate web health",
		"Caddy upstream switch for production's web process",
		"public route verification on production's primary domain",
		"remove the previous production release containers",
	}
}
