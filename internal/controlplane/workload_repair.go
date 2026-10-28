package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/platform"
)

// maintenanceActor identifies control-plane repair work. It is a system actor,
// never an operator: the restart is auditable and not attributable to a user.
var maintenanceActor = contract.Actor{Kind: "system", Subject: "maintenance"}

// servingWorkload is one environment's recorded serving formation.
type servingWorkload struct {
	App         string
	Environment string
	Deployment  platform.DeploymentID
	Node        string
	Instances   []platform.DeploymentInstance
}

// repairServingWorkloads restores a serving container that disappeared while
// the node kept running. A missing set queues one restart; the idempotency key
// makes concurrent workers and repeated starts collapse to the same operation.
func (s *Service) repairServingWorkloads(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, `SELECT a.name,e.name,coalesce(e.serving_deployment_id,''),
        coalesce(dep.instances,'[]'::jsonb),
        coalesce(NULLIF(r.runtime->>'node',''),e.node_name)
        FROM eregion_platform_environments e
        JOIN eregion_platform_apps a ON a.id=e.app_id
        JOIN eregion_platform_deployments dep ON dep.id=e.serving_deployment_id AND dep.state='succeeded'
        LEFT JOIN eregion_platform_environment_revisions r ON r.id=dep.revision_id
        WHERE e.serving_deployment_id IS NOT NULL
        ORDER BY a.name,e.name`)
	if err != nil {
		return unavailable()
	}
	var workloads []servingWorkload
	for rows.Next() {
		var workload servingWorkload
		var rawInstances []byte
		if err := rows.Scan(&workload.App, &workload.Environment, &workload.Deployment,
			&rawInstances, &workload.Node); err != nil {
			rows.Close()
			return unavailable()
		}
		if len(rawInstances) > 0 {
			if err := json.Unmarshal(rawInstances, &workload.Instances); err != nil {
				rows.Close()
				return unavailable()
			}
		}
		workloads = append(workloads, workload)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	var failures []error
	for _, workload := range workloads {
		if len(workload.Instances) == 0 {
			continue
		}
		active, err := s.environmentHasActiveOperation(ctx, s.pool, workload.App, workload.Environment, "")
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if active {
			continue
		}
		observed, err := s.observeNodeWorkloads(ctx, workload.Node, workload.App, workload.Environment)
		if err != nil {
			// An unreachable node is not evidence that a container is missing.
			failures = append(failures, fmt.Errorf("observe %s/%s on %s: %w", workload.App, workload.Environment, workload.Node, err))
			continue
		}
		missing := missingServingInstances(workload, observed.Containers)
		if len(missing) == 0 {
			continue
		}
		input, _ := json.Marshal(platform.RestartInput{
			App: workload.App, Environment: workload.Environment,
			ExpectedServingDeploymentID: workload.Deployment,
		})
		_, err = s.Submit(ctx, operations.Submission{
			Kind:           platform.KindRestart,
			Target:         "env/" + workload.App + "/" + workload.Environment,
			Actor:          maintenanceActor,
			IdempotencyKey: repairIdempotencyKey(workload.Deployment, missing),
			Input:          input,
		})
		if err != nil {
			// A deploy accepted between observation and submission, a serving
			// deployment change, or an active delete refuses the repair. The
			// next maintenance run re-observes instead.
			switch contract.Classify(err).Code {
			case contract.Conflict, contract.PreconditionFailed:
				continue
			}
			failures = append(failures, fmt.Errorf("queue repair for %s/%s: %w", workload.App, workload.Environment, err))
		}
	}
	return errors.Join(failures...)
}

// operationQueryer is the subset of pgxpool.Pool and pgx.Tx used by the repair
// guard, so submission and maintenance can share one query.
type operationQueryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}

type activeOperation struct {
	Kind  string
	State contract.State
}

func activeEnvironmentOperations(ctx context.Context, q operationQueryer, app, environment string, exclude contract.OperationID) ([]activeOperation, error) {
	rows, err := q.Query(ctx, `SELECT kind,state FROM eregion_operations
        WHERE id <> $3 AND target IN ($1,$2) AND state IN ('queued','running','cancelling')`,
		"env/"+app+"/"+environment, "app/"+app, exclude)
	if err != nil {
		return nil, unavailable()
	}
	defer rows.Close()
	var active []activeOperation
	for rows.Next() {
		var operation activeOperation
		var state string
		if err := rows.Scan(&operation.Kind, &state); err != nil {
			return nil, unavailable()
		}
		operation.State = contract.State(state)
		active = append(active, operation)
	}
	return active, rows.Err()
}

