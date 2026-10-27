package cli

import (
	"fmt"
	"strings"

	"github.com/mbvlabs/eregion/internal/cli/ui"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/tokens"
	"github.com/spf13/cobra"
)

// tokenMeta is the shared metadata for per-use deploy credentials.
func tokenMeta(summary, example string) CommandMeta {
	meta := operationMeta(summary, example)
	meta.WhenToUse = []string{summary + ". Tokens are hashed in the node's root-only auth file, scoped to one environment's deploys, expiring and revocable. Minting needs a TOTP code at a terminal; no token is handed off at setup."}
	return meta
}

// tokenSpecs is included in the command tree by tree.go.
func (a *app) tokenSpecs() []spec {
	createMeta := tokenMeta("Mint a per-use deploy token for one environment", "eregion token create --app api --environment production --expires 24h")
	createMeta.Mutating, createMeta.DryRun = true, true
	revokeMeta := tokenMeta("Revoke a deploy token by name", "eregion token revoke deploy-api-production-a1b2c3 --confirm deploy-api-production-a1b2c3 --json")
	revokeMeta.Mutating, revokeMeta.Destructive = true, true
	revokeMeta.Confirmation = "plan-bound"
	revokeMeta.DryRunReason = "Revocation is a local hash removal; list tokens to inspect the target instead of a preview."
	return []spec{{
		use:  "token",
		meta: tokenMeta("Manage per-use deploy tokens", "eregion token list --json"),
		run:  func(cmd *cobra.Command, _ []string) (result, error) { return a.help(cmd), nil },
		children: []spec{
			{use: "create", meta: createMeta, flags: func(cmd *cobra.Command) {
				cmd.Flags().String("app", "", "Application name the token may deploy")
				cmd.Flags().String("environment", "", "Environment name the token may deploy")
				cmd.Flags().String("name", "", "Stable token name for list and revoke; generated when empty")
				cmd.Flags().Duration("expires", tokens.DefaultTTL, "Lifetime (1m–2160h)")
				cmd.Flags().Bool("dry-run", false, "Show the scope and expiry without minting a token")
				cmd.Flags().Bool("yes", false, "Refused: a token needs a code from the authenticator app")
			}, run: a.tokenCreate},
			{use: "list", meta: tokenMeta("List deploy token metadata", "eregion token list --json"), run: a.tokenList},
			{use: "revoke <name>", meta: revokeMeta, args: func(_ *cobra.Command, args []string) error {
				if len(args) != 1 {
					return contract.NewFault(contract.InputRequired, "token name is required", "Supply the name shown by eregion token list.")
				}
				return nil
			}, flags: func(cmd *cobra.Command) {
				cmd.Flags().String("confirm", "", "Exact token name confirmation")
			}, run: a.tokenRevoke},
		},
	}}
}

func (a *app) tokenCreate(cmd *cobra.Command, _ []string) (result, error) {
	appName, _ := cmd.Flags().GetString("app")
	environment, _ := cmd.Flags().GetString("environment")
	name, _ := cmd.Flags().GetString("name")
	ttl, _ := cmd.Flags().GetDuration("expires")
	if err := platform.ValidateName(appName); err != nil {
		return result{}, err
	}
	if err := platform.ValidateName(environment); err != nil {
		return result{}, err
	}
	if name != "" {
		if err := tokens.ValidateName(name); err != nil {
			return result{}, err
		}
	}
	if ttl < tokens.MinTTL || ttl > tokens.MaxTTL {
		return result{}, contract.NewFault(contract.ValidationFailed, "token lifetime out of range", "Use --expires from 1m to 2160h (90 days).")
	}
	request := tokens.CreateRequest{App: appName, Environment: environment, Name: name, TTL: ttl}
	dryRun, _ := cmd.Flags().GetBool("dry-run")
	if dryRun {
		preview := map[string]any{
			"app": appName, "environment": environment, "name": name,
			"scopes":       []string{"operations:submit", "operations:read"},
			"action_grant": map[string]string{"kind": platform.KindDeploy, "target": "env/" + appName + "/" + environment},
			"expires_in":   ttl.String(), "dry_run": true,
		}
		return result{data: preview, plain: fmt.Sprintf("Dry run: would mint a deploy token for env/%s/%s, valid for %s.\n", appName, environment, ttl)}, nil
	}
	if err := requireHuman(cmd, "eregion token create"); err != nil {
		return result{}, err
	}
	code, err := readProtectCode(cmd, "Code from the authenticator app")
	if err != nil {
		return result{}, err
	}
	client, err := a.client(cmd)
	if err != nil {
		return result{}, err
	}
	issued, err := client.TokenCreate(cmd.Context(), request, code)
	if err != nil {
		return result{}, err
	}
	a.secrets.Add(issued.Token)
	printTokenOnce(cmd, issued)
	return result{data: issued.Metadata, plain: tokenMetadataPlain(issued.Metadata) + "\nThe token was shown once on stderr; only its hash is stored on the node.\n"}, nil
}

