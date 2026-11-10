package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/cli/ui"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/vault"
	"github.com/spf13/cobra"
)

// domainCloudflareSpec is the bounded human setup surface for provider DNS
// credentials. It is deliberately not a platform action: no token flag, file,
// environment variable or stdin form exists.
func (a *app) domainCloudflareSpec() spec {
	parent := platformMeta("View and configure Cloudflare provider credentials", "eregion domain cloudflare status --json")

	configure := platformMeta("Configure or replace one Cloudflare provider credential at a hidden terminal prompt", "eregion domain cloudflare configure --zone 0123456789abcdef0123456789abcdef --zone-name example.com")
	configure.Mutating = true
	configure.Prompts = true
	configure.Audience = []string{"human"}
	configure.InputFlags = []string{"zone", "zone-name"}
	configure.DryRunReason = "Configuration always previews a read-only token validation and the observed current revision, then asks for confirmation; there is no noninteractive or secret-flag path."
	configure.WhenToUse = []string{
		"Store or rotate the Cloudflare API token used by explicit --dns cloudflare domain bindings. The token is typed at a no-echo terminal prompt and stored encrypted in PostgreSQL; there is no token flag, file, environment or stdin fallback.",
		"The token is read-validated with a bounded DNS list for the exact configured zone. That proves DNS list access only, not write capability.",
	}
	configure.Examples = []string{
		"eregion domain cloudflare configure --zone 0123456789abcdef0123456789abcdef --zone-name example.com",
	}
	configure = terminalOnlyMeta(configure)
	configure.Next = []string{"eregion domain cloudflare status --json"}
	configure.ErrorCodes = []contract.Code{
		contract.Usage, contract.InvalidOutputMode, contract.InputRequired, contract.ValidationFailed,
		contract.NotFound, contract.Unauthenticated, contract.Forbidden, contract.Conflict,
		contract.PreconditionFailed, contract.NodeVersionMismatch, contract.Unavailable,
		contract.Internal, contract.Cancelled,
	}

	status := platformMeta("Show configured Cloudflare zones and credential revisions without token values", "eregion domain cloudflare status --json")
	status.Next = []string{"eregion domain cloudflare configure"}
	status.WhenToUse = []string{"Inspect which Cloudflare zones have an encrypted credential and their current revision. It never decrypts or prints a token."}

	return spec{
		use:  "cloudflare",
		meta: parent,
		run:  func(cmd *cobra.Command, _ []string) (result, error) { return a.help(cmd), nil },
		children: []spec{
			{use: "configure", meta: configure, args: platformReadArgs, flags: func(cmd *cobra.Command) {
				cmd.Flags().String("zone", "", "Cloudflare zone ID (lowercase 32 characters)")
				cmd.Flags().String("zone-name", "", "Cloudflare zone name such as example.com")
			}, run: a.domainCloudflareConfigure},
			{use: "status", meta: status, args: platformReadArgs, flags: func(cmd *cobra.Command) {
				cmd.Flags().String("zone", "", "Filter by Cloudflare zone ID or name")
			}, run: a.domainCloudflareStatus},
		},
	}
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
	if len(credentials) == 0 {
		plain := "No Cloudflare provider credential is configured"
		if strings.TrimSpace(zone) != "" {
			plain += " for " + strings.TrimSpace(zone)
		}
		return result{data: data, plain: plain + ".\nConfigure one with eregion domain cloudflare configure.\n"}, nil
	}
	lines := make([]string, 0, len(credentials))
	for _, credential := range credentials {
		lines = append(lines, formatCloudflareCredential(credential))
	}
	return result{data: data, plain: strings.Join(lines, "\n") + "\n"}, nil
}

func formatCloudflareCredential(metadata vault.ProviderMetadata) string {
	return fmt.Sprintf("%s (%s) revision %d, updated %s by %s",
		metadata.ZoneName, metadata.ZoneID, metadata.LatestRevision,
		metadata.UpdatedAt.Format(time.RFC3339), metadata.UpdatedBy.Subject)
}
