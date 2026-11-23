package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/riverqueue/river"
)

func claimBuildCapacityTx(ctx context.Context, tx *entities.StoreTx, id contract.OperationID, attempt uint32, node, identity string) (bool, error) {
	state, found, err := tx.Nodes().LockBuildConcurrency(ctx, node)
	if err != nil {
		return false, unavailable(err)
	}
	if !found {
		return false, contract.NewFault(contract.PreconditionFailed, "build node is no longer registered", "Register the node before building.")
	}
	if state.BuildConcurrency < 1 || state.BuildConcurrency > platform.MaxBuildConcurrency {
		return false, contract.NewFault(contract.PreconditionFailed, "build node concurrency is invalid", "Correct the node's build concurrency before building.")
	}
	rec, found, err := tx.Nodes().Connection(ctx, node)
	if err != nil || !found {
		return false, unavailable(err)
	}
	current := buildNodeIdentity(rec)
	if identity == "" || strings.HasSuffix(identity, ":unverified") {
		return false, contract.NewFault(contract.PreconditionFailed, "builder identity is unverified", "Configure a verified builder transport before building.")
	}
	if !strings.HasPrefix(identity, current+":") {
		return false, contract.NewFault(contract.PreconditionFailed, "build node identity changed", "Inspect the original builder before retrying.")
	}
	acquired, err := tx.BuildCapacity().Claim(ctx, id, node, identity, attempt, state.BuildConcurrency)
	if err != nil {
		return false, unavailable(err)
	}
	return acquired, nil
}

