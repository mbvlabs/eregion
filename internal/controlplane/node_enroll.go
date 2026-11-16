package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/entities"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/processmetrics"
)

// ------------------------------------------------------------- acceptance

// wgAllocationLock serializes tunnel-address allocation across processes.
const wgAllocationLock int64 = 0x657267776761 // "ergwga"

func joinSorted(values []string) string {
	sorted := append([]string(nil), values...)
	sort.Strings(sorted)
	return strings.Join(sorted, ",")
}

func (s *Service) resolverConfig() ResolverConfig {
	if s.resolver != nil {
		return s.resolver.config
	}
	return ResolverConfig{}
}

// prepareNodeEnroll records an accepted enrollment in the same transaction as
// the operation and its River job. It writes only what the operator declared:
// roles, the SSH address, and the pinned public host key. Identity, probed
// capabilities, and the enrolled state are written by the worker after the
// node proved itself.
func (s *Service) prepareNodeEnroll(ctx context.Context, tx *entities.StoreTx, id contract.OperationID, in *platform.NodeEnrollInput) error {
	existing, found, err := loadOptionalNodeConn(ctx, tx.Nodes(), in.Node)
	if err != nil {
		return err
	}
	if in.Retry {
		return s.retryNodeEnroll(ctx, tx, id, in, existing, found)
	}
	state, wgAddress, err := enrollmentState(existing, found, in)
	if err != nil {
		return err
	}
	wgEndpoint, wgSSHPort, wgAddress, err := s.enrollmentNetwork(ctx, tx, in, wgAddress)
	if err != nil {
		return err
	}
	if _, err := tx.Nodes().UpsertEnrollment(ctx, entities.NodeEnrollment{
		Name: in.Node, Roles: in.Roles, Capabilities: []string{"unverified"}, Address: in.Address,
		State: state, User: in.User, HostKey: in.HostKey, HostKeyFingerprint: in.HostKeyFingerprint,
		IdentityFingerprint: in.IdentityFingerprint, BuildConcurrency: in.BuildConcurrency,
		BuildUser: in.BuildUser, WGEnabled: in.PrivateNetwork != nil, WGAddress: wgAddress,
		WGEndpoint: wgEndpoint, WGSSHPort: wgSSHPort, OperationID: id,
	}); err != nil {
		return platformWriteError(err, "node enrollment conflicts with another node")
	}
	return nil
}

func loadOptionalNodeConn(ctx context.Context, nodes *entities.NodeStore, name string) (entities.NodeConnection, bool, error) {
	existing, err := loadNodeConn(ctx, nodes, name)
	if err == nil {
		return existing, true, nil
	}
	if fault := contract.Classify(err); fault.Code != contract.NotFound {
		return existing, false, err
	}
	return existing, false, nil
}

func (s *Service) retryNodeEnroll(ctx context.Context, tx *entities.StoreTx, id contract.OperationID, in *platform.NodeEnrollInput, existing entities.NodeConnection, found bool) error {
	if !found {
		return contract.NewFault(contract.NotFound, "node not found", "Enroll the node first; there is no enrollment to retry.")
	}
	if existing.Transport != "ssh" {
		return contract.NewFault(contract.PreconditionFailed, "node was not submitted for enrollment", "Enroll the node with its address and pinned host key.")
	}
	if _, err := tx.Nodes().ResetEnrollment(ctx, in.Node, id); err != nil {
		return unavailable(err)
	}
	return nil
}

func enrollmentState(existing entities.NodeConnection, found bool, in *platform.NodeEnrollInput) (platform.EnrollmentState, string, error) {
	state := platform.EnrollmentPending
	if !found {
		return state, "", nil
	}
	if contains(existing.Roles, platform.RoleControlPlane) {
		return state, "", contract.NewFault(contract.Conflict, "the main node cannot be re-enrolled", "The control-plane node keeps its local transport; enroll additional nodes under other names.")
	}
	if existing.Enrollment == platform.EnrollmentEnrolled {
		if existing.HostKey != in.HostKey {
			return state, "", contract.NewFault(contract.Conflict, "node is enrolled with a different pinned host key",
				"A host key is never replaced by re-submission. Verify the node, then remove and re-enroll it under a new name.")
		}
		if existing.IdentityFP != "" && in.IdentityFingerprint != "" && existing.IdentityFP != in.IdentityFingerprint {
			return state, "", contract.NewFault(contract.Conflict, "node is enrolled with a different identity key", "The pinned identity key is not replaced by re-submission.")
		}
		unchanged := existing.SSHAddress == in.Address && existing.SSHUser == in.User &&
			joinSorted(existing.Roles) == joinSorted(in.Roles) && existing.BuildConcurrency == in.BuildConcurrency &&
			existing.BuildUser == in.BuildUser && existing.WGEnabled == (in.PrivateNetwork != nil) &&
			(in.PrivateNetwork == nil || (existing.WGEndpoint == in.PrivateNetwork.Endpoint && existing.WGSSHPort == in.PrivateNetwork.SSHPort))
		if unchanged {
			state = platform.EnrollmentEnrolled
		}
	}
	return state, existing.WGAddress, nil
}

