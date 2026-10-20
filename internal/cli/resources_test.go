package cli

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/resources"
)

func dataJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func resourceInventoryJSON() json.RawMessage {
	return json.RawMessage(`[
		{
			"spec": {"name":"appdb","engine":"postgresql","version":"17","host":"main","address":"127.0.0.1","database":"app","username":"app","volume":"eregion-pg-appdb-data"},
			"state":"ready",
			"endpoint":{"node":"main","address":"127.0.0.1","port":55432,"database":"app","username":"app","sslmode":"disable"},
			"credential":{"username":"app","database":"app","password_file":"/var/lib/eregion/resources/appdb/password","mode":"0600"},
			"health":{"status":"healthy","checked_at":"2026-10-20T12:00:00Z"}
		}
	]`)
}

func backupInventoryJSON() json.RawMessage {
	return json.RawMessage(`[
		{"archive":"/var/lib/eregion/backups/appdb-abc.dump","manifest":"/var/lib/eregion/backups/appdb-abc.dump.manifest.json","resource":"appdb","host":"main","database":"app","effect_id":"op-backup","size_bytes":1024,"sha256":"abc","created_at":"2026-10-20T12:00:00Z","reason":"manual","verified":true,"verified_at":"2026-10-20T12:01:00Z"}
	]`)
}

func TestResourceCommandMetadataAndDerivedIdempotency(t *testing.T) {
	a := newApp()
	if err := a.meta.validate(a.root); err != nil {
		t.Fatal(err)
	}
	for _, path := range [][]string{
		{"resources", "create"}, {"resources", "list"}, {"resources", "status"},
		{"resources", "credentials"}, {"resources", "connect-info"}, {"resources", "destroy"},
		{"resources", "recover"}, {"resources", "backups", "create"}, {"resources", "backups", "list"},
		{"resources", "backups", "verify"}, {"resources", "backups", "restore"},
		{"resources", "schedule", "set"},
	} {
		cmd, remaining, err := a.root.Find(path)
		if err != nil || len(remaining) != 0 || cmd.RunE == nil {
			t.Fatalf("missing implemented command %s: %v", strings.Join(path, " "), err)
		}
	}

	state, server := newPlatformServer(t)
	platformTestEnvironment(t, server)
	code, stdout, stderr := invokePlatform(t, "resources", "create", "appdb",
		"--host", "main", "--database", "app", "--username", "app",
		"--dry-run", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("dry-run code=%d stderr=%q stdout=%s", code, stderr, stdout)
	}
	decodePlatformEnvelope(t, stdout)
	if len(state.calls) != 1 || !state.calls[0].dryRun || state.calls[0].key != "" {
		t.Fatalf("unexpected dry-run call: %+v", state.calls)
	}
	if state.calls[0].kind != resources.KindResourceCreate || state.calls[0].target != "node/main" {
		t.Fatalf("unexpected action: %+v", state.calls[0])
	}
	var create resources.ResourceCreateInput
	if err := json.Unmarshal(state.calls[0].input, &create); err != nil || create.Spec.Name != "appdb" || create.Spec.Host != "main" {
		t.Fatalf("unexpected input: %s %v", state.calls[0].input, err)
	}

	code, stdout, stderr = invokePlatform(t, "resources", "create", "appdb",
		"--host", "main", "--database", "app", "--username", "app", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("create code=%d stderr=%q stdout=%s", code, stderr, stdout)
	}
	if len(state.calls) != 2 {
		t.Fatalf("calls=%+v", state.calls)
	}
	if !strings.HasPrefix(state.calls[1].key, "resources:"+resources.KindResourceCreate+":") {
		t.Fatalf("idempotency key was not derived: %q", state.calls[1].key)
	}
	if err := contract.ValidateIdempotencyKey(state.calls[1].key); err != nil {
		t.Fatalf("derived key is invalid: %v", err)
	}
}

func TestResourcePreviewPlainRendersNestedPlan(t *testing.T) {
	raw := json.RawMessage(`{
		"kind":"platform.resource.destroy","target":"node/main","dry_run":true,
		"summary":"Destroy integration resource",
		"input":{
			"resource":"appdb","host":"main",
			"steps":[
				{"id":"confirm","description":"Require the exact resource name.","destructive":true},
				{"id":"container","description":"Remove only the owned container.","destructive":true}
			],
			"destructive":true,"confirmation_required":true,"confirmation_phrase":"appdb"
		},
		"warnings":["this cannot be undone"]
	}`)
	text := resourcePreviewPlain(raw, "token-value")
	for _, want := range []string{"Dry run: platform.resource.destroy on node/main", "destructive step: Require the exact resource name.", "confirmation: repeat the exact phrase \"appdb\"", "warning: this cannot be undone", "confirmation_token: token-value"} {
		if !strings.Contains(text, want) {
			t.Fatalf("preview missing %q:\n%s", want, text)
		}
	}
}

