package cli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strings"

	"github.com/mbvlabs/eregion/internal/cli/ui"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/spf13/cobra"
)

// domainSpecs is `eregion domain`: public hostnames that Caddy on the main
// node routes to an environment, each with its own ACME certificate. The
// binding, route, DNS record, certificate and public response are reported
// separately.
func (a *app) domainSpecs() []spec {
	parent := platformMeta("Route public hostnames to environments through Caddy", "eregion domain list --json")
	add := followingMeta(platformMutationMeta("Route a public hostname to an environment", "eregion domain add hello production hello.example.com"))
	add.WhenToUse = []string{
		"Give an environment a public HTTPS URL. Manual DNS is the default: create the exact record it shows and Caddy gets the certificate once it resolves.",
		"With explicit --dns cloudflare --dns-zone ZONE --dns-origin ADDRESS --dns-proxy dns-only|proxied, a dry run reads a real Cloudflare snapshot and apply reconciles only this hostname's exact owned A/AAAA records. Origins are never derived from the WireGuard management endpoint.",
	}
	add.Examples = []string{
		"eregion domain add hello production hello.example.com",
		"eregion domain add hello production hello.example.com --dns cloudflare --dns-zone example.com --dns-origin 93.184.216.34 --dns-proxy proxied --dry-run --json",
	}
	add.Next = []string{"eregion domain list"}
	remove := followingMeta(platformDestructiveMeta("Remove a public hostname's route", "eregion domain remove hello production hello.example.com --confirm hello.example.com"))
	remove.Confirmation = "target"
	remove.Next = []string{"eregion domain list"}
	remove.Examples = []string{
		"eregion domain remove hello production hello.example.com --dry-run --json",
		"eregion domain remove hello production hello.example.com --confirm hello.example.com --json",
	}
	changeFlags := func(cmd *cobra.Command) {
		cmd.Flags().Bool("dry-run", false, "Validate with the API and return a plan without accepting an operation")
		cmd.Flags().String("idempotency-key", "", "Stable submission key; one is generated when empty")
		cmd.Flags().Bool("detach", false, "Return after acceptance instead of following the operation")
		followFlags(cmd)
	}
	listFlags := func(cmd *cobra.Command) {
		cmd.Flags().String("app", "", "Filter by app name")
		cmd.Flags().String("environment", "", "Filter by environment name")
		cmd.Flags().Int("limit", 100, "Maximum items (1–200)")
	}
	return []spec{{
		use:  "domain",
		meta: parent,
		run:  func(cmd *cobra.Command, _ []string) (result, error) { return a.help(cmd), nil },
		children: []spec{
			{use: "add <app> <environment> <hostname>", meta: add, args: cobra.ExactArgs(3), flags: func(cmd *cobra.Command) {
				changeFlags(cmd)
				cmd.Flags().Bool("primary", false, "Make this the environment's primary hostname, demoting the current one")
				cmd.Flags().String("check-path", "", "HTTP path to observe on this hostname; empty uses the environment's web health path")
				addDomainDNSFlags(cmd, true)
			}, run: func(cmd *cobra.Command, args []string) (result, error) {
				return a.domainAdd(cmd, args)
			}},
			{use: "remove <app> <environment> <hostname>", meta: remove, args: cobra.ExactArgs(3), flags: func(cmd *cobra.Command) {
				changeFlags(cmd)
				cmd.Flags().String("confirm", "", "Target-naming confirmation token: the hostname being removed")
				addDomainDNSFlags(cmd, false)
			}, run: func(cmd *cobra.Command, args []string) (result, error) {
				return a.domainRemove(cmd, args)
			}},
			{use: "list", meta: platformMeta("List public hostnames with route, DNS, certificate and public-response state", "eregion domain list --app hello --environment production --json"), args: platformReadArgs,
				flags: listFlags,
				run:   a.domainList},
			a.domainCloudflareSpec(),
		},
	}}
}

