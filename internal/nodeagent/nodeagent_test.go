package nodeagent

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/buildinfo"
)

func TestVersionUsesBuildMetadata(t *testing.T) {
	original := buildinfo.Version
	t.Cleanup(func() { buildinfo.Version = original })
	buildinfo.Version = "1.2.3"

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"version"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("version exit = %d stderr=%s", code, stderr.String())
	}
	if !strings.HasPrefix(stdout.String(), "eregion node-agent 1.2.3\n") {
		t.Fatalf("plain version output = %q", stdout.String())
	}

	stdout.Reset()
	if code := Run([]string{"version", "--json"}, strings.NewReader(""), &stdout, &stderr); code != 0 {
		t.Fatalf("version --json exit = %d stderr=%s", code, stderr.String())
	}
	var info struct {
		Version string `json:"version"`
		Commit  string `json:"commit"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &info); err != nil {
		t.Fatalf("json version output = %q: %v", stdout.String(), err)
	}
	if info.Version != "1.2.3" || info.Commit == "" {
		t.Fatalf("version metadata = %+v", info)
	}
}

func TestVersionRejectsUnknownArguments(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"version", "extra"}, strings.NewReader(""), &stdout, &stderr); code == 0 {
		t.Fatal("version accepted a positional argument")
	}
}
