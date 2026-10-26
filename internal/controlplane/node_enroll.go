package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/platform"
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
func (s *Service) prepareNodeEnroll(ctx context.Context, tx pgx.Tx, id contract.OperationID, in *platform.NodeEnrollInput) error {
	existing, err := loadNodeConn(ctx, tx, in.Node)
	found := err == nil
	if err != nil {
		if fault := contract.Classify(err); fault.Code != contract.NotFound {
			return err
		}
	}
	if in.Retry {
		if !found {
			return contract.NewFault(contract.NotFound, "node not found", "Enroll the node first; there is no enrollment to retry.")
		}
		if existing.Transport != "ssh" {
			return contract.NewFault(contract.PreconditionFailed, "node was not submitted for enrollment", "Enroll the node with its address and pinned host key.")
		}
		_, err := tx.Exec(ctx, `UPDATE eregion_platform_nodes
            SET enrollment_state=CASE WHEN enrollment_state='enrolled' THEN 'enrolled' ELSE 'pending' END,
                enrollment_error='', enroll_operation_id=$2, updated_at=clock_timestamp() WHERE name=$1`, in.Node, string(id))
		if err != nil {
			return unavailable(err)
		}
		return nil
	}
	state := platform.EnrollmentPending
	capabilities := []string{"unverified"}
	wgAddress := ""
	if found {
		if contains(existing.Roles, platform.RoleControlPlane) {
			return contract.NewFault(contract.Conflict, "the main node cannot be re-enrolled", "The control-plane node keeps its local transport; enroll additional nodes under other names.")
		}
		if existing.Enrollment == platform.EnrollmentEnrolled {
			if existing.HostKey != in.HostKey {
				return contract.NewFault(contract.Conflict, "node is enrolled with a different pinned host key",
					"A host key is never replaced by re-submission. Verify the node, then remove and re-enroll it under a new name.")
			}
			if existing.IdentityFP != "" && in.IdentityFingerprint != "" && existing.IdentityFP != in.IdentityFingerprint {
				return contract.NewFault(contract.Conflict, "node is enrolled with a different identity key", "The pinned identity key is not replaced by re-submission.")
			}
			unchanged := existing.SSHAddress == in.Address && existing.SSHUser == in.User &&
				joinSorted(existing.Roles) == joinSorted(in.Roles) && existing.BuildConcurrency == in.BuildConcurrency &&
				existing.BuildUser == in.BuildUser && existing.WGEnabled == (in.PrivateNetwork != nil) &&
				(in.PrivateNetwork == nil || (existing.WGEndpoint == in.PrivateNetwork.Endpoint && existing.WGSSHPort == in.PrivateNetwork.SSHPort))
			if unchanged {
				state = platform.EnrollmentEnrolled
			}
		}
		wgAddress = existing.WGAddress
	}
	wgEndpoint, wgSSHPort := "", 0
	if in.PrivateNetwork != nil {
		wgEndpoint, wgSSHPort = in.PrivateNetwork.Endpoint, in.PrivateNetwork.SSHPort
		if wgAddress == "" {
			if wgAddress, err = s.allocateWireGuardAddress(ctx, tx); err != nil {
				return err
			}
		}
	}
	_, err = tx.Exec(ctx, `INSERT INTO eregion_platform_nodes
        (name,roles,capabilities,address,status,enrollment_state,transport,ssh_address,ssh_user,ssh_host_key,
         ssh_host_key_fingerprint,expected_identity_fingerprint,build_concurrency,build_user,wg_enabled,wg_address,wg_endpoint,wg_ssh_port,enroll_operation_id)
        VALUES($1,$2,$3,$4,'registered',$5,'ssh',$4,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
        ON CONFLICT (name) DO UPDATE SET roles=EXCLUDED.roles, address=EXCLUDED.address, enrollment_state=EXCLUDED.enrollment_state,
            enrollment_error='', transport='ssh', ssh_address=EXCLUDED.ssh_address, ssh_user=EXCLUDED.ssh_user,
            ssh_host_key=EXCLUDED.ssh_host_key, ssh_host_key_fingerprint=EXCLUDED.ssh_host_key_fingerprint,
            expected_identity_fingerprint=CASE WHEN EXCLUDED.expected_identity_fingerprint<>'' THEN EXCLUDED.expected_identity_fingerprint ELSE eregion_platform_nodes.expected_identity_fingerprint END,
            build_concurrency=EXCLUDED.build_concurrency, build_user=EXCLUDED.build_user, wg_enabled=EXCLUDED.wg_enabled,
            wg_address=EXCLUDED.wg_address, wg_endpoint=EXCLUDED.wg_endpoint, wg_ssh_port=EXCLUDED.wg_ssh_port, enroll_operation_id=EXCLUDED.enroll_operation_id,
            updated_at=clock_timestamp()`,
		in.Node, in.Roles, capabilities, in.Address, string(state), in.User, in.HostKey, in.HostKeyFingerprint,
		in.IdentityFingerprint, in.BuildConcurrency, in.BuildUser, in.PrivateNetwork != nil, wgAddress, wgEndpoint, wgSSHPort, string(id))
	if err != nil {
		return platformWriteError(err, "node enrollment conflicts with another node")
	}
	return nil
}

