package integrations

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
)

const (
	ActionDNSCreate = "dns.create"
	ActionDNSUpdate = "dns.update"
	ActionDNSDelete = "dns.delete"

	dnsOwnership = "eregion:managed"
)

type DNSRecord struct {
	ID      string `json:"id,omitempty"`
	Name    string `json:"name"`
	Type    string `json:"type"`
	Content string `json:"content"`
	TTL     int    `json:"ttl"`
	Proxied bool   `json:"proxied"`
	Comment string `json:"comment,omitempty"`
}

type DNSChange struct {
	Action string     `json:"action"`
	Before *DNSRecord `json:"before,omitempty"`
	After  *DNSRecord `json:"after,omitempty"`
}

type DNSReport struct {
	Zone        string      `json:"zone"`
	OperationID string      `json:"operation_id,omitempty"`
	Changes     []DNSChange `json:"changes"`
	Applied     int         `json:"applied"`
	Unchanged   int         `json:"unchanged"`
	FetchedAt   time.Time   `json:"fetched_at"`
}

type CloudflareConfig struct {
	APIToken         string
	ZoneID           string
	ZoneName         string
	BaseURL          string
	HTTPClient       *http.Client
	Authorizer       Authorizer
	Audit            AuditSink
	MaxResponseBytes int64
	Now              func() time.Time
}

type Cloudflare struct {
	token    string
	zoneID   string
	zoneName string
	base     string
	http     *http.Client
	authz    Authorizer
	audit    AuditSink
	maxBody  int64
	now      func() time.Time
}

var (
	zoneIDPattern  = regexp.MustCompile(`^[0-9a-f]{32}$`)
	dnsNamePattern = regexp.MustCompile(`^[a-z0-9_]([a-z0-9_.-]{0,251}[a-z0-9])?$`)
	cnamePattern   = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)
)

func NewCloudflare(cfg CloudflareConfig) (*Cloudflare, error) {
	if len(cfg.APIToken) < 20 || len(cfg.APIToken) > 512 || strings.TrimSpace(cfg.APIToken) != cfg.APIToken || strings.ContainsAny(cfg.APIToken, "\r\n") {
		return nil, contract.NewFault(contract.InputRequired, "a Cloudflare API token is required", "Supply it from a private credential file, never from action input or a command line.")
	}
	if !zoneIDPattern.MatchString(cfg.ZoneID) {
		return nil, contract.NewFault(contract.ValidationFailed, "invalid Cloudflare zone ID", "Use the lowercase 32-character zone identifier.")
	}
	zoneName := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(cfg.ZoneName), "."))
	if !dnsNamePattern.MatchString(zoneName) || !strings.Contains(zoneName, ".") {
		return nil, contract.NewFault(contract.ValidationFailed, "invalid Cloudflare zone name", "Use a lowercase DNS name such as example.com.")
	}
	base := cfg.BaseURL
	if base == "" {
		base = "https://api.cloudflare.com/client/v4"
	}
	parsed, err := url.Parse(base)
	if err != nil || parsed.User != nil || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLoopbackHost(parsed.Hostname()))) {
		return nil, contract.NewFault(contract.ValidationFailed, "invalid Cloudflare API URL", "Use HTTPS, or plain HTTP only for a loopback test endpoint.")
	}
	if cfg.Authorizer == nil {
		return nil, contract.NewFault(contract.ValidationFailed, "Cloudflare mutations require an authorizer", "Configure explicit authorization; DNS changes fail closed.")
	}
	if cfg.Audit == nil {
		return nil, contract.NewFault(contract.ValidationFailed, "Cloudflare mutations require an audit sink", "Configure durable auditing; DNS changes fail closed.")
	}
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 15 * time.Second}
	}
	// The API token is attached to every request. Never let an injected client
	// follow a redirect and forward that token to another origin.
	clientCopy := *client
	clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	client = &clientCopy
	maxBody := cfg.MaxResponseBytes
	if maxBody == 0 {
		maxBody = 8 << 20
	}
	if maxBody < 1024 || maxBody > 32<<20 {
		return nil, contract.NewFault(contract.ValidationFailed, "invalid Cloudflare response bound", "Use 1 KiB–32 MiB.")
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	return &Cloudflare{
		token: cfg.APIToken, zoneID: cfg.ZoneID, zoneName: zoneName,
		base: strings.TrimSuffix(parsed.String(), "/"), http: client,
		authz: cfg.Authorizer, audit: cfg.Audit, maxBody: maxBody, now: now,
	}, nil
}

