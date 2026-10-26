package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
)

// portAllocationLock serializes host/candidate port allocation across control
// plane processes. It is transaction-scoped and separate from River locks.
const portAllocationLock int64 = 0x657265706f727473 // "ereports"

const platformColumns = `id, kind, target, actor_kind, actor_subject, state, attempt, last_sequence, error_code, created_at, updated_at`

func (s *Service) submitPlatform(ctx context.Context, sub operations.Submission) (contract.Operation, error) {
	var zero contract.Operation
	if sub.Confirmation != "" {
		return zero, contract.NewFault(contract.ValidationFailed, "platform actions do not accept confirmation text", "Omit confirmation; destructive actions are guarded by their inputs and operation state.")
	}
	normalized, err := platform.Default().Normalize(sub.Kind, sub.Target, sub.Input)
	if err != nil {
		return zero, err
	}
	canonical := normalized.Canonical
	inputHash := hashInput(canonical)
	id, err := contract.NewOperationID()
	if err != nil {
		return zero, err
	}
	if err := s.ensureLocalNode(ctx); err != nil {
		return zero, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return zero, unavailable()
	}
	defer rollback(tx)
	inserted, err := tx.Exec(ctx, `INSERT INTO eregion_operations
        (id,kind,target,actor_kind,actor_subject,idempotency_key,input_hash,input,state)
        VALUES ($1,$2,$3,$4,$5,$6,$7,$8,'queued')
        ON CONFLICT (actor_kind,actor_subject,kind,target,idempotency_key) DO NOTHING`,
		id, sub.Kind, normalized.Target.String(), sub.Actor.Kind, sub.Actor.Subject, sub.IdempotencyKey, inputHash, canonical)
	if err != nil {
		return zero, unavailable()
	}
	if inserted.RowsAffected() == 0 {
		var existingID contract.OperationID
		var existingHash string
		if err := tx.QueryRow(ctx, `SELECT id,input_hash FROM eregion_operations WHERE actor_kind=$1 AND actor_subject=$2 AND kind=$3 AND target=$4 AND idempotency_key=$5`,
			sub.Actor.Kind, sub.Actor.Subject, sub.Kind, normalized.Target.String(), sub.IdempotencyKey).Scan(&existingID, &existingHash); err != nil {
			return zero, unavailable()
		}
		if existingHash != inputHash {
			return zero, contract.NewFault(contract.Conflict, "idempotency key already binds different inputs", "Use the original inputs to recover the existing operation; use a new key only for deliberately new work.")
		}
		return scanOperation(tx.QueryRow(ctx, "SELECT "+columns+" FROM eregion_operations WHERE id=$1", existingID))
	}
	if err := s.preparePlatform(ctx, tx, id, normalized, sub.Actor); err != nil {
		return zero, err
	}
	if s.enqueuePlatform == nil {
		return zero, unavailable()
	}
	op := contract.Operation{ID: id, Kind: sub.Kind, Target: normalized.Target.String(), Actor: sub.Actor}
	jobID, err := s.enqueuePlatform(ctx, tx, op)
	if err != nil {
		return zero, unavailable()
	}
	if _, err := tx.Exec(ctx, "UPDATE eregion_operations SET job_id=$2 WHERE id=$1", id, jobID); err != nil {
		return zero, unavailable()
	}
	op, err = scanOperation(tx.QueryRow(ctx, "SELECT "+columns+" FROM eregion_operations WHERE id=$1", id))
	if err != nil {
		return zero, unavailable()
	}
	if err := appendEvent(ctx, tx, &op, contract.Event{Kind: "state", State: contract.Queued, Message: platformAcceptedMessage(sub.Kind)}); err != nil {
		return zero, unavailable()
	}
	if err := tx.Commit(ctx); err != nil {
		return zero, unavailable()
	}
	return op, nil
}

func hashInput(canonical json.RawMessage) string {
	// hashInput keeps the canonical digest identical to the diagnostic path.
	// It is intentionally a helper, not a second persistence format.
	return sha256Hex(canonical)
}

func platformAcceptedMessage(kind string) string {
	switch kind {
	case platform.KindAppCreate:
		return "Application record accepted durably."
	case platform.KindEnvironmentCreate:
		return "Environment record accepted durably; placement will be checked by the worker."
	case platform.KindReleaseCreate:
		return "Release accepted durably; the image is verified or built before it becomes ready."
	case platform.KindDeploy, platform.KindRollback:
		return "Deployment accepted durably; the serving release is unchanged until a candidate is healthy."
	case platform.KindNodeRegister:
		return "Node placement facts accepted durably. This does not install or verify host state."
	case platform.KindNodeCheck:
		return "Node check accepted durably."
	case platform.KindNodeEnroll:
		return "Node enrollment accepted durably; the host key is pinned, but nothing is trusted until the node proves its identity."
	case platform.KindNodeReconcile:
		return "Node reconciliation accepted durably."
	case platform.KindDomainAdd, platform.KindDomainRemove:
		return "Domain change accepted durably; the worker rewrites the Caddy routes."
	default:
		return "Platform action accepted durably."
	}
}

