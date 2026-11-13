package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/cli/ui"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/vault"
	"github.com/spf13/cobra"
)

// dnsProviderSpecs is the bounded human setup surface for provider DNS
// credentials. It is deliberately not a platform action: no token flag, file,
// environment variable or stdin form exists.
func (a *app) dnsProviderSpecs() []spec {
	parent := platformMeta("Configure DNS provider credentials and definition policy", "eregion dns status --provider cloudflare --json")
	parent.WhenToUse = []string{"Configure Cloudflare credentials and the separate definition DNS policy. Use domain add, list and remove for application bindings and routing."}

	configure := platformMeta("Configure or replace one Cloudflare provider credential at a hidden terminal prompt", "eregion dns configure --provider cloudflare --zone 0123456789abcdef0123456789abcdef --zone-name example.com")
	configure.Mutating = true
	configure.Prompts = true
	configure.Audience = []string{"human"}
	configure.InputFlags = []string{"provider", "zone", "zone-name"}
	configure.DryRunReason = "Configuration always previews a read-only token validation and the observed current revision, then asks for confirmation; there is no noninteractive or secret-flag path."
	configure.WhenToUse = []string{
		"Store or rotate the Cloudflare API token used by explicit --dns cloudflare domain bindings. The token is typed at a no-echo terminal prompt and stored encrypted in PostgreSQL; there is no token flag, file, environment or stdin fallback.",
		"The token is read-validated with a bounded DNS list for the exact configured zone. That proves DNS list access only, not write capability.",
	}
	configure.Examples = []string{
		"eregion dns configure --provider cloudflare --zone 0123456789abcdef0123456789abcdef --zone-name example.com",
	}
	configure = terminalOnlyMeta(configure)
	configure.Next = []string{"eregion dns status --provider cloudflare --json"}
	configure.ErrorCodes = []contract.Code{
		contract.Usage, contract.InvalidOutputMode, contract.InputRequired, contract.ValidationFailed,
		contract.NotFound, contract.Unauthenticated, contract.Forbidden, contract.Conflict,
		contract.PreconditionFailed, contract.NodeVersionMismatch, contract.Unavailable,
		contract.Internal, contract.Cancelled,
	}

	status := platformMeta("Show Cloudflare credential metadata and definition DNS policy without tokens", "eregion dns status --provider cloudflare --json")
	status.Next = []string{"eregion dns configure --provider cloudflare", "eregion dns policy --provider cloudflare"}
	status.WhenToUse = []string{"Inspect configured Cloudflare zones, credential revisions and the separate definition DNS policy. It never decrypts or prints a token."}

	policy := platformMeta("Set the non-secret definition DNS policy", "eregion dns policy --provider cloudflare --enabled --zone example.com --origin 8.8.8.8 --proxy dns-only --expected-revision 0")
	policy.Mutating = true
	policy.InputFlags = []string{"provider", "enabled", "zone", "origin", "proxy", "expected-revision"}
	policy.DryRunReason = "Read current policy and revision with dns status --provider cloudflare; each change requires the exact observed revision."
	policy.WhenToUse = []string{"Enable or disable definition DNS without reading or rotating a provider token. Enabling requires a configured zone and explicit public origins. This policy does not itself change DNS records."}
	policy.Next = []string{"eregion dns status --provider cloudflare --json", "eregion domain list --json"}

	return []spec{{
		use:  "dns",
		meta: parent,
		run:  func(cmd *cobra.Command, _ []string) (result, error) { return a.help(cmd), nil },
		children: []spec{
			{use: "configure", meta: configure, args: dnsProviderArgs, flags: func(cmd *cobra.Command) {
				addDNSProviderFlag(cmd)
				cmd.Flags().String("zone", "", "Cloudflare zone ID (lowercase 32 characters)")
				cmd.Flags().String("zone-name", "", "Cloudflare zone name such as example.com")
			}, run: a.domainCloudflareConfigure},
			{use: "status", meta: status, args: dnsProviderArgs, flags: func(cmd *cobra.Command) {
				addDNSProviderFlag(cmd)
				cmd.Flags().String("zone", "", "Filter by Cloudflare zone ID or name")
			}, run: a.domainCloudflareStatus},
			{use: "policy", meta: policy, args: dnsProviderArgs, flags: func(cmd *cobra.Command) {
				addDNSProviderFlag(cmd)
				cmd.Flags().Bool("enabled", false, "Enable definition DNS; use --enabled=false to disable")
				cmd.Flags().String("zone", "", "Configured Cloudflare zone ID or name")
				cmd.Flags().StringArray("origin", nil, "Explicit public origin IP, repeatable")
				cmd.Flags().String("proxy", "", "Proxy mode: dns-only or proxied")
				cmd.Flags().Uint64("expected-revision", 0, "Exact current definition DNS policy revision")
			}, run: a.domainCloudflarePolicy},
		},
	}}
}

