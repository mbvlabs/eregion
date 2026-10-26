package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/host"
)

func TestBootstrapAndContextAreAttached(t *testing.T) {
	for _, path := range [][]string{{"bootstrap"}, {"bootstrap", "plan"}, {"bootstrap", "apply"}, {"bootstrap", "confirm"}, {"bootstrap", "remote"}, {"context"}, {"context", "credential", "add"}} {
		code, stdout, stderr := invoke(t, append([]string{"commands", "--path", strings.Join(path, " ")}, "--json")...)
		if code != 0 {
			t.Fatalf("%v: code=%d stdout=%q stderr=%q", path, code, stdout, stderr)
		}
		envelope := decodeOne(t, stdout)
		if !envelope.OK {
			t.Fatalf("%v: %+v", path, envelope.Error)
		}
	}
}

func TestContextClientRequiresSavedContext(t *testing.T) {
	t.Setenv("EREGION_CONTEXT_FILE", filepath.Join(t.TempDir(), "contexts.json"))
	if code, _, _ := invoke(t, "operations", "get", string(followTestID), "--json"); code != 3 {
		t.Fatalf("missing context: code=%d", code)
	}
}

func TestContextCredentialAddIsScopedAndBound(t *testing.T) {
	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")
	operator := strings.Repeat("o", 43)
	hash := sha256.Sum256([]byte(operator))
	if err := host.WriteAuthFile(authPath, []api.Principal{{
		Actor: contract.Actor{Kind: "operator", Subject: "operator"}, TokenSHA256: hex.EncodeToString(hash[:]), Scopes: host.KnownScopes(),
	}}, "", ""); err != nil {
		t.Fatal(err)
	}
	tokenOut := filepath.Join(dir, "ci.token")
	args := []string{"context", "credential", "add", "--auth-file", authPath, "--subject", "ci-deploy", "--scope", "operations:read", "--token-out", tokenOut, "--dry-run", "--json"}
	code, stdout, stderr := invoke(t, args...)
	if code != 0 {
		t.Fatalf("dry-run: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	envelope := decodeOne(t, stdout)
	data, _ := json.Marshal(envelope.Data)
	var plan struct {
		BaseRevision string `json:"base_revision"`
		Confirmation struct {
			Token string `json:"token"`
		} `json:"confirmation"`
	}
	if err := json.Unmarshal(data, &plan); err != nil || plan.BaseRevision == "" || plan.Confirmation.Token == "" {
		t.Fatalf("bad credential plan: %s %v", data, err)
	}
	if strings.Contains(string(data), tokenOut) && strings.Contains(string(data), operator) {
		t.Fatal("credential plan leaked the generated token")
	}
	if code, _, _ := invoke(t, "context", "credential", "add", "--auth-file", authPath, "--subject", "ci-deploy", "--scope", "operations:read", "--token-out", tokenOut, "--json"); code != 5 {
		t.Fatalf("unconfirmed credential add: code=%d", code)
	}
	code, stdout, stderr = invoke(t, "context", "credential", "add", "--auth-file", authPath, "--subject", "ci-deploy", "--scope", "operations:read", "--token-out", tokenOut, "--confirm", plan.Confirmation.Token, "--json")
	if code != 0 {
		t.Fatalf("confirmed credential add: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	principals, err := host.ReadAuthFile(authPath)
	if err != nil || len(principals) != 2 {
		t.Fatalf("auth file: %+v %v", principals, err)
	}
	raw, err := os.ReadFile(tokenOut)
	if err != nil || !host.ValidToken(strings.TrimSpace(string(raw))) {
		t.Fatalf("token output: %v", err)
	}
	info, _ := os.Stat(tokenOut)
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("token mode %o", info.Mode().Perm())
	}
	encoded, _ := json.Marshal(decodeOne(t, stdout).Data)
	if strings.Contains(string(encoded), strings.TrimSpace(string(raw))) {
		t.Fatal("credential result leaked the token")
	}
}

func TestContextCredentialAddStoresActionGrants(t *testing.T) {
	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")
	operator := strings.Repeat("o", 43)
	hash := sha256.Sum256([]byte(operator))
	if err := host.WriteAuthFile(authPath, []api.Principal{{Actor: contract.Actor{Kind: "operator", Subject: "operator"}, TokenSHA256: hex.EncodeToString(hash[:]), Scopes: host.KnownScopes()}}, "", ""); err != nil {
		t.Fatal(err)
	}
	tokenOut := filepath.Join(dir, "ci.token")
	base := []string{"context", "credential", "add", "--auth-file", authPath, "--subject", "ci-deploy",
		"--scope", "operations:submit",
		"--action-grant", "platform.deploy:env/api/production",
		"--action-grant", "platform.release.create:env/api/*",
		"--token-out", tokenOut, "--dry-run", "--json"}
	code, stdout, stderr := invoke(t, base...)
	if code != 0 {
		t.Fatalf("dry-run: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	envelope := decodeOne(t, stdout)
	data, _ := json.Marshal(envelope.Data)
	if !bytes.Contains(data, []byte("platform.deploy")) || !bytes.Contains(data, []byte("env/api/*")) {
		t.Fatalf("plan omitted action grants: %s", data)
	}
	var plan struct {
		Confirmation struct {
			Token string `json:"token"`
		} `json:"confirmation"`
	}
	if err := json.Unmarshal(data, &plan); err != nil || plan.Confirmation.Token == "" {
		t.Fatalf("plan: %s %v", data, err)
	}
	confirm := append([]string(nil), base...)
	confirm = confirm[:len(confirm)-2] // drop --dry-run/--json
	confirm = append(confirm, "--confirm", plan.Confirmation.Token, "--json")
	code, stdout, stderr = invoke(t, confirm...)
	if code != 0 {
		t.Fatalf("apply: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	principals, err := host.ReadAuthFile(authPath)
	if err != nil || len(principals) != 2 {
		t.Fatalf("auth file: %+v %v", principals, err)
	}
	grants := principals[1].ActionGrants
	if len(grants) != 2 || grants[0].Kind != "platform.deploy" || grants[1].Target != "env/api/*" {
		t.Fatalf("stored grants: %+v", grants)
	}
	if _, err := api.NewAuth(principals); err != nil {
		t.Fatalf("server rejected the created credential: %v", err)
	}
	raw, _ := os.ReadFile(tokenOut)
	if bytes.Contains([]byte(stdout+stderr), bytes.TrimSpace(raw)) {
		t.Fatal("credential output leaked the token")
	}
	if code, _, _ := invoke(t, "context", "credential", "add", "--auth-file", authPath, "--subject", "bad",
		"--scope", "operations:submit", "--action-grant", "platform.deploy:env/*/production", "--token-out", tokenOut, "--dry-run", "--json"); code == 0 {
		t.Fatal("an ambiguous action grant was accepted")
	}
}

func TestRemoteBundleNeverPassesSecretsInArguments(t *testing.T) {
	dir := t.TempDir()
	binary := filepath.Join(dir, "eregion")
	if err := os.WriteFile(binary, []byte("binary"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := host.DefaultConfig(host.RoleMain, "main")
	cfg.BinaryPath = binary
	cfg.DatabaseURL = "postgres://u:private-password@127.0.0.1/test?sslmode=disable"
	files, args, err := remoteBundle(cfg, "/var/lib/eregion/bootstrap/remote/test")
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, file := range files {
		if file.Name == "database-url" {
			found = true
			if file.Mode.Perm() != 0o600 {
				t.Fatalf("database file mode %o", file.Mode.Perm())
			}
		}
	}
	if !found {
		t.Fatal("database URL file was not uploaded")
	}
	joined := strings.Join(args, " ")
	if strings.Contains(joined, "private-password") || strings.Contains(joined, cfg.DatabaseURL) {
		t.Fatalf("secret in remote arguments: %s", joined)
	}
	command := remoteCommand("/var/lib/eregion/bootstrap/remote/test", "plan", args, nil)
	if strings.Contains(command, "private-password") {
		t.Fatal("secret in remote command")
	}
	if !strings.Contains(command, "'--database-url-file'") {
		t.Fatalf("database flag missing: %s", command)
	}
}
