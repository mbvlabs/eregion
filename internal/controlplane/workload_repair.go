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

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/entities"
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
	rows, err := s.entities.Platform().Deployments().RepairTargets(ctx)
	if err != nil {
		return unavailable(err)
	}
	workloads, err := servingWorkloads(rows)
	if err != nil {
		return err
	}
	var failures []error
	for _, workload := range workloads {
		if err := s.repairServingWorkload(ctx, workload); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func servingWorkloads(rows []entities.RepairTarget) ([]servingWorkload, error) {
	workloads := make([]servingWorkload, 0, len(rows))
	for _, row := range rows {
		workload := servingWorkload{
			App: row.App, Environment: row.Environment,
			Deployment: row.ServingDeployment, Node: row.Node,
		}
		if len(row.Instances) > 0 {
			if err := json.Unmarshal(row.Instances, &workload.Instances); err != nil {
				return nil, unavailable(err)
			}
		}
		workloads = append(workloads, workload)
	}
	return workloads, nil
}

func (s *Service) repairServingWorkload(ctx context.Context, workload servingWorkload) error {
	if len(workload.Instances) == 0 {
		return nil
	}
	active, err := s.environmentHasActiveOperation(ctx, s.entities.Operations(), workload.App, workload.Environment, "")
	if err != nil {
		return err
	}
	if active {
		return nil
	}
	observed, err := s.observeNodeWorkloads(ctx, workload.Node, workload.App, workload.Environment)
	if err != nil {
		// An unreachable node is not evidence that a container is missing.
		return fmt.Errorf("observe %s/%s on %s: %w", workload.App, workload.Environment, workload.Node, err)
	}
	missing := missingServingInstances(workload, observed.Containers)
	if len(missing) == 0 {
		return nil
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
	if err == nil {
		return nil
	}
	// A deploy accepted between observation and submission, a serving
	// deployment change, or an active delete refuses the repair. The next
	// maintenance run re-observes instead.
	if repairSubmissionIgnored(err) {
		return nil
	}
	return fmt.Errorf("queue repair for %s/%s: %w", workload.App, workload.Environment, err)
}

func repairSubmissionIgnored(err error) bool {
	switch contract.Classify(err).Code {
	case contract.Conflict, contract.PreconditionFailed:
		return true
	case contract.Usage, contract.InvalidOutputMode, contract.InputRequired, contract.ValidationFailed,
		contract.MissingTool, contract.NotFound, contract.Unauthenticated, contract.Forbidden,
		contract.DoctorFailed, contract.ConfirmationRequired, contract.NodeVersionMismatch,
		contract.RemoteUnreachable, contract.Unavailable, contract.Cancelled, contract.Internal:
		return false
	default:
		return false
	}
}

type activeOperation struct {
	Kind  string
	State contract.State
}

// environmentOperationLister is the typed store/tx surface shared by the
// submission guard and the maintenance guard.
type environmentOperationLister interface {
	ListActiveAppEnvironmentOperations(ctx context.Context, exclude contract.OperationID, environmentTarget, appTarget string) ([]entities.ActiveAppEnvironmentOperation, error)
}

func activeEnvironmentOperations(ctx context.Context, operations environmentOperationLister, app, environment string, exclude contract.OperationID) ([]activeOperation, error) {
	rows, err := operations.ListActiveAppEnvironmentOperations(ctx, exclude, "env/"+app+"/"+environment, "app/"+app)
	if err != nil {
		return nil, unavailable(err)
	}
	active := make([]activeOperation, 0, len(rows))
	for _, row := range rows {
		active = append(active, activeOperation{Kind: row.Kind, State: row.State})
	}
	return active, nil
}

func (s *Service) environmentHasActiveOperation(ctx context.Context, operations environmentOperationLister, app, environment string, exclude contract.OperationID) (bool, error) {
	active, err := activeEnvironmentOperations(ctx, operations, app, environment, exclude)
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
// rollback, restart, promotion or delete owns the environment and repair must
// not queue a competing restart.
func environmentOperationBlocksRepair(kind string, state contract.State) bool {
	if state.Terminal() {
		return false
	}
	switch kind {
	case platform.KindDeploy, platform.KindRollback, platform.KindRestart, platform.KindPromote,
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