func addDNSProviderFlag(cmd *cobra.Command) {
	cmd.Flags().String("provider", "", "DNS provider (required: cloudflare)")
	_ = cmd.MarkFlagRequired("provider")
}

func dnsProviderArgs(cmd *cobra.Command, args []string) error {
	if err := platformReadArgs(cmd, args); err != nil {
		return err
	}
	provider, _ := cmd.Flags().GetString("provider")
	if !cmd.Flags().Changed("provider") || strings.TrimSpace(provider) == "" {
		return contract.NewFault(contract.InputRequired, "DNS provider is required", "Pass --provider cloudflare.")
	}
	if provider != "cloudflare" {
		return contract.NewFault(contract.Usage, "unknown DNS provider", "Pass --provider cloudflare.")
	}
	return nil
}

func (a *app) domainCloudflareConfigure(cmd *cobra.Command, _ []string) (result, error) {
	// Structured output and every non-terminal invocation refuse before a
	// token prompt or RPC. The token has no other input path.
	if !humanAtTerminal(cmd) {
		return result{}, contract.NewFault(contract.InputRequired, "Cloudflare credential configuration needs a human at an interactive terminal", "Run it yourself in a terminal without --json, --output json/ndjson, or a pipe. There is no token flag, environment variable, file or stdin fallback.")
	}
	return a.configureCloudflareCredential(cmd, cloudflarePrompts{
		input: ui.Input, secret: ui.Secret, confirm: ui.YesNoDefault,
	})
}

// cloudflarePrompts is the terminal interaction this flow needs. It is a
// parameter rather than a package variable so the regression test can prove a
// missing workspace fails before any prompt without touching global state.
type cloudflarePrompts struct {
	input   func(title, description, def string) (string, error)
	secret  func(title string) (string, error)
	confirm func(title, description string, def bool) (bool, error)
}

// configureCloudflareCredential resolves the selected workspace and client before
// any prompt: a missing workspace or unusable route must fail before the operator
// types a zone or a token. The read-only provider validation still comes only
// after the token is read.
func (a *app) configureCloudflareCredential(cmd *cobra.Command, prompts cloudflarePrompts) (result, error) {
	client, err := a.client(cmd)
	if err != nil {
		return result{}, err
	}
	zoneID, _ := cmd.Flags().GetString("zone")
	zoneName, _ := cmd.Flags().GetString("zone-name")
	zoneID = strings.ToLower(strings.TrimSpace(zoneID))
	zoneName = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(zoneName), "."))
	if zoneID == "" {
		zoneID, err = prompts.input("Cloudflare zone ID", "The lowercase 32-character zone identifier", "")
		if err != nil {
			return result{}, err
		}
		zoneID = strings.ToLower(strings.TrimSpace(zoneID))
	}
	if _, err := vault.NewProviderScope(vault.ProviderCloudflare, zoneID); err != nil {
		return result{}, err
	}
	if zoneName == "" {
		zoneName, err = prompts.input("Cloudflare zone name", "For example example.com", "")
		if err != nil {
			return result{}, err
		}
		zoneName = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(zoneName), "."))
	}
	if err := vault.ValidateProviderZoneName(zoneName); err != nil {
		return result{}, err
	}
	token, err := prompts.secret("Cloudflare API token for " + zoneName + " (input hidden)")
	if err != nil {
		return result{}, err
	}
	if strings.TrimSpace(token) == "" {
		return result{}, contract.NewFault(contract.InputRequired, "a Cloudflare API token is required", "Enter it at the hidden terminal prompt; there is no token flag, environment variable, file or stdin fallback.")
	}
	a.secrets.Add(token)
	tokenBytes := []byte(token)
	defer clearSecretBytes(tokenBytes)

	preview, err := client.PreviewCloudflareCredential(cmd.Context(), zoneID, zoneName, tokenBytes)
	if err != nil {
		return result{}, err
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "Cloudflare credential %s (%s)\n  current revision: %d\n  next revision:    %d\n  validation:       %s\n",
		zoneName, zoneID, preview.CurrentRevision, preview.NextRevision, preview.Validation)
	if preview.CurrentRevision == 0 {
		fmt.Fprintln(cmd.ErrOrStderr(), "  status:           new credential")
	} else {
		fmt.Fprintf(cmd.ErrOrStderr(), "  status:           replaces revision %d only if it is still current\n", preview.CurrentRevision)
	}
	confirmed, err := prompts.confirm("Save this Cloudflare credential?", "The token was read-validated. A concurrent replacement will be refused rather than overwritten.", false)
	if err != nil {
		return result{}, err
	}
	if !confirmed {
		return result{
			data: map[string]any{
				"saved": false, "provider": preview.Provider, "zone_id": preview.ZoneID,
				"zone_name": preview.ZoneName, "current_revision": preview.CurrentRevision,
				"read_validated": preview.ReadValidated, "validation": preview.Validation,
			},
			plain: "Cloudflare credential was not changed (revision " + fmt.Sprint(preview.CurrentRevision) + " remains active).\n",
		}, nil
	}
	metadata, err := client.ConfigureCloudflareCredential(cmd.Context(), zoneID, zoneName, preview.CurrentRevision, tokenBytes)
	if err != nil {
		return result{}, err
	}
	data := map[string]any{
		"saved": true, "credential": metadata,
		"read_validated": true, "validation": preview.Validation,
		"warnings": []string{
			"The token was read-validated with a bounded DNS list; this does not prove DNS write capability.",
			"The token value is not shown again.",
		},
	}
	plain := formatCloudflareCredential(metadata) + "\nThe token was read-validated with a bounded DNS list; this does not prove DNS write capability.\n"
	return result{data: data, plain: plain}, nil
}

