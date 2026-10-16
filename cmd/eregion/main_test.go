package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestExecutableNoninteractiveContract(t *testing.T) {
	name := "eregion"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	binary := filepath.Join(t.TempDir(), name)
	build := exec.Command("go", "build", "-o", binary, ".")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build: %v\n%s", err, out)
	}
	for _, test := range []struct {
		args []string
		exit int
		json bool
	}{
		{[]string{"version", "--json"}, 0, true},
		{[]string{"--bad", "--json"}, 2, true},
		{[]string{"help", "commands", "--json"}, 0, true},
		{[]string{"version", "--help", "--json"}, 0, true},
		{[]string{"version"}, 0, false},
		{[]string{"deploy"}, 2, false},
	} {
		cmd := exec.Command(binary, test.args...)
		cmd.Env = append(os.Environ(), "TERM=dumb", "NO_COLOR=1", "CI=1")
		cmd.Stdin = strings.NewReader("")
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		code := 0
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			code = exit.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		if code != test.exit || bytes.ContainsRune(stdout.Bytes(), '\x1b') || bytes.ContainsRune(stderr.Bytes(), '\x1b') {
			t.Fatalf("%v: exit %d, stdout=%q stderr=%q", test.args, code, stdout.String(), stderr.String())
		}
		if test.json && (!json.Valid(stdout.Bytes()) || stderr.Len() != 0) {
			t.Fatalf("invalid machine output: %q %q", stdout.String(), stderr.String())
		}
		if !test.json && test.exit != 0 && (stdout.Len() != 0 || stderr.Len() == 0) {
			t.Fatal("plain errors must go to stderr")
		}
	}
}
