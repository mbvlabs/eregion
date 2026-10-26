package controlplane

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

// insertEnvironmentRevisionTx appends one immutable revision. The environment
// row lock held by the caller (or the just-inserted row in this transaction)
// serializes the per-environment number.
func insertEnvironmentRevisionTx(ctx context.Context, tx pgx.Tx, envID platform.EnvironmentID, opID contract.OperationID, actor contract.Actor, operation, summary string, state platform.RevisionState) (platform.RevisionID, int, error) {
	if err := platform.NormalizeRevisionState(&state); err != nil {
		return "", 0, err
	}
	revisionID, err := platform.NewEnvironmentRevisionID()
	if err != nil {
		return "", 0, unavailable()
	}
	var number int
	if err := tx.QueryRow(ctx, `SELECT coalesce(max(number),0)+1 FROM eregion_platform_environment_revisions WHERE environment_id=$1`, envID).Scan(&number); err != nil {
		return "", 0, unavailable()
	}
	source, _ := json.Marshal(state.Source)
	runtime, _ := json.Marshal(state.Runtime)
	processes, _ := json.Marshal(state.Processes)
	config, _ := json.Marshal(state.Config)
	if _, err := tx.Exec(ctx, `INSERT INTO eregion_platform_environment_revisions
        (id,environment_id,number,operation_id,actor_kind,actor_subject,operation,summary,
         source,auto_deploy,webhook_secret_name,runtime,processes,config,secret_names,deploy_intent)
        VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)`,
		revisionID, envID, number, nullableOperationID(opID), actor.Kind, actor.Subject, operation, summary,
		source, state.AutoDeploy, state.WebhookSecretName, runtime, processes, config, state.SecretNames, state.DeployIntent); err != nil {
		return "", 0, platformWriteError(err, "environment revision could not be recorded")
	}
	return revisionID, number, nil
}

func nullableOperationID(id contract.OperationID) any {
	if id == "" {
		return nil
	}
	return string(id)
}