func (s *Service) preparePlatform(ctx context.Context, tx pgx.Tx, id contract.OperationID, normalized platform.Normalized, actor contract.Actor) error {
	switch input := normalized.Input.(type) {
	case *platform.AppCreateInput:
		appID, err := platform.NewAppID()
		if err != nil {
			return unavailable()
		}
		if _, err := tx.Exec(ctx, `INSERT INTO eregion_platform_apps(id,name) VALUES($1,$2)`, appID, input.Name); err != nil {
			return platformWriteError(err, "application name already exists")
		}
	case *platform.NodeRegisterInput:
		// Registration records declared facts. It can never overwrite a node that
		// is enrolling or enrolled: verified facts are not replaced by a claim.
		tag, err := tx.Exec(ctx, `INSERT INTO eregion_platform_nodes(name,roles,capabilities,address,status)
            VALUES($1,$2,$3,$4,'registered')
            ON CONFLICT (name) DO UPDATE SET roles=EXCLUDED.roles, capabilities=EXCLUDED.capabilities,
                address=EXCLUDED.address, updated_at=clock_timestamp()
                WHERE eregion_platform_nodes.enrollment_state='registered'`,
			input.Name, input.Roles, input.Capabilities, input.Address)
		if err != nil {
			return platformWriteError(err, "node registration conflicts")
		}
		if tag.RowsAffected() == 0 {
			return contract.NewFault(contract.Conflict, "node is enrolling or enrolled; declared facts cannot overwrite verified ones", "Use platform nodes enroll or platform nodes retry to change an enrolled node.")
		}
	case *platform.EnvironmentCreateInput:
		app, err := appByNameTx(ctx, tx, input.App)
		if err != nil {
			return err
		}
		node, err := nodeByNameTx(ctx, tx, input.Node)
		if err != nil {
			return err
		}
		// Write the name of the actual node record, so an environment created
		// with the "main" alias still references a row when this deployment
		// keeps the older "local" identity.
		input.Node = node.Name
		if err := s.checkPlacement(node, platform.RoleApplication, platform.CapabilityDocker); err != nil {
			return err
		}
		hostPort, candidatePort, err := allocatePorts(ctx, tx, input.HostPort)
		if err != nil {
			return err
		}
		envID, err := platform.NewEnvironmentID()
		if err != nil {
			return unavailable()
		}
		var buildNode any
		if input.BuildNode != "" {
			builder, err := nodeByNameTx(ctx, tx, input.BuildNode)
			if err != nil {
				return err
			}
			if err := s.checkPlacement(builder, platform.RoleBuilder, platform.CapabilityBuildpacks); err != nil {
				return err
			}
			buildNode = input.BuildNode
		}
		env, _ := json.Marshal(input.Env)
		refs, _ := json.Marshal(input.SecretRefs)
		if _, err := tx.Exec(ctx, `INSERT INTO eregion_platform_environments
            (id,app_id,name,node_name,build_node,host_port,candidate_port,container_port,bind_address,health_type,health_path,health_timeout_seconds,env,secret_refs)
            VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
			envID, app.ID, input.Name, input.Node, buildNode, hostPort, candidatePort, input.ContainerPort, input.Bind,
			input.Health.Type, input.Health.Path, input.Health.TimeoutSeconds, env, refs); err != nil {
			return platformWriteError(err, "environment name or port already exists")
		}
	case *platform.DomainInput:
		env, err := environmentByNameTx(ctx, tx, input.App, input.Environment)
		if err != nil {
			return err
		}
		if normalized.Kind == platform.KindDomainRemove {
			tag, err := tx.Exec(ctx, `DELETE FROM eregion_platform_domains WHERE name=$1 AND environment_id=$2`, input.Domain, env.ID)
			if err != nil {
				return unavailable()
			}
			if tag.RowsAffected() == 0 {
				return contract.NewFault(contract.NotFound, input.Domain+" is not routed to "+input.App+"/"+input.Environment, "List routes with eregion domain list.")
			}
			return nil
		}
		node, err := nodeByNameTx(ctx, tx, env.Node)
		if err != nil {
			return err
		}
		if node.Transport != "local" {
			return contract.NewFault(contract.PreconditionFailed, "domains route only to environments on the main node for now", "Create the environment on the main node.")
		}
		if _, err := tx.Exec(ctx, `INSERT INTO eregion_platform_domains(name,environment_id) VALUES($1,$2)`, input.Domain, env.ID); err != nil {
			return platformWriteError(err, input.Domain+" is already routed")
		}
	case *platform.ReleaseCreateInput:
		env, err := environmentByNameTx(ctx, tx, input.App, input.Environment)
		if err != nil {
			return err
		}
		if input.Source.Type != "image" {
			if err := crossNodeBuildUnsupported(env, input.Source.Type); err != nil {
				return err
			}
			node, err := nodeByNameTx(ctx, tx, env.BuildNodeName())
			if err != nil {
				return err
			}
			if err := s.checkPlacement(node, platform.RoleBuilder, platform.CapabilityBuildpacks); err != nil {
				return err
			}
		}
		if input.Source.Type == "directory" {
			if _, err := platform.ResolveBuildSource(input.Source.Path, s.buildSourceRoots()); err != nil {
				return err
			}
		}
		image := input.Source.Image
		if image == "" {
			if input.Build == nil || input.Build.Image == "" {
				image = "eregion/" + input.App + "-" + input.Environment + ":" + strings.TrimPrefix(string(id), "op_")[:12]
			} else {
				image = input.Build.Image
			}
		}
		if err := platform.ValidateImage(image); err != nil {
			return err
		}
		releaseID, err := platform.NewReleaseID()
		if err != nil {
			return unavailable()
		}
		source, _ := json.Marshal(input.Source)
		var build, command []byte
		if input.Build != nil {
			build, _ = json.Marshal(input.Build)
		}
		if input.Command != nil {
			command, _ = json.Marshal(input.Command)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO eregion_platform_releases
            (id,operation_id,app_id,environment_id,image,source,build,command,status) VALUES($1,$2,$3,$4,$5,$6,$7,$8,'pending')`,
			releaseID, id, envAppID(ctx, tx, env.App), env.ID, image, source, nullableJSON(build), nullableJSON(command)); err != nil {
			return platformWriteError(err, "release could not be recorded")
		}
	case *platform.DeployInput:
		env, err := environmentByNameTx(ctx, tx, input.App, input.Environment)
		if err != nil {
			return err
		}
		release, err := releaseByIDTx(ctx, tx, input.ReleaseID)
		if err != nil {
			return err
		}
		if release.App != env.App || release.Environment != env.Name {
			return contract.NewFault(contract.ValidationFailed, "release belongs to another environment", "Use a release created for this app and environment.")
		}
		if release.Status != platform.ReleaseReady {
			return contract.NewFault(contract.PreconditionFailed, "release is not ready", "Wait for the release operation to succeed, then deploy it.")
		}
		if err := s.checkPlacementByTx(ctx, tx, env.Node, platform.RoleApplication, platform.CapabilityDocker); err != nil {
			return err
		}
		depID, err := platform.NewDeploymentID()
		if err != nil {
			return unavailable()
		}
		if _, err := tx.Exec(ctx, `INSERT INTO eregion_platform_deployments
            (id,operation_id,app_id,environment_id,action,release_id,previous_release_id)
            VALUES($1,$2,$3,$4,'deploy',$5,$6)`,
			depID, id, envAppID(ctx, tx, env.App), env.ID, release.ID, nullableReleaseID(env.ServingRelease)); err != nil {
			return unavailable()
		}
		if _, err := tx.Exec(ctx, `UPDATE eregion_platform_environments SET desired_release_id=$2, updated_at=clock_timestamp() WHERE id=$1`, env.ID, release.ID); err != nil {
			return unavailable()
		}
	case *platform.RollbackInput:
		env, err := environmentByNameTx(ctx, tx, input.App, input.Environment)
		if err != nil {
			return err
		}
		targetID := input.ToReleaseID
		if targetID == "" {
			targetID, err = previousRollbackRelease(ctx, tx, env)
			if err != nil {
				return err
			}
		}
		if targetID == "" {
			return contract.NewFault(contract.PreconditionFailed, "environment has no previous release", "Deploy a release before rolling back, or provide to_release_id explicitly.")
		}
		release, err := releaseByIDTx(ctx, tx, targetID)
		if err != nil {
			return err
		}
		if release.App != env.App || release.Environment != env.Name {
			return contract.NewFault(contract.ValidationFailed, "release belongs to another environment", "Use a release created for this app and environment.")
		}
		if release.Status != platform.ReleaseReady {
			return contract.NewFault(contract.PreconditionFailed, "release is not ready", "Roll back to a ready release.")
		}
		if err := s.checkPlacementByTx(ctx, tx, env.Node, platform.RoleApplication, platform.CapabilityDocker); err != nil {
			return err
		}
		depID, err := platform.NewDeploymentID()
		if err != nil {
			return unavailable()
		}
		if _, err := tx.Exec(ctx, `INSERT INTO eregion_platform_deployments
            (id,operation_id,app_id,environment_id,action,release_id,previous_release_id)
            VALUES($1,$2,$3,$4,'rollback',$5,$6)`,
			depID, id, envAppID(ctx, tx, env.App), env.ID, release.ID, nullableReleaseID(env.ServingRelease)); err != nil {
			return unavailable()
		}
		if _, err := tx.Exec(ctx, `UPDATE eregion_platform_environments SET desired_release_id=$2, updated_at=clock_timestamp() WHERE id=$1`, env.ID, release.ID); err != nil {
			return unavailable()
		}
	case *platform.NodeCheckInput:
		if _, err := nodeByNameTx(ctx, tx, input.Node); err != nil {
			return err
		}
	case *platform.NodeEnrollInput:
		return s.prepareNodeEnroll(ctx, tx, id, input)
	case *platform.NodeReconcileInput:
		if _, err := nodeByNameTx(ctx, tx, input.Node); err != nil {
			return err
		}
	default:
		// Extension actions own their domain rows. The operation row, idempotency,
		// River job, event history, and target lock remain shared.
	}
	return nil
}