func (s *Service) enrollmentNetwork(ctx context.Context, tx *entities.StoreTx, in *platform.NodeEnrollInput, wgAddress string) (string, int, string, error) {
	if in.PrivateNetwork == nil {
		return "", 0, wgAddress, nil
	}
	if wgAddress == "" {
		allocated, err := s.allocateWireGuardAddress(ctx, tx)
		if err != nil {
			return "", 0, "", err
		}
		wgAddress = allocated
	}
	return in.PrivateNetwork.Endpoint, in.PrivateNetwork.SSHPort, wgAddress, nil
}

// allocateWireGuardAddress returns the lowest free host address of the private
// subnet, skipping the control plane's own address.
func (s *Service) allocateWireGuardAddress(ctx context.Context, tx *entities.StoreTx) (string, error) {
	config := s.resolverConfig()
	prefix, err := netip.ParsePrefix(config.subnet())
	if err != nil || !prefix.Addr().Is4() {
		return "", contract.NewFault(contract.ValidationFailed, "the WireGuard subnet is invalid", "Set EREGION_NODE_WG_SUBNET to an IPv4 CIDR such as 10.77.0.0/24.")
	}
	control, err := netip.ParseAddr(config.controlAddress())
	if err != nil {
		return "", contract.NewFault(contract.ValidationFailed, "the control plane WireGuard address is invalid", "Set EREGION_NODE_WG_ADDRESS.")
	}
	if err := tx.Nodes().LockWireGuardAllocation(ctx, wgAllocationLock); err != nil {
		return "", unavailable(err)
	}
	addresses, err := tx.Nodes().WireGuardAddresses(ctx)
	if err != nil {
		return "", unavailable(err)
	}
	used := map[string]bool{control.String(): true}
	for _, address := range addresses {
		used[address] = true
	}
	prefix = prefix.Masked()
	for candidate := prefix.Addr().Next(); prefix.Contains(candidate); candidate = candidate.Next() {
		next := candidate.Next()
		if !prefix.Contains(next) {
			break // the broadcast address
		}
		if !used[candidate.String()] {
			return candidate.String(), nil
		}
	}
	return "", contract.NewFault(contract.Conflict, "the WireGuard subnet has no free address", "Use a larger EREGION_NODE_WG_SUBNET.")
}

// ------------------------------------------------------------ enrollment

type attestedHost struct {
	attestation node.Attestation
	fingerprint string
	probe       node.ProbeResult
}

// stepJSON runs one node step and decodes its recorded result.
func (w *platformWorker) stepJSON(ctx context.Context, tr node.Transport, id contract.OperationID, attempt uint32, stepID string, payload any, out any) error {
	raw, err := w.runStepOn(ctx, tr, id, attempt, stepID, payload, nil)
	if err != nil {
		return err
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return contract.NewFault(contract.PreconditionFailed, "the node returned an unreadable "+stepID+" result", "Inspect the node binary version; it must match the control plane.")
	}
	return nil
}

