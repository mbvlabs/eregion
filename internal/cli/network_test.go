package cli

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mbvlabs/eregion/internal/integrations"
)

func accessInventoryJSON() json.RawMessage {
	return json.RawMessage(`[
		{"id":"grant_0123456789abcdef0123456789abcdef","subject":"alice","device_id":"laptop-1","routes":["app.internal/api"],"created_by":"admin","created_by_operation":"op_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","created_at":"2026-10-20T12:00:00Z","expires_at":"2026-10-21T12:00:00Z"},
		{"id":"grant_ffffffffffffffffffffffffffffffff","subject":"bob","device_id":"laptop-2","routes":["10.0.0.0/8"],"created_by":"admin","created_by_operation":"op_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb","created_at":"2026-10-20T12:00:00Z","expires_at":"2026-10-21T12:00:00Z","revoked_at":"2026-10-20T13:00:00Z","revoked_by":"admin"}
	]`)
}

func dnsInventoryJSON() json.RawMessage {
	return json.RawMessage(`[
		{"zone_id":"0123456789abcdef0123456789abcdef","zone":"example.com","id":"rec1","name":"app.example.com","type":"A","content":"203.0.113.10","ttl":300,"proxied":false,"comment":"eregion:managed","managed_by":"eregion"},
		{"zone_id":"0123456789abcdef0123456789abcdef","zone":"other.example","id":"rec2","name":"www.other.example","type":"CNAME","content":"app.example.net","ttl":300,"proxied":true}
	]`)
}

func TestNetworkCommandMetadata(t *testing.T) {
	a := newApp()
	if err := a.meta.validate(a.root); err != nil {
		t.Fatal(err)
	}
	for _, path := range [][]string{
		{"dns", "plan"}, {"dns", "reconcile"}, {"dns", "list"},
		{"access", "devices"}, {"access", "grants", "list"},
		{"access", "grants", "grant"}, {"access", "grants", "revoke"},
	} {
		cmd, remaining, err := a.root.Find(path)
		if err != nil || len(remaining) != 0 || cmd.RunE == nil {
			t.Fatalf("missing implemented command %s: %v", strings.Join(path, " "), err)
		}
	}
}

func TestDNSPlanReconcileAndList(t *testing.T) {
	state, server := newPlatformServer(t)
	state.items["dns"] = dnsInventoryJSON()
	platformTestEnvironment(t, server)

	record := `{"name":"app.example.com","type":"A","content":"203.0.113.10","ttl":300}`
	code, stdout, stderr := invokePlatform(t, "dns", "plan",
		"--zone-id", "0123456789abcdef0123456789abcdef", "--zone-name", "example.com",
		"--token-file", "/run/secrets/cloudflare-token", "--record", record, "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("plan code=%d stderr=%q stdout=%s", code, stderr, stdout)
	}
	if len(state.calls) != 1 || !state.calls[0].dryRun || state.calls[0].key != "" {
		t.Fatalf("unexpected plan call: %+v", state.calls)
	}
	call := state.calls[0]
	if call.kind != integrations.KindDNSReconcile || call.target != "platform" {
		t.Fatalf("unexpected DNS action: %+v", call)
	}
	var input struct {
		ZoneID    string                   `json:"zone_id"`
		ZoneName  string                   `json:"zone_name"`
		TokenFile string                   `json:"api_token_file"`
		Records   []integrations.DNSRecord `json:"records"`
	}
	if err := json.Unmarshal(call.input, &input); err != nil || input.ZoneID != "0123456789abcdef0123456789abcdef" || input.TokenFile != "/run/secrets/cloudflare-token" || len(input.Records) != 1 || input.Records[0].Name != "app.example.com" {
		t.Fatalf("unexpected DNS input: %s %v", call.input, err)
	}
	if strings.Contains(string(call.input), "cf-token") {
		t.Fatal("DNS input contained a token value")
	}

	code, stdout, stderr = invokePlatform(t, "dns", "reconcile",
		"--zone-id", "0123456789abcdef0123456789abcdef", "--zone-name", "example.com",
		"--token-file", "/run/secrets/cloudflare-token", "--record", record,
		"--idempotency-key", "ci:dns-1", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("reconcile code=%d stderr=%q stdout=%s", code, stderr, stdout)
	}
	if state.calls[len(state.calls)-1].key != "ci:dns-1" || state.calls[len(state.calls)-1].dryRun {
		t.Fatalf("unexpected reconcile call: %+v", state.calls[len(state.calls)-1])
	}

	code, stdout, stderr = invokePlatform(t, "dns", "list", "--zone", "example.com", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("list code=%d stderr=%q stdout=%s", code, stderr, stdout)
	}
	envelope := decodePlatformEnvelope(t, stdout)
	var page struct {
		Collection string                  `json:"collection"`
		Items      []integrations.DNSRoute `json:"items"`
	}
	if err := json.Unmarshal(dataJSON(t, envelope.Data), &page); err != nil || len(page.Items) != 1 || page.Items[0].Zone != "example.com" || page.Items[0].ManagedBy != "eregion" {
		t.Fatalf("unexpected DNS inventory: %s %v", envelope.Data, err)
	}
}

