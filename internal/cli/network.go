package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/integrations"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/spf13/cobra"
)

func networkMeta(summary, example string) CommandMeta {
	meta := platformMeta(summary, example)
	meta.Next = []string{"eregion dns list --json", "eregion access grants list --json", "eregion commands --json"}
	return meta
}

func (a *app) networkSpecs() []spec {
	return []spec{a.dnsSpecs(), a.accessSpecs()}
}

func (a *app) dnsSpecs() spec {
	reconcileMeta := networkMeta("Reconcile managed Cloudflare DNS records for one zone", "eregion dns reconcile --zone-id 0123456789abcdef0123456789abcdef --zone-name example.com --token-file /run/secrets/cloudflare-token --record '{\"name\":\"app.example.com\",\"type\":\"A\",\"content\":\"203.0.113.10\",\"ttl\":300}' --idempotency-key ci:dns-1 --json")
	reconcileMeta.Mutating, reconcileMeta.DryRun = true, true
	return spec{
		use:  "dns",
		meta: networkMeta("Plan and reconcile managed Cloudflare DNS records", "eregion dns plan --zone-id 0123456789abcdef0123456789abcdef --zone-name example.com --token-file /run/secrets/cloudflare-token --record '{\"name\":\"app.example.com\",\"type\":\"A\",\"content\":\"203.0.113.10\"}' --json"),
		flags: func(cmd *cobra.Command) {
			cmd.PersistentFlags().String("api-url", "", "API origin URL (defaults to EREGION_API_URL or loopback)")
		},
		run: func(cmd *cobra.Command, _ []string) (result, error) { return a.help(cmd), nil },
		children: []spec{
			{
				use:  "plan",
				meta: networkMeta("Preview DNS changes without accepting an operation", "eregion dns plan --zone-id 0123456789abcdef0123456789abcdef --zone-name example.com --token-file /run/secrets/cloudflare-token --record '{\"name\":\"app.example.com\",\"type\":\"A\",\"content\":\"203.0.113.10\"}' --json"),
				args: platformReadArgs,
				flags: func(cmd *cobra.Command) {
					addDNSFlags(cmd)
				},
				run: func(cmd *cobra.Command, _ []string) (result, error) {
					raw, err := dnsReconcileInput(cmd)
					if err != nil {
						return result{}, err
					}
					return a.submitPlatformRawMode(cmd, integrations.KindDNSReconcile, platform.PlatformTarget().String(), raw, "", true)
				},
			},
			{
				use:  "reconcile",
				meta: reconcileMeta,
				args: platformReadArgs,
				flags: func(cmd *cobra.Command) {
					addDNSFlags(cmd)
					addSubmissionFlags(cmd)
				},
				run: func(cmd *cobra.Command, _ []string) (result, error) {
					raw, err := dnsReconcileInput(cmd)
					if err != nil {
						return result{}, err
					}
					return a.submitPlatformRaw(cmd, integrations.KindDNSReconcile, platform.PlatformTarget().String(), raw)
				},
			},
			{
				use:  "list",
				meta: networkMeta("List observed Cloudflare routes from the read-only DNS inventory", "eregion dns list --zone example.com --json"),
				args: platformReadArgs,
				flags: func(cmd *cobra.Command) {
					cmd.Flags().String("zone", "", "Filter by zone name")
					cmd.Flags().Int("limit", 50, "Maximum routes (1–200)")
				},
				run: a.dnsList,
			},
		},
	}
}

func addDNSFlags(cmd *cobra.Command) {
	cmd.Flags().String("zone-id", "", "Cloudflare zone ID (32 lowercase hexadecimal characters)")
	cmd.Flags().String("zone-name", "", "Cloudflare zone name such as example.com")
	cmd.Flags().String("token-file", "", "Absolute root-owned 0600 Cloudflare API token file")
	cmd.Flags().StringArray("record", nil, "Desired DNS record as one JSON object (repeatable)")
	cmd.Flags().String("records-file", "", "Absolute JSON array of desired DNS records; alternative to --record")
}

type dnsReconcileRequest struct {
	ZoneID    string                   `json:"zone_id"`
	ZoneName  string                   `json:"zone_name"`
	TokenFile string                   `json:"api_token_file"`
	Records   []integrations.DNSRecord `json:"records"`
}