func envAppID(ctx context.Context, tx pgx.Tx, app string) string {
	var id string
	_ = tx.QueryRow(ctx, `SELECT id FROM eregion_platform_apps WHERE name=$1`, app).Scan(&id)
	return id
}

func nullableReleaseID(id platform.ReleaseID) any {
	if id == "" {
		return nil
	}
	return string(id)
}

func sha256Hex(raw []byte) string {
	// Kept in one place so platform and diagnostic submissions bind inputs the
	// same way.
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func platformWriteError(err error, message string) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return contract.NewFault(contract.Conflict, message, "Choose a different name or recover the existing accepted record.")
	}
	if errors.As(err, &pgErr) && pgErr.Code == "23503" {
		return contract.NewFault(contract.PreconditionFailed, "referenced platform record is missing", "Create the app, node, or environment first.")
	}
	return unavailable(err)
}

func allocatePorts(ctx context.Context, tx pgx.Tx, explicitHost int) (int, int, error) {
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, portAllocationLock); err != nil {
		return 0, 0, unavailable()
	}
	host := explicitHost
	if host != 0 {
		var used bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM eregion_platform_environments WHERE host_port=$1 OR candidate_port=$1)`, host).Scan(&used); err != nil {
			return 0, 0, unavailable()
		}
		if used {
			return 0, 0, contract.NewFault(contract.Conflict, "host port is already assigned", "Omit host_port to allocate one automatically.")
		}
	} else if err := tx.QueryRow(ctx, `SELECT p FROM generate_series(20000,29999) p
        WHERE NOT EXISTS (SELECT 1 FROM eregion_platform_environments e WHERE e.host_port=p OR e.candidate_port=p) ORDER BY p LIMIT 1`).Scan(&host); err != nil {
		return 0, 0, contract.NewFault(contract.Unavailable, "no stable host port is available", "Remove an unused environment or choose a host_port explicitly.")
	}
	var candidate int
	if err := tx.QueryRow(ctx, `SELECT p FROM generate_series(30000,39999) p
        WHERE p<>$1 AND NOT EXISTS (SELECT 1 FROM eregion_platform_environments e WHERE e.candidate_port=p OR e.host_port=p) ORDER BY p LIMIT 1`, host).Scan(&candidate); err != nil {
		return 0, 0, contract.NewFault(contract.Unavailable, "no candidate host port is available", "Remove an unused environment.")
	}
	return host, candidate, nil
}

func (s *Service) transportIdentity() (string, bool) {
	provider, ok := s.transport.(interface{ NodeIdentity() string })
	if !ok {
		return "", false
	}
	identity := strings.TrimSpace(provider.NodeIdentity())
	return identity, identity != ""
}

func (s *Service) checkPlacementByTx(ctx context.Context, tx pgx.Tx, nodeName, role, capability string) error {
	node, err := nodeByNameTx(ctx, tx, nodeName)
	if err != nil {
		return err
	}
	return s.checkPlacement(node, role, capability)
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// ensureLocalNode makes the local development host usable without a manual
// registration command. It records only capabilities actually found on the
// host; it never claims remote identity or installs anything.
func (s *Service) ensureLocalNode(ctx context.Context) error {
	identity := strings.TrimSpace(s.localNode)
	if identity == "" {
		return nil
	}
	// loadNode resolves the "local"/"main" alias, so an older deployment is not
	// duplicated when the configured identity changed.
	if _, err := loadNode(ctx, s.pool, identity); err == nil {
		return nil
	} else if fault := contract.Classify(err); fault.Code != contract.NotFound {
		return err
	}
	caps := []string{}
	if _, err := exec.LookPath("docker"); err == nil {
		caps = append(caps, platform.CapabilityDocker)
	}
	if _, err := exec.LookPath("pack"); err == nil {
		caps = append(caps, platform.CapabilityBuildpacks)
	}
	if len(caps) == 0 {
		caps = []string{"unverified"}
	}
	roles := []string{platform.RoleApplication, platform.RoleControlPlane}
	if contains(caps, platform.CapabilityBuildpacks) {
		roles = append(roles, platform.RoleBuilder)
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO eregion_platform_nodes(name,roles,capabilities,address,status,last_seen_at)
        VALUES($1,$2,$3,'','ready',clock_timestamp())
        ON CONFLICT (name) DO NOTHING`, identity, roles, caps)
	if err != nil {
		return unavailable(err)
	}
	return nil
}

