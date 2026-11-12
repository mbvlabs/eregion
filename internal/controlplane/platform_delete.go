package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

// lockPlatformTargets serializes a deletion or managed-resource operation with
// every operation that could touch the same records. An app deletion takes the
// app target (which also serializes environment creation) and every current
// environment target, so a deploy cannot race the cleanup. A resource
// operation also takes its environment target so it cannot race a deployment
// switch. Lock order is sorted to avoid deadlocks.
func (s *Service) lockPlatformTargets(ctx context.Context, normalized platform.Normalized) (func(), error) {
	targets := []string{normalized.Target.String()}
	switch input := normalized.Input.(type) {
	case *platform.AppDeleteInput:
		envTargets, err := s.environmentTargets(ctx, input.App)
		if err != nil {
			return nil, err
		}
		targets = append(targets, envTargets...)
		resourceTargets, err := s.resourceTargets(ctx, input.App, "")
		if err != nil {
			return nil, err
		}
		targets = append(targets, resourceTargets...)
	case *platform.EnvironmentDeleteInput:
		targets = append(targets, "app/"+input.App)
		resourceTargets, err := s.resourceTargets(ctx, input.App, input.Environment)
		if err != nil {
			return nil, err
		}
		targets = append(targets, resourceTargets...)
	case *platform.ResourceApplyInput:
		// A resource container is consumed by the environment's deployments.
		// Taking the environment target serializes provisioning with a deploy
		// switch, in addition to the resource's own target.
		targets = append(targets, "env/"+input.App+"/"+input.Environment)
	case *platform.ResourceDestroyInput:
		// The same environment lock keeps destroy out of a deployment switch;
		// the resource target keeps it out of another resource operation.
		targets = append(targets, "env/"+input.App+"/"+input.Environment)
	}
	return s.lockTargets(ctx, targets)
}

