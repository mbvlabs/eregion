package integrations

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
)

const (
	// KindDNSReconcile applies one zone's desired DNS records through Cloudflare.
	KindDNSReconcile = "platform.dns.reconcile"
	// KindAccessGrant creates an audited private-device grant.
	KindAccessGrant = "platform.access.grant"
	// KindAccessRevoke revokes an audited private-device grant.
	KindAccessRevoke = "platform.access.revoke"

	InventoryCollection = "access"
)

// RuntimeConfig wires durable integration actions. Nil dependencies fail closed
// at execution time; package initialization never requires external services.
type RuntimeConfig struct {
	Authorizer        Authorizer
	Audit             AuditSink
	Access            *AccessController
	CloudflareBaseURL string
	CloudflareClient  *http.Client
}

type dnsInput struct {
	target    platform.Target
	ZoneID    string      `json:"zone_id"`
	ZoneName  string      `json:"zone_name"`
	TokenFile string      `json:"api_token_file"`
	Records   []DNSRecord `json:"records"`
}

func (i dnsInput) Target() platform.Target { return i.target }
func (i dnsInput) Preview() any {
	return map[string]any{
		"zone_id": i.ZoneID, "zone_name": i.ZoneName, "api_token_file": i.TokenFile,
		"records": i.Records,
		"note":    "api_token_file is a path reference; its contents are never part of action input.",
	}
}

type grantInput struct {
	target platform.Target
	GrantSpec
}

func (i grantInput) Target() platform.Target { return i.target }
func (i grantInput) Preview() any {
	return map[string]any{
		"subject": i.GrantSpec.Subject,
		"device":  i.GrantSpec.DeviceID,
		"routes":  i.GrantSpec.Routes,
		"note":    "Access grants are audited; secrets stay in secret references.",
	}
}

type revokeInput struct {
	target  platform.Target
	GrantID string `json:"grant_id"`
}

func (i revokeInput) Target() platform.Target { return i.target }
func (i revokeInput) Preview() any {
	return map[string]any{"grant_id": i.GrantID}
}

// Configure registers DNS and access actions plus the access inventory
// collection. Call once before the API or worker starts. It does not connect to
// Cloudflare or require an access controller to exist.
func Configure(reg *platform.Registry, cfg RuntimeConfig) error {
	if reg == nil {
		reg = platform.Default()
	}
	executor := &actionExecutor{runtime: cfg}
	if err := reg.Register(platform.Definition{
		Kind: KindDNSReconcile, Summary: "Reconcile managed Cloudflare DNS records",
		Normalize: normalizeDNS, Executor: executor,
	}); err != nil {
		return err
	}
	if err := reg.Register(platform.Definition{
		Kind: KindAccessGrant, Summary: "Grant device access to exact private routes",
		Normalize: normalizeGrant, Executor: executor,
	}); err != nil {
		return err
	}
	if err := reg.Register(platform.Definition{
		Kind: KindAccessRevoke, Summary: "Revoke a private-device access grant",
		Normalize: normalizeRevoke, Executor: executor,
	}); err != nil {
		return err
	}
	return reg.RegisterInventory(accessInventory{access: cfg.Access})
}

func normalizeDNS(target platform.Target, raw json.RawMessage) (platform.ActionInput, error) {
	if target.Kind != platform.TargetPlatform {
		return nil, contract.NewFault(contract.ValidationFailed, "DNS reconciliation target must be platform", "Use platform; the zone and records belong to the action input.")
	}
	var input dnsInput
	input.target = target
	if err := strictActionDecode(raw, &input); err != nil {
		return nil, err
	}
	if err := ValidateSecretRef(input.TokenFile); err != nil {
		return nil, err
	}
	if !zoneIDPattern.MatchString(input.ZoneID) {
		return nil, contract.NewFault(contract.ValidationFailed, "invalid Cloudflare zone ID", "Use the lowercase 32-character zone identifier.")
	}
	input.ZoneName = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(input.ZoneName), "."))
	if !dnsNamePattern.MatchString(input.ZoneName) || !strings.Contains(input.ZoneName, ".") {
		return nil, contract.NewFault(contract.ValidationFailed, "invalid Cloudflare zone name", "Use a lowercase DNS name such as example.com.")
	}
	records, err := normalizeDesiredRecords(input.ZoneName, input.Records)
	if err != nil {
		return nil, err
	}
	input.Records = records
	return input, nil
}

