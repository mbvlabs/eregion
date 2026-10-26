package release

import "testing"

func TestParseStable(t *testing.T) {
	for _, version := range []string{"0.0.0", "1.0.0", "1.0.0-alpha.1", "1.2.3-rc.10"} {
		if _, ok := ParseStable(version); !ok {
			t.Fatalf("valid version %q refused", version)
		}
	}
	for _, version := range []string{"", "v1.0.0", "1.0", "01.0.0", "1.0.0-alpha", "1.0.0-alpha.0x", "1.0.0+build"} {
		if _, ok := ParseStable(version); ok {
			t.Fatalf("invalid version %q accepted", version)
		}
	}
}

func TestCompareVersions(t *testing.T) {
	ordered := []string{
		"1.0.0-alpha.2",
		"1.0.0-alpha.10",
		"1.0.0-beta.1",
		"1.0.0-rc.1",
		"1.0.0",
		"1.0.1",
		"1.10.0",
		"2.0.0-alpha.1",
	}
	for i := 1; i < len(ordered); i++ {
		if got := CompareVersions(ordered[i-1], ordered[i]); got >= 0 {
			t.Fatalf("%s should sort before %s, got %d", ordered[i-1], ordered[i], got)
		}
		if got := CompareVersions(ordered[i], ordered[i-1]); got <= 0 {
			t.Fatalf("%s should sort after %s, got %d", ordered[i], ordered[i-1], got)
		}
	}
}

func TestPromotes(t *testing.T) {
	cases := []struct {
		current, candidate string
		want               bool
		err                bool
	}{
		{"", "1.0.0-alpha.1", true, false},
		{"1.0.0-alpha.2", "1.0.0-alpha.10", true, false},
		{"1.0.0-alpha.2", "1.0.0-alpha.2", false, false},
		{"1.0.0", "1.0.1", true, false},
		{"1.1.0", "1.0.1", false, false},
		{"1.1.0", "1.2.0-alpha.1", false, false},
		{"1.0.0-rc.1", "1.0.0", true, false},
		{"", "garbage", false, true},
		{"garbage", "1.0.0", false, true},
	}
	for _, c := range cases {
		got, err := Promotes(c.current, c.candidate)
		if (err != nil) != c.err || got != c.want {
			t.Fatalf("Promotes(%q, %q) = %v, %v; want %v, error %v", c.current, c.candidate, got, err, c.want, c.err)
		}
	}
}
