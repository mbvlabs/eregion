package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

// rowQuerier is satisfied by a pool and by a transaction.
type rowQuerier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// nodeSelect reads the public node record. It never selects a private value.
const nodeSelect = `SELECT name,roles,capabilities,address,status,last_seen_at,created_at,updated_at,
    enrollment_state,enrollment_error,transport,ssh_user,ssh_host_key_fingerprint,identity_fingerprint,
    os_id,os_version,arch,build_concurrency,wg_enabled,wg_address,wg_endpoint,wg_public_key,wg_state,
    wg_reconciled_at,enrolled_at,last_check_at,last_check_state,last_check_detail
    FROM eregion_platform_nodes`

func scanNode(row pgx.Row) (platform.Node, error) {
	var n platform.Node
	var osID, osVersion string
	var wg platform.NodeWireGuard
	var wgState string
	err := row.Scan(&n.Name, &n.Roles, &n.Capabilities, &n.Address, &n.Status, &n.LastSeenAt, &n.CreatedAt, &n.UpdatedAt,
		&n.EnrollmentState, &n.EnrollmentError, &n.Transport, &n.SSHUser, &n.HostKeyFingerprint, &n.IdentityFingerprint,
		&osID, &osVersion, &n.Arch, &n.BuildConcurrency, &wg.Enabled, &wg.Address, &wg.Endpoint, &wg.PublicKey, &wgState,
		&wg.ReconciledAt, &n.EnrolledAt, &n.LastCheckAt, &n.LastCheckState, &n.LastCheckDetail)
	if err != nil {
		return n, err
	}
	if osID != "" {
		n.OS = osID + " " + osVersion
	}
	wg.State = platform.WireGuardState(wgState)
	if wg.Enabled || wg.State != platform.WireGuardNone {
		n.WireGuard = &wg
	}
	return n, nil
}

func loadNode(ctx context.Context, q rowQuerier, name string) (platform.Node, error) {
	n, err := scanNode(q.QueryRow(ctx, nodeSelect+` WHERE name=$1`, name))
	if errors.Is(err, pgx.ErrNoRows) {
		if alternate, ok := alternateLocalNodeName(name); ok {
			n, err = scanNode(q.QueryRow(ctx, nodeSelect+` WHERE name=$1`, alternate))
		}
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return n, contract.NewFault(contract.NotFound, "node not found", "Register or enroll the node before placing work on it.")
	}
	if err != nil {
		return n, unavailable(err)
	}
	return n, nil
}

// localNodeNames are the two accepted names for the in-process local node.
// "local" is the original development identity; "main" is the documented main
// node name. Either may name the same local transport.
var localNodeNames = map[string]bool{"local": true, "main": true}

func isLocalNodeName(name string) bool { return localNodeNames[strings.TrimSpace(name)] }

func alternateLocalNodeName(name string) (string, bool) {
	switch strings.TrimSpace(name) {
	case "local":
		return "main", true
	case "main":
		return "local", true
	default:
		return "", false
	}
}

// localNodeNamesMatch reports whether an authenticated local identity may serve
// a node name. A remote identity never matches through this alias.
func localNodeNamesMatch(identity, name string) bool {
	identity, name = strings.TrimSpace(identity), strings.TrimSpace(name)
	return identity == name || (isLocalNodeName(identity) && isLocalNodeName(name))
}

// nodeConn is the private connection record of a node: everything the control
// plane needs to authenticate and reach it. It holds public material only.
type nodeConn struct {
	Name               string
	Roles              []string
	Enrollment         platform.EnrollmentState
	Transport          string
	SSHAddress         string
	SSHUser            string
	HostKey            string
	ExpectedIdentityFP string
	IdentityKey        string
	IdentityFP         string
	BuildConcurrency   int
	BuildUser          string
	WGEnabled          bool
	WGAddress          string
	WGEndpoint         string
	WGSSHPort          int
	WGPublicKey        string
	ControlWGPublicKey string
	WGState            platform.WireGuardState
}

func loadNodeConn(ctx context.Context, q rowQuerier, name string) (nodeConn, error) {
	rec, err := scanNodeConn(ctx, q, name)
	if errors.Is(err, pgx.ErrNoRows) {
		if alternate, ok := alternateLocalNodeName(name); ok {
			rec, err = scanNodeConn(ctx, q, alternate)
		}
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return rec, contract.NewFault(contract.NotFound, "node not found", "Register or enroll the node before placing work on it.")
	}
	if err != nil {
		return rec, unavailable(err)
	}
	return rec, nil
}

func scanNodeConn(ctx context.Context, q rowQuerier, name string) (nodeConn, error) {
	var rec nodeConn
	var wgState string
	err := q.QueryRow(ctx, `SELECT name,roles,enrollment_state,transport,ssh_address,ssh_user,ssh_host_key,
        expected_identity_fingerprint,identity_public_key,identity_fingerprint,build_concurrency,build_user,
        wg_enabled,wg_address,wg_endpoint,wg_ssh_port,wg_public_key,control_wg_public_key,wg_state
        FROM eregion_platform_nodes WHERE name=$1`, name).
		Scan(&rec.Name, &rec.Roles, &rec.Enrollment, &rec.Transport, &rec.SSHAddress, &rec.SSHUser, &rec.HostKey,
			&rec.ExpectedIdentityFP, &rec.IdentityKey, &rec.IdentityFP, &rec.BuildConcurrency, &rec.BuildUser,
			&rec.WGEnabled, &rec.WGAddress, &rec.WGEndpoint, &rec.WGSSHPort, &rec.WGPublicKey, &rec.ControlWGPublicKey, &wgState)
	if err != nil {
		return rec, err
	}
	rec.WGState = platform.WireGuardState(wgState)
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
func placementViolation(reason, nodeName, message, hint string, extra map[string]any) *contract.Fault {
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
	var fault *contract.Fault
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

// checkRegistryCredential refuses a registry credential whose scope the node's
// roles do not justify: only a builder may hold push credentials, and a node
// without the builder role is limited to pull.
func checkRegistryCredential(n platform.Node, scope platform.RegistryScope) error {
	if scope != platform.RegistryPush && scope != platform.RegistryPull {
		return placementViolation(ReasonRegistryScope, n.Name, "unknown registry credential scope", "Use a push or pull scope.", nil)
	}
	if scope == platform.RegistryPush && !contains(n.Roles, platform.RoleBuilder) {
		return placementViolation(ReasonRegistryScope, n.Name, fmt.Sprintf("node %q has no builder role; it may only hold pull-scoped registry credentials", n.Name),
			"Push credentials are only issued to builder nodes.", map[string]any{"scope": string(scope)})
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
	if !localNodeNamesMatch(identity, n.Name) {
		return placementViolation(ReasonIdentityMismatch, n.Name, fmt.Sprintf("this worker's authenticated transport identifies %q, not %q", identity, n.Name),
			"Enroll the node so the control plane reaches it over its own pinned SSH transport. Hostnames are not authorization.", map[string]any{"transport_identity": identity})
	}
	return nil
}
