package controlplane

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/operations"
	"golang.org/x/crypto/ssh"
)

// executableRegistryIdentity observes only the same public host key that the
// production worker requires. It never creates or reads a private host key.
func executableRegistryIdentity(t *testing.T, nodeDir string) RegistryHostIdentity {
	t.Helper()
	matches, err := filepath.Glob("/etc/ssh/ssh_host_*_key.pub")
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(matches)
	if len(matches) == 0 {
		t.Skip("the production worker requires an installed SSH public host key")
	}
	raw, err := os.ReadFile(matches[0])
	if err != nil {
		t.Fatalf("read installed public host key: %v", err)
	}
	key, _, _, _, err := ssh.ParseAuthorizedKey(raw)
	if err != nil {
		t.Fatalf("parse installed public host key: %v", err)
	}
	identity := RegistryHostIdentity{
		NodeName: "local", HostKeyType: key.Type(),
		HostKeyFingerprint: ssh.FingerprintSHA256(key), StateDir: nodeDir,
	}
	if err := identity.Validate(); err != nil {
		t.Fatal(err)
	}
	return identity
}

type executableProcess struct {
	cmd     *exec.Cmd
	done    <-chan error
	stopped bool
}

type executableLifecycleFixture struct {
	pool        *pgxpool.Pool
	ctx         context.Context
	binary      string
	endpoint    string
	credential  string
	environment []string
	nodeDir     string
}

func TestIntegrationExecutableLifecycle(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("node service processes require Linux")
	}
	if os.Getenv("EREGION_TEST_DATABASE_URL") == "" {
		t.Skip("set EREGION_TEST_DATABASE_URL to run isolated PostgreSQL integration tests")
	}
	f := newExecutableLifecycleFixture(t)
	server := startExecutableProcess(t, f, "node", "serve", "--workers=false", "--listen", strings.TrimPrefix(f.endpoint, "http://"), "--json")
	awaitExecutableReady(t, f)
	client, err := api.NewClient(f.endpoint, f.credential)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := client.Test(f.ctx, operations.TestInput{Steps: 5, StepDelayMS: 25}, "cli:durable-1", false)
	if err != nil {
		t.Fatal(err)
	}
	var accepted contract.Operation
	if err := json.Unmarshal(raw, &accepted); err != nil || accepted.State != contract.Queued {
		t.Fatalf("API did not durably queue: %s %v", raw, err)
	}
	worker := startExecutableProcess(t, f, "node", "worker", "--concurrency", "2", "--json")
	await(t, 30*time.Second, func() bool {
		var state contract.State
		err := f.pool.QueryRow(f.ctx, `SELECT state FROM eregion_operations WHERE id=$1`, accepted.ID).Scan(&state)
		return err == nil && state == contract.Succeeded
	})
	if got := effectCount(t, f.nodeDir, accepted.ID, 5); got != 5 {
		t.Fatalf("executable lifecycle effects: %d", got)
	}
	if got := completedStepRows(t, f.pool, accepted.ID); got != 5 {
		t.Fatalf("executable lifecycle completed steps: %d", got)
	}
	stopExecutableProcesses(t, worker, server)
}

func newExecutableLifecycleFixture(t *testing.T) executableLifecycleFixture {
	t.Helper()
	nodeDir := t.TempDir()
	identity := executableRegistryIdentity(t, nodeDir)
	pool, dsn := testPool(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	t.Cleanup(cancel)
	binary := filepath.Join(t.TempDir(), "eregion")
	if out, err := exec.CommandContext(ctx, "go", "build", "-o", binary, "../../cmd/eregion").CombinedOutput(); err != nil {
		t.Fatalf("build executable: %v: %s", err, out)
	}
	credential := strings.Repeat("e", 64)
	hash := sha256.Sum256([]byte(credential))
	authFile := filepath.Join(t.TempDir(), "auth.json")
	raw, err := json.Marshal([]api.Principal{{Actor: testActor, TokenSHA256: hex.EncodeToString(hash[:]), Scopes: []string{"operations:submit", "operations:read", "operations:cancel", "system:read"}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(authFile, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	nodeConfigPath := filepath.Join(t.TempDir(), "node.json")
	nodeConfig, err := json.Marshal(map[string]string{"node": "local", "state_dir": nodeDir})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(nodeConfigPath, nodeConfig, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := AssignWorkspace(ctx, newTestEntities(t, pool), "test", identity); err != nil {
		t.Fatal(err)
	}
	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reserved.Addr().String()
	if err := reserved.Close(); err != nil {
		t.Fatal(err)
	}
	home := t.TempDir()
	return executableLifecycleFixture{
		pool: pool, ctx: ctx, binary: binary, endpoint: "http://" + address,
		credential: credential, nodeDir: nodeDir,
		environment: []string{
			"PATH=" + os.Getenv("PATH"), "HOME=" + home, "TMPDIR=" + home,
			"EREGION_DATABASE_URL=" + dsn, "EREGION_AUTH_FILE=" + authFile,
			"EREGION_NODE_STATE_DIR=" + nodeDir, "EREGION_NODE_NAME=local",
			"EREGION_NODE_CONFIG=" + nodeConfigPath, "EREGION_NODE_TRANSPORT=local",
			"EREGION_REGISTRY_LISTEN=127.0.0.1:5000",
		},
	}
}

func startExecutableProcess(t *testing.T, f executableLifecycleFixture, args ...string) *executableProcess {
	t.Helper()
	command := exec.CommandContext(f.ctx, f.binary, args...)
	command.Env = f.environment
	command.Stdout, command.Stderr = io.Discard, io.Discard
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	p := &executableProcess{cmd: command, done: done}
	go func() { done <- command.Wait() }()
	t.Cleanup(func() {
		if !p.stopped {
			_ = p.cmd.Process.Kill()
			select {
			case <-p.done:
			case <-time.After(5 * time.Second):
				t.Error("could not reap node service process")
			}
		}
	})
	return p
}

func awaitExecutableReady(t *testing.T, f executableLifecycleFixture) {
	t.Helper()
	httpClient := &http.Client{Timeout: time.Second}
	await(t, 15*time.Second, func() bool {
		request, err := http.NewRequestWithContext(f.ctx, http.MethodGet, f.endpoint+"/readyz", nil)
		if err != nil {
			t.Fatal(err)
		}
		response, err := httpClient.Do(request)
		if err != nil {
			return false
		}
		_ = response.Body.Close()
		return response.StatusCode == http.StatusOK
	})
}

func stopExecutableProcesses(t *testing.T, processes ...*executableProcess) {
	t.Helper()
	for _, p := range processes {
		if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
			t.Fatal(err)
		}
		select {
		case err := <-p.done:
			p.stopped = true
			if err != nil {
				t.Fatalf("unclean service shutdown: %v", err)
			}
		case <-time.After(20 * time.Second):
			t.Fatal("service ignored graceful shutdown")
		}
	}
}
