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
		Channel:        ChannelStable,
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
	if err := Validate(m, ChannelStable, ""); err != nil {
		t.Fatalf("valid manifest refused: %v", err)
	}
	if err := Validate(m, "stable-1.x", "1.0.0-alpha.2"); err != nil {
		t.Fatalf("valid line manifest refused: %v", err)
	}

	cases := map[string]func(*Manifest){
		"wrong channel":    func(m *Manifest) { m.Channel = ChannelEdge },
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
			if err := Validate(broken, ChannelStable, ""); err == nil {
				t.Fatal("invalid manifest accepted")
			}
		})
	}
	if err := Validate(m, ChannelEdge, ""); err == nil {
		t.Fatal("stable manifest accepted on the edge channel")
	}
	if err := Validate(m, "stable-2.x", ""); err == nil {
		t.Fatal("1.x manifest accepted on the 2.x channel")
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
