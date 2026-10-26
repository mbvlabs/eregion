package release

import (
	"strings"
	"testing"
)

func TestChannelIdentity(t *testing.T) {
	cases := []struct {
		channel, version, want string
	}{
		{"edge", "edge-0123456789ab", "https://github.com/mbvlabs/eregion/.github/workflows/publish-edge.yml@refs/heads/master"},
		{"stable", "1.0.0", "https://github.com/mbvlabs/eregion/.github/workflows/publish-stable.yml@refs/tags/v1.0.0"},
		{"stable-1.x", "1.0.0-alpha.2", "https://github.com/mbvlabs/eregion/.github/workflows/publish-stable.yml@refs/tags/v1.0.0-alpha.2"},
	}
	for _, c := range cases {
		got, err := Identity(c.channel, c.version)
		if err != nil {
			t.Fatalf("Identity(%q, %q): %v", c.channel, c.version, err)
		}
		if got != c.want {
			t.Fatalf("Identity(%q, %q) = %q, want %q", c.channel, c.version, got, c.want)
		}
	}
	if _, err := Identity("stable-1.x", "2.0.0"); err == nil {
		t.Fatal("a 2.x version was accepted on the 1.x channel")
	}
	if _, err := Identity("edge", "1.0.0"); err == nil {
		t.Fatal("a stable version was accepted on the edge channel")
	}
	root, err := ChannelRoot("stable-1.x")
	if err != nil || !strings.HasSuffix(root, "/1.x") {
		t.Fatalf("ChannelRoot(stable-1.x) = %q, %v", root, err)
	}
}