func (a *app) domainCloudflareStatus(cmd *cobra.Command, _ []string) (result, error) {
	zone, _ := cmd.Flags().GetString("zone")
	client, err := a.client(cmd)
	if err != nil {
		return result{}, err
	}
	credentials, err := client.ListCloudflareCredentials(cmd.Context(), zone)
	if err != nil {
		return result{}, err
	}
	data := map[string]any{"credentials": credentials}
	policyText := ""
	if policies, ok := client.(definitionDNSPolicyClient); ok {
		policy, err := policies.GetDefinitionDNSPolicy(cmd.Context())
		if err != nil {
			return result{}, err
		}
		data["definition_dns_policy"] = policy
		policyText = formatDefinitionDNSPolicy(policy)
	}
	if len(credentials) == 0 {
		plain := "No Cloudflare provider credential is configured"
		if strings.TrimSpace(zone) != "" {
			plain += " for " + strings.TrimSpace(zone)
		}
		return result{data: data, plain: plain + ".\nConfigure one with eregion dns configure --provider cloudflare.\n" + policyText}, nil
	}
	lines := make([]string, 0, len(credentials))
	for _, credential := range credentials {
		lines = append(lines, formatCloudflareCredential(credential))
	}
	return result{data: data, plain: strings.Join(lines, "\n") + "\n" + policyText}, nil
}

type definitionDNSPolicyClient interface {
	GetDefinitionDNSPolicy(context.Context) (api.DefinitionDNSPolicy, error)
	SetDefinitionDNSPolicy(context.Context, api.DefinitionDNSPolicyInput) (api.DefinitionDNSPolicy, error)
}

func (a *app) domainCloudflarePolicy(cmd *cobra.Command, _ []string) (result, error) {
	if !cmd.Flags().Changed("enabled") || !cmd.Flags().Changed("expected-revision") {
		return result{}, contract.NewFault(contract.InputRequired, "definition DNS policy needs explicit enabled state and expected revision", "Read dns status --provider cloudflare, then pass --enabled=true or --enabled=false and --expected-revision.")
	}
	enabled, _ := cmd.Flags().GetBool("enabled")
	zone, _ := cmd.Flags().GetString("zone")
	origins, _ := cmd.Flags().GetStringArray("origin")
	proxy, _ := cmd.Flags().GetString("proxy")
	revision, _ := cmd.Flags().GetUint64("expected-revision")
	client, err := a.client(cmd)
	if err != nil {
		return result{}, err
	}
	policies, ok := client.(definitionDNSPolicyClient)
	if !ok {
		return result{}, contract.NewFault(contract.Unavailable, "definition DNS policy is unavailable", "Use a matching node and operator build.")
	}
	policy, err := policies.SetDefinitionDNSPolicy(cmd.Context(), api.DefinitionDNSPolicyInput{ExpectedRevision: revision, Enabled: enabled, Zone: zone, Origins: origins, ProxyMode: proxy})
	if err != nil {
		return result{}, err
	}
	return result{data: policy, plain: formatDefinitionDNSPolicy(policy)}, nil
}

func formatDefinitionDNSPolicy(policy api.DefinitionDNSPolicy) string {
	return fmt.Sprintf("Definition DNS policy: enabled=%t revision=%d zone=%s (%s) origins=%s proxy=%s\nPolicy configuration does not apply DNS or prove provider write access.\n", policy.Enabled, policy.Revision, policy.ZoneName, policy.ZoneID, strings.Join(policy.Origins, ","), policy.ProxyMode)
}

func formatCloudflareCredential(metadata vault.ProviderMetadata) string {
	return fmt.Sprintf("%s (%s) revision %d, updated %s by %s",
		metadata.ZoneName, metadata.ZoneID, metadata.LatestRevision,
		metadata.UpdatedAt.Format(time.RFC3339), metadata.UpdatedBy.Subject)
}
