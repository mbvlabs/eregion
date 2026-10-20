package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/host"
	"github.com/mbvlabs/eregion/internal/secrets"
)

func TestWizardRequiresTypedApply(t *testing.T) {
	cfg := host.DefaultConfig(host.RoleApplication, "app1")
	model := wizardModel{
		app: &app{secrets: &secrets.Registry{}}, cmd: nil,
		fields: bootstrapWizardFields(cfg), phase: wizardReview,
		input: newWizardInput("", 32),
	}
	updated, _ := model.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	next := updated.(wizardModel)
	if next.phase != wizardReview || next.err == nil {
		t.Fatalf("empty confirmation was accepted: %+v", next)
	}
	model.input.Set("apply")
	updated, command := model.handleKey(tea.KeyPressMsg{Code: tea.KeyEnter})
	next = updated.(wizardModel)
	if next.phase != wizardApplying || command == nil {
		t.Fatalf("typed confirmation did not start apply: %+v", next)
	}
}

func TestInteractiveBootstrapRequiresATerminal(t *testing.T) {
	code, _, _ := invoke(t, "bootstrap", "--interactive")
	if code != 2 {
		t.Fatalf("non-TTY guided bootstrap: code=%d", code)
	}
}

func TestBootstrapAndContextAreAttached(t *testing.T) {
	for _, path := range [][]string{{"bootstrap"}, {"bootstrap", "plan"}, {"bootstrap", "apply"}, {"bootstrap", "confirm"}, {"bootstrap", "remote"}, {"context"}, {"context", "login"}, {"context", "credential", "add"}} {
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

func TestBootstrapWizardFlagParity(t *testing.T) {
	a := newApp()
	cmd, _, err := a.root.Find([]string{"bootstrap"})
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, name := range bootstrapWizardFlagNames {
		if flagFor(cmd, name) == nil {
			t.Fatalf("guided field --%s has no noninteractive flag", name)
		}
		seen[name] = true
	}
	for _, field := range bootstrapWizardFields(host.DefaultConfig(host.RoleMain, "main")) {
		if !seen[field.Key] {
			t.Fatalf("guided field %s is not in the parity list", field.Key)
		}
	}
}

func TestContextClientPrecedence(t *testing.T) {
	dir := t.TempDir()
	contextPath := filepath.Join(dir, "contexts.json")
	store, err := host.LoadContextStore(contextPath)
	if err != nil {
		t.Fatal(err)
	}
	var savedHits, envHits atomic.Int32
	saved := operationServer(t, &savedHits)
	explicit := operationServer(t, &envHits)
	savedToken := strings.Repeat("s", 32)
	envToken := strings.Repeat("e", 32)
	if _, err := store.Login("saved", saved.URL, savedToken, nil); err != nil {
		t.Fatal(err)
	}
	t.Setenv("EREGION_CONTEXT_FILE", contextPath)
	t.Setenv("EREGION_API_URL", "")
	t.Setenv("EREGION_API_TOKEN", "")
	if code, stdout, stderr := invoke(t, "operations", "get", string(followTestID), "--json"); code != 0 {
		t.Fatalf("saved context: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if savedHits.Load() != 1 || envHits.Load() != 0 {
		t.Fatalf("saved context was not selected: saved=%d env=%d", savedHits.Load(), envHits.Load())
	}

	t.Setenv("EREGION_API_URL", explicit.URL)
	t.Setenv("EREGION_API_TOKEN", envToken)
	if code, stdout, stderr := invoke(t, "operations", "get", string(followTestID), "--json"); code != 0 {
		t.Fatalf("environment: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if envHits.Load() != 1 || savedHits.Load() != 1 {
		t.Fatalf("environment did not win: saved=%d env=%d", savedHits.Load(), envHits.Load())
	}

	// An explicit origin without a token must not receive the saved token.
	t.Setenv("EREGION_API_TOKEN", "")
	if code, _, _ := invoke(t, "operations", "get", string(followTestID), "--api-url", explicit.URL, "--json"); code != 2 {
		t.Fatalf("saved credential leaked to another origin: code=%d", code)
	}
	if envHits.Load() != 1 {
		t.Fatal("request reached the explicit origin without a credential")
	}

	// The same origin may reuse the saved token.
	if code, stdout, stderr := invoke(t, "operations", "get", string(followTestID), "--api-url", saved.URL, "--json"); code != 0 {
		t.Fatalf("same origin: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if savedHits.Load() != 2 {
		t.Fatalf("saved token not reused for the same origin: %d", savedHits.Load())
	}

	// A flag wins over the environment.
	t.Setenv("EREGION_API_URL", explicit.URL)
	t.Setenv("EREGION_API_TOKEN", envToken)
	if code, stdout, stderr := invoke(t, "operations", "get", string(followTestID), "--api-url", saved.URL, "--json"); code != 0 {
		t.Fatalf("flag: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if savedHits.Load() != 3 {
		t.Fatalf("flag did not win: %d", savedHits.Load())
	}
}

func operationServer(t *testing.T, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		writeTestEnvelope(w, followTestOperation(contract.Succeeded))
	}))
	t.Cleanup(server.Close)
	return server
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
		t.Fatal("credential plan leaked the operator token")
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
	cfg.OperatorTokenOut = "/var/lib/eregion/bootstrap/operator.handoff"
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