func isLoopbackHost(host string) bool {
	ip := net.ParseIP(host)
	return host == "localhost" || (ip != nil && ip.IsLoopback())
}

// Reconcile creates, updates, and deletes only records carrying the eregion
// ownership marker. Unmanaged records are never modified or deleted. Every
// planned mutation is authorized and audited before the first HTTP mutation.
func (c *Cloudflare) Reconcile(ctx context.Context, actor contract.Actor, desired []DNSRecord) (DNSReport, error) {
	return c.ReconcileOperation(ctx, actor, "", desired)
}

// ReconcileOperation is the durable action path. The stable operation ID is
// written into the ownership comment of every managed record, which makes a
// timed-out create/update identifiable and convergent when the same operation is
// retried after an acknowledgement loss. The ID is also recorded in every audit
// entry for the plan.
func (c *Cloudflare) ReconcileOperation(ctx context.Context, actor contract.Actor, opID contract.OperationID, desired []DNSRecord) (DNSReport, error) {
	report := DNSReport{Zone: c.zoneName, OperationID: string(opID), FetchedAt: c.now().UTC()}
	if err := actor.Validate(); err != nil {
		return report, err
	}
	if opID != "" {
		if err := opID.Validate(); err != nil {
			return report, err
		}
	}
	normalized, err := c.normalizeDesired(desired)
	if err != nil {
		return report, err
	}
	actual, err := c.listRecords(ctx)
	if err != nil {
		return report, err
	}
	changes, unchanged, err := c.plan(actual, normalized, opID)
	if err != nil {
		return report, err
	}
	report.Changes = changes
	report.Unchanged = unchanged
	if len(changes) == 0 {
		return report, nil
	}
	for _, change := range changes {
		action, target := changeAction(change)
		if err := c.authz.Authorize(ctx, actor, action, target); err != nil {
			_ = c.audit.Record(ctx, AuditEntry{At: c.now().UTC(), Actor: actor.Subject, Action: action, Target: target, Outcome: "denied", Detail: auditDetail(opID, "authorization denied")})
			return report, err
		}
		if err := c.audit.Record(ctx, AuditEntry{At: c.now().UTC(), Actor: actor.Subject, Action: action, Target: target, Outcome: "intent", Detail: string(opID)}); err != nil {
			return report, err
		}
	}
	for index, change := range changes {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		action, target := changeAction(change)
		if err := c.apply(ctx, change); err != nil {
			_ = c.audit.Record(ctx, AuditEntry{At: c.now().UTC(), Actor: actor.Subject, Action: action, Target: target, Outcome: "failed", Detail: string(opID)})
			return report, err
		}
		report.Applied++
		if err := c.audit.Record(ctx, AuditEntry{At: c.now().UTC(), Actor: actor.Subject, Action: action, Target: target, Outcome: "succeeded", Detail: string(opID)}); err != nil {
			// The mutation happened but cannot be audited. Stop; a retry
			// re-reads Cloudflare state and converges, but the caller must not
			// be told the whole reconciliation was fully audited.
			return report, contract.NewFault(contract.Unavailable, "DNS change was applied but its audit record failed", "Stop and inspect the audit sink before continuing; do not assume remaining changes were applied.")
		}
		_ = index
	}
	return report, nil
}

func changeAction(change DNSChange) (string, string) {
	switch change.Action {
	case "create":
		return ActionDNSCreate, change.After.Name + "/" + change.After.Type
	case "update":
		return ActionDNSUpdate, change.After.Name + "/" + change.After.Type
	default:
		return ActionDNSDelete, change.Before.Name + "/" + change.Before.Type
	}
}

func (c *Cloudflare) normalizeDesired(records []DNSRecord) ([]DNSRecord, error) {
	return normalizeDesiredRecords(c.zoneName, records)
}