func (s *Service) environmentTargets(ctx context.Context, app string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT 'env/'||a.name||'/'||e.name
        FROM eregion_platform_environments e JOIN eregion_platform_apps a ON a.id=e.app_id
        WHERE a.name=$1 ORDER BY e.name`, app)
	if err != nil {
		return nil, unavailable()
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var target string
		if err := rows.Scan(&target); err != nil {
			return nil, unavailable()
		}
		out = append(out, target)
	}
	return out, rows.Err()
}

// environmentWorkloadNodes lists every node that may still hold this
// environment's containers: its current node and the runtime node recorded on
// each deployment's revision. A deployment's instances do not carry a node
// name, so the pinned revision is the durable record of where they ran. This
// reaches containers left behind after `env update --node`.
func (s *Service) environmentWorkloadNodes(ctx context.Context, envID platform.EnvironmentID, current string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `SELECT DISTINCT coalesce(r.runtime->>'node','')
        FROM eregion_platform_deployments d
        LEFT JOIN eregion_platform_environment_revisions r ON r.id=d.revision_id
        WHERE d.environment_id=$1`, envID)
	if err != nil {
		return nil, unavailable()
	}
	defer rows.Close()
	nodes := []string{}
	seen := map[string]bool{}
	add := func(name string) {
		if name != "" && !seen[name] {
			seen[name] = true
			nodes = append(nodes, name)
		}
	}
	add(current)
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, unavailable()
		}
		add(name)
	}
	if err := rows.Err(); err != nil {
		return nil, unavailable()
	}
	sort.Strings(nodes)
	return nodes, nil
}

// targetLockKey is the stable PostgreSQL advisory-lock key for one normalized
// target. Session locks in workers and transaction locks in admission use the
// same key, so scale submission waits for an in-flight deployment instead of
// inspecting an ownership row across a lock gap.
func targetLockKey(target string) int64 {
	sum := sha256.Sum256([]byte(target))
	return int64(binary.BigEndian.Uint64(sum[:8]))
}

func uniqueTargets(targets []string) []string {
	sorted := append([]string(nil), targets...)
	sort.Strings(sorted)
	unique := sorted[:0]
	for _, target := range sorted {
		if len(unique) == 0 || unique[len(unique)-1] != target {
			unique = append(unique, target)
		}
	}
	return unique
}

// tryLockTargets is the nonblocking worker form. A contended deployment target
// returns acquired=false immediately, so a queued job can snooze without
// holding a pool connection or consuming a River failure attempt.
func (s *Service) tryLockTargets(ctx context.Context, targets []string) (func(), bool, error) {
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, false, unavailable()
	}
	keys := make([]int64, 0, len(targets))
	release := func() {
		for index := len(keys) - 1; index >= 0; index-- {
			_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, keys[index])
		}
		conn.Release()
	}
	for _, target := range uniqueTargets(targets) {
		key := targetLockKey(target)
		var acquired bool
		if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, key).Scan(&acquired); err != nil {
			release()
			if ctx.Err() != nil {
				return nil, false, ctx.Err()
			}
			return nil, false, unavailable()
		}
		if !acquired {
			release()
			return nil, false, nil
		}
		keys = append(keys, key)
	}
	return release, true, nil
}

func (s *Service) lockTargets(ctx context.Context, targets []string) (func(), error) {
	unique := uniqueTargets(targets)
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, unavailable()
	}
	keys := make([]int64, 0, len(unique))
	for _, target := range unique {
		key := targetLockKey(target)
		if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, key); err != nil {
			for index := len(keys) - 1; index >= 0; index-- {
				_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, keys[index])
			}
			conn.Release()
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, unavailable()
		}
		keys = append(keys, key)
	}
	return func() {
		for index := len(keys) - 1; index >= 0; index-- {
			_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, keys[index])
		}
		conn.Release()
	}, nil
}

// ensureNoActiveDeleteTargets refuses a deletion while another operation for
// the same records is queued, running or cancelling. This is a state check, not
// a queue-row assumption; the target locks then keep new operations out.
func (s *Service) ensureNoActiveDeleteTargets(ctx context.Context, tx pgx.Tx, id contract.OperationID, app, environment string) error {
	statement := `SELECT count(*) FROM eregion_operations
        WHERE id<>$1 AND state IN ('queued','running','cancelling') AND (target=$2`
	args := []any{id, "app/" + app}
	if environment == "" {
		args = append(args, "env/"+app+"/%")
		statement += fmt.Sprintf(" OR target LIKE $%d", len(args))
	} else {
		args = append(args, "env/"+app+"/"+environment)
		statement += fmt.Sprintf(" OR target=$%d", len(args))
		// Two sibling environment deletions accepted before either worker ran
		// could otherwise each look non-last and together remove the app
		// without the app-deletion factor. Serialize all environment deletes
		// for one app; after the first finishes, the next submission re-reads
		// the true remaining count and requires the factor when it is last.
		args = append(args, platform.KindEnvironmentDelete, "env/"+app+"/%")
		statement += fmt.Sprintf(" OR (kind=$%d AND target LIKE $%d)", len(args)-1, len(args))
	}
	statement += ")"
	var active int
	if err := tx.QueryRow(ctx, statement, args...).Scan(&active); err != nil {
		return unavailable()
	}
	if active > 0 {
		return contract.NewFault(contract.Conflict, "another operation is still active for this target",
			"Wait for it to finish or cancel it explicitly, then delete again.")
	}
	ownerStatement := `SELECT count(*) FROM eregion_platform_deployment_owners o
        JOIN eregion_platform_environments e ON e.id=o.environment_id
        JOIN eregion_platform_apps a ON a.id=e.app_id
        WHERE a.name=$1`
	ownerArgs := []any{app}
	if environment != "" {
		ownerStatement += " AND e.name=$2"
		ownerArgs = append(ownerArgs, environment)
	}
	var owners int
	if err := tx.QueryRow(ctx, ownerStatement, ownerArgs...).Scan(&owners); err != nil {
		return unavailable()
	}
	if owners > 0 {
		return contract.NewFault(contract.Conflict, "deployment ownership is still settling",
			"Wait for reconciliation to restore the terminal traffic target, then submit the deletion again.")
	}
	return nil
}

// RequiresHumanFactor reports whether a real submission of this normalized
// action currently needs the D7 factor. Authorization still happens inside the
// submission transaction against locked state; this read is for transports
// that must refuse before they can carry a code (the HTTP API).
func (s *Service) RequiresHumanFactor(ctx context.Context, normalized platform.Normalized) (bool, error) {
	switch input := normalized.Input.(type) {
	case *platform.AppDeleteInput:
		return true, nil
	case *platform.EnvironmentDeleteInput:
		env, err := s.environment(ctx, input.App, input.Environment)
		if err != nil {
			return false, err
		}
		counts, err := s.deletionCounts(ctx, input.App, input.Environment)
		if err != nil {
			return false, err
		}
		return platform.EnvironmentDeleteRequiresHumanFactor(env.Protected, counts.appEnvironments == 1), nil
	case *platform.EnvironmentProtectInput:
		env, err := s.environment(ctx, input.App, input.Environment)
		if err != nil {
			return false, err
		}
		return platform.ProtectionChangeRequiresHumanFactor(env.Protected, input.Protected), nil
	default:
		return false, nil
	}
}

func (w *platformWorker) runDelete(ctx context.Context, id contract.OperationID, attempt uint32, normalized platform.Normalized) error {
	switch input := normalized.Input.(type) {
	case *platform.AppDeleteInput:
		return w.deleteApplication(ctx, id, attempt, input.App)
	case *platform.EnvironmentDeleteInput:
		return w.deleteEnvironment(ctx, id, attempt, input.App, input.Environment)
	default:
		return w.settle(ctx, id, attempt, contract.NewFault(contract.Internal, "deletion input is not recognized", "Do not retry accepted work."))
	}
}

func (w *platformWorker) deleteEnvironment(ctx context.Context, id contract.OperationID, attempt uint32, app, environment string) error {
	env, err := w.service.environment(ctx, app, environment)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	if _, err := w.service.verifyLocalRegistryHost(ctx); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	// Provider cleanup must succeed before any serving workload is removed. A
	// failed Cloudflare read/delete then leaves the retained app alive and
	// retryable instead of taking it offline while its route remains.
	op, _, err := w.service.operationSnapshot(ctx, id)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	if err := w.service.cleanupDomainDNSForEnvironments(ctx, op, attempt, []platform.EnvironmentID{env.ID}); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	if err := w.service.checkDomainDNSBoundary(ctx, id, attempt); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	nodes, err := w.service.environmentWorkloadNodes(ctx, env.ID, env.Node)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	for index, nodeName := range nodes {
		if err := w.service.checkDomainDNSBoundary(ctx, id, attempt); err != nil {
			return w.settle(ctx, id, attempt, err)
		}
		tr, err := w.service.resolver.TransportFor(ctx, nodeName)
		if err != nil {
			return w.settle(ctx, id, attempt, err)
		}
		step := fmt.Sprintf("platform-delete-%02d-v1", index+1)
		if _, err := w.runStepOn(ctx, tr, id, attempt, step, platform.DeletePayload{
			Step: step, App: env.App, Environment: env.Name,
		}, nil); err != nil {
			return w.settle(ctx, id, attempt, err)
		}
	}
	if err := w.service.checkDomainDNSBoundary(ctx, id, attempt); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	if err := w.service.deleteDomainRowsOwned(ctx, id, attempt, []platform.EnvironmentID{env.ID}); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	if err := w.service.syncRoutes(ctx); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	hostRelease, err := w.service.acquireRegistryHostLock(ctx)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	defer hostRelease()
	if err := w.service.finishDomainOperation(ctx, id, attempt, func(tx pgx.Tx, op *contract.Operation) error {
		if err := deleteEnvironmentRowsTx(ctx, tx, env); err != nil {
			return err
		}
		if err := deleteEmptyApplicationTx(ctx, tx, env.App); err != nil {
			return err
		}
		return appendEvent(ctx, tx, op, contract.Event{
			Kind: "log", Stage: "delete",
			Message: "Environment " + env.App + "/" + env.Name + " deleted after workload and route cleanup; operation history was kept.",
		})
	}); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	return nil
}

func (w *platformWorker) deleteApplication(ctx context.Context, id contract.OperationID, attempt uint32, appName string) error {
	app, environments, err := w.service.applicationEnvironments(ctx, appName)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	if _, err := w.service.verifyLocalRegistryHost(ctx); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	environmentIDs := make([]platform.EnvironmentID, 0, len(environments))
	for _, env := range environments {
		environmentIDs = append(environmentIDs, env.ID)
	}
	// Clean every managed binding before removing any serving workload. A
	// provider failure cannot therefore orphan a retained app on dead routes.
	op, _, err := w.service.operationSnapshot(ctx, id)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	if err := w.service.cleanupDomainDNSForEnvironments(ctx, op, attempt, environmentIDs); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	if err := w.service.checkDomainDNSBoundary(ctx, id, attempt); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	byNode := map[string]bool{}
	for _, env := range environments {
		nodes, err := w.service.environmentWorkloadNodes(ctx, env.ID, env.Node)
		if err != nil {
			return w.settle(ctx, id, attempt, err)
		}
		for _, nodeName := range nodes {
			byNode[nodeName] = true
		}
	}
	nodes := make([]string, 0, len(byNode))
	for name := range byNode {
		nodes = append(nodes, name)
	}
	sort.Strings(nodes)
	for index, nodeName := range nodes {
		if err := w.service.checkDomainDNSBoundary(ctx, id, attempt); err != nil {
			return w.settle(ctx, id, attempt, err)
		}
		tr, err := w.service.resolver.TransportFor(ctx, nodeName)
		if err != nil {
			return w.settle(ctx, id, attempt, err)
		}
		step := fmt.Sprintf("platform-delete-%02d-v1", index+1)
		if _, err := w.runStepOn(ctx, tr, id, attempt, step, platform.DeletePayload{
			Step: step, App: app.Name,
		}, nil); err != nil {
			return w.settle(ctx, id, attempt, err)
		}
	}
	if err := w.service.checkDomainDNSBoundary(ctx, id, attempt); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	if err := w.service.deleteDomainRowsOwned(ctx, id, attempt, environmentIDs); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	if err := w.service.syncRoutes(ctx); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	hostRelease, err := w.service.acquireRegistryHostLock(ctx)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	defer hostRelease()
	if err := w.service.finishDomainOperation(ctx, id, attempt, func(tx pgx.Tx, op *contract.Operation) error {
		for _, env := range environments {
			if err := deleteEnvironmentRowsTx(ctx, tx, env); err != nil {
				return err
			}
		}
		if err := deleteApplicationRowsTx(ctx, tx, app); err != nil {
			return err
		}
		return appendEvent(ctx, tx, op, contract.Event{
			Kind: "log", Stage: "delete",
			Message: "Application " + app.Name + " and every environment were deleted after workload and route cleanup; operation history was kept.",
		})
	}); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	return nil
}

func deleteEnvironmentRowsTx(ctx context.Context, tx pgx.Tx, env platform.Environment) error {
	if _, err := tx.Exec(ctx, `UPDATE eregion_platform_environments
        SET desired_release_id=NULL,applied_release_id=NULL,serving_release_id=NULL,
            serving_deployment_id=NULL,current_revision_id=NULL,updated_at=clock_timestamp()
        WHERE id=$1`, env.ID); err != nil {
		return unavailable()
	}
	for _, statement := range []string{
		`DELETE FROM eregion_platform_deployments WHERE environment_id=$1`,
		`DELETE FROM eregion_platform_releases WHERE environment_id=$1`,
		`DELETE FROM eregion_platform_builds WHERE environment_id=$1`,
		`DELETE FROM eregion_platform_environment_revisions WHERE environment_id=$1`,
		`DELETE FROM eregion_platform_domains WHERE environment_id=$1`,
		`DELETE FROM eregion_platform_changes WHERE environment_id=$1`,
	} {
		if _, err := tx.Exec(ctx, statement, env.ID); err != nil {
			return unavailable()
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM eregion_secrets WHERE app=$1 AND environment=$2`, env.App, env.Name); err != nil {
		return unavailable()
	}
	if _, err := tx.Exec(ctx, `DELETE FROM eregion_platform_environments WHERE id=$1`, env.ID); err != nil {
		return unavailable()
	}
	return nil
}