func (s *Service) environmentHasActiveOperation(ctx context.Context, q operationQueryer, app, environment string, exclude contract.OperationID) (bool, error) {
	active, err := activeEnvironmentOperations(ctx, q, app, environment, exclude)
	if err != nil {
		return false, err
	}
	for _, operation := range active {
		if environmentOperationBlocksRepair(operation.Kind, operation.State) {
			return true, nil
		}
	}
	return false, nil
}

// repairSubmissionAllowed is the pure submission guard for a repair restart:
// the observed serving deployment must still be current and no blocking
// operation may own the environment.
func repairSubmissionAllowed(expected, serving platform.DeploymentID, active []activeOperation) bool {
	if expected == "" || expected != serving {
		return false
	}
	for _, operation := range active {
		if environmentOperationBlocksRepair(operation.Kind, operation.State) {
			return false
		}
	}
	return true
}

// repairRestartStillApplies refuses a queued repair whose serving deployment
// changed after it was accepted.
func repairRestartStillApplies(input platform.RestartInput, env platform.Environment) bool {
	return input.ExpectedServingDeploymentID == "" || input.ExpectedServingDeploymentID == env.ServingDeploymentID
}

// environmentOperationBlocksRepair is the pure skip rule: an active deploy,
// rollback, restart or delete owns the environment and repair must not queue a
// competing restart.
func environmentOperationBlocksRepair(kind string, state contract.State) bool {
	if state.Terminal() {
		return false
	}
	switch kind {
	case platform.KindDeploy, platform.KindRollback, platform.KindRestart,
		platform.KindAppDelete, platform.KindEnvironmentDelete:
		return true
	default:
		return false
	}
}

// missingServingInstances compares recorded names with the node's labelled
// observation. A container that is not running under this app, environment and
// serving deployment is missing, even when a container of that name exists.
func missingServingInstances(workload servingWorkload, observed []platform.ContainerObservation) []string {
	byName := map[string]platform.ContainerObservation{}
	for _, container := range observed {
		if container.App != workload.App || container.Environment != workload.Environment {
			continue
		}
		if container.Deployment != string(workload.Deployment) {
			continue
		}
		byName[container.Name] = container
	}
	missing := []string{}
	for _, instance := range workload.Instances {
		if instance.Name == "" {
			continue
		}
		container, ok := byName[instance.Name]
		if !ok || !servingContainerRunning(container.State) {
			missing = append(missing, instance.Name)
		}
	}
	sort.Strings(missing)
	return missing
}

// servingContainerRunning accepts Docker's running and restarting states. An
// exited container that under `unless-stopped` was not restarted is missing.
func servingContainerRunning(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "running", "restarting":
		return true
	default:
		return false
	}
}

// repairIdempotencyKey is stable across workers and independent of the observed
// order of the missing names.
func repairIdempotencyKey(deployment platform.DeploymentID, missing []string) string {
	sorted := append([]string(nil), missing...)
	sort.Strings(sorted)
	sum := sha256.Sum256([]byte(strings.Join(sorted, "\x00")))
	return "repair:" + string(deployment) + ":" + hex.EncodeToString(sum[:8])
}

// observeNodeWorkloads reads Docker ownership labels through the node
// transport. It is read-only and carries no operation identity.
func (s *Service) observeNodeWorkloads(ctx context.Context, nodeName, app, environment string) (platform.Workloads, error) {
	if s.resolver == nil {
		return platform.Workloads{}, contract.NewFault(contract.Unavailable, "the worker has no node resolver", "Run workload repair in a worker configured with node transports.")
	}
	tr, err := s.resolver.TransportFor(ctx, nodeName)
	if err != nil {
		return platform.Workloads{}, err
	}
	payload, err := json.Marshal(node.ObservePayload{App: app, Environment: environment})
	if err != nil {
		return platform.Workloads{}, err
	}
	response, err := tr.Call(ctx, node.Request{
		Method: node.MethodObserve, RequestID: newRequestID(), Payload: payload,
	}, nil)
	if err != nil {
		return platform.Workloads{}, err
	}
	if response.Status != engine.StatusCompleted {
		code := contract.Code(response.ErrorCode)
		if code == "" {
			code = contract.Unavailable
		}
		return platform.Workloads{}, contract.NewFault(code, "the node did not return a workload observation", response.Detail)
	}
	var workloads platform.Workloads
	if err := json.Unmarshal(response.Result, &workloads); err != nil {
		return platform.Workloads{}, contract.NewFault(contract.Unavailable, "the node returned an unreadable workload observation", err.Error())
	}
	return workloads, nil
}
