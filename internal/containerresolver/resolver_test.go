package containerresolver

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeExecutable(t *testing.T, path string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), mode); err != nil {
		t.Fatal(err)
	}
}

func TestResolveSearchOrder(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "first", "migrate")
	second := filepath.Join(root, "second", "migrate")
	writeExecutable(t, first, 0o644)
	writeExecutable(t, second, 0o755)

	got, err := Resolve("migrate", filepath.Join(root, "first")+":"+filepath.Join(root, "second"), root)
	if err != nil {
		t.Fatal(err)
	}
	if got != second {
		t.Fatalf("Resolve() = %q, want %q", got, second)
	}
}

func TestResolveRelativeAndEmptyEntries(t *testing.T) {
	cwd := t.TempDir()
	writeExecutable(t, filepath.Join(cwd, "bin", "migrate"), 0o755)
	writeExecutable(t, filepath.Join(cwd, "worker"), 0o755)

	got, err := Resolve("migrate", "bin:/unused", cwd)
	if err != nil || got != filepath.Join(cwd, "bin", "migrate") {
		t.Fatalf("relative PATH Resolve() = %q, %v", got, err)
	}
	got, err = Resolve("worker", ":/unused", cwd)
	if err != nil || got != filepath.Join(cwd, "worker") {
		t.Fatalf("empty-entry PATH Resolve() = %q, %v", got, err)
	}
}

func TestResolveRelativePathBeforeCwdFallback(t *testing.T) {
	cwd := t.TempDir()
	writeExecutable(t, filepath.Join(cwd, "bin", "migrate"), 0o755)
	writeExecutable(t, filepath.Join(cwd, "migrate"), 0o755)
	got, err := Resolve("migrate", "bin:/usr/bin", cwd)
	if err != nil || got != filepath.Join(cwd, "bin", "migrate") {
		t.Fatalf("Resolve() = %q, %v; want the relative bin entry before cwd fallback", got, err)
	}
}

func TestResolvePathBeforeCwdFallback(t *testing.T) {
	cwd := t.TempDir()
	writeExecutable(t, filepath.Join(cwd, "migrate"), 0o755)
	pathDir := filepath.Join(t.TempDir(), "migrate")
	writeExecutable(t, pathDir, 0o755)
	got, err := Resolve("migrate", filepath.Dir(pathDir), cwd)
	if err != nil || got != pathDir {
		t.Fatalf("Resolve() = %q, %v; want PATH before cwd fallback", got, err)
	}
}

func TestResolveCwdFallback(t *testing.T) {
	cwd := t.TempDir()
	writeExecutable(t, filepath.Join(cwd, "migrate"), 0o755)
	got, err := Resolve("migrate", "/does/not/exist", cwd)
	if err != nil || got != filepath.Join(cwd, "migrate") {
		t.Fatalf("cwd fallback Resolve() = %q, %v", got, err)
	}
}

func TestResolveSymlinkAndDotDotAreNotCleaned(t *testing.T) {
	root := t.TempDir()
	realApp := filepath.Join(root, "real", "app")
	if err := os.MkdirAll(realApp, 0o755); err != nil {
		t.Fatal(err)
	}
	writeExecutable(t, filepath.Join(root, "real", "bin", "migrate"), 0o755)
	link := filepath.Join(root, "link")
	if err := os.Symlink(realApp, link); err != nil {
		t.Fatal(err)
	}

	got, err := Resolve("migrate", "../bin", link)
	if err != nil {
		t.Fatal(err)
	}
	want := link + "/../bin/migrate"
	if got != want {
		t.Fatalf("Resolve() = %q, want uncleaned %q", got, want)
	}
}

func TestResolveSkipsDirectoriesAndNonExecutableFiles(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "dir", "migrate")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(root, "plain", "migrate")
	writeExecutable(t, plain, 0o644)
	good := filepath.Join(root, "good", "migrate")
	writeExecutable(t, good, 0o755)

	got, err := Resolve("migrate", filepath.Join(root, "dir")+":"+filepath.Join(root, "plain")+":"+filepath.Join(root, "good"), root)
	if err != nil || got != good {
		t.Fatalf("Resolve() = %q, %v; want %q", got, err, good)
	}
}