func normalizeDesiredRecords(zoneName string, records []DNSRecord) ([]DNSRecord, error) {
	if len(records) == 0 {
		return nil, contract.NewFault(contract.ValidationFailed, "at least one DNS record is required", "Reconciling an empty desired set would delete every managed record; list records explicitly.")
	}
	if len(records) > 200 {
		return nil, contract.NewFault(contract.ValidationFailed, "too many desired DNS records", "Limit one reconciliation to 200 records.")
	}
	seen := map[string]bool{}
	result := make([]DNSRecord, 0, len(records))
	for _, record := range records {
		if record.ID != "" {
			return nil, contract.NewFault(contract.ValidationFailed, "desired DNS records cannot carry IDs", "Reconciliation matches records by name and type and owns only records with the eregion marker.")
		}
		name := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(record.Name), "."))
		if name == "@" {
			name = zoneName
		}
		if name != zoneName && !strings.HasSuffix(name, "."+zoneName) {
			return nil, contract.NewFault(contract.ValidationFailed, "DNS record is outside the configured zone", "Desired names must equal the zone or be a name inside it.")
		}
		if !dnsNamePattern.MatchString(name) {
			return nil, contract.NewFault(contract.ValidationFailed, "invalid DNS record name", "Use lowercase DNS labels without control or whitespace characters.")
		}
		record.Name = name
		record.Type = strings.ToUpper(strings.TrimSpace(record.Type))
		switch record.Type {
		case "A":
			ip := net.ParseIP(record.Content)
			if ip == nil || ip.To4() == nil {
				return nil, contract.NewFault(contract.ValidationFailed, "invalid A record content", "Use an IPv4 address.")
			}
			record.Content = ip.To4().String()
		case "AAAA":
			ip := net.ParseIP(record.Content)
			if ip == nil || ip.To4() != nil || ip.To16() == nil {
				return nil, contract.NewFault(contract.ValidationFailed, "invalid AAAA record content", "Use an IPv6 address.")
			}
			record.Content = ip.String()
		case "CNAME":
			record.Content = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(record.Content), "."))
			if !cnamePattern.MatchString(record.Content) || !strings.Contains(record.Content, ".") {
				return nil, contract.NewFault(contract.ValidationFailed, "invalid CNAME content", "Use a lowercase fully qualified target name.")
			}
		case "TXT":
			if record.Content == "" || len(record.Content) > 2048 || strings.ContainsAny(record.Content, "\r\n\x00") {
				return nil, contract.NewFault(contract.ValidationFailed, "invalid TXT content", "Use 1–2048 bytes without control characters.")
			}
		default:
			return nil, contract.NewFault(contract.ValidationFailed, "unsupported DNS record type", "Use A, AAAA, CNAME, or TXT; add a deliberate extension for other types.")
		}
		if record.Proxied && record.Type == "TXT" {
			return nil, contract.NewFault(contract.ValidationFailed, "TXT records cannot be proxied", "Disable proxying for TXT.")
		}
		if record.TTL == 0 {
			record.TTL = 1
		}
		if record.TTL != 1 && (record.TTL < 60 || record.TTL > 86400) {
			return nil, contract.NewFault(contract.ValidationFailed, "invalid DNS TTL", "Use 1 (automatic) or 60–86400 seconds.")
		}
		record.Comment = strings.TrimSpace(record.Comment)
		if len(record.Comment) > 256 || strings.ContainsAny(record.Comment, "\r\n\x00") {
			return nil, contract.NewFault(contract.ValidationFailed, "invalid DNS comment", "Use at most 256 bytes without control characters.")
		}
		key := name + "\x00" + record.Type
		if seen[key] {
			return nil, contract.NewFault(contract.ValidationFailed, "duplicate DNS name and type", "Describe at most one desired record per name/type pair.")
		}
		seen[key] = true
		result = append(result, record)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Name == result[j].Name {
			return result[i].Type < result[j].Type
		}
		return result[i].Name < result[j].Name
	})
	return result, nil
}

func managed(record DNSRecord) bool {
	return strings.HasPrefix(strings.TrimSpace(record.Comment), dnsOwnership)
}

func (c *Cloudflare) desiredComment(opID contract.OperationID, comment string) string {
	marker := dnsOwnership
	if opID != "" {
		marker += " op=" + string(opID)
	}
	if comment != "" {
		marker += "; " + comment
	}
	if len(marker) > 256 {
		marker = marker[:256]
	}
	return marker
}

