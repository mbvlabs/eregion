package platform

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/engine"
)

func TestContainerStepAcceptsBareAndPrefixedLabels(t *testing.T) {
	exec := engine.Execution{OperationID: contract.OperationID("op_" + strings.Repeat("a", 32)), Attempt: 1, Step: "platform-container-01-v1"}
	payload := ContainerPayload{
		Step: "platform-container-01-v1", Name: "eregion-test-candidate", Image: "sha256:" + strings.Repeat("b", 64),
		ContainerPort: 8080, HostPort: 30000, Bind: "127.0.0.1",
		Labels: map[string]string{
			"io.eregion.app": "api", "io.eregion.environment": "production",
			"io.eregion.release": "rel_" + strings.Repeat("c", 32), "io.eregion.role": "candidate",
			"com.example/team": "platform",
		},
	}
	raw, _ := json.Marshal(payload)
	if _, err := buildContainerStep(exec, raw); err != nil {
		t.Fatalf("valid labels were refused: %v", err)
	}
	payload.Labels = map[string]string{"bad label": "x"}
	raw, _ = json.Marshal(payload)
	if _, err := buildContainerStep(exec, raw); err == nil {
		t.Fatal("a label with whitespace was accepted")
	}
	payload.Labels = map[string]string{"bad_prefix/name": "x"}
	raw, _ = json.Marshal(payload)
	if _, err := buildContainerStep(exec, raw); err == nil {
		t.Fatal("an invalid DNS prefix was accepted")
	}
}

func TestProcessCommandArgsUseTheLauncherOnlyForBuildpacksImages(t *testing.T) {
	for _, tc := range []struct {
		command    []string
		buildpacks bool
		want       string
	}{
		{nil, false, "img"},
		{[]string{"web"}, true, "--entrypoint /cnb/lifecycle/launcher img -- web"},
		{[]string{"/app/bin/queue", "--workers", "4"}, false, "--entrypoint /app/bin/queue img --workers 4"},
	} {
		if got := strings.Join(processCommandArgs("img", tc.command, tc.buildpacks), " "); got != tc.want {
			t.Errorf("processCommandArgs(%q, %v) = %q, want %q", tc.command, tc.buildpacks, got, tc.want)
		}
	}
}

func TestPackPayloadAcceptsGitContextPathOnlyWithAGitSource(t *testing.T) {
	exec := engine.Execution{OperationID: contract.OperationID("op_" + strings.Repeat("a", 32)), Attempt: 1, Step: "platform-pack-01-v1"}
	payload := PackPayload{
		Step: "platform-pack-01-v1", Image: "eregion/app-production:abc", Builder: DefaultBuilder,
		GitURL: "https://github.com/acme/app.git", GitRef: "main", GitContextPath: "apps/web",
	}
	raw, _ := json.Marshal(payload)
	if _, err := buildPackStep(exec, raw); err != nil {
		t.Fatalf("a git context path was refused: %v", err)
	}
	payload.GitURL = ""
	payload.Path = t.TempDir()
	raw, _ = json.Marshal(payload)
	if _, err := buildPackStep(exec, raw); err == nil || !strings.Contains(err.Error(), "requires a git source") {
		t.Fatalf("a directory source accepted a git context path: %v", err)
	}
	payload.GitURL = "https://github.com/acme/app.git"
	payload.Path = ""
	payload.GitContextPath = "../outside"
	raw, _ = json.Marshal(payload)
	if _, err := buildPackStep(exec, raw); err == nil {
		t.Fatal("a context path that leaves the repository was accepted")
	}
}

func TestResolveGitContextPathStaysInsideTheCheckout(t *testing.T) {
	root := t.TempDir()
	checkout := filepath.Join(root, "checkout")
	if err := os.MkdirAll(filepath.Join(checkout, "apps", "web"), 0o700); err != nil {
		t.Fatal(err)
	}
	realCheckout, err := filepath.EvalSymlinks(checkout)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(root, "outside")
	if err := os.MkdirAll(outside, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "..", "outside"), filepath.Join(checkout, "apps", "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("web", filepath.Join(checkout, "apps", "inside")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(checkout, "apps", "file"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		contextPath string
		want        string
		wantErr     string
	}{
		{contextPath: "", want: realCheckout},
		{contextPath: ".", want: realCheckout},
		{contextPath: "apps/web", want: filepath.Join(realCheckout, "apps", "web")},
		{contextPath: "apps/inside", want: filepath.Join(realCheckout, "apps", "web")},
		{contextPath: "apps/missing", wantErr: "not present"},
		{contextPath: "apps/file", wantErr: "not a directory"},
		{contextPath: "apps/escape", wantErr: "outside the repository"},
		{contextPath: "../outside", wantErr: "outside the repository"},
	} {
		got, err := resolveGitContextPath(checkout, tc.contextPath)
		if tc.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) || !strings.Contains(err.Error(), tc.contextPath) {
				t.Fatalf("context %q: got %q, %v; want an error naming the path and containing %q", tc.contextPath, got, err, tc.wantErr)
			}
			continue
		}
		if err != nil {
			t.Fatalf("context %q: %v", tc.contextPath, err)
		}
		if got != tc.want {
			t.Fatalf("context %q resolved to %q, want %q", tc.contextPath, got, tc.want)
		}
	}
}

func TestContainerAlreadyGone(t *testing.T) {
	for _, output := range []string{
		"Error response from daemon: No such container: eregion-demo-production-web-1",
		"error: no such object: eregion-demo-production-web-1",
	} {
		if !containerAlreadyGone(output) {
			t.Errorf("missing container output was not classified: %q", output)
		}
	}
	for _, output := range []string{
		"Error response from daemon: container is in use",
		"permission denied while trying to connect to the Docker daemon socket",
		"",
	} {
		if containerAlreadyGone(output) {
			t.Errorf("real docker rm failure was classified as missing: %q", output)
		}
	}
}
