package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

const deleteWorkloadStep = "platform-delete-01-v1"

// lockPlatformTargets serializes a deletion with every operation that could
// touch the same records. An app deletion takes the app target (which also
// serializes environment creation) and every current environment target, so a
// deploy cannot race the cleanup. Lock order is sorted to avoid deadlocks.
func (s *Service) lockPlatformTargets(ctx context.Context, normalized platform.Normalized) (func(), error) {
	targets := []string{normalized.Target.String()}
	switch input := normalized.Input.(type) {
	case *platform.AppDeleteInput:
		envTargets, err := s.environmentTargets(ctx, input.App)
		if err != nil {
			return nil, err
		}
		targets = append(targets, envTargets...)
	case *platform.EnvironmentDeleteInput:
		targets = append(targets, "app/"+input.App)
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

func (s *Service) lockTargets(ctx context.Context, targets []string) (func(), error) {
	sorted := append([]string(nil), targets...)
	sort.Strings(sorted)
	unique := sorted[:0]
	for _, target := range sorted {
		if len(unique) == 0 || unique[len(unique)-1] != target {
			unique = append(unique, target)
		}
	}
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return nil, unavailable()
	}
	keys := make([]int64, 0, len(unique))
	for _, target := range unique {
		sum := sha256.Sum256([]byte(target))
		key := int64(binary.BigEndian.Uint64(sum[:8]))
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
	return nil
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
	tr, err := w.service.resolver.TransportFor(ctx, env.Node)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	if _, err := w.runStepOn(ctx, tr, id, attempt, deleteWorkloadStep, platform.DeletePayload{
		Step: deleteWorkloadStep, App: env.App, Environment: env.Name,
	}, nil); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	if err := w.service.writeRoutesExcluding(ctx, map[platform.EnvironmentID]bool{env.ID: true}); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	return w.service.finishPlatformOperation(ctx, id, attempt, func(tx pgx.Tx, op *contract.Operation) error {
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
	})
}

func (w *platformWorker) deleteApplication(ctx context.Context, id contract.OperationID, attempt uint32, appName string) error {
	app, environments, err := w.service.applicationEnvironments(ctx, appName)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	byNode := map[string]bool{}
	excluded := map[platform.EnvironmentID]bool{}
	for _, env := range environments {
		byNode[env.Node] = true
		excluded[env.ID] = true
	}
	nodes := make([]string, 0, len(byNode))
	for name := range byNode {
		nodes = append(nodes, name)
	}
	sort.Strings(nodes)
	for index, nodeName := range nodes {
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
	if err := w.service.writeRoutesExcluding(ctx, excluded); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	return w.service.finishPlatformOperation(ctx, id, attempt, func(tx pgx.Tx, op *contract.Operation) error {
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
	})
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
	return deleteApplicationRowsTx(ctx, tx, platform.App{Name: app})
}

func deleteApplicationRowsTx(ctx context.Context, tx pgx.Tx, app platform.App) error {
	// Changes and any remaining environments cascade with the app row. Secret
	// namespaces are not foreign keys to platform apps and are removed here.
	if _, err := tx.Exec(ctx, `DELETE FROM eregion_platform_apps WHERE name=$1`, app.Name); err != nil {
		return unavailable()
	}
	if _, err := tx.Exec(ctx, `DELETE FROM eregion_secrets WHERE app=$1`, app.Name); err != nil {
		return unavailable()
	}
	return nil
}

// writeRoutesExcluding rewrites the Caddy routes file without the named
// environments, leaving every other app's upstreams untouched, then loads it.
// It is used before deletion rows are removed so a failed reload leaves the
// records available for a retry.
func (s *Service) writeRoutesExcluding(ctx context.Context, excluded map[platform.EnvironmentID]bool) error {
	rows, err := s.pool.Query(ctx, `SELECT d.name,e.id,coalesce(dep.instances,'[]'::jsonb)
        FROM eregion_platform_domains d
        JOIN eregion_platform_environments e ON e.id=d.environment_id
        LEFT JOIN eregion_platform_deployments dep ON dep.id=e.serving_deployment_id
        ORDER BY d.name`)
	if err != nil {
		return unavailable()
	}
	routes := []platform.Route{}
	for rows.Next() {
		var route platform.Route
		var environmentID platform.EnvironmentID
		var rawInstances []byte
		if err := rows.Scan(&route.Domain, &environmentID, &rawInstances); err != nil {
			rows.Close()
			return unavailable()
		}
		if excluded[environmentID] {
			continue
		}
		if len(rawInstances) > 0 {
			var instances []platform.DeploymentInstance
			if err := json.Unmarshal(rawInstances, &instances); err != nil {
				rows.Close()
				return unavailable()
			}
			route.Upstreams = platform.WebUpstreams(instances)
		}
		routes = append(routes, route)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return unavailable()
	}
	previous, err := readCaddyRoutes()
	if err != nil {
		return err
	}
	if err := writeCaddyRoutes(platform.RenderRoutes(routes)); err != nil {
		return err
	}
	if err := loadCaddyfile(ctx); err != nil {
		_ = writeCaddyRoutes(previous)
		_ = loadCaddyfile(context.WithoutCancel(ctx))
		return err
	}
	return nil
}

// deletionPreview builds the dry-run plan without deleting anything.
func (s *Service) deletionPreview(ctx context.Context, normalized platform.Normalized) (platform.Preview, error) {
	input := normalized.Input
	preview := platform.Preview{Kind: normalized.Kind, Target: normalized.Target.String(), DryRun: true, Input: input.Preview(), Summary: normalized.Summary}
	var app, environment string
	switch value := input.(type) {
	case *platform.AppDeleteInput:
		app = value.App
		if err := s.ensureApplication(ctx, app); err != nil {
			return preview, err
		}
	case *platform.EnvironmentDeleteInput:
		app, environment = value.App, value.Environment
		if _, err := s.environment(ctx, app, environment); err != nil {
			return preview, err
		}
	default:
		return preview, contract.NewFault(contract.Internal, "deletion preview input is not recognized", "Do not retry accepted work.")
	}
	counts, err := s.deletionCounts(ctx, app, environment)
	if err != nil {
		return preview, err
	}
	preview.Steps = platform.DeletionSteps(app, environment)
	preview.Summary = fmt.Sprintf("Delete %s: %d environment(s), %d release(s), %d deployment(s), %d build(s), %d domain(s), %d secret namespace(s).",
		normalized.Target.String(), counts.environments, counts.releases, counts.deployments, counts.builds, counts.domains, counts.secrets)
	preview.Warnings = append(preview.Warnings,
		"Containers selected by ownership labels and their Caddy routes are removed before any database row.",
		"Durable operation and event history is kept; only platform state and secret values are deleted.",
	)
	if environment == "" {
		preview.Warnings = append(preview.Warnings, "Every environment of the application is deleted.")
	} else if counts.environments == 1 {
		preview.Warnings = append(preview.Warnings, "This is the app's last environment; deleting it also deletes the application record.")
	}
	return preview, nil
}

type deletionCounts struct {
	environments int
	releases     int
	deployments  int
	builds       int
	domains      int
	secrets      int
}

func (s *Service) deletionCounts(ctx context.Context, app, environment string) (deletionCounts, error) {
	var counts deletionCounts
	statement := `SELECT
        (SELECT count(*) FROM eregion_platform_environments e JOIN eregion_platform_apps a ON a.id=e.app_id WHERE a.name=$1 AND ($2='' OR e.name=$2)),
        (SELECT count(*) FROM eregion_platform_releases r JOIN eregion_platform_apps a ON a.id=r.app_id JOIN eregion_platform_environments e ON e.id=r.environment_id WHERE a.name=$1 AND ($2='' OR e.name=$2)),
        (SELECT count(*) FROM eregion_platform_deployments d JOIN eregion_platform_apps a ON a.id=d.app_id JOIN eregion_platform_environments e ON e.id=d.environment_id WHERE a.name=$1 AND ($2='' OR e.name=$2)),
        (SELECT count(*) FROM eregion_platform_builds b JOIN eregion_platform_apps a ON a.id=b.app_id JOIN eregion_platform_environments e ON e.id=b.environment_id WHERE a.name=$1 AND ($2='' OR e.name=$2)),
        (SELECT count(*) FROM eregion_platform_domains d JOIN eregion_platform_environments e ON e.id=d.environment_id JOIN eregion_platform_apps a ON a.id=e.app_id WHERE a.name=$1 AND ($2='' OR e.name=$2)),
        (SELECT count(*) FROM eregion_secrets s WHERE s.app=$1 AND ($2='' OR s.environment=$2))`
	if err := s.pool.QueryRow(ctx, statement, app, environment).Scan(
		&counts.environments, &counts.releases, &counts.deployments, &counts.builds, &counts.domains, &counts.secrets,
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
	if err := s.pool.QueryRow(ctx, `SELECT id,name,created_at,updated_at FROM eregion_platform_apps WHERE name=$1`, app).
		Scan(&record.ID, &record.Name, &record.CreatedAt, &record.UpdatedAt); err != nil {
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

// readCaddyRoutes and writeCaddyRoutes are the small file boundary used by the
// exclusion rewrite; routes.go keeps the normal reconcile path.
func readCaddyRoutes() ([]byte, error) {
	return os.ReadFile(platform.CaddyRoutesPath)
}

func writeCaddyRoutes(raw []byte) error {
	return os.WriteFile(platform.CaddyRoutesPath, raw, 0o644) //nolint:gosec // G306: Caddy reads it.
}