// verifyNode runs the identity challenge and the capability probe against a
// node and checks them against the pinned record. It is shared by enrollment
// and by node checks, so both apply exactly the same rules.
func (w *platformWorker) verifyNode(ctx context.Context, tr node.Transport, id contract.OperationID, attempt uint32, rec entities.NodeConnection) (attestedHost, error) {
	var host attestedHost
	nonce := node.IdentityNonce(string(id), rec.Name)
	if err := w.stepJSON(ctx, tr, id, attempt, "node-identity-01-v1", node.IdentityPayload{Step: "node-identity-01-v1", Node: rec.Name, Nonce: nonce}, &host.attestation); err != nil {
		return host, err
	}
	fingerprint, err := node.VerifyAttestation(host.attestation, rec.Name, nonce, rec.IdentityKey, rec.ExpectedIdentityFP)
	if err != nil {
		return host, placementViolation(ReasonIdentityMismatch, rec.Name, "node identity verification failed: "+err.Error(),
			"The host answered with a key that is not the node's pinned identity. Do not trust it; investigate the host or re-enroll under a new name.", nil)
	}
	host.fingerprint = fingerprint
	if err := w.service.checkCancelled(ctx, id); err != nil {
		return host, err
	}
	if err := w.stepJSON(ctx, tr, id, attempt, "node-probe-01-v1", node.ProbePayload{Step: "node-probe-01-v1"}, &host.probe); err != nil {
		return host, err
	}
	if !host.probe.Supported {
		return host, contract.NewFault(contract.PreconditionFailed, "unsupported node: "+host.probe.Unsupported,
			"Supported: Debian 13 and Ubuntu 26.04 on amd64 or arm64.")
	}
	var missing []string
	for _, capability := range platform.RequiredCapabilities(rec.Roles) {
		if !contains(host.probe.Capabilities, capability) {
			missing = append(missing, capability)
		}
	}
	if len(missing) > 0 {
		return host, placementViolation(ReasonCapabilityMissing, rec.Name, fmt.Sprintf("node %q cannot take its declared roles %s: the probe found no %s", rec.Name, strings.Join(rec.Roles, ","), strings.Join(missing, ",")),
			"Install the missing tools on the node, then retry.", map[string]any{"required_capability": strings.Join(missing, ",")})
	}
	return host, nil
}

func (w *platformWorker) runNodeEnroll(ctx context.Context, id contract.OperationID, attempt uint32, normalized platform.Normalized) error {
	input, err := typedOperationInput[*platform.NodeEnrollInput](normalized.Input)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	rec, err := loadNodeConn(ctx, w.service.entities.Nodes(), input.Node)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	if rec.Transport != "ssh" || rec.HostKey == "" {
		return w.settleEnroll(ctx, id, attempt, rec.Name, contract.NewFault(contract.PreconditionFailed, "node was not submitted for enrollment", "Enroll the node with its address and pinned host key."))
	}
	tr, closer, err := w.service.resolver.Direct(ctx, rec)
	if err != nil {
		return w.settleEnroll(ctx, id, attempt, rec.Name, err)
	}
	defer closer()
	host, err := w.verifyNode(ctx, tr, id, attempt, rec)
	if err != nil {
		return w.settleEnroll(ctx, id, attempt, rec.Name, err)
	}
	if err := w.service.checkCancelled(ctx, id); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	if err := w.stepJSON(ctx, tr, id, attempt, "node-hostcfg-01-v1", node.HostcfgPayload{
		Step: "node-hostcfg-01-v1", Roles: rec.Roles, BuildConcurrency: buildConcurrencyFor(rec), BuildUser: rec.BuildUser,
	}, nil); err != nil {
		return w.settleEnroll(ctx, id, attempt, rec.Name, err)
	}
	capabilities := host.probe.Capabilities
	if len(capabilities) == 0 {
		capabilities = []string{"none"}
	}
	probeJSON, _ := json.Marshal(host.probe)
	detail := fmt.Sprintf("enrolled: %s %s %s; capabilities %s", host.probe.OS.ID, host.probe.OS.VersionID, host.probe.Arch, strings.Join(capabilities, ","))
	if err := w.service.finishPlatformOperation(ctx, id, attempt, func(tx *entities.StoreTx, op *contract.Operation) error {
		if _, err := tx.Nodes().RecordEnrolled(ctx, entities.NodeEnrollmentResult{
			Name: rec.Name, IdentityPublicKey: host.attestation.PublicKey,
			IdentityFingerprint: host.fingerprint, Capabilities: capabilities,
			OSID: host.probe.OS.ID, OSVersion: host.probe.OS.VersionID, Arch: host.probe.Arch,
			Probe: probeJSON, Detail: detail,
		}); err != nil {
			return err
		}
		return appendEvent(ctx, tx, op, contract.Event{Kind: "log", Stage: "enrolled", Message: fmt.Sprintf("Node %s enrolled; identity %s, host key %s pinned.", rec.Name, host.fingerprint, hostKeyFingerprintOf(rec.HostKey))})
	}); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return w.settleEnroll(ctx, id, attempt, rec.Name, placementViolation(ReasonIdentityMismatch, rec.Name,
				"the node's identity key is already enrolled as another node", "A cloned machine must regenerate its identity key before it is enrolled under a new name.", nil))
		}
		return w.settle(ctx, id, attempt, err)
	}
	return nil
}