func (s *Service) adoptBuildCapacity(ctx context.Context, id contract.OperationID, attempt uint32, node, identity string) error {
	_, owned, err := s.mutateOwned(ctx, id, attempt, func(tx *entities.StoreTx, _ *contract.Operation) error {
		if _, found, err := tx.Nodes().LockBuildConcurrency(ctx, node); err != nil || !found {
			return unavailable(err)
		}
		rec, found, err := tx.Nodes().Connection(ctx, node)
		if err != nil || !found {
			return unavailable(err)
		}
		current := buildNodeIdentity(rec)
		if identity == "" {
			identity = current + ":unverified"
		}
		if !strings.HasPrefix(identity, current+":") {
			return unavailable()
		}
		if err := tx.BuildCapacity().AdoptExisting(ctx, id, node, identity, attempt); err != nil {
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

// buildTransportFor binds a local fallback to the already verified physical
// registry-host identity. Name-only fallback transports remain unverified and
// cannot reserve capacity for a new build.
func (s *Service) buildTransportFor(ctx context.Context, name string) (node.Transport, string, error) {
	tr, identity, err := s.resolver.transportForBuild(ctx, name)
	if err != nil {
		return nil, "", err
	}
	if local, ok := tr.(*node.Local); ok && s.registryHostIdentity != nil &&
		local.NodeIdentity() == name && s.registryHostIdentity.NodeName == name &&
		local.StateDir() == s.registryHostIdentity.StateDir {
		binding := s.registryHostIdentity
		hash := sha256.Sum256([]byte(strings.Join([]string{
			binding.NodeName, binding.HostKeyType, binding.HostKeyFingerprint, binding.StateDir,
		}, "\x00")))
		identity = strings.TrimSuffix(identity, ":unverified") + ":local:" + hex.EncodeToString(hash[:])
	}
	return tr, identity, nil
}

// beginBuildDispatch commits the reservation and the fenced dispatch intent in
// the same transaction. There is no transaction across the node call.
func (s *Service) beginBuildDispatch(ctx context.Context, id contract.OperationID, attempt uint32, node, identity, inputsHash, executionID string, buildID platform.BuildID, releaseID platform.ReleaseID) (bool, error) { //nolint:gocognit // Placement, capacity and intent must commit together under the operation fence.
	already := false
	_, owned, err := s.mutateOwned(ctx, id, attempt, func(tx *entities.StoreTx, op *contract.Operation) error {
		// Cancellation and Pack intent serialize on this operation row. A
		// cancellation accepted after the caller's last check wins here.
		if op.State == contract.Cancelling {
			return errPlatformCancelled
		}
		if err := guardBuildTargetDispatch(ctx, tx, op); err != nil {
			return err
		}
		placement, placed, err := tx.BuildPlacement(ctx, id)
		if err != nil {
			return unavailable(err)
		}
		records, err := tx.Steps().List(ctx, id)
		if err != nil {
			return unavailable(err)
		}
		if ambiguousPackHistory(records, placed) || (placed && (placement.Node != node || placement.Identity != identity)) {
			return unknownBuildPlacement()
		}
		completed, err := completedBuildDispatchTx(ctx, tx, id, inputsHash)
		if err != nil {
			return err
		}
		if completed {
			already = true
			return nil
		}
		acquired, err := claimBuildCapacityTx(ctx, tx, id, attempt, node, identity)
		if err != nil {
			return err
		}
		if !acquired {
			return river.JobSnooze(deploymentOwnerDefer)
		}
		if !placed {
			if err := tx.InsertBuildPlacement(ctx, id, entities.PackPlacement{Node: node, Identity: identity}); err != nil {
				return unavailable(err)
			}
		}
		if buildID != "" {
			if err := markBuildDispatchRunningTx(ctx, tx, id, buildID, releaseID); err != nil {
				return err
			}
		}
		already, err = beginStepTx(ctx, tx, op, attempt, deploymentPackStep, inputsHash, executionID)
		return err
	})
	if err != nil {
		return false, err
	}
	if !owned {
		return false, errNotOwned
	}
	return already, nil
}

func unknownBuildPlacement() error {
	return contract.NewFault(contract.PreconditionFailed, "original build placement is unknown or changed",
		"Inspect the original builder and its Pack effect; do not guess from the current environment.")
}

func ambiguousPackHistory(records []entities.StepRecord, placed bool) bool {
	for _, record := range records {
		if strings.HasPrefix(record.Step, "platform-pack-") && (!placed || record.Step != deploymentPackStep) {
			return true
		}
	}
	return false
}

func completedBuildDispatchTx(ctx context.Context, tx *entities.StoreTx, id contract.OperationID, inputsHash string) (bool, error) {
	record, found, err := tx.Steps().GetForUpdate(ctx, id, deploymentPackStep)
	if err != nil {
		return false, unavailable(err)
	}
	if !found || record.State != "completed" {
		return false, nil
	}
	if record.InputsHash != inputsHash {
		return false, contract.NewFault(contract.PreconditionFailed, "step inputs changed for an existing effect", "Never reuse an operation ID with different inputs; inspect the node journal.")
	}
	return true, nil
}

func markBuildDispatchRunningTx(ctx context.Context, tx *entities.StoreTx, id contract.OperationID, buildID platform.BuildID, releaseID platform.ReleaseID) error {
	updated, err := tx.Platform().Builds().MarkRunningForOperation(ctx, buildID, id, releaseID)
	if err != nil {
		return unavailable(err)
	}
	if updated {
		return nil
	}
	build, found, err := tx.Platform().Builds().GetByID(ctx, buildID)
	if err != nil {
		return unavailable(err)
	}
	if found && build.OperationID == id && build.ReleaseID == releaseID && build.State == platform.BuildCancelled {
		return errPlatformCancelled
	}
	return contract.NewFault(contract.PreconditionFailed, "build dispatch ownership or state changed",
		"Inspect the original build and release before dispatching Pack.")
}

// releaseBuildCapacity is called only after the pack effect has completed and
// been acknowledged. An older attempt cannot release a successor's claim.
func (s *Service) releaseBuildCapacity(ctx context.Context, id contract.OperationID, attempt uint32, node string) error {
	_, owned, err := s.mutateOwned(ctx, id, attempt, func(tx *entities.StoreTx, _ *contract.Operation) error {
		_, err := tx.BuildCapacity().Release(ctx, id, node, attempt)
		if err != nil {
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

// reconcileTerminalBuildCapacity visits a bounded, rotating batch. Retirement
// takes place on the builder recorded at admission, never on the current
// environment placement, and no database transaction spans the node call.
func (s *Service) reconcileTerminalBuildCapacity(ctx context.Context) error {
	claims, err := s.entities.BuildCapacity().TerminalClaims(ctx, 8)
	if err != nil {
		return unavailable(err)
	}
	for _, claim := range claims {
		touched, err := s.entities.BuildCapacity().TouchTerminal(ctx, claim)
		if err != nil {
			return unavailable(err)
		}
		if !touched {
			continue
		}
		if err := s.retireTerminalBuildClaim(ctx, claim); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	return nil
}

func (s *Service) retireTerminalBuildClaim(ctx context.Context, claim entities.BuildCapacityClaim) error {
	if reason := s.retireBuildOnPinnedNode(ctx, claim); reason != "" {
		return nil
	}
	if _, err := s.entities.BuildCapacity().DeleteTerminal(ctx, claim); err != nil {
		return unavailable(fmt.Errorf("delete retired build capacity claim: %w", err))
	}
	return nil
}

// An empty reason means the pinned node confirmed retirement. A nonempty
// reason is deliberately conservative and never implies the build is stopped.
func (s *Service) retireBuildOnPinnedNode(ctx context.Context, claim entities.BuildCapacityClaim) string {
	if s.resolver == nil || claim.Identity == "" || strings.HasSuffix(claim.Identity, ":unverified") {
		return "builder identity is unverified"
	}
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	tr, identity, err := s.buildTransportFor(callCtx, claim.Node)
	if err != nil || identity != claim.Identity {
		return "pinned builder identity could not be verified"
	}
	exec := engine.Execution{
		OperationID: claim.OperationID,
		Attempt:     claim.Attempt,
		Step:        deploymentPackStep,
		RequestID:   newRequestID(),
	}
	response, err := tr.Call(callCtx, node.Request{Method: node.MethodRetireBuild, RequestID: exec.RequestID, Execution: exec}, nil)
	if err != nil || response.Status != engine.StatusCompleted {
		return "builder did not confirm retirement"
	}
	return ""
}