func TestResolveNotFound(t *testing.T) {
	if _, err := Resolve("missing", t.TempDir(), t.TempDir()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Resolve() error = %v, want ErrNotFound", err)
	}
}

func TestRunPreservesArgvEnvironmentAndUsesNoShell(t *testing.T) {
	cwd := t.TempDir()
	writeExecutable(t, filepath.Join(cwd, "bin", "migrate"), 0o755)
	var gotPath string
	var gotArgv, gotEnv []string
	fakeExec := func(path string, argv []string, env []string) error {
		gotPath, gotArgv, gotEnv = path, append([]string(nil), argv...), append([]string(nil), env...)
		return nil
	}
	env := []string{"PATH=bin", "SECRET=value"}
	var stderr bytes.Buffer
	code := runWith([]string{"migrate", "--yes"}, cwd, "bin", env, fakeExec, &stderr)
	if code != 0 {
		t.Fatalf("runWith() = %d, stderr %q", code, stderr.String())
	}
	if gotPath != filepath.Join(cwd, "bin", "migrate") {
		t.Fatalf("exec path = %q", gotPath)
	}
	if strings.Join(gotArgv, "\x00") != "migrate\x00--yes" {
		t.Fatalf("argv = %q, want original argv", gotArgv)
	}
	if strings.Join(gotEnv, "\x00") != strings.Join(env, "\x00") {
		t.Fatalf("env = %q, want unchanged %q", gotEnv, env)
	}
	for _, arg := range gotArgv {
		if arg == "/bin/sh" || arg == "sh" || arg == "-c" {
			t.Fatalf("resolver introduced a shell: %q", gotArgv)
		}
	}
}

func TestRunExplicitPathExecsExactArgv(t *testing.T) {
	var gotPath string
	var gotArgv []string
	fakeExec := func(path string, argv []string, _ []string) error {
		gotPath, gotArgv = path, append([]string(nil), argv...)
		return nil
	}
	var stderr bytes.Buffer
	code := runWith([]string{"/bin/echo", "hello"}, t.TempDir(), "", []string{"PATH=/usr/bin"}, fakeExec, &stderr)
	if code != 0 {
		t.Fatalf("runWith() = %d, stderr %q", code, stderr.String())
	}
	if gotPath != "/bin/echo" || strings.Join(gotArgv, "\x00") != "/bin/echo\x00hello" {
		t.Fatalf("exec = %q %q", gotPath, gotArgv)
	}
}

func TestRunExitCodes(t *testing.T) {
	var stderr bytes.Buffer
	if code := runWith([]string{"missing"}, t.TempDir(), "/missing", []string{"PATH=/missing"}, func(string, []string, []string) error { return nil }, &stderr); code != 127 {
		t.Fatalf("not-found exit = %d, want 127; stderr %q", code, stderr.String())
	}
	if strings.Contains(stderr.String(), "missing: missing:") {
		t.Fatalf("resolver duplicated the command in its error: %q", stderr.String())
	}
	cwd := t.TempDir()
	writeExecutable(t, filepath.Join(cwd, "migrate"), 0o755)
	stderr.Reset()
	if code := runWith([]string{"migrate"}, cwd, "", []string{"PATH="}, func(string, []string, []string) error {
		return errors.New("exec failed")
	}, &stderr); code != 126 {
		t.Fatalf("exec-error exit = %d, want 126; stderr %q", code, stderr.String())
	}
}

func TestIsInvocation(t *testing.T) {
	if !IsInvocation([]string{Invocation, "migrate"}) {
		t.Fatal("valid invocation was not recognized")
	}
	if IsInvocation(nil) || IsInvocation([]string{"migrate"}) {
		t.Fatal("invalid invocation was recognized")
	}
}