func TestResourceSubmissionCanFollowTheSharedViewer(t *testing.T) {
	// operationAPI doubles the submission and follow endpoints. The resource
	// command must hand the accepted operation ID to the existing follower
	// rather than growing a second observation loop.
	newOperationAPI(t, contract.Succeeded)
	code, stdout, stderr := invoke(t, "resources", "create", "appdb",
		"--host", "main", "--database", "app", "--username", "app",
		"--follow", "--idempotency-key", "ops:appdb-create", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("follow code=%d stderr=%q stdout=%s", code, stderr, stdout)
	}
	envelope := decodeOne(t, stdout)
	if !envelope.OK {
		t.Fatalf("follow failed: %+v", envelope.Error)
	}
	raw, _ := json.Marshal(envelope.Data)
	var data struct {
		State    contract.State     `json:"state"`
		Events   uint64             `json:"events"`
		Detached bool               `json:"detached"`
		Op       contract.Operation `json:"operation"`
	}
	if err := json.Unmarshal(raw, &data); err != nil {
		t.Fatal(err)
	}
	if data.State != contract.Succeeded || data.Events != 3 || data.Detached || data.Op.ID != followTestID {
		t.Fatalf("unexpected followed result: %s", raw)
	}
}

func TestResourceDestroyUsesPlanBoundConfirmation(t *testing.T) {
	state, server := newPlatformServer(t)
	state.items["resources"] = resourceInventoryJSON()
	platformTestEnvironment(t, server)

	code, stdout, stderr := invokePlatform(t, "resources", "destroy", "appdb", "--remove-volume", "--json")
	assertPlatformJSONFault(t, code, stdout, stderr, contract.ConfirmationRequired, "--confirm")
	if len(state.calls) != 1 || state.calls[0].method != http.MethodGet {
		t.Fatalf("destroy without confirmation submitted work: %+v", state.calls)
	}

	code, stdout, stderr = invokePlatform(t, "resources", "destroy", "appdb", "--remove-volume", "--dry-run", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("dry-run code=%d stderr=%q stdout=%s", code, stderr, stdout)
	}
	envelope := decodePlatformEnvelope(t, stdout)
	var preview struct {
		Preview struct {
			DryRun bool `json:"dry_run"`
		} `json:"preview"`
		ConfirmationToken string `json:"confirmation_token"`
	}
	if err := json.Unmarshal(dataJSON(t, envelope.Data), &preview); err != nil || preview.ConfirmationToken == "" || !preview.Preview.DryRun {
		t.Fatalf("dry-run did not return a bound token: %s %v", envelope.Data, err)
	}

	code, stdout, stderr = invokePlatform(t, "resources", "destroy", "appdb", "--remove-volume",
		"--confirm", preview.ConfirmationToken, "--idempotency-key", "ops:appdb-destroy", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("confirmed destroy code=%d stderr=%q stdout=%s", code, stderr, stdout)
	}
	last := state.calls[len(state.calls)-1]
	if last.kind != resources.KindResourceDestroy || last.target != "node/main" || last.key != "ops:appdb-destroy" {
		t.Fatalf("unexpected destroy call: %+v", last)
	}
	var destroy resources.ResourceDestroyInput
	if err := json.Unmarshal(last.input, &destroy); err != nil || destroy.Confirmation != "appdb" || !destroy.RemoveVolume {
		t.Fatalf("unexpected destroy input: %s %v", last.input, err)
	}

	submissions := 0
	for _, call := range state.calls {
		if call.method == http.MethodPost {
			submissions++
		}
	}
	code, stdout, stderr = invokePlatform(t, "resources", "destroy", "appdb", "--remove-volume",
		"--confirm", "wrong", "--idempotency-key", "ops:appdb-destroy", "--json")
	assertPlatformJSONFault(t, code, stdout, stderr, contract.PreconditionFailed, "confirmation token")
	after := 0
	for _, call := range state.calls {
		if call.method == http.MethodPost {
			after++
		}
	}
	if after != submissions {
		t.Fatalf("mismatched confirmation reached the API: %+v", state.calls)
	}
}