// ---------------------------------------------------------------- inventory

// Inventory is the shared read model for GET /v1/platform.
func (s *Service) Inventory(ctx context.Context, actor contract.Actor, query platform.InventoryQuery) (platform.Inventory, error) {
	if err := actor.Validate(); err != nil {
		return platform.Inventory{}, err
	}
	if query.Limit == 0 {
		query.Limit = 50
	}
	if query.Limit < 1 || query.Limit > 200 {
		return platform.Inventory{}, contract.NewFault(contract.ValidationFailed, "inventory limit out of range", "Use a limit from 1 to 200.")
	}
	for _, value := range []string{query.App, query.Environment, query.Node, query.State} {
		if value != "" && len(value) > 128 {
			return platform.Inventory{}, contract.NewFault(contract.ValidationFailed, "inventory filter is too long", "Use a documented app, environment, node, or state filter.")
		}
	}
	inventory := platform.Inventory{Collection: query.Collection, Limit: query.Limit}
	switch query.Collection {
	case "summary":
		items, err := s.inventorySummary(ctx)
		if err != nil {
			return inventory, err
		}
		inventory.Items = items
	case "nodes":
		items, err := s.inventoryNodes(ctx, query)
		if err != nil {
			return inventory, err
		}
		inventory.Items = items
	case "apps":
		items, err := s.inventoryApps(ctx, query)
		if err != nil {
			return inventory, err
		}
		inventory.Items = items
	case "environments":
		items, err := s.inventoryEnvironments(ctx, query)
		if err != nil {
			return inventory, err
		}
		inventory.Items = items
	case "releases":
		items, err := s.inventoryReleases(ctx, query)
		if err != nil {
			return inventory, err
		}
		inventory.Items = items
	case "deployments":
		items, err := s.inventoryDeployments(ctx, query)
		if err != nil {
			return inventory, err
		}
		inventory.Items = items
	case "changes":
		items, err := s.inventoryChanges(ctx, query)
		if err != nil {
			return inventory, err
		}
		inventory.Items = items
	default:
		if provider, ok := platform.Default().InventoryProvider(query.Collection); ok {
			items, err := provider.Read(ctx, query)
			if err != nil {
				return inventory, err
			}
			if items == nil {
				items = []any{}
			}
			inventory.Items = items
			return inventory, nil
		}
		return inventory, contract.NewFault(contract.ValidationFailed, "unknown inventory collection", "Use summary, nodes, apps, environments, releases, deployments, or changes.")
	}
	return inventory, nil
}

func (s *Service) inventorySummary(ctx context.Context) (map[string]int64, error) {
	out := map[string]int64{}
	rows, err := s.pool.Query(ctx, `SELECT 'apps', count(*) FROM eregion_platform_apps
        UNION ALL SELECT 'environments', count(*) FROM eregion_platform_environments
        UNION ALL SELECT 'releases', count(*) FROM eregion_platform_releases
        UNION ALL SELECT 'deployments', count(*) FROM eregion_platform_deployments
        UNION ALL SELECT 'nodes', count(*) FROM eregion_platform_nodes
        UNION ALL SELECT 'serving', count(*) FROM eregion_platform_environments WHERE serving_release_id IS NOT NULL`)
	if err != nil {
		return nil, unavailable()
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var count int64
		if err := rows.Scan(&name, &count); err != nil {
			return nil, unavailable()
		}
		out[name] = count
	}
	return out, rows.Err()
}

