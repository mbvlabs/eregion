package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/host"
	"github.com/mbvlabs/eregion/internal/release"
	"github.com/mbvlabs/eregion/internal/rpc"
	"golang.org/x/crypto/ssh"
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

// fakeNodeTransport answers the node wire protocol in-process and records the
// calls a nodeClient makes.
type fakeNodeTransport struct {
	version  rpcVersionResult
	fresh    rpcVersionResult
	methods  []string
	uploads  int
	installs int
}

func (*fakeNodeTransport) Dial(context.Context) (*ssh.Client, error) {
	return nil, errors.New("the fake transport does not dial")
}

func (f *fakeNodeTransport) Upload(context.Context, string, []host.RemoteFile) error {
	f.uploads++
	return nil
}

func (*fakeNodeTransport) Remove(context.Context, string) error { return nil }

func (f *fakeNodeTransport) Run(_ context.Context, _ string, stdin io.Reader, stdout, _ io.Writer) error {
	request, _, err := rpc.DecodeRequest(stdin)
	if err != nil {
		return err
	}
	f.methods = append(f.methods, request.Method)
	var data any
	switch request.Method {
	case methodVersion:
		data = f.version
	case methodUpgradeInstall:
		f.installs++
		f.version = f.fresh
		data = map[string]bool{"installed": true}
	default:
		return fmt.Errorf("unexpected method %q", request.Method)
	}
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	line, err := rpc.MarshalResult(rpc.Result{OK: true, Data: raw})
	if err != nil {
		return err
	}
	_, err = stdout.Write(line)
	return err
}

func (f *fakeNodeTransport) called(method string) bool {
	for _, got := range f.methods {
		if got == method {
			return true
		}
	}
	return false
}

func TestEnsureVersionNeverInstallsUnderDryRun(t *testing.T) {
	released := rpc.Build{Version: "v1.0.0-alpha.2", Commit: "fresh"}
	dev := rpc.Build{Version: "dev", Commit: "local"}
	for _, test := range []struct {
		name         string
		node         rpcVersionResult
		want         rpc.Build
		channel      string
		goos         string
		goarch       string
		autoUpgrade  bool
		dryRun       bool
		wantCode     contract.Code
		wantMessage  string
		wantUploads  int
		wantInstalls int
	}{
		{
			name: "matching build", node: rpcVersionResult{Version: released.Version, Commit: released.Commit, Arch: "amd64"},
			want: released, channel: "stable", goos: "linux", goarch: "amd64", autoUpgrade: true,
		},
		{
			name: "mismatch without auto upgrade", node: rpcVersionResult{Version: "v1.0.0-alpha.1", Commit: "old", Arch: "amd64"},
			want: released, channel: "stable", goos: "linux", goarch: "amd64", wantCode: contract.NodeVersionMismatch,
		},
		{
			name: "dry run never installs", node: rpcVersionResult{Version: "v1.0.0-alpha.1", Commit: "old", Arch: "amd64"},
			want: released, channel: "stable", goos: "linux", goarch: "amd64", autoUpgrade: true, dryRun: true,
			wantCode: contract.NodeVersionMismatch, wantMessage: "would upgrade node from",
		},
		{
			name: "dry run reports the mismatch where the real install mode fails", node: rpcVersionResult{Version: "v1.0.0-alpha.1", Commit: "old", Arch: "amd64"},
			want: dev, channel: "dev", goos: "darwin", goarch: "arm64", autoUpgrade: true, dryRun: true,
			wantCode: contract.NodeVersionMismatch, wantMessage: "would upgrade node from",
		},
		{
			name: "dry run reports the mismatch on an architecture mismatch", node: rpcVersionResult{Version: "v1.0.0-alpha.1", Commit: "old", Arch: "arm64"},
			want: dev, channel: "dev", goos: "linux", goarch: "amd64", autoUpgrade: true, dryRun: true,
			wantCode: contract.NodeVersionMismatch, wantMessage: "would upgrade node from",
		},
		{
			name: "the same mode without dry run fails before installing", node: rpcVersionResult{Version: "v1.0.0-alpha.1", Commit: "old", Arch: "amd64"},
			want: dev, channel: "dev", goos: "darwin", goarch: "arm64", autoUpgrade: true, wantCode: contract.PreconditionFailed,
		},
		{
			name: "install without dry run", node: rpcVersionResult{Version: "v1.0.0-alpha.1", Commit: "old", Arch: "amd64"},
			want: released, channel: "stable", goos: "linux", goarch: "amd64", autoUpgrade: true,
			wantUploads: 1, wantInstalls: 1,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			transport := &fakeNodeTransport{
				version: test.node,
				fresh:   rpcVersionResult{Version: test.want.Version, Commit: test.want.Commit, Arch: test.goarch},
			}
			client := &nodeClient{remote: transport, autoUpgrade: test.autoUpgrade, dryRun: test.dryRun}
			err := client.ensureVersionFor(context.Background(), test.want, test.goos, test.goarch, test.channel)
			if test.wantCode == "" {
				if err != nil {
					t.Fatalf("ensureVersionFor = %v, want nil", err)
				}
			} else {
				fault := contract.Classify(err)
				if err == nil || fault.Code != test.wantCode {
					t.Fatalf("ensureVersionFor = %v, want %s", err, test.wantCode)
				}
				if test.wantMessage != "" && !strings.Contains(fault.Message, test.wantMessage) {
					t.Fatalf("mismatch message %q does not contain %q", fault.Message, test.wantMessage)
				}
			}
			if transport.uploads != test.wantUploads || transport.installs != test.wantInstalls {
				t.Fatalf("uploads=%d installs=%d, want %d and %d", transport.uploads, transport.installs, test.wantUploads, test.wantInstalls)
			}
			if test.dryRun && transport.called(methodUpgradeInstall) {
				t.Fatal("a dry run called upgrade.install")
			}
		})
	}
}

