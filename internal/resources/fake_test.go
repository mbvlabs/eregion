package resources

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeHost is a deterministic Docker/PostgreSQL command interpreter for unit
// tests. It records every argv and models only the state the service is allowed
// to inspect. Real behaviour is covered by the opt-in Docker integration test.
type fakeHost struct {
	mu           sync.Mutex
	spec         Spec
	nodeIdentity string
	container    bool
	running      bool
	volume       bool
	port         int
	databases    map[string]bool
	dump         []byte
	listing      []byte
	calls        [][]string
	failProbe    string
	failRestore  bool
	waitFirst    bool
}

func newFakeHost(spec Spec) *fakeHost {
	return &fakeHost{spec: spec, nodeIdentity: spec.Host, port: 55432, databases: map[string]bool{}, dump: []byte("fake-pg-dump-archive"), listing: []byte("; archive TOC\n1; 0 0 TABLE public.widgets app\n")}
}

func (f *fakeHost) NodeIdentity(context.Context) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.nodeIdentity == "" {
		return f.spec.Host, nil
	}
	return f.nodeIdentity, nil
}

func (f *fakeHost) Run(ctx context.Context, command Command) (CommandResult, error) {
	if f.waitFirst {
		<-ctx.Done()
		return CommandResult{}, ctx.Err()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, append([]string{command.Name}, command.Args...))
	if command.Name != "docker" {
		return CommandResult{}, fmt.Errorf("unexpected program %q", command.Name)
	}
	args := command.Args
	if len(args) == 0 {
		return CommandResult{ExitCode: 1, Stderr: []byte("empty docker args")}, nil
	}
	switch args[0] {
	case "inspect":
		if !f.container {
			return CommandResult{ExitCode: 1, Stderr: []byte("Error: No such object: " + containerName(f.spec))}, nil
		}
		format := ""
		for index, arg := range args {
			if arg == "--format" && index+1 < len(args) {
				format = args[index+1]
			}
		}
		if strings.Contains(format, "State.Running") {
			return CommandResult{Stdout: []byte(fmt.Sprintf("%t\n", f.running))}, nil
		}
		raw, _ := json.Marshal(ownershipLabels(f.spec))
		return CommandResult{Stdout: append(raw, '\n')}, nil
	case "volume":
		if len(args) < 2 {
			return CommandResult{ExitCode: 1}, nil
		}
		switch args[1] {
		case "inspect":
			if !f.volume {
				return CommandResult{ExitCode: 1, Stderr: []byte("Error: No such volume: " + f.spec.Volume)}, nil
			}
			raw, _ := json.Marshal(ownershipLabels(f.spec))
			return CommandResult{Stdout: append(raw, '\n')}, nil
		case "create":
			f.volume = true
			return CommandResult{Stdout: []byte(f.spec.Volume + "\n")}, nil
		case "rm":
			f.volume = false
			return CommandResult{Stdout: []byte(f.spec.Volume + "\n")}, nil
		}
	case "run":
		f.container, f.running = true, true
		for _, arg := range args {
			if strings.HasPrefix(arg, "POSTGRES_DB=") {
				f.databases[strings.TrimPrefix(arg, "POSTGRES_DB=")] = true
			}
		}
		return CommandResult{Stdout: []byte("container-id\n")}, nil
	case "start":
		f.running = true
		return CommandResult{}, nil
	case "rm":
		f.container, f.running = false, false
		return CommandResult{Stdout: []byte(containerName(f.spec) + "\n")}, nil
	case "port":
		if f.port == 0 {
			return CommandResult{ExitCode: 1}, nil
		}
		return CommandResult{Stdout: []byte(fmt.Sprintf("127.0.0.1:%d\n", f.port))}, nil
	case "exec":
		return f.exec(command)
	}
	return CommandResult{ExitCode: 1, Stderr: []byte("unsupported fake docker command")}, nil
}