func (a *app) domainAdd(cmd *cobra.Command, args []string) (result, error) {
	primary, _ := cmd.Flags().GetBool("primary")
	input := domainAddInput(cmd, args, primary)
	if err := applyDomainDNSFlags(cmd, &input, true); err != nil {
		return result{}, err
	}
	return a.domainChange(cmd, platform.KindDomainAdd, input)
}

func addDomainDNSFlags(cmd *cobra.Command, forAdd bool) {
	cmd.Flags().String("dns", "", "DNS mode: manual (default) or cloudflare; manual never changes the provider")
	cmd.Flags().String("dns-zone", "", "Configured Cloudflare zone name or ID; required with --dns cloudflare")
	if forAdd {
		cmd.Flags().String("dns-proxy", "", "Explicit Cloudflare proxy mode: dns-only or proxied; required with --dns cloudflare")
		cmd.Flags().StringArray("dns-origin", nil, "Explicit public IPv4/IPv6 origin address for Cloudflare DNS; repeatable and required")
	}
}

// applyDomainDNSFlags keeps manual DNS the default and requires every managed
// DNS choice to be explicit. Public origins come only from --dns-origin, never
// from the context's WireGuard management endpoint.
func applyDomainDNSFlags(cmd *cobra.Command, input *platform.DomainInput, forAdd bool) error {
	mode, _ := cmd.Flags().GetString("dns")
	zone, _ := cmd.Flags().GetString("dns-zone")
	proxy, _ := cmd.Flags().GetString("dns-proxy")
	origins, _ := cmd.Flags().GetStringArray("dns-origin")
	mode = strings.ToLower(strings.TrimSpace(mode))
	zone = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(zone), "."))
	proxy = strings.ToLower(strings.TrimSpace(proxy))
	if !cmd.Flags().Changed("dns") {
		if cmd.Flags().Changed("dns-zone") || cmd.Flags().Changed("dns-proxy") || cmd.Flags().Changed("dns-origin") {
			return contract.NewFault(contract.Usage, "--dns-zone, --dns-origin and --dns-proxy need --dns cloudflare", "Omit DNS provider flags for manual DNS, or select --dns cloudflare explicitly.")
		}
		return nil
	}
	switch mode {
	case "manual":
		if zone != "" || proxy != "" || len(origins) > 0 {
			return contract.NewFault(contract.Usage, "--dns manual cannot carry a zone, origin or proxy mode", "Use --dns cloudflare with --dns-zone, --dns-origin and --dns-proxy for managed DNS.")
		}
		input.DNSMode = "manual"
		return nil
	case "cloudflare":
	default:
		return contract.NewFault(contract.Usage, "unknown --dns mode", "Use manual (the default) or cloudflare.")
	}
	if zone == "" {
		return contract.NewFault(contract.Usage, "--dns cloudflare needs --dns-zone", "Configure the zone with eregion domain cloudflare configure first.")
	}
	input.DNSMode, input.DNSZone = "cloudflare", zone
	if !forAdd {
		if proxy != "" {
			return contract.NewFault(contract.Usage, "--dns-proxy applies only to domain add", "Domain removal deletes only exact owned records and has no proxy choice.")
		}
		return nil
	}
	if !cmd.Flags().Changed("dns-proxy") {
		return contract.NewFault(contract.Usage, "--dns cloudflare needs an explicit --dns-proxy", "Use --dns-proxy dns-only or --dns-proxy proxied; the choice is never inferred.")
	}
	proxied := false
	switch proxy {
	case "dns-only":
	case "proxied":
		proxied = true
	default:
		return contract.NewFault(contract.Usage, "unknown --dns-proxy mode", "Use dns-only or proxied.")
	}
	input.DNSProxied = &proxied
	if !cmd.Flags().Changed("dns-origin") || len(origins) == 0 {
		return contract.NewFault(contract.Usage, "--dns cloudflare needs explicit --dns-origin addresses", "Pass each public origin address with --dns-origin; the WireGuard management endpoint is not used.")
	}
	normalizedOrigins, err := platform.NormalizeDNSOrigins(origins)
	if err != nil {
		return err
	}
	input.DNSOrigins = normalizedOrigins
	input.Addresses = append([]string(nil), normalizedOrigins...)
	return nil
}

