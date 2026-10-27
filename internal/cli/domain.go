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
	"github.com/mbvlabs/eregion/internal/host"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/spf13/cobra"
)

// domainSpecs is `eregion domain`: public hostnames that Caddy on the main
// node routes to an environment, each with its own ACME certificate. The
// binding, route, DNS record, certificate and public response are reported
// separately.
func (a *app) domainSpecs() []spec {
	parent := platformMeta("Route public hostnames to environments through Caddy", "eregion domain list --json")
	add := platformMutationMeta("Route a public hostname to an environment", "eregion domain add hello production hello.example.com")
	add.WhenToUse = []string{"Give an environment a public HTTPS URL. Create the DNS record it shows; Caddy gets the certificate once it resolves."}
	add.Next = []string{"eregion domain list"}
	remove := platformDestructiveMeta("Remove a public hostname's route", "eregion domain remove hello production hello.example.com --confirm hello.example.com")
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
			}, run: func(cmd *cobra.Command, args []string) (result, error) {
				return a.domainAdd(cmd, args)
			}},
			{use: "remove <app> <environment> <hostname>", meta: remove, args: cobra.ExactArgs(3), flags: func(cmd *cobra.Command) {
				changeFlags(cmd)
				cmd.Flags().String("confirm", "", "Target-naming confirmation token: the hostname being removed")
			}, run: func(cmd *cobra.Command, args []string) (result, error) {
				return a.domainRemove(cmd, args)
			}},
			{use: "list", meta: platformMeta("List public hostnames with route, DNS, certificate and public-response state", "eregion domain list --app hello --environment production --json"), args: platformReadArgs,
				flags: listFlags,
				run:   a.domainList},
		},
	}}
}

func (a *app) domainAdd(cmd *cobra.Command, args []string) (result, error) {
	primary, _ := cmd.Flags().GetBool("primary")
	input := platform.DomainInput{
		App: args[0], Environment: args[1], Domain: args[2],
		Primary: primary, Addresses: mainNodeAddresses(cmd.Context()),
	}
	return a.domainChange(cmd, platform.KindDomainAdd, input)
}

func (a *app) domainRemove(cmd *cobra.Command, args []string) (result, error) {
	confirm, _ := cmd.Flags().GetString("confirm")
	input := platform.DomainInput{App: args[0], Environment: args[1], Domain: args[2], Confirm: confirm}
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
			preview.plain += "\n" + domainRecordText(input)
			var value any
			_ = json.Unmarshal(preview.data.(json.RawMessage), &value)
			encoded, _ := json.Marshal(map[string]any{"preview": value, "dns_records": records})
			preview.data = encoded
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
			DNSRecords: domainRecords(input.Domain, input.Addresses),
		}
		plain := "Accepted " + kind + " for " + input.Domain + "\noperation: " + string(op.ID) + "\nnext: eregion operations follow " + string(op.ID)
		if kind == platform.KindDomainAdd {
			plain += "\n" + domainRecordText(input)
		}
		return result{data: data, plain: plain}, nil
	}
	config := followConfig(cmd)
	follow := config != nil && config.mode == "plain" && isTerminal(config.in) && isTerminal(config.out)
	if follow {
		// Show the record before the worker starts so it can be created while
		// the operation runs.
		fmt.Fprintln(config.out, domainRecordText(input))
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
		return data, "Accepted removal of " + input.Domain + "; the worker rewrites the Caddy routes.\noperation: " + string(op.ID)
	}
	if binding, err := a.domainBinding(cmd, input.App, input.Environment, input.Domain); err == nil {
		data.RouteState, data.RouteError = binding.RouteState, binding.RouteError
		data.Certificate = string(binding.CertificateState)
		data.PublicState, data.PublicError = binding.PublicState, binding.PublicError
		data.Primary = binding.Primary
	}
	observation := platform.LookupPublicDNS(cmd.Context(), input.Domain)
	data.DNSAddresses, data.DNSError = observation.Addresses, observation.Error
	data.DNSState = platform.ClassifyDNS(input.Addresses, observation.Addresses)
	data.Ready = data.RouteState == platform.RouteApplied && data.DNSState == platform.DNSOK &&
		data.Certificate == string(platform.CertificateValid) && data.PublicState == platform.PublicOK
	return data, domainChangeText(data)
}

func (a *app) domainStateText(cmd *cobra.Command, input platform.DomainInput) string {
	data, _ := a.domainChangeResult(cmd, platform.KindDomainAdd, input, contract.Operation{})
	return domainStateLines(data)
}

func (a *app) domainBinding(cmd *cobra.Command, app, environment, domain string) (*platform.DomainBinding, error) {
	raw, err := a.platformReadInventoryWith(cmd, "domains", url.Values{"app": {app}, "environment": {environment}})
	if err != nil {
		return nil, err
	}
	var page struct {
		Items []platform.DomainBinding `json:"items"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		return nil, contract.NewFault(contract.Unavailable, "the node returned invalid domain state", "Check binary compatibility before assuming the route is absent.")
	}
	for index := range page.Items {
		if page.Items[index].Domain == domain {
			return &page.Items[index], nil
		}
	}
	return nil, contract.NewFault(contract.NotFound, domain+" is not in the domain inventory", "Refresh with eregion domain list.")
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
			item.Domain, primaryMark(item.Primary), item.App, item.Environment,
			string(item.RouteState), string(item.DNSState), string(item.CertificateState), string(item.PublicState), ready,
		})
		if message := domainBindingError(item); message != "" {
			rows = append(rows, []string{item.Domain, "", "", "", "", "", "", "", message})
		}
	}
	return result{data: page.Items, plain: ui.Header("Domains", 72) + "\n" + writeTable([]string{"DOMAIN", "PRI", "APP", "ENVIRONMENT", "ROUTE", "DNS", "CERTIFICATE", "PUBLIC", "READY"}, rows)}, nil
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
	lines = append(lines, domainRecordLines(data)...)
	lines = append(lines, domainStateLines(data))
	return strings.Join(lines, "\n")
}

func domainRecordText(input platform.DomainInput) string {
	return strings.Join(domainRecordLines(domainChangeData{Domain: input.Domain, DNSRecords: domainRecords(input.Domain, input.Addresses)}), "\n")
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
// current context's WireGuard endpoint (the main node's public address), or
// the SSH target when the context predates WireGuard.
func mainNodeAddresses(ctx context.Context) []string {
	store, err := host.LoadContextStore("")
	if err != nil {
		return nil
	}
	current, err := store.Get("")
	if err != nil || current.SSH == nil {
		return nil
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