func (f *fakeHost) exec(command Command) (CommandResult, error) {
	args := command.Args
	index := 1
	for index < len(args) && args[index] != containerName(f.spec) {
		index++
	}
	index++
	if index >= len(args) {
		return CommandResult{ExitCode: 1, Stderr: []byte("missing exec command")}, nil
	}
	program := args[index]
	tail := args[index+1:]
	switch program {
	case "pg_isready":
		return CommandResult{Stdout: []byte("accepting connections\n")}, nil
	case "pg_dump":
		if command.Stdout == nil {
			return CommandResult{ExitCode: 1, Stderr: []byte("pg_dump stdout was not connected")}, nil
		}
		if _, err := command.Stdout.Write(f.dump); err != nil {
			return CommandResult{}, err
		}
		return CommandResult{}, nil
	case "pg_restore":
		if f.failRestore {
			return CommandResult{ExitCode: 1, Stderr: []byte("injected pg_restore failure")}, nil
		}
		if contains(tail, "--list") {
			return CommandResult{Stdout: f.listing}, nil
		}
		if command.Stdin != nil {
			_, _ = io.Copy(io.Discard, command.Stdin)
		}
		return CommandResult{}, nil
	case "psql":
		return f.psql(tail)
	}
	return CommandResult{ExitCode: 1, Stderr: []byte("unsupported fake exec program " + program)}, nil
}

func (f *fakeHost) psql(args []string) (CommandResult, error) {
	database := ""
	sql := ""
	for _, arg := range args {
		if strings.HasPrefix(arg, "--dbname=") {
			database = strings.TrimPrefix(arg, "--dbname=")
		}
		if strings.HasPrefix(arg, "--command=") {
			sql = strings.TrimPrefix(arg, "--command=")
		}
	}
	if f.failProbe != "" && database == f.failProbe && sql == "SELECT 1" {
		return CommandResult{ExitCode: 1, Stderr: []byte("injected read probe failure")}, nil
	}
	switch {
	case strings.HasPrefix(sql, "SELECT 1 FROM pg_database WHERE datname="):
		quoted := strings.TrimPrefix(sql, "SELECT 1 FROM pg_database WHERE datname=")
		name := unquoteLiteral(quoted)
		if f.databases[name] {
			return CommandResult{Stdout: []byte("1\n")}, nil
		}
		return CommandResult{}, nil
	case strings.HasPrefix(sql, "CREATE DATABASE "):
		name := unquoteIdentifier(strings.TrimPrefix(sql, "CREATE DATABASE "))
		f.databases[name] = true
		return CommandResult{}, nil
	case strings.HasPrefix(sql, "DROP DATABASE IF EXISTS "):
		rest := strings.TrimPrefix(sql, "DROP DATABASE IF EXISTS ")
		name := unquoteIdentifier(strings.Split(rest, " WITH ")[0])
		delete(f.databases, name)
		return CommandResult{}, nil
	case strings.HasPrefix(sql, "ALTER DATABASE "):
		rest := strings.TrimPrefix(sql, "ALTER DATABASE ")
		parts := strings.Split(rest, " RENAME TO ")
		if len(parts) != 2 {
			return CommandResult{ExitCode: 1, Stderr: []byte("bad rename")}, nil
		}
		from := unquoteIdentifier(parts[0])
		to := unquoteIdentifier(parts[1])
		if !f.databases[from] {
			return CommandResult{ExitCode: 1, Stderr: []byte("database does not exist: " + from)}, nil
		}
		delete(f.databases, from)
		f.databases[to] = true
		return CommandResult{}, nil
	case strings.HasPrefix(sql, "SELECT pg_terminate_backend"):
		return CommandResult{Stdout: []byte("t\n")}, nil
	case strings.HasPrefix(sql, "SELECT 1"):
		return CommandResult{Stdout: []byte("1\n")}, nil
	}
	return CommandResult{ExitCode: 1, Stderr: []byte("unsupported fake SQL: " + sql)}, nil
}

func unquoteIdentifier(value string) string {
	return strings.ReplaceAll(strings.Trim(value, `"`), `""`, `"`)
}

func unquoteLiteral(value string) string {
	return strings.ReplaceAll(strings.Trim(value, "'"), "''", "'")
}

func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func (f *fakeHost) callArgs() [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]string, len(f.calls))
	for index, call := range f.calls {
		out[index] = append([]string(nil), call...)
	}
	return out
}

func newTestService(t *testing.T, spec Spec) (*Service, *fakeHost) {
	t.Helper()
	fake := newFakeHost(spec)
	service, err := New(Config{
		Host: spec.Host, StateDir: t.TempDir(), BackupDir: t.TempDir(),
		Runner: fake, CommandTimeout: 5 * time.Second, StartTimeout: 5 * time.Second, PollInterval: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	return service, fake
}

func testSpec() Spec {
	return Spec{
		Name: "appdb", Engine: EnginePostgreSQL, Version: "17", Host: "app-1",
		Address: "127.0.0.1", Database: "app", Username: "app",
		Volume: "eregion-pg-appdb-data", Protection: ProtectionApplication,
	}
}