// domainAddInput records the selected context's public addresses as the
// binding's expected DNS addresses. It follows --workspace / EREGION_WORKSPACE.
func domainAddInput(cmd *cobra.Command, args []string, primary bool) platform.DomainInput {
	checkPath, _ := cmd.Flags().GetString("check-path")
	return platform.DomainInput{
		App: args[0], Environment: args[1], Domain: args[2],
		Primary: primary, Addresses: mainNodeAddresses(cmd), CheckPath: checkPath,
	}
}

func (a *app) domainRemove(cmd *cobra.Command, args []string) (result, error) {
	confirm, _ := cmd.Flags().GetString("confirm")
	input := platform.DomainInput{App: args[0], Environment: args[1], Domain: args[2], Confirm: confirm}
	if err := applyDomainDNSFlags(cmd, &input, false); err != nil {
		return result{}, err
	}
	return a.domainChange(cmd, platform.KindDomainRemove, input)
}

func (a *app) domainChange(cmd *cobra.Command, kind string, input platform.DomainInput) (result, error) {
	target, err := platform.NewEnvironmentTarget(input.App, input.Environment)
	if err != nil {
		return result{}, err
	}
	normalized, err := platform.Default().Normalize(kind, target.String(), mustJSON(input))
	if err != nil {
		return result{}, err
	}
	input = *normalized.Input.(*platform.DomainInput)
	client, err := a.client(cmd)
	if err != nil {
		return result{}, err
	}
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	config := followConfig(cmd)
	ndjson := config != nil && config.mode == "ndjson"
	follow := !boolFlag(cmd, "detach") && config != nil &&
		(ndjson || (config.mode == "plain" && isTerminal(config.in) && isTerminal(config.out)))
	if err := validateFollowRequest(cmd, follow); err != nil {
		return result{}, err
	}
	rawInput := normalized.Canonical
	if kind == platform.KindDomainRemove && !dryRun {
		if err := platform.ValidateDomainConfirmation(input.Domain, input.Confirm); err != nil {
			return result{}, err
		}
	}
	if dryRun {
		preview, err := submitDomain(cmd, client, kind, target.String(), rawInput, "", true)
		if err != nil {
			return result{}, err
		}
		if kind == platform.KindDomainRemove {
			preview.data, preview.plain = platformWithConfirmation(preview.data.(json.RawMessage), input.Domain)
		} else {
			records := domainRecords(input.Domain, input.Addresses)
			raw, ok := preview.data.(json.RawMessage)
			if !ok {
				return result{}, invalidNodeData("domain preview")
			}
			data, err := domainAddPreview(raw, input.Domain, records)
			if err != nil {
				return result{}, err
			}
			var envelope struct {
				DNSRecords []domainRecord `json:"dns_records"`
			}
			if err := json.Unmarshal(data, &envelope); err != nil {
				return result{}, contract.NewFault(contract.Internal, "could not read the domain preview records", "Report the command and binary version.")
			}
			preview.plain += "\n" + strings.Join(domainRecordLines(domainChangeData{
				Domain: input.Domain, DNSRecords: envelope.DNSRecords,
			}), "\n")
			preview.data = data
		}
		return preview, nil
	}
	key, _ := cmd.Flags().GetString("idempotency-key")
	if key == "" {
		key = "cli:" + strings.TrimPrefix(kind, "platform.") + ":" + randomHex(8)
	}
	if err := contract.ValidateIdempotencyKey(key); err != nil {
		return result{}, err
	}
	submission, err := submitDomain(cmd, client, kind, target.String(), rawInput, key, false)
	if err != nil {
		return result{}, err
	}
	var op contract.Operation
	if err := json.Unmarshal(submission.data.(json.RawMessage), &op); err != nil || op.ID == "" {
		return result{}, contract.NewFault(contract.Internal, "unexpected operation answer", "Check binary compatibility before assuming the change was rejected.")
	}

	detach, _ := cmd.Flags().GetBool("detach")
	if detach {
		data := domainChangeData{
			Domain: input.Domain, App: input.App, Environment: input.Environment,
			Primary: input.Primary, OperationID: op.ID,
			DNSMode: input.DNSMode, DNSZone: input.DNSZone, DNSOrigins: input.DNSOrigins, DNSProxied: input.DNSProxied,
			DNSRecords: domainRecords(input.Domain, input.Addresses),
		}
		plain := "Accepted " + kind + " for " + input.Domain + "\noperation: " + string(op.ID) + "\nnext: eregion operations follow " + string(op.ID)
		if kind == platform.KindDomainAdd {
			if binding, err := a.domainBinding(cmd, input.App, input.Environment, input.Domain); err == nil {
				applyManagedDNSBinding(&data, binding)
			}
			plain += "\n" + strings.Join(domainRecordLines(domainChangeData{
				Domain: input.Domain, DNSRecords: data.DNSRecords,
			}), "\n")
		}
		return result{data: data, plain: plain}, nil
	}
	if follow {
		// Show the record before the worker starts so it can be created while
		// the operation runs. NDJSON carries only the event protocol.
		if ndjson {
			_, followErr := a.followOperation(cmd, client, op.ID)
			if followErr != nil {
				return result{streamed: true}, followErr
			}
			return result{streamed: true}, nil
		}
		recordData := domainChangeData{Domain: input.Domain, DNSRecords: domainRecords(input.Domain, input.Addresses)}
		if binding, err := a.domainBinding(cmd, input.App, input.Environment, input.Domain); err == nil {
			applyManagedDNSBinding(&recordData, binding)
		}
		fmt.Fprintln(config.out, strings.Join(domainRecordLines(recordData), "\n"))
		_, followErr := a.followOperation(cmd, client, op.ID)
		if followErr != nil {
			return result{streamed: true}, followErr
		}
		fmt.Fprintln(config.out, a.domainStateText(cmd, input))
		return result{streamed: true}, nil
	}

	data, plain := a.domainChangeResult(cmd, kind, input, op)
	return result{data: data, plain: plain}, nil
}