func (s *Service) inventoryNodes(ctx context.Context, query platform.InventoryQuery) ([]platform.Node, error) {
	items := []platform.Node{}
	rows, err := s.pool.Query(ctx, nodeSelect+` WHERE ($2='' OR name=$2) AND ($3='' OR status=$3 OR enrollment_state=$3) ORDER BY name LIMIT $1`, query.Limit, query.Node, query.State)
	if err != nil {
		return nil, unavailable()
	}
	defer rows.Close()
	for rows.Next() {
		node, err := scanNode(rows)
		if err != nil {
			return nil, unavailable()
		}
		items = append(items, node)
	}
	return items, rows.Err()
}

func (s *Service) inventoryApps(ctx context.Context, query platform.InventoryQuery) ([]platform.App, error) {
	items := []platform.App{}
	rows, err := s.pool.Query(ctx, `SELECT id,name,created_at,updated_at FROM eregion_platform_apps ORDER BY name LIMIT $1`, query.Limit)
	if err != nil {
		return nil, unavailable()
	}
	defer rows.Close()
	for rows.Next() {
		var app platform.App
		if err := rows.Scan(&app.ID, &app.Name, &app.CreatedAt, &app.UpdatedAt); err != nil {
			return nil, unavailable()
		}
		items = append(items, app)
	}
	return items, rows.Err()
}

const environmentSelect = `SELECT e.id,a.name,e.name,e.node_name,coalesce(e.build_node,''),e.host_port,e.candidate_port,e.container_port,
    e.bind_address,e.health_type,e.health_path,e.health_timeout_seconds,e.env,e.secret_refs,e.secret_revision,
    coalesce(e.desired_release_id,''),coalesce(e.applied_release_id,''),coalesce(e.serving_release_id,''),
    e.health_state,e.health_detail,e.health_checked_at,
    coalesce((SELECT array_agg(d.name ORDER BY d.name) FROM eregion_platform_domains d WHERE d.environment_id=e.id),'{}'),
    e.created_at,e.updated_at
    FROM eregion_platform_environments e JOIN eregion_platform_apps a ON a.id=e.app_id`

func scanEnvironmentRow(row pgx.Row) (platform.Environment, error) {
	var env platform.Environment
	var rawEnv, rawRefs []byte
	var desired, applied, serving string
	err := row.Scan(&env.ID, &env.App, &env.Name, &env.Node, &env.BuildNode, &env.HostPort, &env.CandidatePort, &env.ContainerPort,
		&env.Bind, &env.Health.Type, &env.Health.Path, &env.Health.TimeoutSeconds, &rawEnv, &rawRefs, &env.SecretRevision,
		&desired, &applied, &serving, &env.HealthState, &env.HealthDetail, &env.HealthCheckedAt, &env.Domains, &env.CreatedAt, &env.UpdatedAt)
	if err != nil {
		return env, err
	}
	env.Env = map[string]string{}
	env.SecretRefs = map[string]string{}
	if len(rawEnv) > 0 {
		if err := json.Unmarshal(rawEnv, &env.Env); err != nil {
			return env, err
		}
	}
	if len(rawRefs) > 0 {
		if err := json.Unmarshal(rawRefs, &env.SecretRefs); err != nil {
			return env, err
		}
	}
	env.DesiredRelease, env.AppliedRelease, env.ServingRelease = platform.ReleaseID(desired), platform.ReleaseID(applied), platform.ReleaseID(serving)
	return env, nil
}

func (s *Service) inventoryEnvironments(ctx context.Context, query platform.InventoryQuery) ([]platform.Environment, error) {
	items := []platform.Environment{}
	statement := environmentSelect + " WHERE 1=1"
	args := []any{}
	if query.App != "" {
		args = append(args, query.App)
		statement += fmt.Sprintf(" AND a.name=$%d", len(args))
	}
	if query.Environment != "" {
		args = append(args, query.Environment)
		statement += fmt.Sprintf(" AND e.name=$%d", len(args))
	}
	if query.Node != "" {
		args = append(args, query.Node)
		statement += fmt.Sprintf(" AND e.node_name=$%d", len(args))
	}
	if query.State != "" {
		args = append(args, query.State)
		statement += fmt.Sprintf(" AND e.health_state=$%d", len(args))
	}
	args = append(args, query.Limit)
	statement += fmt.Sprintf(" ORDER BY a.name,e.name LIMIT $%d", len(args))
	rows, err := s.pool.Query(ctx, statement, args...)
	if err != nil {
		return nil, unavailable()
	}
	defer rows.Close()
	for rows.Next() {
		env, err := scanEnvironmentRow(rows)
		if err != nil {
			return nil, unavailable()
		}
		items = append(items, env)
	}
	return items, rows.Err()
}

const releaseSelect = `SELECT r.id,a.name,e.name,r.image,r.image_id,r.source,r.build,r.command,r.status,
    r.error_code,r.error,r.created_at,r.completed_at
    FROM eregion_platform_releases r
    JOIN eregion_platform_apps a ON a.id=r.app_id
    JOIN eregion_platform_environments e ON e.id=r.environment_id`