func dnsReconcileInput(cmd *cobra.Command) (json.RawMessage, error) {
	zoneID, _ := cmd.Flags().GetString("zone-id")
	zoneName, _ := cmd.Flags().GetString("zone-name")
	tokenFile, _ := cmd.Flags().GetString("token-file")
	if strings.TrimSpace(zoneID) == "" || strings.TrimSpace(zoneName) == "" || strings.TrimSpace(tokenFile) == "" {
		return nil, contract.NewFault(contract.InputRequired, "--zone-id, --zone-name, and --token-file are required", "Name the exact Cloudflare zone and a protected token file; the token value is never accepted on the command line.")
	}
	records, err := dnsRecords(cmd)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(dnsReconcileRequest{ZoneID: zoneID, ZoneName: zoneName, TokenFile: tokenFile, Records: records})
	if err != nil {
		return nil, contract.NewFault(contract.Internal, "could not encode DNS input", "Report the command and binary version.")
	}
	return raw, nil
}

func dnsRecords(cmd *cobra.Command) ([]integrations.DNSRecord, error) {
	values, _ := cmd.Flags().GetStringArray("record")
	path, _ := cmd.Flags().GetString("records-file")
	if len(values) > 0 && path != "" {
		return nil, contract.NewFault(contract.Usage, "--record and --records-file are mutually exclusive", "Describe records once, inline or in one JSON array file.")
	}
	if len(values) == 0 && path == "" {
		return nil, contract.NewFault(contract.InputRequired, "at least one desired DNS record is required", "Use --record with a JSON object or --records-file with a JSON array.")
	}
	if path != "" {
		raw, err := readBoundedJSONFile(path, 64*1024)
		if err != nil {
			return nil, err
		}
		var records []integrations.DNSRecord
		if err := strictJSON(raw, &records); err != nil {
			return nil, contract.NewFault(contract.ValidationFailed, "invalid DNS records file", "Provide one JSON array of DNS record objects with documented fields.")
		}
		return records, nil
	}
	records := make([]integrations.DNSRecord, 0, len(values))
	for _, value := range values {
		var record integrations.DNSRecord
		if err := strictJSON([]byte(value), &record); err != nil {
			return nil, contract.NewFault(contract.ValidationFailed, "invalid --record value", "Pass one JSON object with name, type, content, and optional ttl, proxied, and comment.")
		}
		records = append(records, record)
	}
	return records, nil
}

func strictJSON(raw []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return err
	}
	return nil
}

func readBoundedJSONFile(path string, max int64) ([]byte, error) {
	if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "\x00\r\n") {
		return nil, contract.NewFault(contract.ValidationFailed, "DNS records file must be an absolute path", "Use a readable absolute path outside shell history.")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, contract.NewFault(contract.PreconditionFailed, "DNS records file is unavailable or not a regular file", "Use a real regular file; symlinks are rejected.")
	}
	if info.Size() <= 0 || info.Size() > max {
		return nil, contract.NewFault(contract.PreconditionFailed, "DNS records file size is outside the supported bound", "Use a nonempty file no larger than 64 KiB.")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, contract.NewFault(contract.PreconditionFailed, "DNS records file could not be read", "Check permissions on the file.")
	}
	return raw, nil
}

func (a *app) dnsList(cmd *cobra.Command, _ []string) (result, error) {
	limit, _ := cmd.Flags().GetInt("limit")
	filters := url.Values{"limit": {fmt.Sprintf("%d", limit)}}
	raw, err := a.platformReadInventoryWith(cmd, integrations.InventoryCollectionDNS, filters)
	if err != nil {
		return result{}, err
	}
	zone, _ := cmd.Flags().GetString("zone")
	if zone != "" {
		raw, err = filterDNSRoutes(raw, zone)
		if err != nil {
			return result{}, err
		}
	}
	return result{data: raw, plain: dnsRouteTable(raw)}, nil
}

