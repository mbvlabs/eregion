package retention

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
)

func TestDefaultPolicyMatchesD30(t *testing.T) {
	policy := DefaultPolicy()
	if policy.KeepReleases != 10 {
		t.Fatalf("keep releases = %d, want 10", policy.KeepReleases)
	}
	if policy.KeepNewerThanDays != 14 {
		t.Fatalf("keep newer than days = %d, want 14", policy.KeepNewerThanDays)
	}
	if policy.ArtifactGraceDays != 14 {
		t.Fatalf("artifact grace days = %d, want 14", policy.ArtifactGraceDays)
	}
	if policy.BuildpackCacheMaxBytes != 10<<30 {
		t.Fatalf("buildpack cache max bytes = %d, want 10 GiB", policy.BuildpackCacheMaxBytes)
	}
	if policy.BuildpackCacheMaxPercent != 80 {
		t.Fatalf("buildpack cache max percent = %d, want 80", policy.BuildpackCacheMaxPercent)
	}
	if err := policy.Validate(); err != nil {
		t.Fatalf("defaults are invalid: %v", err)
	}
}

func TestOverrideMergesFieldByField(t *testing.T) {
	server := DefaultPolicy()
	keepReleases, grace := 3, 30
	merged, err := Merge(server, Override{KeepReleases: &keepReleases, ArtifactGraceDays: &grace})
	if err != nil {
		t.Fatal(err)
	}
	if merged.KeepReleases != 3 || merged.ArtifactGraceDays != 30 {
		t.Fatalf("merged = %+v, want overridden keep_releases=3 artifact_grace_days=30", merged)
	}
	if merged.KeepNewerThanDays != server.KeepNewerThanDays {
		t.Fatalf("keep_newer_than_days = %d, want inherited %d", merged.KeepNewerThanDays, server.KeepNewerThanDays)
	}
	if merged.BuildpackCacheMaxBytes != server.BuildpackCacheMaxBytes || merged.BuildpackCacheMaxPercent != server.BuildpackCacheMaxPercent {
		t.Fatalf("buildpack cache = %+v, want inherited server limits", merged)
	}
}

func TestEmptyOverrideInheritsEverything(t *testing.T) {
	server := DefaultPolicy()
	merged, err := Merge(server, Override{})
	if err != nil {
		t.Fatal(err)
	}
	if merged != server {
		t.Fatalf("merged = %+v, want server policy %+v", merged, server)
	}
	zero := Override{}.IsZero()
	if !zero {
		t.Fatal("empty override is not zero")
	}
}

func TestOverrideCloneDoesNotAlias(t *testing.T) {
	keepReleases := 3
	original := Override{KeepReleases: &keepReleases}
	clone := original.Clone()
	*clone.KeepReleases = 7
	if *original.KeepReleases != 3 {
		t.Fatalf("clone mutated the original: %d", *original.KeepReleases)
	}
}

func TestPolicyValidationRejectsNonsense(t *testing.T) {
	tests := []struct {
		name   string
		policy Policy
	}{
		{name: "keep zero releases", policy: func() Policy { p := DefaultPolicy(); p.KeepReleases = 0; return p }()},
		{name: "keep too many releases", policy: func() Policy { p := DefaultPolicy(); p.KeepReleases = 1001; return p }()},
		{name: "zero age window", policy: func() Policy { p := DefaultPolicy(); p.KeepNewerThanDays = 0; return p }()},
		{name: "zero artifact grace", policy: func() Policy { p := DefaultPolicy(); p.ArtifactGraceDays = 0; return p }()},
		{name: "tiny cache cap", policy: func() Policy { p := DefaultPolicy(); p.BuildpackCacheMaxBytes = 1; return p }()},
		{name: "cache percent zero", policy: func() Policy { p := DefaultPolicy(); p.BuildpackCacheMaxPercent = 0; return p }()},
		{name: "cache percent above disk", policy: func() Policy { p := DefaultPolicy(); p.BuildpackCacheMaxPercent = 100; return p }()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := test.policy.Validate()
			if err == nil {
				t.Fatal("invalid policy was accepted")
			}
			if code := contract.Classify(err).Code; code != contract.ValidationFailed {
				t.Fatalf("code = %s, want %s: %v", code, contract.ValidationFailed, err)
			}
		})
	}
}