func normalizeGrant(target platform.Target, raw json.RawMessage) (platform.ActionInput, error) {
	if target.Kind != platform.TargetPlatform {
		return nil, contract.NewFault(contract.ValidationFailed, "access grant target must be platform", "Use platform; subject, device, and routes belong to the action input.")
	}
	var input grantInput
	input.target = target
	if err := strictActionDecode(raw, &input); err != nil {
		return nil, err
	}
	if !subjectPattern.MatchString(input.Subject) {
		return nil, contract.NewFault(contract.ValidationFailed, "invalid access subject", "Use a nonempty authenticated actor subject of at most 256 bytes.")
	}
	if !deviceIDPattern.MatchString(input.DeviceID) {
		return nil, contract.NewFault(contract.ValidationFailed, "invalid private device ID", "Use 1–128 lowercase letters, digits, dots, underscores, colons, or hyphens.")
	}
	if len(input.Routes) == 0 || len(input.Routes) > 32 {
		return nil, contract.NewFault(contract.ValidationFailed, "invalid access route count", "Grant 1–32 exact routes.")
	}
	grant := DeviceGrant{}
	if err := validateRoutes(&grant, input.Routes); err != nil {
		return nil, err
	}
	input.Routes = grant.Routes
	if len(input.Note) > 256 || strings.ContainsAny(input.Note, "\r\n\x00") {
		return nil, contract.NewFault(contract.ValidationFailed, "invalid access grant note", "Use at most 256 bytes without control characters.")
	}
	if !input.ExpiresAt.IsZero() && !input.ExpiresAt.After(time.Now().UTC()) {
		return nil, contract.NewFault(contract.ValidationFailed, "access grant expiry must be in the future", "Use a future expiry within the configured maximum.")
	}
	return input, nil
}

func normalizeRevoke(target platform.Target, raw json.RawMessage) (platform.ActionInput, error) {
	if target.Kind != platform.TargetPlatform {
		return nil, contract.NewFault(contract.ValidationFailed, "access revoke target must be platform", "Use platform; the grant ID belongs to the action input.")
	}
	var input revokeInput
	input.target = target
	if err := strictActionDecode(raw, &input); err != nil {
		return nil, err
	}
	if !grantIDPattern.MatchString(input.GrantID) {
		return nil, contract.NewFault(contract.ValidationFailed, "invalid access grant ID", "Use the opaque grant ID returned by the grant action.")
	}
	return input, nil
}

func strictActionDecode(raw json.RawMessage, value any) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage(`{}`)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return contract.NewFault(contract.ValidationFailed, "invalid integration action input", "Use the documented fields; unknown fields are rejected.")
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return contract.NewFault(contract.ValidationFailed, "integration action input has trailing data", "Provide exactly one JSON object.")
	}
	return nil
}

type actionExecutor struct {
	runtime RuntimeConfig
}

func (e *actionExecutor) Execute(ctx context.Context, exec platform.Execution) (platform.Result, error) {
	switch exec.Kind {
	case KindDNSReconcile:
		return e.dns(ctx, exec)
	case KindAccessGrant:
		return e.grant(ctx, exec)
	case KindAccessRevoke:
		return e.revoke(ctx, exec)
	default:
		return platform.Result{}, contract.NewFault(contract.ValidationFailed, "unsupported integration action", "Use a documented platform.dns.* or platform.access.* kind.")
	}
}

