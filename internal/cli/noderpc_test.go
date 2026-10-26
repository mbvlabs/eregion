package cli

import (
	"testing"

	"github.com/mbvlabs/eregion/internal/rpc"
)

func TestNodeRPCCommand(t *testing.T) {
	if got := nodeRPCCommand("admin"); got != "sudo -n /usr/local/bin/eregion node rpc" {
		t.Fatalf("admin command: %q", got)
	}
	if got := nodeRPCCommand("root"); got != "/usr/local/bin/eregion node rpc" {
		t.Fatalf("root command: %q", got)
	}
}

func TestPostUpgradeComparisonUsesTheFreshNodeVersion(t *testing.T) {
	want := rpc.Build{Version: "v1.0.0-alpha.2", Commit: "fresh"}
	stale := rpcVersionResult{Version: "v1.0.0-alpha.1", Commit: "old"}
	fresh := rpcVersionResult{Version: "v1.0.0-alpha.2", Commit: "fresh"}
	if nodeBuildMatches(stale, want) {
		t.Fatal("the stale pre-upgrade version matched")
	}
	if !nodeBuildMatches(fresh, want) {
		t.Fatal("the freshly read version did not match after the upgrade")
	}
}

func TestNodeUpgradeModePushesOrFetchesTheVerifiedLinuxBuild(t *testing.T) {
	for _, test := range []struct {
		name     string
		nodeArch string
		goos     string
		goarch   string
		version  string
		channel  string
		mode     string
		fails    bool
	}{
		{name: "linux matching arch uploads itself", nodeArch: "amd64", goos: "linux", goarch: "amd64", version: "v1.0.0-alpha.2", channel: "edge", mode: "own"},
		{name: "macOS fetches linux", nodeArch: "amd64", goos: "darwin", goarch: "arm64", version: "v1.0.0-alpha.2", channel: "edge", mode: "fetch"},
		{name: "arch mismatch fetches linux", nodeArch: "arm64", goos: "linux", goarch: "amd64", version: "v1.0.0-alpha.2", channel: "edge", mode: "fetch"},
		{name: "dev macOS build has no release", nodeArch: "amd64", goos: "darwin", goarch: "arm64", version: "dev", channel: "dev", fails: true},
		{name: "dev arch mismatch has no release", nodeArch: "arm64", goos: "linux", goarch: "amd64", version: "dev", channel: "dev", fails: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			mode, fault := nodeUpgradeModeFor(rpcVersionResult{Arch: test.nodeArch}, test.goos, test.goarch, test.version, test.channel)
			if test.fails {
				if fault == nil {
					t.Fatalf("expected a clear error, got mode %q", mode)
				}
				return
			}
			if fault != nil || mode != test.mode {
				t.Fatalf("mode=%q fault=%v", mode, fault)
			}
		})
	}
}