func hostKeyFingerprintOf(hostKey string) string {
	_, fingerprint, err := platform.ParseHostKey(hostKey)
	if err != nil {
		return "(unreadable)"
	}
	return fingerprint
}

func buildConcurrencyFor(rec entities.NodeConnection) int {
	if !contains(rec.Roles, platform.RoleBuilder) {
		return 0
	}
	if rec.BuildConcurrency < 1 {
		return 1
	}
	return rec.BuildConcurrency
}

// settleEnroll records a terminal enrollment failure on the node row, then
// settles the operation. A transient failure only records the last error: the
// enrollment stays pending and River retries it.
func (w *platformWorker) settleEnroll(ctx context.Context, id contract.OperationID, attempt uint32, nodeName string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, errPlatformCancelled) || errors.Is(err, errOperationTerminal) || errors.Is(err, errNotOwned) {
		return w.settle(ctx, id, attempt, err)
	}
	fault := contract.Classify(err)
	terminal := false
	switch fault.Code {
	case contract.ValidationFailed, contract.PreconditionFailed, contract.NotFound, contract.Forbidden, contract.Conflict:
		terminal = true
	case contract.Usage, contract.InvalidOutputMode, contract.InputRequired, contract.MissingTool,
		contract.Unauthenticated, contract.DoctorFailed, contract.ConfirmationRequired,
		contract.NodeVersionMismatch, contract.RemoteUnreachable, contract.Unavailable,
		contract.Cancelled, contract.Internal:
		terminal = false
	}
	message := truncateText(fault.Message, 1024)
	_, _ = w.service.entities.Nodes().RecordEnrollmentFailure(
		context.WithoutCancel(ctx), nodeName, message, checkStateFor(err), terminal,
	)
	return w.settle(ctx, id, attempt, err)
}

func checkStateFor(err error) string {
	if reason, ok := IsPlacementViolation(err); ok {
		return reason
	}
	if contract.Classify(err).Code == contract.Unavailable {
		return "unreachable"
	}
	return "failed"
}

func truncateText(text string, limit int) string {
	if len(text) > limit {
		return text[:limit]
	}
	return text
}

// ------------------------------------------------------------------ check

