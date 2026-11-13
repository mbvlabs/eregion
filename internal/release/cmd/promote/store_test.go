package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestProductionConditionalAWSCalls(t *testing.T) {
	dir := t.TempDir()
	aws := `#!/bin/sh
printf '%s\n' "$*" >>"$TEST_CALLS"
case "$TEST_RESPONSE" in
 conflict) echo 'An error occurred (PreconditionFailed) when calling PutObject' >&2; exit 1 ;;
 conflict409) echo 'An error occurred (ConditionalRequestConflict) when calling PutObject' >&2; exit 1 ;;
 denied) echo 'An error occurred (AccessDenied) when calling PutObject' >&2; exit 1 ;;
 missing) echo 'An error occurred (NoSuchKey) when calling GetObject' >&2; exit 1 ;;
esac
if [ "$2" = get-object ]; then
  for arg do dest="$arg"; done
  printf 'pointer bytes' >"$dest"
fi
printf '{"ETag":"\"object-etag\""}\n'
`
	if err := os.WriteFile(filepath.Join(dir, "aws"), []byte(aws), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TEST_CALLS", filepath.Join(dir, "calls"))
	s := &productionStore{bucket: "test", key: "edge/manifest.json", endpoint: "https://example.invalid", manifest: filepath.Join(dir, "candidate")}
	if err := s.write(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	if err := s.write(context.Background(), `"etag"`); err != nil {
		t.Fatal(err)
	}
	calls, err := os.ReadFile(filepath.Join(dir, "calls"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(calls), "--if-none-match *") || !strings.Contains(string(calls), `--if-match "etag"`) {
		t.Fatalf("missing conditions: %s", calls)
	}
	dest := filepath.Join(dir, "out")
	etag, exists, err := s.get(context.Background(), s.key, dest)
	if err != nil || !exists || etag != `"object-etag"` {
		t.Fatalf("get: etag=%q exists=%v err=%v", etag, exists, err)
	}
	body, err := os.ReadFile(dest)
	if err != nil || string(body) != "pointer bytes" {
		t.Fatalf("get body: %q %v", body, err)
	}
	for _, response := range []string{"conflict", "conflict409"} {
		t.Setenv("TEST_RESPONSE", response)
		if err := s.write(context.Background(), `"etag"`); err != errConflict {
			t.Fatalf("%s: %v", response, err)
		}
	}
	t.Setenv("TEST_RESPONSE", "denied")
	if err := s.write(context.Background(), `"etag"`); err == nil || err == errConflict {
		t.Fatalf("access denial: %v", err)
	}
	t.Setenv("TEST_RESPONSE", "missing")
	if _, exists, err := s.get(context.Background(), s.key, filepath.Join(dir, "out")); err != nil || exists {
		t.Fatalf("missing: exists=%v err=%v", exists, err)
	}
	t.Setenv("TEST_RESPONSE", "denied")
	if _, _, err := s.get(context.Background(), s.key, filepath.Join(dir, "out")); err == nil {
		t.Fatal("access denial treated as absence")
	}
}

func TestGitProofAndAncestry(t *testing.T) {
	dir := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	git("config", "user.name", "Fixture")
	git("config", "user.email", "fixture@example.invalid")
	git("commit", "--allow-empty", "-qm", "first")
	first := git("rev-parse", "HEAD")
	git("tag", "v1.0.0-alpha.1")
	git("commit", "--allow-empty", "-qm", "second")
	second := git("rev-parse", "HEAD")
	git("update-ref", "refs/remotes/origin/master", second)
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	for _, tc := range []struct{ version, want string }{
		{"v-unused", ""},
		{"1.0.0-alpha.1", first},
		{"edge-" + second[:12], second},
	} {
		got, err := gitVersionCommit(context.Background(), tc.version)
		if tc.want == "" {
			if err == nil {
				t.Fatalf("missing tag accepted: %s", got)
			}
		} else if err != nil || got != tc.want {
			t.Fatalf("%s: %s %v", tc.version, got, err)
		}
	}
	ok, err := edgePromotes(pointer{version: "1.0.0-alpha.1", commit: first, etag: "a"}, pointer{version: "edge-" + second[:12], commit: second}, gitAncestor)
	if err != nil || !ok {
		t.Fatalf("descendant: %v %v", ok, err)
	}
	ok, err = edgePromotes(pointer{version: "edge-" + first[:12], commit: first, etag: "a"}, pointer{version: "1.0.0-alpha.1", commit: first}, gitAncestor)
	if err != nil || !ok {
		t.Fatalf("same commit tagged release: %v %v", ok, err)
	}
	ok, err = edgePromotes(pointer{version: "edge-" + second[:12], commit: second, etag: "a"}, pointer{version: "1.0.0-alpha.1", commit: first}, gitAncestor)
	if err != nil || ok {
		t.Fatalf("ancestor: %v %v", ok, err)
	}
	git("checkout", "-q", first)
	git("commit", "--allow-empty", "-qm", "divergent")
	divergent := git("rev-parse", "HEAD")
	_, err = edgePromotes(pointer{version: "edge-" + second[:12], commit: second, etag: "a"}, pointer{version: "edge-" + divergent[:12], commit: divergent}, gitAncestor)
	if err == nil {
		t.Fatal("divergent commits accepted")
	}
	if _, err := gitVersionCommit(context.Background(), "edge-"+divergent[:12]); err == nil {
		t.Fatal("commit outside master accepted")
	}
}