func filterDNSRoutes(raw json.RawMessage, zone string) (json.RawMessage, error) {
	var page struct {
		Collection string                  `json:"collection"`
		Items      []integrations.DNSRoute `json:"items"`
		Limit      int                     `json:"limit"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, contract.NewFault(contract.Unavailable, "API returned an invalid DNS inventory", "Check binary compatibility.")
	}
	items := make([]integrations.DNSRoute, 0, len(page.Items))
	for _, item := range page.Items {
		if item.Zone == zone {
			items = append(items, item)
		}
	}
	page.Items = items
	encoded, _ := json.Marshal(page)
	return encoded, nil
}

func dnsRouteTable(raw json.RawMessage) string {
	var page struct {
		Items []integrations.DNSRoute `json:"items"`
	}
	if err := json.Unmarshal(raw, &page); err != nil || len(page.Items) == 0 {
		return "(none)\n"
	}
	var out strings.Builder
	fmt.Fprintf(&out, "%-20s %-32s %-6s %-28s %-6s %s\n", "ZONE", "NAME", "TYPE", "CONTENT", "TTL", "MANAGED")
	for _, item := range page.Items {
		managed := item.ManagedBy
		if managed == "" {
			managed = "-"
		}
		fmt.Fprintf(&out, "%-20s %-32s %-6s %-28s %-6d %s\n", item.Zone, item.Name, item.Type, item.Content, item.TTL, managed)
	}
	return out.String()
}

func (a *app) accessSpecs() spec {
	grantMeta := networkMeta("Grant one device access to exact private routes", "eregion access grants grant --subject alice --device laptop-1 --route app.internal/api --idempotency-key ci:access-alice-1 --json")
	grantMeta.Mutating, grantMeta.DryRun = true, true
	revokeMeta := networkMeta("Revoke one audited private-device grant", "eregion access grants revoke --grant-id grant_0123456789abcdef0123456789abcdef --idempotency-key ci:access-revoke-1 --json")
	revokeMeta.Mutating, revokeMeta.DryRun = true, true
	return spec{
		use:  "access",
		meta: networkMeta("Inspect private devices and grant or revoke audited route access", "eregion access grants list --json"),
		flags: func(cmd *cobra.Command) {
			cmd.PersistentFlags().String("api-url", "", "API origin URL (defaults to EREGION_API_URL or loopback)")
		},
		run: func(cmd *cobra.Command, _ []string) (result, error) { return a.help(cmd), nil },
		children: []spec{
			{
				use:  "devices",
				meta: networkMeta("List devices that have private-access grants", "eregion access devices --json"),
				args: platformReadArgs,
				flags: func(cmd *cobra.Command) {
					cmd.Flags().String("subject", "", "Filter by grant subject")
					cmd.Flags().String("device", "", "Filter by private device ID")
					cmd.Flags().Int("limit", 50, "Maximum devices (1–200)")
				},
				run: a.accessDevices,
			},
			{
				use:  "grants",
				meta: networkMeta("Inspect and change private-access grants", "eregion access grants list --json"),
				run:  func(cmd *cobra.Command, _ []string) (result, error) { return a.help(cmd), nil },
				children: []spec{
					{
						use:  "list",
						meta: networkMeta("List private-access grants including revoked and expired records", "eregion access grants list --subject alice --json"),
						args: platformReadArgs,
						flags: func(cmd *cobra.Command) {
							cmd.Flags().String("subject", "", "Filter by grant subject")
							cmd.Flags().String("device", "", "Filter by private device ID")
							cmd.Flags().Int("limit", 50, "Maximum grants (1–200)")
						},
						run: a.accessGrantsList,
					},
					{
						use:  "grant",
						meta: grantMeta,
						args: platformReadArgs,
						flags: func(cmd *cobra.Command) {
							addSubmissionFlags(cmd)
							cmd.Flags().String("subject", "", "Authenticated subject receiving access")
							cmd.Flags().String("device", "", "Opaque private device ID")
							cmd.Flags().StringArray("route", nil, "Exact private route (repeatable); no wildcards or schemes")
							cmd.Flags().String("expires-at", "", "RFC3339 expiry; defaults to 24 hours within the configured maximum")
							cmd.Flags().String("note", "", "Optional audited note of at most 256 bytes")
						},
						run: func(cmd *cobra.Command, _ []string) (result, error) {
							subject, _ := cmd.Flags().GetString("subject")
							device, _ := cmd.Flags().GetString("device")
							routes, _ := cmd.Flags().GetStringArray("route")
							note, _ := cmd.Flags().GetString("note")
							if strings.TrimSpace(subject) == "" || strings.TrimSpace(device) == "" || len(routes) == 0 {
								return result{}, contract.NewFault(contract.InputRequired, "--subject, --device, and at least one --route are required", "Grant exact routes to one authenticated subject and device.")
							}
							spec := integrations.GrantSpec{Subject: subject, DeviceID: device, Routes: routes, Note: note}
							if text, _ := cmd.Flags().GetString("expires-at"); text != "" {
								expires, err := time.Parse(time.RFC3339, text)
								if err != nil {
									return result{}, contract.NewFault(contract.ValidationFailed, "invalid --expires-at", "Use an RFC3339 timestamp with a timezone.")
								}
								spec.ExpiresAt = expires.UTC()
							}
							return a.submitPlatform(cmd, integrations.KindAccessGrant, platform.PlatformTarget().String(), spec)
						},
					},
					{
						use:  "revoke",
						meta: revokeMeta,
						args: platformReadArgs,
						flags: func(cmd *cobra.Command) {
							addSubmissionFlags(cmd)
							cmd.Flags().String("grant-id", "", "Opaque grant ID from access grants list")
						},
						run: func(cmd *cobra.Command, _ []string) (result, error) {
							grantID, _ := cmd.Flags().GetString("grant-id")
							if strings.TrimSpace(grantID) == "" {
								return result{}, contract.NewFault(contract.InputRequired, "--grant-id is required", "List grants and pass the opaque grant ID.")
							}
							return a.submitPlatform(cmd, integrations.KindAccessRevoke, platform.PlatformTarget().String(), map[string]string{"grant_id": grantID})
						},
					},
				},
			},
		},
	}
}

func (a *app) accessGrantsList(cmd *cobra.Command, _ []string) (result, error) {
	grants, err := a.accessInventory(cmd)
	if err != nil {
		return result{}, err
	}
	return result{data: map[string]any{"grants": grants}, plain: accessGrantTable(grants)}, nil
}

func (a *app) accessDevices(cmd *cobra.Command, _ []string) (result, error) {
	grants, err := a.accessInventory(cmd)
	if err != nil {
		return result{}, err
	}
	type device struct {
		Device string   `json:"device_id"`
		Routes []string `json:"routes"`
		Grants []string `json:"grants"`
	}
	now := time.Now().UTC()
	byID := map[string]*device{}
	for _, grant := range grants {
		key := grant.Subject + "/" + grant.DeviceID
		item := byID[key]
		if item == nil {
			item = &device{Device: grant.DeviceID}
			byID[key] = item
		}
		item.Grants = append(item.Grants, grant.ID)
		if grant.RevokedAt == nil && grant.ExpiresAt.After(now) {
			item.Routes = append(item.Routes, grant.Routes...)
		}
	}
	devices := make([]map[string]any, 0, len(byID))
	for _, item := range byID {
		devices = append(devices, map[string]any{
			"device": item.Device, "active_routes": item.Routes, "grants": item.Grants,
		})
	}
	var out strings.Builder
	fmt.Fprintf(&out, "%-32s %-8s %s\n", "DEVICE", "GRANTS", "ACTIVE ROUTES")
	for _, item := range devices {
		routes := item["active_routes"].([]string)
		grants := item["grants"].([]string)
		fmt.Fprintf(&out, "%-32s %-8d %s\n", item["device"], len(grants), strings.Join(routes, ","))
	}
	if len(devices) == 0 {
		return result{data: map[string]any{"devices": []any{}}, plain: "(none)\n"}, nil
	}
	return result{data: map[string]any{"devices": devices}, plain: out.String()}, nil
}

func (a *app) accessInventory(cmd *cobra.Command) ([]integrations.DeviceGrant, error) {
	limit, _ := cmd.Flags().GetInt("limit")
	raw, err := a.platformReadInventoryWith(cmd, integrations.InventoryCollection, url.Values{"limit": {fmt.Sprintf("%d", limit)}})
	if err != nil {
		return nil, err
	}
	var page struct {
		Items []integrations.DeviceGrant `json:"items"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, contract.NewFault(contract.Unavailable, "API returned an invalid access inventory", "Check binary compatibility.")
	}
	subject, _ := cmd.Flags().GetString("subject")
	device, _ := cmd.Flags().GetString("device")
	grants := make([]integrations.DeviceGrant, 0, len(page.Items))
	for _, grant := range page.Items {
		if subject != "" && grant.Subject != subject {
			continue
		}
		if device != "" && grant.DeviceID != device {
			continue
		}
		grants = append(grants, grant)
	}
	return grants, nil
}

func accessGrantTable(grants []integrations.DeviceGrant) string {
	if len(grants) == 0 {
		return "(none)\n"
	}
	var out strings.Builder
	fmt.Fprintf(&out, "%-40s %-24s %-32s %-20s %-8s %s\n", "ID", "SUBJECT", "DEVICE", "EXPIRES", "REVOKED", "ROUTES")
	now := time.Now().UTC()
	for _, grant := range grants {
		revoked := grant.RevokedAt != nil || !grant.ExpiresAt.After(now)
		fmt.Fprintf(&out, "%-40s %-24s %-32s %-20s %-8t %s\n", grant.ID, grant.Subject, grant.DeviceID, grant.ExpiresAt.UTC().Format("2006-01-02T15:04:05Z"), revoked, strings.Join(grant.Routes, ","))
	}
	return out.String()
}
