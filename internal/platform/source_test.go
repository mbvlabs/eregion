package platform

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
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

func TestMatchGitRefUsesOneExactCommit(t *testing.T) {
	const (
		head    = "1111111111111111111111111111111111111111"
		branch  = "2222222222222222222222222222222222222222"
		tag     = "3333333333333333333333333333333333333333"
		peeled  = "4444444444444444444444444444444444444444"
		ignored = "abcdef"
	)
	output := strings.Join([]string{
		head + "\tHEAD",
		branch + "\trefs/heads/main",
		tag + "\trefs/tags/v1",
		peeled + "\trefs/tags/v1^{}",
		ignored + "\tnot-a-valid-line",
	}, "\n")

	for _, tc := range []struct {
		ref     string
		want    string
		wantErr contract.Code
		wantMsg string
	}{
		{ref: "main", want: branch},
		{ref: "v1", want: peeled},
		{ref: "", want: head},
		{ref: "missing", wantErr: contract.PreconditionFailed, wantMsg: "missing"},
	} {
		got, err := matchGitRef(tc.ref, output)
		if tc.wantErr != "" {
			if contract.Classify(err).Code != tc.wantErr {
				t.Fatalf("ref %q: got %v, want %s", tc.ref, err, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Fatalf("ref %q: error does not name the ref: %v", tc.ref, err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Fatalf("ref %q: got %q, %v; want %q", tc.ref, got, err, tc.want)
		}
	}
}

func TestMatchGitRefRefusesANameThatIsBothBranchAndTag(t *testing.T) {
	output := strings.Join([]string{
		"1111111111111111111111111111111111111111\trefs/heads/release",
		"2222222222222222222222222222222222222222\trefs/tags/release",
	}, "\n")
	_, err := matchGitRef("release", output)
	if code := contract.Classify(err).Code; code != contract.ValidationFailed {
		t.Fatalf("ambiguous ref code = %s, want %s: %v", code, contract.ValidationFailed, err)
	}
}

func TestResolveGitRefReturnsAFullCommitAndRefusesAnAbbreviatedOne(t *testing.T) {
	const sha = "0123456789abcdef0123456789abcdef01234567"
	got, err := ResolveGitRef(context.Background(), "https://github.com/acme/app.git", sha, nil)
	if err != nil || got != sha {
		t.Fatalf("full commit = %q, %v; want %q", got, err, sha)
	}
	_, err = ResolveGitRef(context.Background(), "https://github.com/acme/app.git", sha[:12], nil)
	if code := contract.Classify(err).Code; code != contract.ValidationFailed {
		t.Fatalf("abbreviated commit code = %s, want %s: %v", code, contract.ValidationFailed, err)
	}
}

func TestGitCredentialEnvironmentKeepsTheValueOutOfArgv(t *testing.T) {
	work := t.TempDir()
	env, err := gitCredentialEnvironment(nil, work, "s3cret")
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(env, "\n")
	if !strings.Contains(joined, "EREGION_GIT_PASSWORD=s3cret") {
		t.Fatalf("the credential is not in the child environment:\n%s", joined)
	}
	askpass := filepath.Join(work, "git-askpass.sh")
	if info, err := os.Stat(askpass); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("askpass helper is not private: %v %v", info, err)
	}
	// ResolveGitRef passes only the URL and ref patterns to git; the credential
	// key exists nowhere except this child environment.
	if !strings.Contains(strings.Join(env, " "), "GIT_ASKPASS="+askpass) {
		t.Fatal("the askpass helper is not wired into the child environment")
	}
}

func TestRedactGitStderrKeepsTheCredentialOutOfFaults(t *testing.T) {
	got := redactGitStderr("fatal: authentication failed for token s3cret\nmore", []byte("s3cret"))
	if strings.Contains(got, "s3cret") || !strings.Contains(got, "***") {
		t.Fatalf("stderr hint leaked the credential: %q", got)
	}
	if got != "fatal: authentication failed for token ***" {
		t.Fatalf("hint = %q", got)
	}
	if got := redactGitStderr("", []byte("s3cret")); got != "" {
		t.Fatalf("empty stderr became %q", got)
	}
}

func TestResolveGitRefRefusesARefusedHostAddress(t *testing.T) {
	const (
		sha = "0123456789abcdef0123456789abcdef01234567"
		url = "https://github.com/acme/app.git"
	)
	previous := gitHostLookup
	t.Cleanup(func() { gitHostLookup = previous })

	// A full commit is already exact: it must not resolve the host or run git.
	gitHostLookup = func(context.Context, string) ([]net.IPAddr, error) {
		t.Fatal("a full commit resolved the git host")
		return nil, nil
	}
	if got, err := ResolveGitRef(context.Background(), url, sha, nil); err != nil || got != sha {
		t.Fatalf("full commit = %q, %v; want %q", got, err, sha)
	}

	gitHostLookup = func(context.Context, string) ([]net.IPAddr, error) {
		return []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}, nil
	}
	_, err := ResolveGitRef(context.Background(), url, "main", nil)
	if code := contract.Classify(err).Code; code != contract.PreconditionFailed {
		t.Fatalf("refused address code = %s, want %s: %v", code, contract.PreconditionFailed, err)
	}
	if fault := contract.Classify(err); !strings.Contains(fault.Hint, "non-routable") {
		t.Fatalf("refusal does not name the address rule: %s", fault.Hint)
	}
}