// domainAddPreview nests the dry-run preview and the DNS records. Its result is
// a json.RawMessage, not []byte: a []byte in result.data would serialize as a
// base64 string instead of the object agents read with jq .data.preview.
func domainAddPreview(raw json.RawMessage, domain string, records []domainRecord) (json.RawMessage, error) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, contract.NewFault(contract.Unavailable, "the node returned an invalid preview", "Check binary compatibility before assuming the domain was refused.")
	}
	payload := map[string]any{"preview": value, "dns_records": records}
	if provider := previewProviderData(value); provider != nil {
		payload["dns_provider"] = provider
	}
	if managed := previewManagedData(value); managed != nil {
		payload["dns_managed"] = managed
		if persisted, ok := previewManagedRecords(managed, domain); ok {
			payload["dns_records"] = persisted
		}
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, contract.NewFault(contract.Internal, "could not encode the domain preview", "Report the command and binary version.")
	}
	return json.RawMessage(encoded), nil
}

func previewManagedRecords(managed any, domain string) ([]domainRecord, bool) {
	object, ok := managed.(map[string]any)
	if !ok {
		return nil, false
	}
	raw, ok := object["origins"].([]any)
	if !ok {
		return nil, false
	}
	origins := make([]string, 0, len(raw))
	for _, value := range raw {
		if text, ok := value.(string); ok {
			origins = append(origins, text)
		}
	}
	return domainRecords(domain, origins), true
}

func previewProviderData(value any) any {
	object, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	data, ok := object["data"].(map[string]any)
	if !ok {
		return nil
	}
	return data["dns_provider"]
}

func previewManagedData(value any) any {
	object, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	data, ok := object["data"].(map[string]any)
	if !ok {
		return nil
	}
	return data["dns_managed"]
}