func scanReleaseRow(row pgx.Row) (platform.Release, error) {
	var release platform.Release
	var source, build, command []byte
	err := row.Scan(&release.ID, &release.App, &release.Environment, &release.Image, &release.ImageID,
		&source, &build, &command, &release.Status, &release.ErrorCode, &release.Error, &release.CreatedAt, &release.CompletedAt)
	if err != nil {
		return release, err
	}
	if len(source) > 0 {
		if err := json.Unmarshal(source, &release.Source); err != nil {
			return release, err
		}
	}
	if len(build) > 0 {
		var value platform.Build
		if err := json.Unmarshal(build, &value); err != nil {
			return release, err
		}
		release.Build = &value
	}
	if len(command) > 0 {
		var value platform.CommandSpec
		if err := json.Unmarshal(command, &value); err != nil {
			return release, err
		}
		release.Command = &value
	}
	return release, nil
}

func (s *Service) inventoryReleases(ctx context.Context, query platform.InventoryQuery) ([]platform.Release, error) {
	items := []platform.Release{}
	statement := releaseSelect + " WHERE 1=1"
	args := []any{}
	if query.App != "" {
		args = append(args, query.App)
		statement += fmt.Sprintf(" AND a.name=$%d", len(args))
	}
	if query.Environment != "" {
		args = append(args, query.Environment)
		statement += fmt.Sprintf(" AND e.name=$%d", len(args))
	}
	if query.State != "" {
		args = append(args, query.State)
		statement += fmt.Sprintf(" AND r.status=$%d", len(args))
	}
	args = append(args, query.Limit)
	statement += fmt.Sprintf(" ORDER BY r.created_at DESC LIMIT $%d", len(args))
	rows, err := s.pool.Query(ctx, statement, args...)
	if err != nil {
		return nil, unavailable()
	}
	defer rows.Close()
	for rows.Next() {
		release, err := scanReleaseRow(rows)
		if err != nil {
			return nil, unavailable()
		}
		items = append(items, release)
	}
	return items, rows.Err()
}

const deploymentSelect = `SELECT d.id,d.operation_id,a.name,e.name,d.action,d.release_id,
    coalesce(d.previous_release_id,''),d.state,d.error_code,d.message,d.created_at,d.updated_at,d.completed_at
    FROM eregion_platform_deployments d
    JOIN eregion_platform_apps a ON a.id=d.app_id
    JOIN eregion_platform_environments e ON e.id=d.environment_id`

func scanDeploymentRow(row pgx.Row) (platform.Deployment, error) {
	var dep platform.Deployment
	var previous string
	err := row.Scan(&dep.ID, &dep.OperationID, &dep.App, &dep.Environment, &dep.Action, &dep.ReleaseID,
		&previous, &dep.State, &dep.ErrorCode, &dep.Message, &dep.CreatedAt, &dep.UpdatedAt, &dep.CompletedAt)
	dep.PreviousReleaseID = platform.ReleaseID(previous)
	return dep, err
}

func (s *Service) inventoryDeployments(ctx context.Context, query platform.InventoryQuery) ([]platform.Deployment, error) {
	items := []platform.Deployment{}
	statement := deploymentSelect + " WHERE 1=1"
	args := []any{}
	if query.App != "" {
		args = append(args, query.App)
		statement += fmt.Sprintf(" AND a.name=$%d", len(args))
	}
	if query.Environment != "" {
		args = append(args, query.Environment)
		statement += fmt.Sprintf(" AND e.name=$%d", len(args))
	}
	if query.State != "" {
		args = append(args, query.State)
		statement += fmt.Sprintf(" AND d.state=$%d", len(args))
	}
	args = append(args, query.Limit)
	statement += fmt.Sprintf(" ORDER BY d.created_at DESC LIMIT $%d", len(args))
	rows, err := s.pool.Query(ctx, statement, args...)
	if err != nil {
		return nil, unavailable()
	}
	defer rows.Close()
	for rows.Next() {
		dep, err := scanDeploymentRow(rows)
		if err != nil {
			return nil, unavailable()
		}
		items = append(items, dep)
	}
	return items, rows.Err()
}

func (s *Service) inventoryChanges(ctx context.Context, query platform.InventoryQuery) ([]platform.Change, error) {
	items := []platform.Change{}
	statement := `SELECT c.id,a.name,coalesce(e.name,''),c.sequence,c.kind,c.operation_id,c.actor_kind,c.actor_subject,c.summary,c.desired,c.applied,c.created_at
        FROM eregion_platform_changes c
        JOIN eregion_platform_apps a ON a.id=c.app_id
        LEFT JOIN eregion_platform_environments e ON e.id=c.environment_id WHERE 1=1`
	args := []any{}
	if query.App != "" {
		args = append(args, query.App)
		statement += fmt.Sprintf(" AND a.name=$%d", len(args))
	}
	if query.Environment != "" {
		args = append(args, query.Environment)
		statement += fmt.Sprintf(" AND e.name=$%d", len(args))
	}
	if query.State != "" {
		args = append(args, query.State)
		statement += fmt.Sprintf(" AND c.kind=$%d", len(args))
	}
	args = append(args, query.Limit)
	statement += fmt.Sprintf(" ORDER BY c.created_at DESC LIMIT $%d", len(args))
	rows, err := s.pool.Query(ctx, statement, args...)
	if err != nil {
		return nil, unavailable()
	}
	defer rows.Close()
	for rows.Next() {
		var change platform.Change
		var desired, applied []byte
		if err := rows.Scan(&change.ID, &change.App, &change.Environment, &change.Sequence, &change.Kind,
			&change.OperationID, &change.Actor.Kind, &change.Actor.Subject, &change.Summary, &desired, &applied, &change.CreatedAt); err != nil {
			return nil, unavailable()
		}
		if len(desired) > 0 {
			_ = json.Unmarshal(desired, &change.Desired)
		}
		if len(applied) > 0 {
			_ = json.Unmarshal(applied, &change.Applied)
		}
		items = append(items, change)
	}
	return items, rows.Err()
}