func TestAccessGrantRevokeAndInventory(t *testing.T) {
	state, server := newPlatformServer(t)
	state.items["access"] = accessInventoryJSON()
	platformTestEnvironment(t, server)

	code, stdout, stderr := invokePlatform(t, "access", "grants", "grant",
		"--subject", "alice", "--device", "laptop-1", "--route", "app.internal/api",
		"--expires-at", "2026-10-21T12:00:00Z", "--note", "drill",
		"--idempotency-key", "ci:access-1", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("grant code=%d stderr=%q stdout=%s", code, stderr, stdout)
	}
	call := state.calls[len(state.calls)-1]
	if call.kind != integrations.KindAccessGrant || call.target != "platform" || call.key != "ci:access-1" {
		t.Fatalf("unexpected grant call: %+v", call)
	}
	var grant integrations.GrantSpec
	if err := json.Unmarshal(call.input, &grant); err != nil || grant.Subject != "alice" || grant.DeviceID != "laptop-1" || len(grant.Routes) != 1 || grant.Note != "drill" {
		t.Fatalf("unexpected grant input: %s %v", call.input, err)
	}

	code, stdout, stderr = invokePlatform(t, "access", "grants", "revoke",
		"--grant-id", "grant_0123456789abcdef0123456789abcdef",
		"--idempotency-key", "ci:access-revoke-1", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("revoke code=%d stderr=%q stdout=%s", code, stderr, stdout)
	}
	call = state.calls[len(state.calls)-1]
	if call.kind != integrations.KindAccessRevoke || !strings.Contains(string(call.input), "grant_0123456789abcdef0123456789abcdef") {
		t.Fatalf("unexpected revoke call: %+v", call)
	}

	code, stdout, stderr = invokePlatform(t, "access", "grants", "list", "--subject", "alice", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("list code=%d stderr=%q stdout=%s", code, stderr, stdout)
	}
	envelope := decodePlatformEnvelope(t, stdout)
	var page struct {
		Grants []integrations.DeviceGrant `json:"grants"`
	}
	if err := json.Unmarshal(dataJSON(t, envelope.Data), &page); err != nil || len(page.Grants) != 1 || page.Grants[0].Subject != "alice" {
		t.Fatalf("unexpected access inventory: %s %v", envelope.Data, err)
	}

	code, stdout, stderr = invokePlatform(t, "access", "devices", "--json")
	if code != 0 || stderr != "" {
		t.Fatalf("devices code=%d stderr=%q stdout=%s", code, stderr, stdout)
	}
	if !strings.Contains(stdout, "laptop-1") || !strings.Contains(stdout, "laptop-2") {
		t.Fatalf("devices output omitted a device: %s", stdout)
	}
}

func TestAccessGrantRequiresDryRunCompatibleFlags(t *testing.T) {
	state, server := newPlatformServer(t)
	platformTestEnvironment(t, server)
	code, stdout, stderr := invokePlatform(t, "access", "grants", "grant", "--subject", "alice", "--json")
	if code == 0 || stderr != "" {
		t.Fatalf("missing flags succeeded: code=%d stderr=%q stdout=%s", code, stderr, stdout)
	}
	if len(state.calls) != 0 {
		t.Fatalf("invalid grant reached the API: %+v", state.calls)
	}
}
