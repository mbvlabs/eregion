package platform

import (
	"strings"
	"testing"
)

func TestValidateGitSourceURL(t *testing.T) {
	t.Setenv("EREGION_GIT_ALLOWED_HOSTS", "")
	allowed := []string{
		"https://github.com/acme/app.git",
		"https://gist.github.com/acme/app.git",
		"https://gitlab.com/acme/app",
	}
	for _, raw := range allowed {
		if err := ValidateGitSourceURL(raw); err != nil {
			t.Fatalf("allowed URL %q refused: %v", raw, err)
		}
	}
	refused := []string{
		"http://github.com/acme/app.git",
		"https://user:token@github.com/acme/app.git",
		"https://github.com/acme/app.git?ref=main",
		"https://github.com/acme/app.git#main",
		"https://127.0.0.1/acme/app.git",
		"https://[::1]/acme/app.git",
		"https://localhost/acme/app.git",
		"https://git.internal/acme/app.git",
		"https://github.com:8443/acme/app.git",
		"https://evil.example/acme/app.git",
	}
	for _, raw := range refused {
		if err := ValidateGitSourceURL(raw); err == nil {
			t.Fatalf("refused URL %q was accepted", raw)
		}
	}
}

func TestGitAllowlistIsExplicitAndReplacesDefaults(t *testing.T) {
	t.Setenv("EREGION_GIT_ALLOWED_HOSTS", "git.corp.example")
	if err := ValidateGitSourceURL("https://git.corp.example/acme/app.git"); err != nil {
		t.Fatalf("configured host refused: %v", err)
	}
	if err := ValidateGitSourceURL("https://github.com/acme/app.git"); err == nil {
		t.Fatal("an explicit allowlist still accepted a default host")
	}
}

func TestBuildIsolationDisablesGitRedirects(t *testing.T) {
	env, err := buildIsolation(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(env, "\n")
	for _, want := range []string{
		"GIT_CONFIG_KEY_0=http.followRedirects",
		"GIT_CONFIG_VALUE_0=false",
		"GIT_ALLOW_PROTOCOL=https",
		"GIT_TERMINAL_PROMPT=0",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("build isolation is missing %q:\n%s", want, joined)
		}
	}
}