// previousRollbackRelease chooses the release that served before the current
// serving release became active. AppliedRelease is not sufficient: it normally
// equals the current serving release. A latest successful deployment's
// previous_release_id is the authoritative one-step history.
func previousRollbackRelease(ctx context.Context, tx pgx.Tx, env platform.Environment) (platform.ReleaseID, error) {
	if env.ServingRelease != "" {
		var previous string
		err := tx.QueryRow(ctx, `SELECT coalesce(previous_release_id,'') FROM eregion_platform_deployments
            WHERE environment_id=$1 AND state='succeeded' AND release_id=$2 AND previous_release_id IS NOT NULL
            ORDER BY completed_at DESC NULLS LAST, created_at DESC LIMIT 1`, env.ID, env.ServingRelease).Scan(&previous)
		if err == nil && previous != "" {
			return platform.ReleaseID(previous), nil
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return "", unavailable()
		}
	}
	var previous string
	err := tx.QueryRow(ctx, `SELECT release_id FROM eregion_platform_deployments
        WHERE environment_id=$1 AND state='succeeded' AND release_id IS DISTINCT FROM $2
        ORDER BY completed_at DESC NULLS LAST, created_at DESC LIMIT 1`, env.ID, nullableReleaseID(env.ServingRelease)).Scan(&previous)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", unavailable()
	}
	return platform.ReleaseID(previous), nil
}

func (s *Service) previousRollbackRelease(ctx context.Context, env platform.Environment) (platform.ReleaseID, error) {
	if env.ServingRelease != "" {
		var previous string
		err := s.pool.QueryRow(ctx, `SELECT coalesce(previous_release_id,'') FROM eregion_platform_deployments
            WHERE environment_id=$1 AND state='succeeded' AND release_id=$2 AND previous_release_id IS NOT NULL
            ORDER BY completed_at DESC NULLS LAST, created_at DESC LIMIT 1`, env.ID, env.ServingRelease).Scan(&previous)
		if err == nil && previous != "" {
			return platform.ReleaseID(previous), nil
		}
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return "", unavailable()
		}
	}
	var previous string
	err := s.pool.QueryRow(ctx, `SELECT release_id FROM eregion_platform_deployments
        WHERE environment_id=$1 AND state='succeeded' AND release_id IS DISTINCT FROM $2
        ORDER BY completed_at DESC NULLS LAST, created_at DESC LIMIT 1`, env.ID, nullableReleaseID(env.ServingRelease)).Scan(&previous)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", unavailable()
	}
	return platform.ReleaseID(previous), nil
}

// --------------------------------------------------------------- tx lookups

func appByNameTx(ctx context.Context, tx pgx.Tx, name string) (platform.App, error) {
	var app platform.App
	err := tx.QueryRow(ctx, `SELECT id,name,created_at,updated_at FROM eregion_platform_apps WHERE name=$1`, name).
		Scan(&app.ID, &app.Name, &app.CreatedAt, &app.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return app, contract.NewFault(contract.NotFound, "application not found", "Create the application first.")
	}
	if err != nil {
		return app, unavailable()
	}
	return app, nil
}

func nodeByNameTx(ctx context.Context, tx pgx.Tx, name string) (platform.Node, error) {
	return loadNode(ctx, tx, name)
}