func submitDomain(cmd *cobra.Command, client Backend, kind, target string, input json.RawMessage, key string, dryRun bool) (result, error) {
	raw, err := client.SubmitAction(cmd.Context(), kind, target, input, key, "", dryRun)
	if err != nil {
		return result{}, err
	}
	return platformOperationResult(raw, dryRun), nil
}

type domainRecord struct {
	Type  string `json:"type"`
	Name  string `json:"name"`
	Value string `json:"value"`
}

type domainChangeData struct {
	Domain       string               `json:"domain"`
	App          string               `json:"app"`
	Environment  string               `json:"environment"`
	Primary      bool                 `json:"primary"`
	OperationID  contract.OperationID `json:"operation_id,omitempty"`
	DNSMode      string               `json:"dns_mode,omitempty"`
	DNSZone      string               `json:"dns_zone,omitempty"`
	DNSOrigins   []string             `json:"dns_origins,omitempty"`
	DNSProxied   *bool                `json:"dns_proxied,omitempty"`
	DNSRecords   []domainRecord       `json:"dns_records"`
	DNSState     platform.DNSState    `json:"dns_state,omitempty"`
	DNSAddresses []string             `json:"dns_addresses,omitempty"`
	DNSError     string               `json:"dns_error,omitempty"`
	RouteState   platform.RouteState  `json:"route_state,omitempty"`
	RouteError   string               `json:"route_error,omitempty"`
	Certificate  string               `json:"certificate_state,omitempty"`
	PublicState  platform.PublicState `json:"public_state,omitempty"`
	PublicError  string               `json:"public_error,omitempty"`
	Ready        bool                 `json:"ready"`
}

func (a *app) domainChangeResult(cmd *cobra.Command, kind string, input platform.DomainInput, op contract.Operation) (domainChangeData, string) {
	data := domainChangeData{
		Domain: input.Domain, App: input.App, Environment: input.Environment, Primary: input.Primary,
		OperationID: op.ID, DNSRecords: domainRecords(input.Domain, input.Addresses),
	}
	if kind == platform.KindDomainRemove {
		plain := "Accepted removal of " + input.Domain + "; the worker rewrites the Caddy routes.\noperation: " + string(op.ID)
		if input.DNSMode == "cloudflare" {
			plain += "\nCloudflare DNS: remove only this binding's exact owned records in zone " + input.DNSZone + "."
		} else {
			plain += "\nDNS: any persisted managed-DNS ledger is cleaned with this removal; an unmanaged binding keeps manual DNS unchanged."
		}
		return data, plain
	}
	if binding, err := a.domainBinding(cmd, input.App, input.Environment, input.Domain); err == nil {
		data.RouteState, data.RouteError = binding.RouteState, binding.RouteError
		data.Certificate = string(binding.CertificateState)
		data.PublicState, data.PublicError = binding.PublicState, binding.PublicError
		data.Primary = binding.Primary
		applyManagedDNSBinding(&data, binding)
	}
	observation := platform.LookupPublicDNS(cmd.Context(), input.Domain)
	data.DNSAddresses, data.DNSError = observation.Addresses, observation.Error
	expected := input.Addresses
	if len(data.DNSOrigins) > 0 {
		expected = data.DNSOrigins
	}
	data.DNSState = platform.ClassifyDNS(expected, observation.Addresses)
	data.Ready = data.RouteState == platform.RouteApplied && platform.DNSReachesNode(data.DNSState) &&
		data.Certificate == string(platform.CertificateValid) && data.PublicState == platform.PublicOK
	return data, domainChangeText(data)
}