func auditDetail(opID contract.OperationID, detail string) string {
	if opID == "" {
		return detail
	}
	if detail == "" {
		return string(opID)
	}
	return string(opID) + ": " + detail
}

func (c *Cloudflare) plan(actual, desired []DNSRecord, opID contract.OperationID) ([]DNSChange, int, error) {
	byKey := map[string][]DNSRecord{}
	for _, record := range actual {
		key := record.Name + "\x00" + record.Type
		byKey[key] = append(byKey[key], record)
	}
	desiredKeys := map[string]bool{}
	var changes []DNSChange
	unchanged := 0
	for _, want := range desired {
		key := want.Name + "\x00" + want.Type
		desiredKeys[key] = true
		existing := byKey[key]
		var managedRecords []DNSRecord
		for _, record := range existing {
			if managed(record) {
				managedRecords = append(managedRecords, record)
			}
		}
		if len(managedRecords) > 1 {
			return nil, 0, contract.NewFault(contract.PreconditionFailed, "multiple managed DNS records share a name and type", "Resolve the duplicate records in Cloudflare before reconciling.")
		}
		if len(managedRecords) == 0 {
			if len(existing) > 0 {
				return nil, 0, contract.NewFault(contract.PreconditionFailed, "unmanaged DNS record occupies the desired name and type", "Inspect the existing record; eregion never overwrites records it does not own.")
			}
			want.Comment = c.desiredComment(opID, want.Comment)
			changes = append(changes, DNSChange{Action: "create", After: &want})
			continue
		}
		current := managedRecords[0]
		want.ID = current.ID
		want.Comment = c.desiredComment(opID, want.Comment)
		if current.Content == want.Content && current.TTL == want.TTL && current.Proxied == want.Proxied && current.Comment == want.Comment {
			unchanged++
			continue
		}
		before := current
		changes = append(changes, DNSChange{Action: "update", Before: &before, After: &want})
	}
	for _, record := range actual {
		key := record.Name + "\x00" + record.Type
		if !managed(record) || desiredKeys[key] {
			continue
		}
		before := record
		changes = append(changes, DNSChange{Action: "delete", Before: &before})
	}
	sort.Slice(changes, func(i, j int) bool {
		left, right := changes[i], changes[j]
		leftName := ""
		if left.After != nil {
			leftName = left.After.Name + "/" + left.After.Type
		} else if left.Before != nil {
			leftName = left.Before.Name + "/" + left.Before.Type
		}
		rightName := ""
		if right.After != nil {
			rightName = right.After.Name + "/" + right.After.Type
		} else if right.Before != nil {
			rightName = right.Before.Name + "/" + right.Before.Type
		}
		if leftName == rightName {
			return left.Action < right.Action
		}
		return leftName < rightName
	})
	return changes, unchanged, nil
}

func (c *Cloudflare) apply(ctx context.Context, change DNSChange) error {
	switch change.Action {
	case "create":
		body := map[string]any{
			"name": change.After.Name, "type": change.After.Type, "content": change.After.Content,
			"ttl": change.After.TTL, "proxied": change.After.Proxied, "comment": change.After.Comment,
		}
		var created DNSRecord
		if err := c.request(ctx, http.MethodPost, c.zonePath()+"/dns_records", body, &created); err != nil {
			return err
		}
		return nil
	case "update":
		body := map[string]any{
			"name": change.After.Name, "type": change.After.Type, "content": change.After.Content,
			"ttl": change.After.TTL, "proxied": change.After.Proxied, "comment": change.After.Comment,
		}
		var updated DNSRecord
		return c.request(ctx, http.MethodPut, c.zonePath()+"/dns_records/"+url.PathEscape(change.After.ID), body, &updated)
	case "delete":
		return c.request(ctx, http.MethodDelete, c.zonePath()+"/dns_records/"+url.PathEscape(change.Before.ID), nil, nil)
	default:
		return contract.NewFault(contract.Internal, "unknown DNS plan action", "Do not apply this plan.")
	}
}

func (c *Cloudflare) zonePath() string {
	return "/zones/" + c.zoneID
}