func TestResourceBackupsAndConnectInfoUseInventory(t *testing.T) {
	state, server := newPlatformServer(t)
	state.items["resources"] = resourceInventoryJSON()
	state.items["backups"] = backupInventoryJSON()
	platformTestEnvironment(t, server)

	code, stdout, stderr := invokePlatform(t, "resources", "backups", "list", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	if len(state.calls) != 1 || state.calls[0].query.Get("collection") != "backups" {
		t.Fatalf("unexpected inventory call: %+v", state.calls)
	}

	code, stdout, stderr = invokePlatform(t, "resources", "connect-info", "appdb", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("connect-info code=%d stderr=%q stdout=%s", code, stderr, stdout)
	}
	envelope := decodePlatformEnvelope(t, stdout)
	if !strings.Contains(string(dataJSON(t, envelope.Data)), "postgres://app@127.0.0.1:55432/app?sslmode=disable") {
		t.Fatalf("connect-info omitted the endpoint: %s", envelope.Data)
	}
	if strings.Contains(string(dataJSON(t, envelope.Data)), "password=") {
		t.Fatalf("connect-info leaked a password: %s", envelope.Data)
	}
	if !strings.Contains(string(dataJSON(t, envelope.Data)), "password_file") {
		t.Fatalf("connect-info omitted the credential reference: %s", envelope.Data)
	}
	encodedInfo := string(dataJSON(t, envelope.Data))
	if !strings.Contains(encodedInfo, "vault:") || !strings.Contains(encodedInfo, "DATABASE_URL") {
		t.Fatalf("connect-info omitted the environment secret reference: %s", encodedInfo)
	}

	code, stdout, stderr = invokePlatform(t, "resources", "backups", "verify", "appdb",
		"--archive", "appdb-abc.dump", "--idempotency-key", "ops:appdb-verify", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("verify code=%d stderr=%q stdout=%s", code, stderr, stdout)
	}
	last := state.calls[len(state.calls)-1]
	if last.kind != resources.KindBackupVerify || last.target != "node/main" {
		t.Fatalf("unexpected verify call: %+v", last)
	}
	var verify resources.BackupVerifyInput
	if err := json.Unmarshal(last.input, &verify); err != nil || verify.Archive != "appdb-abc.dump" || verify.Spec.Name != "appdb" {
		t.Fatalf("unexpected verify input: %s %v", last.input, err)
	}
}

func TestResourceRestoreAndSchedulePolicy(t *testing.T) {
	state, server := newPlatformServer(t)
	state.items["resources"] = resourceInventoryJSON()
	platformTestEnvironment(t, server)

	code, stdout, stderr := invokePlatform(t, "resources", "backups", "restore", "appdb",
		"--archive", "appdb-abc.dump", "--dry-run", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("restore plan code=%d stderr=%q stdout=%s", code, stderr, stdout)
	}
	envelope := decodePlatformEnvelope(t, stdout)
	var preview struct {
		ConfirmationToken string `json:"confirmation_token"`
	}
	if err := json.Unmarshal(dataJSON(t, envelope.Data), &preview); err != nil || preview.ConfirmationToken == "" {
		t.Fatalf("restore plan did not return a token: %s %v", envelope.Data, err)
	}
	code, stdout, stderr = invokePlatform(t, "resources", "backups", "restore", "appdb",
		"--archive", "appdb-abc.dump", "--confirm", preview.ConfirmationToken,
		"--idempotency-key", "ops:appdb-restore", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("restore code=%d stderr=%q stdout=%s", code, stderr, stdout)
	}
	last := state.calls[len(state.calls)-1]
	if last.kind != resources.KindBackupRestore {
		t.Fatalf("unexpected restore call: %+v", last)
	}
	var restore resources.BackupRestoreInput
	if err := json.Unmarshal(last.input, &restore); err != nil || restore.Archive != "appdb-abc.dump" || restore.Confirmation != "appdb" {
		t.Fatalf("unexpected restore input: %s %v", last.input, err)
	}

	code, stdout, stderr = invokePlatform(t, "resources", "schedule", "set", "appdb",
		"--cron", "0 3 * * *", "--timezone", "UTC", "--keep-last", "7", "--verify-after",
		"--idempotency-key", "ops:appdb-schedule", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("schedule code=%d stderr=%q stdout=%s", code, stderr, stdout)
	}
	last = state.calls[len(state.calls)-1]
	if last.kind != resources.KindBackupSchedule || last.target != "node/main" {
		t.Fatalf("unexpected schedule call: %+v", last)
	}
	var schedule resources.BackupScheduleInput
	if err := json.Unmarshal(last.input, &schedule); err != nil || schedule.Policy.Schedule.Cron != "0 3 * * *" || schedule.Policy.Retention.KeepLast != 7 || !schedule.Policy.VerifyAfter {
		t.Fatalf("unexpected schedule input: %s %v", last.input, err)
	}
}