// applyManagedDNSBinding replaces transient manual input with the persisted
// managed-DNS desired state. A re-add or promotion of an existing managed
// binding never renders a false manual DNS mode or stale node addresses.
func applyManagedDNSBinding(data *domainChangeData, binding *platform.DomainBinding) {
	if binding == nil || binding.ManagedDNS == nil {
		return
	}
	data.DNSMode = "cloudflare"
	data.DNSZone = binding.ManagedDNS.ZoneName
	data.DNSOrigins = append([]string(nil), binding.ManagedDNS.Origins...)
	proxied := binding.ManagedDNS.Proxied
	data.DNSProxied = &proxied
	data.DNSRecords = domainRecords(binding.Domain, binding.ManagedDNS.Origins)
}

func (a *app) domainStateText(cmd *cobra.Command, input platform.DomainInput) string {
	data, _ := a.domainChangeResult(cmd, platform.KindDomainAdd, input, contract.Operation{})
	return domainStateLines(data)
}

func (a *app) domainBinding(cmd *cobra.Command, app, environment, domain string) (*platform.DomainBinding, error) {
	bindings, err := a.readDomainBindings(cmd, app, environment)
	if err != nil {
		return nil, err
	}
	for index := range bindings {
		if bindings[index].Domain == domain {
			return &bindings[index], nil
		}
	}
	return nil, contract.NewFault(contract.NotFound, domain+" is not in the domain inventory", "Refresh with eregion domain list.")
}

func (a *app) readDomainBindings(cmd *cobra.Command, app, environment string) ([]platform.DomainBinding, error) {
	raw, err := a.platformReadInventoryWith(cmd, "domains", url.Values{
		"app": {app}, "environment": {environment}, "limit": {"200"},
	})
	if err != nil {
		return nil, err
	}
	var page struct {
		Items []platform.DomainBinding `json:"items"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, contract.NewFault(contract.Unavailable, "the node returned invalid domain state", "Check binary compatibility before assuming the route is absent.")
	}
	return page.Items, nil
}

func (a *app) domainList(cmd *cobra.Command, _ []string) (result, error) {
	filters := url.Values{}
	for _, name := range []string{"app", "environment"} {
		value, _ := cmd.Flags().GetString(name)
		if value != "" {
			filters.Set(name, value)
		}
	}
	limit, _ := cmd.Flags().GetInt("limit")
	filters.Set("limit", fmt.Sprint(limit))
	raw, err := a.platformReadInventoryWith(cmd, "domains", filters)
	if err != nil {
		return result{}, err
	}
	var page struct {
		Items []platform.DomainBinding `json:"items"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return result{}, contract.NewFault(contract.Unavailable, "the node returned invalid domain state", "Check binary compatibility before assuming domains are absent.")
	}
	if len(page.Items) == 0 {
		return result{data: []platform.DomainBinding{}, plain: ui.Header("Domains", 72) + "\nNo domains. Add one with eregion domain add <app> <environment> <hostname>.\n"}, nil
	}
	rows := make([][]string, 0, len(page.Items))
	for _, item := range page.Items {
		ready := "no"
		if item.Ready() {
			ready = "yes"
		}
		rows = append(rows, []string{
			item.Domain, primaryMark(item.Primary), item.App, item.Environment, item.CheckPath,
			string(item.RouteState), string(item.DNSState), string(item.CertificateState), string(item.PublicState), ready,
		})
		if message := domainBindingError(item); message != "" {
			rows = append(rows, []string{item.Domain, "", "", "", "", "", "", "", "", message})
		}
	}
	return result{data: page.Items, plain: ui.Header("Domains", 72) + "\n" + writeTable([]string{"DOMAIN", "PRI", "APP", "ENVIRONMENT", "CHECK PATH", "ROUTE", "DNS", "CERTIFICATE", "PUBLIC", "READY"}, rows)}, nil
}

func domainBindingError(item platform.DomainBinding) string {
	parts := []string{}
	for _, part := range []struct{ name, value string }{
		{"route", item.RouteError}, {"dns", item.DNSError},
		{"certificate", item.CertificateError}, {"public", item.PublicError},
	} {
		if part.value != "" {
			parts = append(parts, part.name+": "+part.value)
		}
	}
	return strings.Join(parts, "; ")
}

