package controlplane

import (
	"fmt"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/platform"
)

func TestPointerOwnershipValidation(t *testing.T) {
	if !validPointerOwnership("app", "env", "app", "env") {
		t.Fatal("matching ownership rejected")
	}
	for _, tc := range [][4]string{{"app", "env", "other", "env"}, {"app", "env", "app", "other"}, {"", "env", "", "env"}} {
		if validPointerOwnership(tc[0], tc[1], tc[2], tc[3]) {
			t.Fatalf("accepted ownership %v", tc)
		}
	}
}

func TestOperationRequiresPlacement(t *testing.T) {
	for _, kind := range []string{platform.KindDeploy, platform.KindRollback, platform.KindRestart, platform.KindPromote, platform.KindBuildRetry} {
		if !operationRequiresPlacement(kind) {
			t.Fatalf("missing placement requirement for %s", kind)
		}
	}
	if operationRequiresPlacement(platform.KindNodeRegister) {
		t.Fatal("node registration has no deployment placement")
	}
}

func TestStoredSecretRevisionValidation(t *testing.T) {
	hash := strings.Repeat("a", 64)
	for _, tc := range []struct {
		raw   string
		valid bool
	}{
		{`{}`, true},
		{fmt.Sprintf(`{"TOKEN":{"revision":1,"hash":"%s","key_id":"key-1"}}`, hash), true},
		{fmt.Sprintf(`{"TOKEN":{"revision":0,"hash":"%s","key_id":"key-1"}}`, hash), false},
		{`{"TOKEN":{"revision":1,"hash":"bad","key_id":"key-1"}}`, false},
		{fmt.Sprintf(`{"TOKEN":{"revision":1,"hash":"%s","key_id":"!"}}`, hash), false},
		{fmt.Sprintf(`{"bad-name":{"revision":1,"hash":"%s","key_id":"key-1"}}`, hash), false},
		{`null`, false}, {`[]`, false},
	} {
		if _, err := decodeDeploymentSecretRevisions([]byte(tc.raw)); (err == nil) != tc.valid {
			t.Fatalf("raw=%s err=%v", tc.raw, err)
		}
	}
}

func TestStoredResourceValidation(t *testing.T) {
	id := platform.ResourceID("res_0123456789abcdef0123456789abcdef")
	good := platform.Resource{
		ID: id, State: platform.ResourceReady, HealthState: platform.HealthUnknown,
		Spec:    platform.ResourceSpec{Image: "redis:7", Health: platform.ResourceHealthSpec{Command: []string{"true"}}},
		Volumes: []platform.ResourceVolumeStatus{{Name: "er-res-0123456789abcdef0123456789abcdef-data", Destination: "/data"}},
	}
	if err := validateStoredResource(good); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*platform.Resource){
		func(r *platform.Resource) { r.State = "bad" },
		func(r *platform.Resource) { r.HealthState = "bad" },
		func(r *platform.Resource) { r.Spec.Image = "" },
		func(r *platform.Resource) { r.Volumes = nil },
		func(r *platform.Resource) {
			r.Volumes = []platform.ResourceVolumeStatus{{Name: "er-res-ffffffffffffffffffffffffffffffff-data", Destination: "/data"}}
		},
		func(r *platform.Resource) {
			r.Volumes = []platform.ResourceVolumeStatus{{Name: "er-res-0123456789abcdef0123456789abcdef-data", Destination: "/proc"}}
		},
	} {
		bad := good
		mutate(&bad)
		if err := validateStoredResource(bad); err == nil {
			t.Fatalf("accepted invalid resource: %+v", bad)
		}
	}
}
