package release

import (
	"encoding/json"
	"strings"
	"testing"
)

func sampleManifest(t *testing.T) Manifest {
	t.Helper()
	base := versionRoot("1.0.0-alpha.2")
	m := Manifest{
		SchemaVersion:  1,
		Channel:        ChannelEdge,
		Version:        "1.0.0-alpha.2",
		ReleaseBaseURL: base,
		Artifacts:      map[string]Artifact{},
	}
	for _, key := range artifactKeys {
		m.Artifacts[key] = Artifact{URL: base + "/" + key + "/eregion", SHA256: strings.Repeat("a", 64)}
	}
	return m
}

func TestManifestValidate(t *testing.T) {
	m := sampleManifest(t)
	if err := Validate(m, ChannelEdge, "1.0.0-alpha.2"); err != nil {
		t.Fatalf("valid manifest refused: %v", err)
	}

	cases := map[string]func(*Manifest){
		"wrong channel":    func(m *Manifest) { m.Channel = ChannelStable },
		"wrong version":    func(m *Manifest) { m.Version = "1.0.0-alpha.3" },
		"wrong schema":     func(m *Manifest) { m.SchemaVersion = 2 },
		"wrong base":       func(m *Manifest) { m.ReleaseBaseURL = "https://elsewhere.example" },
		"missing artifact": func(m *Manifest) { delete(m.Artifacts, "darwin/arm64") },
		"bad digest": func(m *Manifest) {
			m.Artifacts["linux/amd64"] = Artifact{URL: m.Artifacts["linux/amd64"].URL, SHA256: "nope"}
		},
		"bad artifact URL": func(m *Manifest) {
			m.Artifacts["linux/amd64"] = Artifact{URL: "https://elsewhere.example/eregion", SHA256: strings.Repeat("a", 64)}
		},
	}
	for name, change := range cases {
		t.Run(name, func(t *testing.T) {
			broken := sampleManifest(t)
			change(&broken)
			if err := Validate(broken, ChannelEdge, "1.0.0-alpha.2"); err == nil {
				t.Fatal("invalid manifest accepted")
			}
		})
	}
	if err := Validate(m, ChannelStable, ""); err == nil {
		t.Fatal("edge manifest accepted on the stable channel")
	}
	if err := Validate(m, "stable-2.x", ""); err == nil {
		t.Fatal("1.x manifest accepted on the 2.x channel")
	}
}

func TestPointerPolicy(t *testing.T) {
	for _, v := range []string{"1.0.0-alpha.1", "1.0.0-beta.1", "1.0.0-rc.1", "1.0.0"} {
		m := sampleManifest(t)
		m.Version = v
		m.Channel, _ = VersionChannel(v)
		m.ReleaseBaseURL = versionRoot(v)
		for _, key := range artifactKeys {
			m.Artifacts[key] = Artifact{URL: m.ReleaseBaseURL + "/" + key + "/eregion", SHA256: strings.Repeat("a", 64)}
		}
		for _, channel := range []string{ChannelStable, "stable-1.x", ChannelRC, ChannelEdge} {
			want := channel == m.Channel || (channel == "stable-1.x" && m.Channel == ChannelStable)
			if got := Validate(m, channel, "") == nil; got != want {
				t.Errorf("pointer %s %s accepted = %v", channel, v, got)
			}
		}
		if err := Validate(m, m.Channel, v); err != nil {
			t.Errorf("pinned %s: %v", v, err)
		}
	}
}

func TestHistoricalBetaMetadataIsRejected(t *testing.T) {
	m := sampleManifest(t)
	m.Channel = ChannelStable
	if err := Validate(m, ChannelEdge, m.Version); err == nil {
		t.Fatal("historical signing-family metadata was accepted as logical edge")
	}
}

func TestDecodeManifestIsStrict(t *testing.T) {
	raw, err := json.Marshal(sampleManifest(t))
	if err != nil {
		t.Fatal(err)
	}
	m, err := decodeManifest(raw)
	if err != nil {
		t.Fatalf("valid manifest refused: %v", err)
	}
	if m.Raw == nil || peekVersion(raw) != m.Version {
		t.Fatal("decoded manifest lost its raw bytes or version")
	}

	var fields map[string]any
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	fields["unexpected"] = true
	withUnknown, _ := json.Marshal(fields)
	if _, err := decodeManifest(withUnknown); err == nil {
		t.Fatal("manifest with an unknown field was accepted")
	}
	if _, err := decodeManifest(append(raw, []byte(`{}`)...)); err == nil {
		t.Fatal("manifest with trailing JSON was accepted")
	}
}

func TestStableAndLinePointersRejectPrereleases(t *testing.T) {
	for _, version := range []string{"1.0.0-alpha.1", "1.0.0-beta.1", "1.0.0-rc.1"} {
		m := sampleManifest(t)
		m.Version = version
		m.Channel, _ = VersionChannel(version)
		m.ReleaseBaseURL = versionRoot(version)
		for _, key := range artifactKeys {
			m.Artifacts[key] = Artifact{URL: m.ReleaseBaseURL + "/" + key + "/eregion", SHA256: strings.Repeat("a", 64)}
		}
		for _, channel := range []string{ChannelStable, "stable-1.x"} {
			if err := Validate(m, channel, ""); err == nil {
				t.Errorf("%s pointer accepted signing-family-stable prerelease %s", channel, version)
			}
		}
		if err := Validate(m, m.Channel, version); err != nil {
			t.Errorf("pinned %s was refused: %v", version, err)
		}
	}
}

func TestRCPointerRejectsNonRCVersion(t *testing.T) {
	for _, version := range []string{"1.0.0-alpha.1", "1.0.0-beta.1", "1.0.0"} {
		m := sampleManifest(t)
		m.Version = version
		m.ReleaseBaseURL = versionRoot(version)
		for _, key := range artifactKeys {
			m.Artifacts[key] = Artifact{URL: m.ReleaseBaseURL + "/" + key + "/eregion", SHA256: strings.Repeat("a", 64)}
		}
		if err := Validate(m, ChannelRC, ""); err == nil {
			t.Errorf("rc pointer accepted %s", version)
		}
	}
}
