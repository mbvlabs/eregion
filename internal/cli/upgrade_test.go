package cli

import (
	"context"
	"reflect"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/release"
)

type upgradeResolverStub struct {
	values map[string]release.Manifest
	errors map[string]error
	calls  []string
}

func (s *upgradeResolverStub) Resolve(_ context.Context, channel, version string) (release.Manifest, error) {
	s.calls = append(s.calls, channel+" "+version)
	return s.values[channel], s.errors[channel]
}

func TestUpgradeRCResolution(t *testing.T) {
	notFound := contract.NewFault(contract.NotFound, "missing", "")
	badSignature := contract.NewFault(contract.PreconditionFailed, "bad signature", "")
	for _, tc := range []struct {
		name, channel, version, current, want string
		defaultChannel                        bool
		values                                map[string]release.Manifest
		errors                                map[string]error
		calls                                 []string
		wantErr                               bool
	}{
		{"candidate major final", "rc", "", "1.0.0-rc.1", "2.0.0", true, map[string]release.Manifest{"rc": {Version: "2.0.0-rc.2"}, "stable-2.x": {Version: "2.0.0"}}, nil, []string{"rc ", "stable-2.x "}, false},
		{"missing rc", "rc", "", "1.0.0-rc.1", "1.0.0", true, map[string]release.Manifest{"stable-1.x": {Version: "1.0.0"}}, map[string]error{"rc": notFound}, []string{"rc ", "stable-1.x "}, false},
		{"bad rc signature", "rc", "", "1.0.0-rc.1", "", true, nil, map[string]error{"rc": badSignature}, []string{"rc "}, true},
		{"bad final signature", "rc", "", "1.0.0-rc.1", "", true, map[string]release.Manifest{"rc": {Version: "1.0.0-rc.2"}}, map[string]error{"stable-1.x": badSignature}, []string{"rc ", "stable-1.x "}, true},
		{"missing both pointers", "rc", "", "1.0.0-rc.1", "", true, nil, map[string]error{"rc": notFound, "stable-1.x": notFound}, []string{"rc ", "stable-1.x "}, true},
		{"explicit rc opt in", "rc", "", "1.0.0", "1.1.0-rc.1", false, map[string]release.Manifest{"rc": {Version: "1.1.0-rc.1"}}, nil, []string{"rc "}, false},
		{"stable default", "stable-1.x", "", "1.0.0", "1.0.1", true, map[string]release.Manifest{"stable-1.x": {Version: "1.0.1"}}, nil, []string{"stable-1.x "}, false},
		{"exact pin", "stable", "1.1.0-beta.1", "1.0.0", "1.1.0-beta.1", true, map[string]release.Manifest{"stable": {Version: "1.1.0-beta.1"}}, nil, []string{"stable 1.1.0-beta.1"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := &upgradeResolverStub{values: tc.values, errors: tc.errors}
			m, err := resolveUpgradeManifest(context.Background(), s, tc.channel, tc.version, tc.defaultChannel, tc.current)
			if (err != nil) != tc.wantErr || m.Version != tc.want || !reflect.DeepEqual(s.calls, tc.calls) {
				t.Fatalf("got %q, %v, calls %v", m.Version, err, s.calls)
			}
		})
	}
}

func attachUpgrade(t *testing.T) *app {
	t.Helper()
	a := newApp()
	for _, spec := range a.upgradeSpecs() {
		a.attach(a.root, spec)
	}
	if err := a.meta.validate(a.root); err != nil {
		t.Fatalf("upgrade metadata does not validate: %v", err)
	}
	return a
}

func TestUpgradeSpecsMetadata(t *testing.T) {
	a := attachUpgrade(t)
	cmd, _, err := a.root.Find([]string{"upgrade"})
	if err != nil {
		t.Fatal(err)
	}
	meta := a.meta[cmd]
	if !meta.Mutating || !meta.DryRun || !meta.Prompts || meta.Destructive {
		t.Fatalf("upgrade metadata is wrong: %+v", meta)
	}
	if meta.Confirmation != "" {
		t.Fatalf("upgrade unexpectedly requires a plan-bound confirmation: %q", meta.Confirmation)
	}
	for _, platform := range meta.Platforms {
		if platform == "windows" {
			t.Fatal("upgrade advertises a Windows build")
		}
	}
	for _, name := range []string{"version", "channel", "yes"} {
		if flagFor(cmd, name) == nil {
			t.Fatalf("upgrade is missing --%s", name)
		}
	}
	if cmd.HasSubCommands() {
		t.Fatal("the removed upgrade subcommands are still attached")
	}
}