func (c *Cloudflare) listRecords(ctx context.Context) ([]DNSRecord, error) {
	var result []DNSRecord
	for page := 1; page <= 100; page++ {
		endpoint := fmt.Sprintf("%s%s/dns_records?per_page=100&page=%d", c.base, c.zonePath(), page)
		var records []DNSRecord
		var info struct {
			Page       int `json:"page"`
			TotalPages int `json:"total_pages"`
		}
		if err := c.requestWithInfo(ctx, http.MethodGet, endpoint, nil, &records, &info); err != nil {
			return nil, err
		}
		result = append(result, records...)
		if info.TotalPages > page {
			continue
		}
		if info.TotalPages == 0 && len(records) == 100 {
			// Older or proxied API responses may omit result_info. Keep
			// paging while full pages arrive, bounded by the loop above.
			continue
		}
		break
	}
	if len(result) > 10000 {
		return nil, contract.NewFault(contract.Unavailable, "Cloudflare returned too many DNS records", "Reduce the zone size or add pagination limits before reconciling.")
	}
	return result, nil
}

type cloudflareError struct {
	Code int `json:"code"`
}

type cloudflareEnvelope struct {
	Success    bool              `json:"success"`
	Errors     []cloudflareError `json:"errors"`
	Result     json.RawMessage   `json:"result"`
	ResultInfo json.RawMessage   `json:"result_info"`
}

func (c *Cloudflare) request(ctx context.Context, method, path string, body any, target any) error {
	return c.requestWithInfo(ctx, method, c.base+path, body, target, nil)
}

func (c *Cloudflare) requestWithInfo(ctx context.Context, method, endpoint string, body any, target any, info any) error {
	var reader io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return contract.NewFault(contract.Internal, "could not encode DNS request", "Do not retry with a fresh key.")
		}
		reader = bytes.NewReader(raw)
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return contract.NewFault(contract.Internal, "could not construct Cloudflare request", "Check the configured API URL.")
	}
	request.Header.Set("Authorization", "Bearer "+c.token)
	request.Header.Set("User-Agent", "eregion")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return contract.NewFault(contract.Unavailable, "Cloudflare API is unreachable", "Retry through the same operation; reconciliation is idempotent.")
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(response.Body, c.maxBody+1))
	if err != nil {
		return contract.NewFault(contract.Unavailable, "Cloudflare response could not be read", "Retry through the same operation; reconciliation is idempotent.")
	}
	if int64(len(raw)) > c.maxBody {
		return contract.NewFault(contract.Unavailable, "Cloudflare response exceeded the bound", "Check the Cloudflare API and retry.")
	}
	if response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden {
		return contract.NewFault(contract.Forbidden, "Cloudflare rejected the API token", "Rotate the credential file and retry; no DNS change was authorized by Cloudflare.")
	}
	if response.StatusCode == http.StatusTooManyRequests {
		return contract.NewFault(contract.Unavailable, "Cloudflare rate limited the request", "Retry the operation later; reconciliation is idempotent.")
	}
	if response.StatusCode < 200 || response.StatusCode > 299 {
		return contract.NewFault(contract.Unavailable, "Cloudflare returned an error", "Inspect Cloudflare status and the credential scope; no upstream error body is exposed.")
	}
	var envelope cloudflareEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return contract.NewFault(contract.Unavailable, "Cloudflare returned an invalid response", "Retry later; do not assume the change failed.")
	}
	if !envelope.Success {
		return contract.NewFault(contract.Unavailable, "Cloudflare refused the request", "Inspect token scope and zone permissions; no upstream error text is exposed.")
	}
	if target != nil && len(envelope.Result) > 0 && string(envelope.Result) != "null" {
		if err := json.Unmarshal(envelope.Result, target); err != nil {
			return contract.NewFault(contract.Unavailable, "Cloudflare returned an unexpected result shape", "Retry later; do not assume the change failed.")
		}
	}
	if info != nil && len(envelope.ResultInfo) > 0 && string(envelope.ResultInfo) != "null" {
		if err := json.Unmarshal(envelope.ResultInfo, info); err != nil {
			return contract.NewFault(contract.Unavailable, "Cloudflare returned invalid pagination data", "Retry later; do not assume the change failed.")
		}
	}
	return nil
}

// ListRecords returns the current zone records for inventory. It performs no
// mutation and needs no audit entry.
func (c *Cloudflare) ListRecords(ctx context.Context) ([]DNSRecord, error) {
	return c.listRecords(ctx)
}

func (c *Cloudflare) String() string {
	return "cloudflare:" + c.zoneName
}