func (e *actionExecutor) dns(ctx context.Context, exec platform.Execution) (platform.Result, error) {
	if e.runtime.Authorizer == nil || e.runtime.Audit == nil {
		return platform.Result{}, contract.NewFault(contract.Unavailable, "DNS reconciliation is not configured", "Wire explicit authorization and a durable audit sink; DNS changes fail closed.")
	}
	var input dnsInput
	if err := strictActionDecode(exec.Input, &input); err != nil {
		return platform.Result{}, err
	}
	target, err := platform.ParseTarget(exec.Target)
	if err != nil || target.Kind != platform.TargetPlatform {
		return platform.Result{}, contract.NewFault(contract.ValidationFailed, "invalid DNS target", "Use platform.")
	}
	input.target = target
	raw, err := ReadSecretFile(input.TokenFile, 4096)
	if err != nil {
		return platform.Result{}, err
	}
	token := strings.TrimRight(string(raw), "\r\n")
	client, err := NewCloudflare(CloudflareConfig{
		APIToken: token, ZoneID: input.ZoneID, ZoneName: input.ZoneName,
		BaseURL: e.runtime.CloudflareBaseURL, HTTPClient: e.runtime.CloudflareClient,
		Authorizer: e.runtime.Authorizer, Audit: e.runtime.Audit,
	})
	if err != nil {
		return platform.Result{}, err
	}
	if exec.Report != nil {
		exec.Report(platform.Event{Level: "info", Stage: "preflight", Message: "Authorizing and planning managed DNS changes."})
	}
	report, err := client.ReconcileOperation(ctx, exec.Actor, exec.OperationID, input.Records)
	if err != nil {
		return platform.Result{}, err
	}
	return platform.Result{
		Message: "DNS reconciliation completed.",
		Data: map[string]any{
			"zone": report.Zone, "operation_id": report.OperationID,
			"changes": len(report.Changes), "applied": report.Applied, "unchanged": report.Unchanged,
		},
	}, nil
}

func (e *actionExecutor) grant(ctx context.Context, exec platform.Execution) (platform.Result, error) {
	if e.runtime.Access == nil {
		return platform.Result{}, contract.NewFault(contract.Unavailable, "private access control is not configured", "Wire the audited access controller before submitting grants.")
	}
	var input grantInput
	if err := strictActionDecode(exec.Input, &input); err != nil {
		return platform.Result{}, err
	}
	target, err := platform.ParseTarget(exec.Target)
	if err != nil || target.Kind != platform.TargetPlatform {
		return platform.Result{}, contract.NewFault(contract.ValidationFailed, "invalid access target", "Use platform.")
	}
	input.target = target
	grant, err := e.runtime.Access.CreateGrantForOperation(ctx, exec.Actor, exec.OperationID, input.GrantSpec)
	if err != nil {
		return platform.Result{}, err
	}
	return platform.Result{
		Message: "Private access grant created.",
		Data: map[string]any{
			"id": grant.ID, "subject": grant.Subject, "device_id": grant.DeviceID,
			"routes": grant.Routes, "expires_at": grant.ExpiresAt,
		},
	}, nil
}

func (e *actionExecutor) revoke(ctx context.Context, exec platform.Execution) (platform.Result, error) {
	if e.runtime.Access == nil {
		return platform.Result{}, contract.NewFault(contract.Unavailable, "private access control is not configured", "Wire the audited access controller before revoking grants.")
	}
	var input revokeInput
	if err := strictActionDecode(exec.Input, &input); err != nil {
		return platform.Result{}, err
	}
	target, err := platform.ParseTarget(exec.Target)
	if err != nil || target.Kind != platform.TargetPlatform {
		return platform.Result{}, contract.NewFault(contract.ValidationFailed, "invalid access target", "Use platform.")
	}
	input.target = target
	grant, err := e.runtime.Access.RevokeGrantForOperation(ctx, exec.Actor, exec.OperationID, input.GrantID)
	if err != nil {
		return platform.Result{}, err
	}
	return platform.Result{
		Message: "Private access grant revoked.",
		Data:    map[string]any{"id": grant.ID, "revoked_at": grant.RevokedAt},
	}, nil
}

type accessInventory struct {
	access *AccessController
}

func (accessInventory) Collection() string { return InventoryCollection }

func (p accessInventory) Read(ctx context.Context, query platform.InventoryQuery) (any, error) {
	limit := boundedLimit(query.Limit)
	if p.access == nil {
		return []DeviceGrant{}, nil
	}
	items, err := p.access.ListGrants(ctx, "", "")
	if err != nil {
		return nil, err
	}
	if len(items) > limit {
		items = items[:limit]
	}
	return items, nil
}

func boundedLimit(limit int) int {
	if limit < 1 || limit > 200 {
		return 50
	}
	return limit
}
