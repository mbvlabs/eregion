package controlplane

import (
	"context"
	"errors"
	"fmt"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/platform"
)

func loadNode(ctx context.Context, nodes *entities.NodeStore, name string) (platform.Node, error) {
	n, found, err := nodes.Get(ctx, name)
	if err != nil {
		return n, unavailable(err)
	}
	if !found {
		return n, contract.NewFault(contract.NotFound, "node not found", "Register or enroll the node before placing work on it.")
	}
	return n, nil
}

// loadNodeConn reads the private connection facts through the typed node view.
// It holds public material only.
func loadNodeConn(ctx context.Context, nodes *entities.NodeStore, name string) (entities.NodeConnection, error) {
	rec, found, err := nodes.Connection(ctx, name)
	if err != nil {
		return rec, unavailable(err)
	}
	if !found {
		return rec, contract.NewFault(contract.NotFound, "node not found", "Register or enroll the node before placing work on it.")
	}
	return rec, nil
}

// ---------------------------------------------------------------- placement

// Placement violation reasons. They are stable machine-readable values in the
// fault details and in the operation event.
const (
	ReasonNotEnrolled        = "not_enrolled"
	ReasonRoleMissing        = "role_missing"
	ReasonCapabilityMissing  = "capability_missing"
	ReasonBuilderRequired    = "builder_required"
	ReasonIdentityMismatch   = "identity_mismatch"
	ReasonTransportUnpinned  = "transport_unpinned"
	ReasonRegistryScope      = "registry_scope"
	ReasonBuildCapacity      = "build_capacity"
	placementViolationPrefix = "placement_violation: "
)

// placementViolation is the structured refusal for work outside a node's
// declared placement or capability constraints. The code is
// precondition_failed; the reason, node, and requirement are in the details.
func placementViolation(reason, nodeName, message, hint string, extra map[string]any) *contract.FaultError {
	fault := contract.NewFault(contract.PreconditionFailed, placementViolationPrefix+message, hint)
	fault.Details = map[string]any{"placement": true, "reason": reason, "node": nodeName}
	for key, value := range extra {
		fault.Details[key] = value
	}
	return fault
}

// IsPlacementViolation reports whether err is a structured placement refusal
// and returns its reason.
func IsPlacementViolation(err error) (string, bool) {
	var fault *contract.FaultError
	if !errors.As(err, &fault) || fault == nil || fault.Details == nil {
		return "", false
	}
	if placement, _ := fault.Details["placement"].(bool); !placement {
		return "", false
	}
	reason, _ := fault.Details["reason"].(string)
	return reason, true
}

// nodePlacement is the pure placement rule: a node receives work only when it
// is usable, holds the role the work needs, and has proven the capability.
// Roles are declared by the operator; capabilities are what a probe found.
func nodePlacement(n platform.Node, role, capability string) error {
	switch n.EnrollmentState {
	case platform.EnrollmentPending, platform.EnrollmentFailed:
		return placementViolation(ReasonNotEnrolled, n.Name, fmt.Sprintf("node %q is not enrolled (%s)", n.Name, n.EnrollmentState),
			"Finish node enrollment, then submit the work again.", nil)
	case platform.EnrollmentRegistered, platform.EnrollmentEnrolled:
		// Enrolled nodes continue through the placement checks below;
		// registered nodes are still becoming usable.
	}
	if n.EnrollmentState == platform.EnrollmentEnrolled && n.LastCheckState == ReasonIdentityMismatch {
		// The last authenticated check found a different identity key on this
		// host. It is quarantined until a check verifies the pinned identity again.
		return placementViolation(ReasonIdentityMismatch, n.Name, fmt.Sprintf("node %q failed its last identity check and is quarantined", n.Name),
			"Investigate the host; the node receives work again only after a node check verifies its pinned identity.", nil)
	}
	if !contains(n.Roles, role) {
		if role == platform.RoleBuilder && contains(n.Roles, platform.RoleApplication) {
			return placementViolation(ReasonBuilderRequired, n.Name, fmt.Sprintf("node %q only has the application role; builds never run on an application-only node", n.Name),
				"Enroll a node with the builder role, or use an image source.", map[string]any{"required_role": role})
		}
		return placementViolation(ReasonRoleMissing, n.Name, fmt.Sprintf("node %q lacks the %s role", n.Name, role),
			"Enroll the node with the "+role+" role.", map[string]any{"required_role": role})
	}
	if capability != "" && !contains(n.Capabilities, capability) {
		return placementViolation(ReasonCapabilityMissing, n.Name, fmt.Sprintf("node %q lacks the %s capability", n.Name, capability),
			"Install or enable "+capability+" on the node, then re-run the node check.", map[string]any{"required_capability": capability})
	}
	return nil
}

// checkPlacement enforces declared roles/capabilities and the transport's
// authenticated node identity. A hostname is never authorization.
func (s *Service) checkPlacement(n platform.Node, role, capability string) error {
	if err := nodePlacement(n, role, capability); err != nil {
		return err
	}
	if n.EnrollmentState == platform.EnrollmentEnrolled {
		// The worker authenticates an enrolled node at dispatch: the SSH host
		// key is pinned and the handshake must report the enrolled name.
		return nil
	}
	return s.checkFallbackIdentity(n)
}

// checkFallbackIdentity applies to a node that was registered, not enrolled:
// it can only be served by the worker's own configured transport, and that
// transport must authenticate as exactly this node.
func (s *Service) checkFallbackIdentity(n platform.Node) error {
	identity, known := s.transportIdentity()
	if !known {
		if s.transport != nil {
			return placementViolation(ReasonTransportUnpinned, n.Name, "node transport identity is not pinned", "Configure the transport's expected node name so placement is verified, not inferred from a hostname.", nil)
		}
		return nil // Insert-only API process: the worker rechecks before dispatch.
	}
	if identity != n.Name {
		return placementViolation(ReasonIdentityMismatch, n.Name, fmt.Sprintf("this worker's authenticated transport identifies %q, not %q", identity, n.Name),
			"Enroll the node so the control plane reaches it over its own pinned SSH transport. Hostnames are not authorization.", map[string]any{"transport_identity": identity})
	}
	return nil
}
