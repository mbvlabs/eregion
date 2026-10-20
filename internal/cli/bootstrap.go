package cli

import (
	"os"
	"strings"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/host"
	"github.com/spf13/cobra"
)

// bootstrapSpecs is attached by the CLI owner; no init-time registration.
func (a *app) bootstrapSpecs() []spec {
	meta := readMeta("Inspect local role-aware bootstrap", []string{"Bootstrap a fresh supported Linux host; no interactive prompts. Access-changing apply is explicitly unsupported."}, []string{"eregion host plan --role application --node app1 --node-binary /private/eregion-node --json"})
	meta.Platforms = []string{"linux"}
	meta.Environment = []string{"EREGION_DATABASE_URL"}
	meta.ErrorCodes = append(meta.ErrorCodes, contract.ValidationFailed, contract.PreconditionFailed, contract.ConfirmationRequired, contract.Forbidden)
	var children []spec
	for _, action := range []string{"plan", "diff", "status", "apply"} {
		m := meta
		m.Summary = "Host bootstrap " + action
		if action == "apply" {
			m.Mutating, m.DryRun, m.Confirmation, m.NeedsRestart = true, true, "plan-bound", "required"
		}
		children = append(children, spec{use: action, meta: m, flags: bootstrapFlags, run: func(cmd *cobra.Command, _ []string) (result, error) {
			cfg, err := a.bootstrapConfig(cmd)
			if err != nil {
				return result{}, err
			}
			runner, err := host.NewRunner(cfg, a.secrets)
			if err != nil {
				return result{}, err
			}
			dry, _ := cmd.Flags().GetBool("dry-run")
			if action == "status" {
				report, err := runner.Status(cmd.Context())
				return hostResult(report), err
			}
			if action != "apply" || dry {
				plan, err := runner.Plan(cmd.Context())
				return hostResult(plan), err
			}
			apply, _ := cmd.Flags().GetBool("apply")
			if !apply {
				return result{}, contract.NewFault(contract.ConfirmationRequired, "host mutation requires --apply", "Run host apply --dry-run, then supply --apply and --confirm with its plan token.")
			}
			confirm, _ := cmd.Flags().GetString("confirm")
			report, err := runner.Apply(cmd.Context(), confirm, false)
			return hostResult(report), err
		}})
	}
	return []spec{{use: "host", meta: meta, run: func(cmd *cobra.Command, _ []string) (result, error) { return a.help(cmd), nil }, children: children}}
}

func bootstrapFlags(cmd *cobra.Command) {
	f := cmd.Flags()
	f.String("role", "main", "Placement: main, application, or builder (no telemetry)")
	f.String("node", "local", "Node name; main defaults to the core local execution identity")
	f.String("database-url-file", "", "Owner-only external PostgreSQL URL file; otherwise EREGION_DATABASE_URL")
	f.String("operator-token-file", "", "Optional existing owner-only operator token input")
	f.String("operator-token-out", "", "Required absolute operator token output for main; mode 0600")
	f.String("automation-subject", "", "Optional CI credential subject")
	f.StringSlice("automation-scope", nil, "Explicit CI scopes")
	f.String("automation-token-out", "", "Absolute CI token output; mode 0600")
	f.String("binary", "", "Control-plane binary source; defaults to this executable")
	f.String("node-binary", "", "eregion-node source for application/builder")
	f.String("api-listen", "127.0.0.1:8080", "API listener; non-loopback apply is unsupported")
	f.String("api-tls-cert", "", "TLS certificate source (plan only; access-changing apply unsupported)")
	f.String("api-tls-key", "", "TLS private key source (plan only)")
	f.String("node-authorized-keys-file", "", "SSH public keys source (plan only; enrollment apply unsupported)")
	f.Int("worker-concurrency", 2, "Worker concurrency (1–16)")
	f.Bool("dry-run", false, "Read-only plan/diff; never applies")
	f.Bool("apply", false, "Explicitly authorize host mutation (apply command only)")
	f.String("confirm", "", "Exact unexpired plan-bound confirmation token")
}

func (a *app) bootstrapConfig(cmd *cobra.Command) (host.Config, error) {
	f := cmd.Flags()
	get := func(name string) string { v, _ := f.GetString(name); return v }
	cfg := host.DefaultConfig(host.Role(get("role")), get("node"))
	cfg.DatabaseURL = os.Getenv("EREGION_DATABASE_URL")
	if path := get("database-url-file"); path != "" {
		raw, err := host.ReadPrivateInput(path)
		if err != nil {
			return cfg, err
		}
		cfg.DatabaseURL = strings.TrimSpace(string(raw))
	}
	a.secrets.Add(cfg.DatabaseURL)
	cfg.OperatorTokenFile, cfg.OperatorTokenOut = get("operator-token-file"), get("operator-token-out")
	cfg.AutomationSubject, cfg.AutomationTokenOut = get("automation-subject"), get("automation-token-out")
	cfg.AutomationScopes, _ = f.GetStringSlice("automation-scope")
	cfg.BinaryPath, cfg.NodeBinaryPath = get("binary"), get("node-binary")
	cfg.APIListen, cfg.APITLSCert, cfg.APITLSKey = get("api-listen"), get("api-tls-cert"), get("api-tls-key")
	cfg.NodeAuthorizedKeysFile = get("node-authorized-keys-file")
	cfg.WorkerConcurrency, _ = f.GetInt("worker-concurrency")
	return cfg, nil
}
