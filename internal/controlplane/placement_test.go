package controlplane

import (
	"errors"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/node"
	"github.com/mbvlabs/eregion/internal/platform"
)

func testNode(name string, state platform.EnrollmentState, roles []string, capabilities ...string) platform.Node {
	return platform.Node{Name: name, EnrollmentState: state, Roles: roles, Capabilities: capabilities}
}

func wantViolation(t *testing.T, err error, reason string) {
	t.Helper()
	got, ok := IsPlacementViolation(err)
	if !ok || got != reason {
		t.Fatalf("want a placement_violation with reason %q, got ok=%v reason=%q err=%v", reason, ok, got, err)
	}
	fault := contract.Classify(err)
	if fault.Code != contract.PreconditionFailed || !strings.HasPrefix(fault.Message, "placement_violation: ") {
		t.Fatalf("a violation is a structured precondition_failed: %+v", fault)
	}
	if fault.Details["reason"] != reason || fault.Details["node"] == "" || fault.Hint == "" {
		t.Fatalf("a violation names its reason, node, and a hint: %+v", fault)
	}
}

func TestNodePlacementRules(t *testing.T) {
	app := testNode("app-1", platform.EnrollmentEnrolled, []string{platform.RoleApplication}, platform.CapabilityDocker)
	builder := testNode("build-1", platform.EnrollmentEnrolled, []string{platform.RoleBuilder}, platform.CapabilityDocker, platform.CapabilityBuildpacks)
	main := testNode("main", platform.EnrollmentRegistered, []string{platform.RoleApplication, platform.RoleControlPlane, platform.RoleBuilder}, platform.CapabilityDocker, platform.CapabilityBuildpacks)

	if err := nodePlacement(app, platform.RoleApplication, platform.CapabilityDocker); err != nil {
		t.Fatalf("an application node takes application work: %v", err)
	}
	if err := nodePlacement(builder, platform.RoleBuilder, platform.CapabilityBuildpacks); err != nil {
		t.Fatalf("a builder takes builds: %v", err)
	}
	if err := nodePlacement(main, platform.RoleBuilder, platform.CapabilityBuildpacks); err != nil {
		t.Fatalf("the main node builds by default: %v", err)
	}

	// Builds never run on a node that only has the application role.
	wantViolation(t, nodePlacement(app, platform.RoleBuilder, platform.CapabilityBuildpacks), ReasonBuilderRequired)
	// Runtime workloads never land on a builder.
	wantViolation(t, nodePlacement(builder, platform.RoleApplication, platform.CapabilityDocker), ReasonRoleMissing)
	// A role without the probed capability is not enough.
	noPack := testNode("build-2", platform.EnrollmentEnrolled, []string{platform.RoleBuilder}, platform.CapabilityDocker)
	wantViolation(t, nodePlacement(noPack, platform.RoleBuilder, platform.CapabilityBuildpacks), ReasonCapabilityMissing)
	// An unfinished or failed enrollment receives nothing.
	for _, state := range []platform.EnrollmentState{platform.EnrollmentPending, platform.EnrollmentFailed} {
		wantViolation(t, nodePlacement(testNode("new", state, []string{platform.RoleApplication}, platform.CapabilityDocker), platform.RoleApplication, platform.CapabilityDocker), ReasonNotEnrolled)
	}
}

func TestRegistryCredentialScopeFollowsRoles(t *testing.T) {
	app := testNode("app-1", platform.EnrollmentEnrolled, []string{platform.RoleApplication})
	builder := testNode("build-1", platform.EnrollmentEnrolled, []string{platform.RoleBuilder})
	if err := checkRegistryCredential(builder, platform.RegistryPush); err != nil {
		t.Fatalf("a builder may push: %v", err)
	}
	if err := checkRegistryCredential(app, platform.RegistryPull); err != nil {
		t.Fatalf("a runtime node may pull: %v", err)
	}
	wantViolation(t, checkRegistryCredential(app, platform.RegistryPush), ReasonRegistryScope)
	wantViolation(t, checkRegistryCredential(app, platform.RegistryScope("admin")), ReasonRegistryScope)
}

type namedTransport struct {
	node.Transport
	name string
}

func (n namedTransport) NodeIdentity() string { return n.name }

func TestFallbackIdentityIsRequiredForRegisteredNodes(t *testing.T) {
	registered := testNode("main", platform.EnrollmentRegistered, []string{platform.RoleApplication}, platform.CapabilityDocker)

	// An insert-only API process has no transport and defers to the worker.
	insertOnly := &Service{}
	if err := insertOnly.checkPlacement(registered, platform.RoleApplication, platform.CapabilityDocker); err != nil {
		t.Fatal(err)
	}
	// A worker whose transport authenticates as another node refuses.
	other := &Service{transport: namedTransport{name: "local"}}
	wantViolation(t, other.checkPlacement(registered, platform.RoleApplication, platform.CapabilityDocker), ReasonIdentityMismatch)
	// A transport that does not pin an identity is refused rather than guessed.
	unpinned := &Service{transport: namedTransport{name: ""}}
	wantViolation(t, unpinned.checkPlacement(registered, platform.RoleApplication, platform.CapabilityDocker), ReasonTransportUnpinned)
	// The matching identity passes.
	same := &Service{transport: namedTransport{name: "main"}}
	if err := same.checkPlacement(registered, platform.RoleApplication, platform.CapabilityDocker); err != nil {
		t.Fatal(err)
	}
	// An enrolled node is authenticated at dispatch by its own pinned transport,
	// so the worker's fallback identity is irrelevant to it.
	enrolled := testNode("app-1", platform.EnrollmentEnrolled, []string{platform.RoleApplication}, platform.CapabilityDocker)
	if err := other.checkPlacement(enrolled, platform.RoleApplication, platform.CapabilityDocker); err != nil {
		t.Fatalf("an enrolled node is not served by the fallback: %v", err)
	}
}

func TestIsPlacementViolationIgnoresOtherErrors(t *testing.T) {
	if _, ok := IsPlacementViolation(errors.New("boom")); ok {
		t.Fatal("a plain error is not a violation")
	}
	if _, ok := IsPlacementViolation(contract.NewFault(contract.PreconditionFailed, "x", "y")); ok {
		t.Fatal("a fault without placement details is not a violation")
	}
}

func TestBuildSlotKeysAreStablePerNodeAndSlot(t *testing.T) {
	if buildSlotKey("a", 0) == buildSlotKey("a", 1) || buildSlotKey("a", 0) == buildSlotKey("b", 0) {
		t.Fatal("each node slot needs its own lock")
	}
	if buildSlotKey("a", 0) != buildSlotKey("a", 0) {
		t.Fatal("the key must be stable across processes")
	}
}