func domainChangeText(data domainChangeData) string {
	lines := []string{"Accepted the domain binding " + data.Domain + " for " + data.App + "/" + data.Environment + "."}
	if data.OperationID != "" {
		lines = append(lines, "operation: "+string(data.OperationID))
	}
	if data.DNSMode == "cloudflare" {
		proxy := "dns-only"
		if data.DNSProxied != nil && *data.DNSProxied {
			proxy = "proxied"
		}
		lines = append(lines, "Cloudflare DNS: zone "+data.DNSZone+", origins "+strings.Join(data.DNSOrigins, ", ")+", "+proxy+"; provider state is not public readiness.")
	}
	lines = append(lines, domainRecordLines(data)...)
	lines = append(lines, domainStateLines(data))
	return strings.Join(lines, "\n")
}

func domainRecordLines(data domainChangeData) []string {
	lines := []string{}
	if len(data.DNSRecords) == 0 {
		return []string{"No DNS record address is known; add an A/AAAA record pointing at the main node."}
	}
	lines = append(lines, "", "DNS record:")
	for _, record := range data.DNSRecords {
		lines = append(lines, fmt.Sprintf("  %-4s  %-36s  %s", record.Type, record.Name, record.Value))
	}
	return lines
}

func domainStateLines(data domainChangeData) string {
	route := string(data.RouteState)
	if route == "" {
		route = string(platform.RoutePending)
	}
	certificate := data.Certificate
	if certificate == "" {
		certificate = string(platform.CertificatePending)
	}
	public := string(data.PublicState)
	if public == "" {
		public = string(platform.PublicPending)
	}
	dns := string(data.DNSState)
	if dns == "" {
		dns = string(platform.DNSPending)
	}
	lines := []string{
		fmt.Sprintf("Route: %s", route),
		fmt.Sprintf("DNS: %s", dns),
		fmt.Sprintf("Certificate: %s", certificate),
		fmt.Sprintf("Public: %s", public),
	}
	if !data.Ready {
		lines = append(lines, "Ready: no (DNS, route, certificate and public response must all succeed)")
	} else {
		lines = append(lines, "Ready: yes")
	}
	return strings.Join(lines, "\n")
}

func domainRecords(domain string, addresses []string) []domainRecord {
	records := make([]domainRecord, 0, len(addresses))
	for _, address := range addresses {
		addr, err := netip.ParseAddr(address)
		if err != nil {
			continue
		}
		if addr.Unmap().Is4() {
			records = append(records, domainRecord{Type: "A", Name: domain, Value: addr.Unmap().String()})
		} else {
			records = append(records, domainRecord{Type: "AAAA", Name: domain, Value: addr.String()})
		}
	}
	return records
}

func primaryMark(primary bool) string {
	if primary {
		return "yes"
	}
	return ""
}

// mainNodeAddresses is the address the domain's DNS record points at: the
// selected context's WireGuard endpoint (the main node's public address), or
// the SSH target when the context predates WireGuard. It follows --context /
// EREGION_WORKSPACE through selectedWorkspaceAccess so a binding never records another
// server's address.
func mainNodeAddresses(cmd *cobra.Command) []string {
	current, err := selectedWorkspaceAccess(cmd)
	if err != nil || current.SSH == nil {
		return nil
	}
	ctx := context.Background()
	if cmd != nil && cmd.Context() != nil {
		ctx = cmd.Context()
	}
	target := current.SSH.Address
	if current.SSH.WireGuard != nil && current.SSH.WireGuard.Endpoint != "" {
		target = current.SSH.WireGuard.Endpoint
	}
	hostText := target
	if hostPart, _, err := net.SplitHostPort(target); err == nil {
		hostText = hostPart
	}
	if addr, err := netip.ParseAddr(hostText); err == nil {
		return []string{addr.Unmap().String()}
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", hostText)
	if err != nil {
		return nil
	}
	out := make([]string, 0, len(ips))
	seen := map[string]bool{}
	for _, ip := range ips {
		value := ip.Unmap().String()
		if !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