// allocateWireGuardAddress returns the lowest free host address of the private
// subnet, skipping the control plane's own address.
func (s *Service) allocateWireGuardAddress(ctx context.Context, tx pgx.Tx) (string, error) {
	config := s.resolverConfig()
	prefix, err := netip.ParsePrefix(config.subnet())
	if err != nil || !prefix.Addr().Is4() {
		return "", contract.NewFault(contract.ValidationFailed, "the WireGuard subnet is invalid", "Set EREGION_NODE_WG_SUBNET to an IPv4 CIDR such as 10.77.0.0/24.")
	}
	control, err := netip.ParseAddr(config.controlAddress())
	if err != nil {
		return "", contract.NewFault(contract.ValidationFailed, "the control plane WireGuard address is invalid", "Set EREGION_NODE_WG_ADDRESS.")
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, wgAllocationLock); err != nil {
		return "", unavailable(err)
	}
	rows, err := tx.Query(ctx, `SELECT wg_address FROM eregion_platform_nodes WHERE wg_address<>''`)
	if err != nil {
		return "", unavailable(err)
	}
	used := map[string]bool{control.String(): true}
	for rows.Next() {
		var address string
		if err := rows.Scan(&address); err != nil {
			rows.Close()
			return "", unavailable(err)
		}
		used[address] = true
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return "", unavailable(err)
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
func (w *platformWorker) verifyNode(ctx context.Context, tr node.Transport, id contract.OperationID, attempt uint32, rec nodeConn) (attestedHost, error) {
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
	input := normalized.Input.(*platform.NodeEnrollInput)
	rec, err := loadNodeConn(ctx, w.service.pool, input.Node)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	if rec.Transport != "ssh" || rec.HostKey == "" {
		return w.settleEnroll(ctx, id, attempt, rec.Name, contract.NewFault(contract.PreconditionFailed, "node was not submitted for enrollment", "Enroll the node with its address and pinned host key."))
	}
	tr, closer, err := w.service.resolver.Direct(rec)
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
	if err := w.service.finishPlatformOperation(ctx, id, attempt, func(tx pgx.Tx, op *contract.Operation) error {
		if _, err := tx.Exec(ctx, `UPDATE eregion_platform_nodes SET
            enrollment_state='enrolled', enrollment_error='', enrolled_at=coalesce(enrolled_at,clock_timestamp()),
            identity_public_key=$2, identity_fingerprint=$3, capabilities=$4, os_id=$5, os_version=$6, arch=$7, probe=$8,
            status='ready', last_seen_at=clock_timestamp(), last_check_at=clock_timestamp(), last_check_state='ok', last_check_detail=$9,
            updated_at=clock_timestamp()
            WHERE name=$1`, rec.Name, host.attestation.PublicKey, host.fingerprint, capabilities,
			host.probe.OS.ID, host.probe.OS.VersionID, host.probe.Arch, probeJSON, detail); err != nil {
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

func buildConcurrencyFor(rec nodeConn) int {
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
	}
	message := truncateText(fault.Message, 1024)
	_, _ = w.service.pool.Exec(context.WithoutCancel(ctx), `UPDATE eregion_platform_nodes
        SET enrollment_state=CASE WHEN $3 AND enrollment_state='pending' THEN 'failed' ELSE enrollment_state END,
            enrollment_error=CASE WHEN enrollment_state='enrolled' THEN enrollment_error ELSE $2 END,
            last_check_at=clock_timestamp(), last_check_state=$4, last_check_detail=$2, updated_at=clock_timestamp()
        WHERE name=$1`, nodeName, message, terminal, checkStateFor(err))
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
func (w *platformWorker) runEnrolledCheck(ctx context.Context, id contract.OperationID, attempt uint32, rec nodeConn) error {
	tr, err := w.service.resolver.TransportFor(ctx, rec.Name)
	if err != nil {
		w.service.recordNodeCheck(ctx, rec.Name, checkStateFor(err), contract.Classify(err).Message, false)
		return w.settle(ctx, id, attempt, err)
	}
	host, err := w.verifyNode(ctx, tr, id, attempt, rec)
	if err != nil {
		state := checkStateFor(err)
		w.service.recordNodeCheck(ctx, rec.Name, state, contract.Classify(err).Message, false)
		return w.settle(ctx, id, attempt, err)
	}
	capabilities := host.probe.Capabilities
	if len(capabilities) == 0 {
		capabilities = []string{"none"}
	}
	probeJSON, _ := json.Marshal(host.probe)
	if err := w.service.finishPlatformOperation(ctx, id, attempt, func(tx pgx.Tx, op *contract.Operation) error {
		_, err := tx.Exec(ctx, `UPDATE eregion_platform_nodes SET capabilities=$2, os_id=$3, os_version=$4, arch=$5, probe=$6,
            status='ready', last_seen_at=clock_timestamp(), last_check_at=clock_timestamp(), last_check_state='ok',
            last_check_detail=$7, updated_at=clock_timestamp() WHERE name=$1`,
			rec.Name, capabilities, host.probe.OS.ID, host.probe.OS.VersionID, host.probe.Arch, probeJSON,
			"identity "+host.fingerprint+" verified; capabilities "+strings.Join(capabilities, ","))
		return err
	}); err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	return nil
}

// recordNodeCheck stores the outcome and time of an observation.
func (s *Service) recordNodeCheck(ctx context.Context, name, state, detail string, ready bool) {
	status := "unreachable"
	if ready {
		status = "ready"
	}
	_, _ = s.pool.Exec(context.WithoutCancel(ctx), `UPDATE eregion_platform_nodes
        SET status=CASE WHEN status='disabled' THEN status ELSE $2 END, last_check_at=clock_timestamp(), last_check_state=$3,
            last_check_detail=$4, updated_at=clock_timestamp() WHERE name=$1`, name, status, state, truncateText(detail, 1024))
}

// -------------------------------------------------------------- reconcile

// runNodeReconcile converges a node's role configuration and its WireGuard
// peers. Every step is an idempotent Check/Apply on the node; running it again
// changes nothing when the node already matches.
func (w *platformWorker) runNodeReconcile(ctx context.Context, id contract.OperationID, attempt uint32, normalized platform.Normalized) error {
	input := normalized.Input.(*platform.NodeReconcileInput)
	rec, err := loadNodeConn(ctx, w.service.pool, input.Node)
	if err != nil {
		return w.settle(ctx, id, attempt, err)
	}
	if rec.Enrollment != platform.EnrollmentEnrolled || rec.Transport != "ssh" {
		return w.settle(ctx, id, attempt, placementViolation(ReasonNotEnrolled, rec.Name, fmt.Sprintf("node %q is not enrolled", rec.Name), "Enroll the node before reconciling it.", nil))
	}
	var tr node.Transport
	if input.Via == "direct" {
		direct, closer, err := w.service.resolver.Direct(rec)
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
	if _, err := w.service.pool.Exec(ctx, `UPDATE eregion_platform_nodes SET wg_public_key=$2, control_wg_public_key=$3,
        wg_state=CASE WHEN wg_state IN ('none','failed') THEN 'keyed' ELSE wg_state END, updated_at=clock_timestamp() WHERE name=$1`,
		rec.Name, key.PublicKey, controlPublic); err != nil {
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
		_, _ = w.service.pool.Exec(context.WithoutCancel(ctx), `UPDATE eregion_platform_nodes SET wg_state='failed', updated_at=clock_timestamp() WHERE name=$1 AND wg_state<>'applied'`, rec.Name)
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
	if err := w.service.finishPlatformOperation(ctx, id, attempt, func(tx pgx.Tx, op *contract.Operation) error {
		if state != nil {
			if _, err := tx.Exec(ctx, `UPDATE eregion_platform_nodes SET wg_state=$2, wg_reconciled_at=clock_timestamp(), updated_at=clock_timestamp() WHERE name=$1`, nodeName, string(*state)); err != nil {
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
func (s *Service) wireGuardPeers(ctx context.Context, rec nodeConn, controlPublic string) ([]node.WireGuardPeer, error) {
	control, err := netip.ParseAddr(s.resolverConfig().controlAddress())
	if err != nil {
		return nil, contract.NewFault(contract.ValidationFailed, "the control plane WireGuard address is invalid", "Set EREGION_NODE_WG_ADDRESS.")
	}
	peers := []node.WireGuardPeer{{PublicKey: controlPublic, AllowedIPs: []string{control.String() + "/32"}}}
	rows, err := s.pool.Query(ctx, `SELECT wg_public_key,wg_address,wg_endpoint FROM eregion_platform_nodes
        WHERE name<>$1 AND enrollment_state='enrolled' AND wg_enabled AND wg_public_key<>'' AND wg_address<>'' AND wg_endpoint<>'' ORDER BY name`, rec.Name)
	if err != nil {
		return nil, unavailable(err)
	}
	defer rows.Close()
	for rows.Next() {
		var key, address, endpoint string
		if err := rows.Scan(&key, &address, &endpoint); err != nil {
			return nil, unavailable(err)
		}
		peers = append(peers, node.WireGuardPeer{PublicKey: key, AllowedIPs: []string{address + "/32"}, Endpoint: endpoint, Keepalive: 25})
	}
	if err := rows.Err(); err != nil {
		return nil, unavailable(err)
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].PublicKey < peers[j].PublicKey })
	return peers, nil
}

// ----------------------------------------------------------- build slots

// buildSlotKey is the advisory-lock key of one build slot of a node.
func buildSlotKey(nodeName string, slot int) int64 {
	return advisoryKey("eregion-build-slot\x00" + nodeName + "\x00" + strconv.Itoa(slot))
}

// acquireBuildSlot takes one of a node's build slots, waiting for a free one.
// The limit is per node and enforced across every control-plane process with
// session advisory locks, so a dead worker releases its slot. onWait reports
// once that the build is queued behind others.
func (s *Service) acquireBuildSlot(ctx context.Context, nodeName string, limit int, onWait func()) (func(), error) {
	if limit < 1 {
		limit = 1
	}
	waited := false
	deadline := time.Now().Add(buildSlotWait)
	for {
		conn, err := s.pool.Acquire(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, unavailable(err)
		}
		for slot := range limit {
			var got bool
			if err := conn.QueryRow(ctx, `SELECT pg_try_advisory_lock($1)`, buildSlotKey(nodeName, slot)).Scan(&got); err != nil {
				conn.Release()
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				return nil, unavailable(err)
			}
			if got {
				key := buildSlotKey(nodeName, slot)
				return func() {
					_, _ = conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, key)
					conn.Release()
				}, nil
			}
		}
		conn.Release()
		if !waited && onWait != nil {
			waited = true
			onWait()
		}
		if time.Now().After(deadline) {
			return nil, placementViolation(ReasonBuildCapacity, nodeName, fmt.Sprintf("node %q has all %d build slots busy", nodeName, limit),
				"Wait for running builds to finish, or raise the node's build concurrency.", map[string]any{"build_concurrency": limit})
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(buildSlotPoll):
		}
	}
}

// buildSlotWait bounds how long a build waits for a free slot; buildSlotPoll is
// how often it looks. Tests shorten them.
var (
	buildSlotWait = 10 * time.Minute
	buildSlotPoll = time.Second
)

// advisoryKey derives a stable 64-bit advisory-lock key from text.
func advisoryKey(text string) int64 {
	sum := sha256.Sum256([]byte(text))
	return int64(binary.BigEndian.Uint64(sum[:8]))
}