func deleteEmptyApplicationTx(ctx context.Context, tx pgx.Tx, app string) error {
	var remaining int
	if err := tx.QueryRow(ctx, `SELECT count(*) FROM eregion_platform_environments e
        JOIN eregion_platform_apps a ON a.id=e.app_id WHERE a.name=$1`, app).Scan(&remaining); err != nil {
		return unavailable()
	}
	if remaining != 0 {
		return nil
	}
	var appID platform.AppID
	err := tx.QueryRow(ctx, `SELECT id FROM eregion_platform_apps WHERE name=$1`, app).Scan(&appID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return unavailable()
	}
	return deleteApplicationRowsTx(ctx, tx, platform.App{ID: appID, Name: app})
}

func deleteApplicationRowsTx(ctx context.Context, tx pgx.Tx, app platform.App) error {
	if app.ID == "" {
		return contract.NewFault(contract.Internal, "application identity is missing for deletion", "Reload the application and retry; deletion is bound to the durable app ID, not its name alone.")
	}
	// Changes and any remaining environments cascade with the app row. Secret
	// namespaces are not foreign keys to platform apps and are removed here.
	tag, err := tx.Exec(ctx, `DELETE FROM eregion_platform_apps WHERE id=$1`, app.ID)
	if err != nil {
		return unavailable()
	}
	if tag.RowsAffected() == 0 {
		// The identity-bound row is already gone; never erase another app's
		// same-named secret namespace.
		return nil
	}
	if _, err := tx.Exec(ctx, `DELETE FROM eregion_secrets WHERE app=$1`, app.Name); err != nil {
		return unavailable()
	}
	return nil
}