// insertEnvironmentTx writes the environment row, its first revision and the
// current-revision pointer in one transaction.
func (s *Service) insertEnvironmentTx(ctx context.Context, tx pgx.Tx, app platform.App, spec *platform.EnvironmentSpec, opID contract.OperationID, actor contract.Actor, operation, summary string, deployIntent bool) (platform.EnvironmentID, error) {
	node, err := nodeByNameTx(ctx, tx, spec.Node)
	if err != nil {
		return "", err
	}
	spec.Node = node.Name
	spec.Runtime.Node = node.Name
	if err := s.checkPlacement(node, platform.RoleApplication, platform.CapabilityDocker); err != nil {
		return "", err
	}
	hostPort, candidatePort, err := allocatePorts(ctx, tx, spec.HostPort)
	if err != nil {
		return "", err
	}
	var buildNode any
	if spec.BuildNode != "" {
		builder, err := nodeByNameTx(ctx, tx, spec.BuildNode)
		if err != nil {
			return "", err
		}
		if err := s.checkPlacement(builder, platform.RoleBuilder, platform.CapabilityBuildpacks); err != nil {
			return "", err
		}
		buildNode = builder.Name
	}
	state := revisionStateFromSpec(*spec, node.Name, deployIntent)
	if err := platform.NormalizeRevisionState(&state); err != nil {
		return "", err
	}
	envID, err := platform.NewEnvironmentID()
	if err != nil {
		return "", unavailable()
	}
	config, _ := json.Marshal(state.Config)
	secretRefs := spec.SecretRefs
	if len(state.SecretNames) > 0 {
		secretRefs = map[string]string{}
	}
	refs, _ := json.Marshal(secretRefs)
	containerPort := spec.ContainerPort
	for _, process := range state.Processes {
		if process.Kind == "web" && process.ContainerPort > 0 {
			containerPort = process.ContainerPort
			break
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO eregion_platform_environments
        (id,app_id,name,node_name,build_node,host_port,candidate_port,container_port,bind_address,health_type,health_path,health_timeout_seconds,env,secret_refs)
        VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)`,
		envID, app.ID, spec.Name, node.Name, buildNode, hostPort, candidatePort, containerPort, spec.Bind,
		state.Runtime.HealthType, state.Runtime.HealthPath, state.Runtime.HealthTimeoutSeconds, config, refs); err != nil {
		return "", platformWriteError(err, "environment name or port already exists")
	}
	revisionID, _, err := insertEnvironmentRevisionTx(ctx, tx, envID, opID, actor, operation, summary, state)
	if err != nil {
		return "", err
	}
	if _, err := tx.Exec(ctx, `UPDATE eregion_platform_environments SET current_revision_id=$2,updated_at=clock_timestamp() WHERE id=$1`, envID, revisionID); err != nil {
		return "", unavailable()
	}
	return envID, nil
}

func revisionStateFromSpec(spec platform.EnvironmentSpec, node string, deployIntent bool) platform.RevisionState {
	state := platform.RevisionState{
		Source:            spec.Source,
		AutoDeploy:        spec.AutoDeploy,
		WebhookSecretName: spec.WebhookSecretName,
		Runtime:           spec.Runtime,
		Processes:         spec.Processes,
		Config:            spec.Config,
		SecretNames:       spec.SecretNames,
		DeployIntent:      deployIntent,
	}
	if state.Runtime.Node == "" {
		state.Runtime.Node = node
	}
	if len(state.Config) == 0 && len(spec.Env) > 0 {
		state.Config = spec.Env
	}
	if len(state.Processes) == 0 {
		port := spec.ContainerPort
		if port == 0 {
			port = platform.DefaultContainerPort
		}
		state.Processes = []platform.ProcessSpec{{Name: "web", Kind: "web", Args: []string{}, Replicas: 1, ContainerPort: port}}
	}
	return state
}

// publishEnvironmentRevisionTx appends a revision and mirrors the legacy
// columns the current deploy worker still reads. The mirror goes away with the
// per-process container work in work list row 5.
func publishEnvironmentRevisionTx(ctx context.Context, tx pgx.Tx, env platform.Environment, opID contract.OperationID, actor contract.Actor, operation, summary string, state platform.RevisionState, buildNode *string) (platform.RevisionID, error) {
	if err := platform.NormalizeRevisionState(&state); err != nil {
		return "", err
	}
	revisionID, _, err := insertEnvironmentRevisionTx(ctx, tx, env.ID, opID, actor, operation, summary, state)
	if err != nil {
		return "", err
	}
	node := state.Runtime.Node
	if node == "" {
		node = env.Node
	}
	containerPort := env.ContainerPort
	for _, process := range state.Processes {
		if process.Kind == "web" && process.ContainerPort > 0 {
			containerPort = process.ContainerPort
			break
		}
	}
	config, _ := json.Marshal(state.Config)
	refs := env.SecretRefs
	if len(state.SecretNames) > 0 {
		refs = map[string]string{}
	}
	refsJSON, _ := json.Marshal(refs)
	buildNodeValue := nullableString(env.BuildNode)
	if buildNode != nil {
		buildNodeValue = nullableString(*buildNode)
	}
	if _, err := tx.Exec(ctx, `UPDATE eregion_platform_environments
        SET node_name=$2,build_node=$3,container_port=$4,health_type=$5,health_path=$6,
            health_timeout_seconds=$7,env=$8,secret_refs=$9,current_revision_id=$10,updated_at=clock_timestamp()
        WHERE id=$1`, env.ID, node, buildNodeValue, containerPort,
		state.Runtime.HealthType, state.Runtime.HealthPath, state.Runtime.HealthTimeoutSeconds,
		config, refsJSON, revisionID); err != nil {
		return "", platformWriteError(err, "environment revision could not be applied")
	}
	return revisionID, nil
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func currentRevisionStateTx(ctx context.Context, tx pgx.Tx, env platform.Environment) (platform.RevisionState, error) {
	if env.Revision != nil {
		return platform.CloneRevisionState(env.Revision.RevisionState), nil
	}
	// The migration backfills every pre-existing environment, so this is only a
	// defensive fallback for a row created by an older binary mid-upgrade.
	state := platform.RevisionState{
		Runtime: platform.RuntimeSpec{
			Node: env.Node, HealthType: env.Health.Type, HealthPath: env.Health.Path,
			HealthTimeoutSeconds: env.Health.TimeoutSeconds,
		},
		Processes: []platform.ProcessSpec{{Name: "web", Kind: "web", Args: []string{}, Replicas: 1, ContainerPort: env.ContainerPort}},
		Config:    env.Env,
	}
	return state, nil
}

// ---------------------------------------------------------------- inventory

const revisionSelect = `SELECT r.id,a.name,e.name,r.number,coalesce(r.operation_id,''),
    r.actor_kind,r.actor_subject,r.operation,r.summary,r.source,r.auto_deploy,r.webhook_secret_name,
    r.runtime,r.processes,r.config,r.secret_names,r.deploy_intent,r.created_at
    FROM eregion_platform_environment_revisions r
    JOIN eregion_platform_environments e ON e.id=r.environment_id
    JOIN eregion_platform_apps a ON a.id=e.app_id`

func scanRevisionRow(row pgx.Row) (platform.EnvironmentRevision, error) {
	var revision platform.EnvironmentRevision
	var source, runtime, processes, config []byte
	err := row.Scan(&revision.ID, &revision.App, &revision.Environment, &revision.Number,
		&revision.OperationID, &revision.Actor.Kind, &revision.Actor.Subject, &revision.Operation,
		&revision.Summary, &source, &revision.AutoDeploy, &revision.WebhookSecretName,
		&runtime, &processes, &config, &revision.SecretNames, &revision.DeployIntent, &revision.CreatedAt)
	if err != nil {
		return revision, err
	}
	if len(source) > 0 {
		if err := json.Unmarshal(source, &revision.Source); err != nil {
			return revision, err
		}
	}
	if len(runtime) > 0 {
		if err := json.Unmarshal(runtime, &revision.Runtime); err != nil {
			return revision, err
		}
	}
	if len(processes) > 0 {
		if err := json.Unmarshal(processes, &revision.Processes); err != nil {
			return revision, err
		}
	}
	revision.Config = map[string]string{}
	if len(config) > 0 {
		if err := json.Unmarshal(config, &revision.Config); err != nil {
			return revision, err
		}
	}
	if revision.Processes == nil {
		revision.Processes = []platform.ProcessSpec{}
	}
	if revision.SecretNames == nil {
		revision.SecretNames = []string{}
	}
	return revision, nil
}

func (s *Service) revisionByID(ctx context.Context, id platform.RevisionID) (platform.EnvironmentRevision, error) {
	revision, err := scanRevisionRow(s.pool.QueryRow(ctx, revisionSelect+" WHERE r.id=$1", id))
	if err != nil {
		return revision, unavailable(err)
	}
	return revision, nil
}

func (s *Service) inventoryRevisions(ctx context.Context, query platform.InventoryQuery) ([]platform.EnvironmentRevision, error) {
	items := []platform.EnvironmentRevision{}
	statement := revisionSelect + " WHERE 1=1"
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
		statement += fmt.Sprintf(" AND r.operation=$%d", len(args))
	}
	args = append(args, query.Limit)
	statement += fmt.Sprintf(" ORDER BY r.created_at DESC LIMIT $%d", len(args))
	rows, err := s.pool.Query(ctx, statement, args...)
	if err != nil {
		return nil, unavailable()
	}
	defer rows.Close()
	for rows.Next() {
		revision, err := scanRevisionRow(rows)
		if err != nil {
			return nil, unavailable()
		}
		items = append(items, revision)
	}
	return items, rows.Err()
}

func (s *Service) inventoryDeployability(ctx context.Context, query platform.InventoryQuery) ([]platform.EnvironmentDeployabilityReport, error) {
	environments, err := s.inventoryEnvironments(ctx, query)
	if err != nil {
		return nil, err
	}
	items := make([]platform.EnvironmentDeployabilityReport, 0, len(environments))
	for _, env := range environments {
		items = append(items, platform.EnvironmentDeployabilityReport{
			App: env.App, Environment: env.Name, CurrentRevisionID: env.RevisionID,
			Deployability: env.Deployability,
		})
	}
	return items, nil
}