func TestOverrideValidationRejectsNonsense(t *testing.T) {
	keepReleases := 0
	if err := (Override{KeepReleases: &keepReleases}).Validate(); err == nil {
		t.Fatal("keep_releases=0 override was accepted")
	}
	days := -1
	if err := (Override{KeepNewerThanDays: &days}).Validate(); err == nil {
		t.Fatal("negative keep_newer_than_days override was accepted")
	}
	grace := 0
	if err := (Override{ArtifactGraceDays: &grace}).Validate(); err == nil {
		t.Fatal("artifact_grace_days=0 override was accepted")
	}
	valid := 2
	if err := (Override{KeepReleases: &valid}).Validate(); err != nil {
		t.Fatalf("valid override refused: %v", err)
	}
}

func TestMergeValidatesBothSides(t *testing.T) {
	badServer := DefaultPolicy()
	badServer.KeepReleases = 0
	if _, err := Merge(badServer, Override{}); err == nil {
		t.Fatal("invalid server policy was merged")
	}
	zero := 0
	if _, err := Merge(DefaultPolicy(), Override{KeepReleases: &zero}); err == nil {
		t.Fatal("invalid environment override was merged")
	}
}

func TestPolicyJSONRoundTrip(t *testing.T) {
	policy := DefaultPolicy()
	raw, err := json.Marshal(policy)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"keep_releases":10`, `"keep_newer_than_days":14`, `"artifact_grace_days":14`, `"buildpack_cache_max_bytes":10737418240`, `"buildpack_cache_max_percent":80`} {
		if !strings.Contains(string(raw), field) {
			t.Fatalf("policy JSON %s is missing %s", raw, field)
		}
	}
	var decoded Policy
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded != policy {
		t.Fatalf("round trip = %+v, want %+v", decoded, policy)
	}
}

func TestBuildpackCacheExceeded(t *testing.T) {
	policy := DefaultPolicy()
	cases := []struct {
		name  string
		cache int64
		used  int64
		total int64
		want  bool
	}{
		{name: "below both limits", cache: 5 << 30, used: 50 << 30, total: 100 << 30, want: false},
		{name: "above absolute limit", cache: 11 << 30, used: 20 << 30, total: 200 << 30, want: true},
		{name: "above disk utilization", cache: 5 << 30, used: 90 << 30, total: 100 << 30, want: true},
		{name: "at disk utilization is not above", cache: 5 << 30, used: 80 << 30, total: 100 << 30, want: false},
		{name: "unknown disk uses absolute only", cache: 9 << 30, used: 90 << 30, total: 0, want: false},
		{name: "unknown disk above absolute", cache: 11 << 30, used: 0, total: 0, want: true},
		{name: "negative use clamps to zero", cache: 5 << 30, used: -1, total: 100 << 30, want: false},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got := policy.BuildpackCacheExceeded(test.cache, test.used, test.total)
			if got != test.want {
				t.Fatalf("BuildpackCacheExceeded(%d, %d, %d) = %t, want %t", test.cache, test.used, test.total, got, test.want)
			}
		})
	}
}

func TestNewRunID(t *testing.T) {
	id, err := NewRunID()
	if err != nil {
		t.Fatal(err)
	}
	if len(id) != len("rtr_")+32 || !strings.HasPrefix(id, "rtr_") {
		t.Fatalf("run ID = %q, want rtr_ + 32 hex characters", id)
	}
	other, err := NewRunID()
	if err != nil {
		t.Fatal(err)
	}
	if id == other {
		t.Fatal("two run IDs collided")
	}
}