// deleteDomainRows removes the row-6 domain state for the environments being
// deleted. The caller then uses the one serialized route writer (syncRoutes),
// so deletion does not introduce a second Caddy write path.
func (s *Service) deleteDomainRows(ctx context.Context, environmentIDs []platform.EnvironmentID) error {
	if len(environmentIDs) == 0 {
		return nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return unavailable()
	}
	defer rollback(tx)
	if err := deleteDomainRowsTx(ctx, tx, environmentIDs); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return unavailable()
	}
	return nil
}

// deleteDomainRowsOwned performs the same deletion under the operation row
// lock, so a cancellation accepted before the boundary retains the rows.
func (s *Service) deleteDomainRowsOwned(ctx context.Context, id contract.OperationID, attempt uint32, environmentIDs []platform.EnvironmentID) error {
	if len(environmentIDs) == 0 {
		return nil
	}
	_, owned, err := s.mutateOwned(ctx, id, attempt, func(tx pgx.Tx, op *contract.Operation) error {
		if op.State == contract.Cancelling {
			return errPlatformCancelled
		}
		return deleteDomainRowsTx(ctx, tx, environmentIDs)
	})
	if err != nil {
		return err
	}
	if !owned {
		return errNotOwned
	}
	return nil
}

func deleteDomainRowsTx(ctx context.Context, tx pgx.Tx, environmentIDs []platform.EnvironmentID) error {
	ids := make([]string, len(environmentIDs))
	for index, id := range environmentIDs {
		ids[index] = string(id)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM eregion_platform_domain_dns x
        USING eregion_platform_domains d
        WHERE x.domain_id=d.id AND d.environment_id = ANY($1)
          AND x.state IN ('cleanup_pending','cleanup_verified','cleanup_failed')`, ids); err != nil {
		return unavailable()
	}
	if _, err := tx.Exec(ctx, `DELETE FROM eregion_platform_domains WHERE environment_id = ANY($1)`, ids); err != nil {
		return unavailable()
	}
	return nil
}

// deletionPreview builds the dry-run plan without deleting anything.
func (s *Service) deletionPreview(ctx context.Context, normalized platform.Normalized) (platform.Preview, error) {
	input := normalized.Input
	preview := platform.Preview{Kind: normalized.Kind, Target: normalized.Target.String(), DryRun: true, Input: input.Preview(), Summary: normalized.Summary}
	var app, environment string
	protected, lastEnvironment := false, false
	switch value := input.(type) {
	case *platform.AppDeleteInput:
		app = value.App
		if err := s.ensureApplication(ctx, app); err != nil {
			return preview, err
		}
	case *platform.EnvironmentDeleteInput:
		app, environment = value.App, value.Environment
		env, err := s.environment(ctx, app, environment)
		if err != nil {
			return preview, err
		}
		protected = env.Protected
		counts, err := s.deletionCounts(ctx, app, environment)
		if err != nil {
			return preview, err
		}
		lastEnvironment = counts.appEnvironments == 1
	default:
		return preview, contract.NewFault(contract.Internal, "deletion preview input is not recognized", "Do not retry accepted work.")
	}
	counts, err := s.deletionCounts(ctx, app, environment)
	if err != nil {
		return preview, err
	}
	preview.Steps = platform.DeletionSteps(app, environment)
	preview.Summary = deletionSummary(normalized.Target.String(), counts)
	preview.Warnings = append(preview.Warnings,
		"Containers selected by ownership labels and their Caddy routes are removed before any database row.",
		"Durable operation and event history is kept; only platform state and secret values are deleted.",
	)
	preview.Warnings = append(preview.Warnings, deletionWarnings(counts, environment)...)
	switch {
	case environment == "":
		preview.Warnings = append(preview.Warnings, "Every application deletion requires the D7 TOTP human factor, regardless of environment protection.")
	case platform.EnvironmentDeleteRequiresHumanFactor(protected, lastEnvironment):
		reason := "This environment is protected"
		if lastEnvironment && !protected {
			reason = "This is the last environment, so deleting it also deletes the application"
		}
		preview.Warnings = append(preview.Warnings, reason+"; the real deletion requires the D7 TOTP human factor at a human terminal.")
	default:
		preview.Warnings = append(preview.Warnings, "This environment is unprotected and is not the last one; the real deletion needs only the exact target confirmation and is available to agents, JSON and no-TTY calls.")
	}
	return preview, nil
}

// deletionSummary names the rows the target deletion removes. Every count is
// scoped to the target environment when one is named; appEnvironments is only
// used by the last-environment warning.
func deletionSummary(target string, counts deletionCounts) string {
	return fmt.Sprintf("Delete %s: %d environment(s), %d release(s), %d deployment(s), %d build(s), %d domain(s), %d secret namespace(s).",
		target, counts.environments, counts.releases, counts.deployments, counts.builds, counts.domains, counts.secrets)
}

// deletionWarnings states the scope of the deletion. counts.environments is
// target-scoped, so the last-environment warning must use the application-wide
// count instead; otherwise every named-environment delete would claim to be
// the last one.
func deletionWarnings(counts deletionCounts, environment string) []string {
	if environment == "" {
		return []string{"Every environment of the application is deleted."}
	}
	if counts.appEnvironments == 1 {
		return []string{"This is the app's last environment; deleting it also deletes the application record."}
	}
	return nil
}

type deletionCounts struct {
	environments    int
	appEnvironments int
	releases        int
	deployments     int
	builds          int
	domains         int
	secrets         int
}

func (s *Service) deletionCounts(ctx context.Context, app, environment string) (deletionCounts, error) {
	var counts deletionCounts
	statement := `SELECT
        (SELECT count(*) FROM eregion_platform_environments e JOIN eregion_platform_apps a ON a.id=e.app_id WHERE a.name=$1 AND ($2='' OR e.name=$2)),
        (SELECT count(*) FROM eregion_platform_releases r JOIN eregion_platform_apps a ON a.id=r.app_id JOIN eregion_platform_environments e ON e.id=r.environment_id WHERE a.name=$1 AND ($2='' OR e.name=$2)),
        (SELECT count(*) FROM eregion_platform_deployments d JOIN eregion_platform_apps a ON a.id=d.app_id JOIN eregion_platform_environments e ON e.id=d.environment_id WHERE a.name=$1 AND ($2='' OR e.name=$2)),
        (SELECT count(*) FROM eregion_platform_builds b JOIN eregion_platform_apps a ON a.id=b.app_id JOIN eregion_platform_environments e ON e.id=b.environment_id WHERE a.name=$1 AND ($2='' OR e.name=$2)),
        (SELECT count(*) FROM eregion_platform_domains d JOIN eregion_platform_environments e ON e.id=d.environment_id JOIN eregion_platform_apps a ON a.id=e.app_id WHERE a.name=$1 AND ($2='' OR e.name=$2)),
        (SELECT count(*) FROM eregion_secrets s WHERE s.app=$1 AND ($2='' OR s.environment=$2)),
        (SELECT count(*) FROM eregion_platform_environments e JOIN eregion_platform_apps a ON a.id=e.app_id WHERE a.name=$1)`
	if err := s.pool.QueryRow(ctx, statement, app, environment).Scan(
		&counts.environments, &counts.releases, &counts.deployments, &counts.builds, &counts.domains, &counts.secrets, &counts.appEnvironments,
	); err != nil {
		return counts, unavailable()
	}
	return counts, nil
}

func (s *Service) ensureApplication(ctx context.Context, app string) error {
	var id string
	err := s.pool.QueryRow(ctx, `SELECT id FROM eregion_platform_apps WHERE name=$1`, app).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return contract.NewFault(contract.NotFound, "application not found", "List applications with eregion app list.")
	}
	if err != nil {
		return unavailable()
	}
	return nil
}

func (s *Service) applicationEnvironments(ctx context.Context, app string) (platform.App, []platform.Environment, error) {
	var record platform.App
	if err := s.pool.QueryRow(ctx, `SELECT id,name,image_repository,created_at,updated_at FROM eregion_platform_apps WHERE name=$1`, app).
		Scan(&record.ID, &record.Name, &record.ImageRepository, &record.CreatedAt, &record.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return record, nil, contract.NewFault(contract.NotFound, "application not found", "List applications with eregion app list.")
		}
		return record, nil, unavailable()
	}
	rows, err := s.pool.Query(ctx, environmentSelect+" WHERE a.name=$1 ORDER BY e.name", app)
	if err != nil {
		return record, nil, unavailable()
	}
	defer rows.Close()
	environments := []platform.Environment{}
	for rows.Next() {
		env, err := scanEnvironmentRow(rows)
		if err != nil {
			return record, nil, unavailable()
		}
		environments = append(environments, env)
	}
	if err := rows.Err(); err != nil {
		return record, nil, unavailable()
	}
	return record, environments, nil
}