func environmentByNameTx(ctx context.Context, tx pgx.Tx, app, name string) (platform.Environment, error) {
	env, err := scanEnvironmentRow(tx.QueryRow(ctx, environmentSelect+" WHERE a.name=$1 AND e.name=$2 FOR UPDATE OF e", app, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return env, contract.NewFault(contract.NotFound, "environment not found", "Create the environment under this app first.")
	}
	if err != nil {
		return env, unavailable()
	}
	return env, nil
}

func releaseByIDTx(ctx context.Context, tx pgx.Tx, id platform.ReleaseID) (platform.Release, error) {
	release, err := scanReleaseRow(tx.QueryRow(ctx, releaseSelect+" WHERE r.id=$1", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return release, contract.NewFault(contract.NotFound, "release not found", "Use a release ID from inventory.")
	}
	if err != nil {
		return release, unavailable()
	}
	return release, nil
}

// ---------------------------------------------------------------- dry run

// Preview implements the optional dry-run planner used by the API.
func (s *Service) Preview(ctx context.Context, actor contract.Actor, normalized platform.Normalized) (platform.Preview, error) {
	if err := actor.Validate(); err != nil {
		return platform.Preview{}, err
	}
	preview := platform.Preview{Kind: normalized.Kind, Target: normalized.Target.String(), DryRun: true, Input: normalized.Input.Preview(), Summary: normalized.Summary}
	switch input := normalized.Input.(type) {
	case *platform.DeployInput:
		env, release, err := s.planDeployment(ctx, normalized.Target, input.ReleaseID)
		if err != nil {
			return preview, err
		}
		preview.Steps = deploymentSteps()
		preview.Summary = fmt.Sprintf("Deploy %s to %s/%s after a candidate health check; the serving release stays until the switch.", release.Image, env.App, env.Name)
		addReleaseWarnings(&preview, release, env)
	case *platform.RollbackInput:
		target := input.ToReleaseID
		env, err := s.environment(ctx, normalized.Target.App, normalized.Target.Environment)
		if err != nil {
			return preview, err
		}
		if target == "" {
			target, err = s.previousRollbackRelease(ctx, env)
			if err != nil {
				return preview, err
			}
		}
		if target == "" {
			return preview, contract.NewFault(contract.PreconditionFailed, "environment has no previous release", "Deploy a release before rolling back, or provide to_release_id explicitly.")
		}
		_, release, err := s.planDeployment(ctx, normalized.Target, target)
		if err != nil {
			return preview, err
		}
		preview.Steps = deploymentSteps()
		preview.Summary = fmt.Sprintf("Roll back %s/%s to %s using the same health-first path.", env.App, env.Name, release.Image)
		addReleaseWarnings(&preview, release, env)
	case *platform.EnvironmentCreateInput:
		preview.Steps = []string{"record environment", "allocate stable and candidate ports"}
		preview.Summary = "Create environment " + input.Name + " on node " + input.Node + "."
		if input.BuildNode != "" {
			preview.Summary += " Builds run on builder node " + input.BuildNode + "."
			if input.BuildNode != input.Node {
				preview.Warnings = append(preview.Warnings, "A distinct build node needs a shared registry to move the image to the application node; release.create refuses that path until registry transfer is configured.")
			}
		}
	case *platform.ReleaseCreateInput:
		preview.Summary = "Record release from " + input.Source.Type + " source."
		if input.Source.Type == "image" {
			preview.Steps = []string{"verify image on node", "mark release ready"}
		} else {
			env, err := s.environment(ctx, normalized.Target.App, normalized.Target.Environment)
			if err != nil {
				return preview, err
			}
			if err := crossNodeBuildUnsupported(env, input.Source.Type); err != nil {
				return preview, err
			}
			preview.Steps = []string{"prepare source", "pack build", "verify image", "mark release ready"}
			preview.Summary = "Build on " + env.BuildNodeName() + ", then mark the release ready."
			preview.Warnings = append(preview.Warnings, "Buildpacks execution requires pack on the environment's builder-capable node.")
		}
	case *platform.NodeCheckInput:
		preview.Steps = []string{"check docker", "check pack"}
		preview.Summary = "Observe runtime availability on node " + input.Node + "."
	case *platform.NodeRegisterInput:
		preview.Summary = "Record placement facts for node " + input.Name + "; this does not install or verify host state."
	case *platform.NodeEnrollInput:
		preview.Steps = []string{"pin SSH host key", "verify node identity", "probe host capabilities", "configure role directories", "record enrollment"}
		preview.Summary = "Enroll node " + input.Node + " over pinned SSH; it receives work only after it proves its identity and the probe satisfies its roles."
		if input.Retry {
			preview.Summary = "Re-run the recorded enrollment of node " + input.Node + "; completed steps are not repeated."
		}
		if input.PrivateNetwork != nil {
			preview.Warnings = append(preview.Warnings, "Private network keys are generated on the node and reconciled by platform.node.reconcile; enrollment itself does not change WireGuard.")
		}
	case *platform.NodeReconcileInput:
		preview.Steps = []string{"converge role directories", "generate node WireGuard key", "render peer configuration", "apply peers"}
		preview.Summary = "Converge host configuration and WireGuard peers on node " + input.Node + "; rerunning changes nothing when the node already matches."
	default:
		if preview.Summary == "" {
			preview.Summary = "Execute registered platform action " + normalized.Kind + "."
		}
	}
	return preview, nil
}

func deploymentSteps() []string {
	return []string{"preflight (verify the release image on the node)", "release command", "candidate", "health", "switch", "verify", "cleanup"}
}

func addReleaseWarnings(preview *platform.Preview, release platform.Release, env platform.Environment) {
	if release.Status != platform.ReleaseReady {
		preview.Warnings = append(preview.Warnings, "Release is not ready; submission will fail until its release operation succeeds.")
	}
	if env.ServingRelease == "" {
		preview.Warnings = append(preview.Warnings, "No release is serving yet; a candidate failure leaves the environment without a serving container.")
	}
	if env.Health.Type == "http" {
		preview.Warnings = append(preview.Warnings, "Direct Docker switching has a short restart window; zero-downtime edge switching is not implemented.")
	}
}

func (s *Service) planDeployment(ctx context.Context, target platform.Target, releaseID platform.ReleaseID) (platform.Environment, platform.Release, error) {
	env, err := s.environment(ctx, target.App, target.Environment)
	if err != nil {
		return env, platform.Release{}, err
	}
	release, err := s.releaseByID(ctx, releaseID)
	if err != nil {
		return env, release, err
	}
	if release.App != env.App || release.Environment != env.Name {
		return env, release, contract.NewFault(contract.ValidationFailed, "release belongs to another environment", "Use a release created for this app and environment.")
	}
	node, err := s.node(ctx, env.Node)
	if err != nil {
		return env, release, err
	}
	if err := s.checkPlacement(node, platform.RoleApplication, platform.CapabilityDocker); err != nil {
		return env, release, err
	}
	if release.Status != platform.ReleaseReady {
		return env, release, contract.NewFault(contract.PreconditionFailed, "release is not ready", "Wait for the release operation to succeed, then deploy it.")
	}
	return env, release, nil
}

func (s *Service) environment(ctx context.Context, app, name string) (platform.Environment, error) {
	env, err := scanEnvironmentRow(s.pool.QueryRow(ctx, environmentSelect+" WHERE a.name=$1 AND e.name=$2", app, name))
	if errors.Is(err, pgx.ErrNoRows) {
		return env, contract.NewFault(contract.NotFound, "environment not found", "Create the environment under this app first.")
	}
	if err != nil {
		return env, unavailable()
	}
	return env, nil
}

func (s *Service) releaseByID(ctx context.Context, id platform.ReleaseID) (platform.Release, error) {
	release, err := scanReleaseRow(s.pool.QueryRow(ctx, releaseSelect+" WHERE r.id=$1", id))
	if errors.Is(err, pgx.ErrNoRows) {
		return release, contract.NewFault(contract.NotFound, "release not found", "Use a release ID from inventory.")
	}
	if err != nil {
		return release, unavailable()
	}
	return release, nil
}

func (s *Service) node(ctx context.Context, name string) (platform.Node, error) {
	return loadNode(ctx, s.pool, name)
}
