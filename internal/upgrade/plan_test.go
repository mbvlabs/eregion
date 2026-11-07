package upgrade

import (
	"context"
	"testing"

	"github.com/mbvlabs/eregion/internal/release"
)

func TestCmpSemver(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"1.0.0-alpha.2", "1.0.0-alpha.10", -1},
		{"1.0.0-alpha.10", "1.0.0-beta.1", -1},
		{"1.0.0-beta.1", "1.0.0-rc.1", -1},
		{"1.0.0-rc.1", "1.0.0", -1},
		{"1.0.0", "1.0.0-rc.9", 1},
		{"1.0.0-alpha.1", "1.0.0-alpha.1", 0},
		{"1.0.1-alpha.1", "1.0.0", 1},
		{"1.10.0", "1.9.0", 1},
		{"2.0.0-alpha.1", "1.99.99", 1},
		{"1.0.0", "1.0.0", 0},
	}
	for _, c := range cases {
		if got := cmpSemver(c.a, c.b); got != c.want {
			t.Errorf("cmpSemver(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestSelectChannel(t *testing.T) {
	cases := []struct {
		name    string
		flag    string
		channel string
		version string
		want    string
		err     bool
	}{
		{"stable release follows its line", "", "stable", "1.4.2", "stable-1.x", false},
		{"alpha follows its line", "", "stable", "1.0.0-alpha.1", "stable-1.x", false},
		{"rc follows rc", "", "stable", "1.0.0-rc.1", "rc", false},
		{"next major follows its line", "", "stable", "2.0.0", "stable-2.x", false},
		{"flag wins", "stable", "stable", "1.4.2", "stable", false},
		{"explicit line", "stable-2.x", "stable", "1.4.2", "stable-2.x", false},
		{"edge follows edge", "", "edge", "edge-0123456789ab", "edge", false},
		{"dev needs a flag", "", "dev", "dev", "", true},
		{"dev with a flag", "edge", "dev", "dev", "edge", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := selectChannel(c.flag, c.channel, c.version)
			if (err != nil) != c.err {
				t.Fatalf("error = %v, want error %v", err, c.err)
			}
			if got != c.want {
				t.Fatalf("got %q, want %q", got, c.want)
			}
		})
	}
}

func TestSelectVersionChannel(t *testing.T) {
	for _, c := range []struct{ flag, version, want string }{
		{"", "1.0.0-alpha.1", "stable"}, {"", "edge-0123456789ab", "edge"}, {"rc", "1.0.0-rc.1", "rc"},
	} {
		got, err := SelectVersionChannel(c.flag, c.version)
		if err != nil || got != c.want {
			t.Errorf("SelectVersionChannel(%q,%q) = %q, %v", c.flag, c.version, got, err)
		}
	}
}

func TestPreferFinal(t *testing.T) {
	for _, c := range []struct{ rc, final, want string }{
		{"1.0.0-rc.10", "1.0.0", "1.0.0"},
		{"1.1.0-rc.1", "1.0.9", "1.1.0-rc.1"},
	} {
		got := PreferFinal(release.Manifest{Version: c.rc}, release.Manifest{Version: c.final})
		if got.Version != c.want {
			t.Errorf("PreferFinal(%s, %s) = %s", c.rc, c.final, got.Version)
		}
	}
}

func TestDecide(t *testing.T) {
	stable := func(v string) release.Manifest { return release.Manifest{Channel: "stable", Version: v} }
	cases := []struct {
		name      string
		from      Version
		m         release.Manifest
		requested bool
		changed   bool
		warnings  []string
	}{
		{"same version", Version{"1.0.0-alpha.2", "stable"}, stable("1.0.0-alpha.2"), false, false, nil},
		{"next alpha", Version{"1.0.0-alpha.2", "stable"}, stable("1.0.0-alpha.10"), false, true, nil},
		{"alpha to final", Version{"1.0.0-rc.1", "stable"}, stable("1.0.0"), false, true, nil},
		{"never downgrades unasked", Version{"1.0.0", "stable"}, stable("1.0.0-rc.1"), false, false, nil},
		{"pinned downgrade warns", Version{"1.0.0", "stable"}, stable("1.0.0-rc.1"), true, true, []string{"downgrade"}},
		{"explicit channel downgrade warns", Version{"1.1.0-rc.5", "stable"}, stable("1.0.0"), true, true, []string{"downgrade"}},
		{"major move warns", Version{"1.4.2", "stable"}, stable("2.0.0"), false, true, []string{"major_line_changed"}},
		{"pinned major downgrade", Version{"2.0.0", "stable"}, stable("1.4.3"), true, true, []string{"downgrade", "major_line_changed"}},
		{"edge to stable", Version{"edge-0123456789ab", "edge"}, stable("1.0.0-alpha.1"), false, true, []string{"channel_changed"}},
		{"explicit rc candidate", Version{"1.0.0", "stable"}, stable("1.1.0-rc.1"), false, true, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			plan := Decide(c.from, c.m, c.requested)
			if plan.Changed != c.changed {
				t.Fatalf("changed = %v, want %v", plan.Changed, c.changed)
			}
			var codes []string
			for _, w := range plan.Warnings {
				codes = append(codes, w.Code)
			}
			if len(codes) != len(c.warnings) {
				t.Fatalf("warnings = %v, want %v", codes, c.warnings)
			}
			for i := range codes {
				if codes[i] != c.warnings[i] {
					t.Fatalf("warnings = %v, want %v", codes, c.warnings)
				}
			}
		})
	}
}

func TestFetchNodeBinaryValidatesPlatform(t *testing.T) {
	for _, arch := range []string{"386", "arm", ""} {
		if _, _, err := FetchNodeBinary(context.Background(), "edge", "", arch); err == nil {
			t.Fatalf("FetchNodeBinary accepted architecture %q", arch)
		}
	}
	if _, _, err := FetchNodeBinary(context.Background(), "", "", "amd64"); err == nil {
		t.Fatal("FetchNodeBinary accepted an empty channel")
	}
}