// runEnrolledCheck re-verifies an enrolled node end to end: the identity key
// must still be the pinned one, the host must still be supported, and the
// declared roles must still be backed by probed capabilities. The result and
// its time are recorded either way, so the CLI can show freshness honestly.
func (w *platformWorker) runEnrolledCheck(ctx context.Context, id contract.OperationID, attempt uint32, rec entities.NodeConnection) error {
	tr, err := w.service.resolver.TransportFor(ctx, rec.Name)
	if err != nil {
		w.service.recordNodeCheck(ctx, rec.Name, checkStateFor(err), contract.Classify(err).Message)
		return w.settle(ctx, id, attempt, err)
	}
	host, err := w.verifyNode(ctx, tr, id, attempt, rec)
	if err != nil {
		state := checkStateFor(err)
		w.service.recordNodeCheck(ctx, rec.Name, state, contract.Classify(err).Message)
		return w.settle(ctx, id, attempt, err)
	}
	capabilities := host.probe.Capabilities
	if len(capabilities) == 0 {
		capabilities = []string{"none"}
	}
	probeJSON, _ := json.Marshal(host.probe)
	if err := w.service.finishPlatformOperation(ctx, id, attempt, func(tx *entities.StoreTx, op *contract.Operation) error {
		if _, err := tx.Nodes().RecordCheckSuccess(ctx, entities.NodeCheckResult{
			Name: rec.Name, Capabilities: capabilities, OSID: host.probe.OS.ID,
			OSVersion: host.probe.OS.VersionID, Arch: host.probe.Arch, Probe: probeJSON,
			Detail: "identity " + host.fingerprint + " verified; capabilities " + strings.Join(capabilities, ","),
		}); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	return nil
}

// recordNodeCheck stores the outcome and time of an observation.
func (s *Service) recordNodeCheck(ctx context.Context, name, state, detail string) {
	_, _ = s.entities.Nodes().RecordCheckStatus(context.WithoutCancel(ctx),
		name, "unreachable", state, truncateText(detail, 1024))
}

// -------------------------------------------------------------- reconcile

// runNodeReconcile converges a node's role configuration and its WireGuard
// peers. Every step is an idempotent Check/Apply on the node; running it again
// changes nothing when the node already matches.
func (w *platformWorker) runNodeReconcile(ctx context.Context, id contract.OperationID, attempt uint32, normalized platform.Normalized) error {
	input, err := typedOperationInput[*platform.NodeReconcileInput](normalized.Input)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	rec, err := loadNodeConn(ctx, w.service.entities.Nodes(), input.Node)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	if rec.Enrollment != platform.EnrollmentEnrolled || rec.Transport != "ssh" {
		return w.settle(ctx, id, attempt, placementViolation(ReasonNotEnrolled, rec.Name, fmt.Sprintf("node %q is not enrolled", rec.Name), "Enroll the node before reconciling it.", nil))
	}
	var tr node.Transport
	if input.Via == "direct" {
		direct, closer, err := w.service.resolver.Direct(ctx, rec)
		if err != nil {
			return w.settle(ctx, id, attempt, err)
		}
		defer closer()
		tr = direct
	} else if tr, err = w.service.resolver.TransportFor(ctx, rec.Name); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	if err := w.stepJSON(ctx, tr, id, attempt, "node-hostcfg-01-v1", node.HostcfgPayload{
		Step: "node-hostcfg-01-v1", Roles: rec.Roles, BuildConcurrency: buildConcurrencyFor(rec), BuildUser: rec.BuildUser,
	}, nil); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	if !rec.WGEnabled {
		return w.finishReconcile(ctx, id, attempt, rec.Name, "Role configuration converged; the node has no private network.", nil)
	}
	if err := w.service.checkCancelled(ctx, id); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	var key node.WireGuardResult
	if err := w.stepJSON(ctx, tr, id, attempt, "node-wireguard-01-v1", node.WireGuardPayload{Step: "node-wireguard-01-v1", Action: "keygen"}, &key); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	controlPublic, err := w.service.resolver.ControlPublicKey(rec.Name)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	// Persist the public keys first. They are public values; the node's private
	// key stays in its private state directory and the control plane's stays in
	// its protected key file.
	if _, err := w.service.entities.Nodes().SetWireGuardKeys(ctx, rec.Name, key.PublicKey, controlPublic); err != nil {
		return w.settle(ctx, id, attempt, unavailable(err))
	}
	peers, err := w.service.wireGuardPeers(ctx, rec, controlPublic)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	prefix, _ := netip.ParsePrefix(w.service.resolverConfig().subnet())
	payload := node.WireGuardPayload{
		Step: "node-wireguard-02-v1", Action: "apply",
		Address:    fmt.Sprintf("%s/%d", rec.WGAddress, prefix.Bits()),
		ListenPort: wireGuardListenPort, // the endpoint may be a NAT or published port; the node always listens here
		Peers:      peers,
	}
	var applied node.WireGuardResult
	if err := w.stepJSON(ctx, tr, id, attempt, "node-wireguard-02-v1", payload, &applied); err != nil {
		_, _ = w.service.entities.Nodes().SetWireGuardFailed(context.WithoutCancel(ctx), rec.Name)
		return w.settle(ctx, id, attempt, err)
	}
	state := platform.WireGuardRender
	if applied.State == "applied" {
		state = platform.WireGuardApplied
	}
	message := fmt.Sprintf("WireGuard %s with %d peers (config %s).", state, applied.PeerCount, shortHash(applied.ConfigHash))
	if state == platform.WireGuardRender {
		message += " The node has no privileged apply helper configured, so the peer configuration is rendered but not loaded."
	}
	return w.finishReconcile(ctx, id, attempt, rec.Name, message, &state)
}

func shortHash(hash string) string {
	if len(hash) > 12 {
		return hash[:12]
	}
	return hash
}

func (w *platformWorker) finishReconcile(ctx context.Context, id contract.OperationID, attempt uint32, nodeName, message string, state *platform.WireGuardState) error {
	if err := w.service.finishPlatformOperation(ctx, id, attempt, func(tx *entities.StoreTx, op *contract.Operation) error {
		if state != nil {
			if _, err := tx.Nodes().SetWireGuardState(ctx, nodeName, *state); err != nil {
				return err
			}
		}
		return appendEvent(ctx, tx, op, contract.Event{Kind: "log", Stage: "reconcile", Message: message})
	}); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	return nil
}

// wireGuardPeers is the desired peer set of one node: the control plane (which
// dials in, so it needs no endpoint) and every other enrolled node that has a
// key and a public endpoint (a full mesh among nodes). Sorted by key, so the
// rendered configuration is deterministic.
func (s *Service) wireGuardPeers(ctx context.Context, rec entities.NodeConnection, controlPublic string) ([]node.WireGuardPeer, error) {
	control, err := netip.ParseAddr(s.resolverConfig().controlAddress())
	if err != nil {
		return nil, contract.NewFault(contract.ValidationFailed, "the control plane WireGuard address is invalid", "Set EREGION_NODE_WG_ADDRESS.")
	}
	peers := []node.WireGuardPeer{{PublicKey: controlPublic, AllowedIPs: []string{control.String() + "/32"}}}
	records, err := s.entities.Nodes().WireGuardPeers(ctx, rec.Name)
	if err != nil {
		return nil, unavailable(err)
	}
	for _, peer := range records {
		peers = append(peers, node.WireGuardPeer{
			PublicKey: peer.PublicKey, AllowedIPs: []string{peer.Address + "/32"},
			Endpoint: peer.Endpoint, Keepalive: 25,
		})
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].PublicKey < peers[j].PublicKey })
	return peers, nil
}