func printTokenOnce(cmd *cobra.Command, issued tokens.Issued) {
	_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
		"\nDeploy token %s for env/%s/%s (expires %s):\n\n%s\n\n"+
			"Use it as: curl -H \"Authorization: Bearer <token>\" -H \"Content-Type: application/json\" "+
			"-d '{\"ref\":\"<commit>\"}' https://<main-hostname>/v1/environments/%s/%s/deployments\n\n"+
			"The node stores only its hash and does not show it again.\n",
		issued.Metadata.Name, issued.Metadata.App, issued.Metadata.Environment,
		tokens.FormatExpiry(issued.Metadata.ExpiresAt), issued.Token,
		issued.Metadata.App, issued.Metadata.Environment)
}

func (a *app) tokenList(cmd *cobra.Command, _ []string) (result, error) {
	client, err := a.client(cmd)
	if err != nil {
		return result{}, err
	}
	list, err := client.TokenList(cmd.Context())
	if err != nil {
		return result{}, err
	}
	var b strings.Builder
	b.WriteString(ui.Header("Deploy tokens", 72) + "\n")
	if len(list) == 0 {
		b.WriteString("  none\n")
	} else {
		rows := make([][]string, 0, len(list))
		for _, item := range list {
			rows = append(rows, []string{
				item.Name, item.App, item.Environment, strings.Join(item.Scopes, ","),
				tokens.FormatExpiry(item.ExpiresAt), item.Fingerprint,
			})
		}
		b.WriteString(writeTable([]string{"NAME", "APP", "ENVIRONMENT", "SCOPES", "EXPIRES", "FINGERPRINT"}, rows))
	}
	return result{data: map[string]any{"tokens": list}, plain: b.String()}, nil
}

func (a *app) tokenRevoke(cmd *cobra.Command, args []string) (result, error) {
	name := args[0]
	confirmation, _ := cmd.Flags().GetString("confirm")
	if confirmation != name {
		return result{}, contract.NewFault(contract.ConfirmationRequired, "token revocation requires exact name confirmation", "Pass --confirm "+name+" after checking eregion token list.")
	}
	client, err := a.client(cmd)
	if err != nil {
		return result{}, err
	}
	metadata, err := client.TokenRevoke(cmd.Context(), name, confirmation)
	if err != nil {
		return result{}, err
	}
	return result{data: metadata, plain: "Revoked " + metadata.Name + " for env/" + metadata.App + "/" + metadata.Environment + ".\n"}, nil
}

func tokenMetadataPlain(item tokens.Metadata) string {
	return fmt.Sprintf("%s  env/%s/%s  %s  expires %s  %s",
		item.Name, item.App, item.Environment, strings.Join(item.Scopes, ","),
		tokens.FormatExpiry(item.ExpiresAt), item.Fingerprint)
}