type fakeReleaseManifest struct {
	manifest release.Manifest
	err      error
	calls    []string
}

func (f *fakeReleaseManifest) Resolve(_ context.Context, channel, version string) (release.Manifest, error) {
	f.calls = append(f.calls, channel+" "+version)
	return f.manifest, f.err
}

func TestReleaseNodeInstallsOnlyTheVerifiedReleaseArtifact(t *testing.T) {
	uploaded := strings.Repeat("a", 64)
	other := strings.Repeat("b", 64)
	matching := release.Manifest{Artifacts: map[string]release.Artifact{
		"linux/amd64": {SHA256: uploaded},
	}}
	releaseParams := rpcInstallParams{Path: "/tmp/eregion-x/eregion", SHA256: uploaded, Version: "1.0.0-alpha.1", Channel: "stable"}
	for _, test := range []struct {
		name        string
		nodeChannel string
		goarch      string
		params      rpcInstallParams
		manifest    release.Manifest
		resolveErr  error
		wantCode    contract.Code
		wantHint    string
		wantResolve bool
	}{
		{name: "dev node keeps the checksum-only path", nodeChannel: "dev", goarch: "amd64", params: releaseParams},
		{name: "an empty channel is a dev node", nodeChannel: "", goarch: "amd64", params: releaseParams},
		{name: "release node accepts the manifest artifact", nodeChannel: "stable", goarch: "amd64", params: releaseParams, manifest: matching, wantResolve: true},
		{
			name: "release node refuses another hash", nodeChannel: "stable", goarch: "amd64", params: releaseParams,
			manifest: release.Manifest{Artifacts: map[string]release.Artifact{"linux/amd64": {SHA256: other}}},
			wantCode: contract.PreconditionFailed, wantResolve: true,
		},
		{
			name: "release node refuses an artifact for another architecture", nodeChannel: "edge", goarch: "arm64",
			params:   rpcInstallParams{Version: "edge-0123456789ab", Channel: "edge"},
			manifest: matching, wantCode: contract.PreconditionFailed, wantResolve: true,
		},
		{
			name: "release node refuses a dev CLI", nodeChannel: "stable", goarch: "amd64",
			params:   rpcInstallParams{Version: "dev", Channel: "dev"},
			wantCode: contract.PreconditionFailed, wantHint: "released eregion CLI",
		},
		{
			name: "an unreachable manifest is unavailable", nodeChannel: "stable", goarch: "amd64", params: releaseParams,
			resolveErr: contract.NewFault(contract.Unavailable, "release URL is unreachable", "Check the node's network."),
			wantCode:   contract.Unavailable, wantResolve: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			resolver := &fakeReleaseManifest{manifest: test.manifest, err: test.resolveErr}
			err := verifyReleaseUpload(context.Background(), resolver, test.params, uploaded, test.nodeChannel, test.goarch)
			if test.wantCode == "" {
				if err != nil {
					t.Fatalf("verifyReleaseUpload = %v, want nil", err)
				}
			} else {
				fault := contract.Classify(err)
				if err == nil || fault.Code != test.wantCode {
					t.Fatalf("verifyReleaseUpload = %v, want %s", err, test.wantCode)
				}
				if test.wantHint != "" && !strings.Contains(fault.Hint, test.wantHint) {
					t.Fatalf("hint %q does not name %q", fault.Hint, test.wantHint)
				}
			}
			if got := len(resolver.calls) > 0; got != test.wantResolve {
				t.Fatalf("resolved the manifest = %v, want %v", got, test.wantResolve)
			}
		})
	}
}

func TestInventoryParamsCarryTheCollection(t *testing.T) {
	query, err := rpcInventoryParams{Collection: "apps", Filters: map[string][]string{"limit": {"50"}}}.query()
	if err != nil {
		t.Fatal(err)
	}
	if query.Collection != "apps" || query.Limit != 50 {
		t.Fatalf("query = %+v", query)
	}
}

func TestWaitLabelNamesTheRemoteRead(t *testing.T) {
	if got := waitLabel(methodInventory, rpcInventoryParams{
		Collection: "apps",
		Filters:    map[string][]string{"app": {"andurel"}},
	}); got != "Reading app andurel…" {
		t.Fatalf("app read label = %q", got)
	}
	if got := waitLabel(methodSubmit, rpcSubmitParams{Kind: "platform.deploy"}); got != "Submitting…" {
		t.Fatalf("submit label = %q", got)
	}
	if got := waitLabel(methodHostDoctor, struct{}{}); got != "Running doctor…" {
		t.Fatalf("doctor label = %q", got)
	}
}