// ----------------------------------------------------------- build slots

// acquireBuildSlot serializes each admission with capacity changes on the node
// row and counts occupied locks across the entire supported keyspace. Only a
// session advisory lease remains after the short admission transaction.
func (s *Service) acquireBuildSlot(ctx context.Context, nodeName string, onWait func()) (func(), error) {
	waited := false
	endWait := func() {}
	defer func() { endWait() }()
	deadline := time.Now().Add(buildSlotWait)
	for {
		probe, err := s.entities.Nodes().ProbeBuildSlots(ctx, nodeName, platform.MaxBuildConcurrency)
		if err != nil {
			return nil, buildSlotProbeError(ctx, err)
		}
		if err := validateBuildSlotProbe(probe); err != nil {
			return nil, err
		}
		if release := buildSlotRelease(probe); release != nil {
			return release, nil
		}
		if !waited {
			waited = true
			endWait = processmetrics.BeginBuildCapacityWait(ctx)
			if onWait != nil {
				onWait()
			}
		}
		if time.Now().After(deadline) {
			return nil, buildSlotCapacityFault(nodeName, probe.Limit)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(buildSlotPoll):
		}
	}
}

func buildSlotProbeError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, entities.ErrPlatformPointerIntegrity) {
		return contract.NewFault(contract.PreconditionFailed, "build node is no longer registered", "Register the node before building.")
	}
	if errors.Is(err, entities.ErrPlatformLeaseUncertain) {
		return contract.NewFault(contract.Unavailable, "build slot probe cleanup is uncertain", "Retry when the control database is available.")
	}
	return unavailable(err)
}

func validateBuildSlotProbe(probe entities.BuildSlotProbe) error {
	if probe.Limit < 1 || probe.Limit > platform.MaxBuildConcurrency {
		return contract.NewFault(contract.PreconditionFailed, "build node concurrency is invalid", "Correct the node's build concurrency before building.")
	}
	return nil
}

func buildSlotRelease(probe entities.BuildSlotProbe) func() {
	if probe.Lease == nil {
		return nil
	}
	release := probe.Lease
	var once sync.Once
	return func() {
		once.Do(func() {
			_ = release.Release()
		})
	}
}

func buildSlotCapacityFault(nodeName string, limit int) error {
	return placementViolation(ReasonBuildCapacity, nodeName, fmt.Sprintf("node %q has all %d build slots busy", nodeName, limit),
		"Wait for running builds to finish, or raise the node's build concurrency.", map[string]any{"build_concurrency": limit})
}

// buildSlotWait bounds how long a build waits for a free slot; buildSlotPoll is
// how often it looks. Tests shorten them.
var (
	buildSlotWait = 10 * time.Minute
	buildSlotPoll = time.Second
)
