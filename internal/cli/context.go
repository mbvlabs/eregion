package cli

import (
	"encoding/json"
	"os"
	"strings"
	"time"

	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/host"
	"github.com/spf13/cobra"
)

func hostResult(data any) result {
	raw, _ := json.MarshalIndent(data, "", "  ")
	return result{data: data, plain: string(raw)}
}

// contextClient is the integration hook for a.client in runtime.go. Precedence
// is explicit: --api-url, then EREGION_API_URL, then the active saved context.
// A saved credential is never sent to a different explicit origin.
func (a *app) contextClient(cmd *cobra.Command) (*api.Client, error) {
	flagEndpoint := ""
	if cmd != nil && cmd.Flags().Lookup("api-url") != nil {
		flagEndpoint, _ = cmd.Flags().GetString("api-url")
	}
	envEndpoint := os.Getenv("EREGION_API_URL")
	token := os.Getenv("EREGION_API_TOKEN")
	endpoint := flagEndpoint
	explicitOrigin := endpoint != ""
	if endpoint == "" {
		endpoint = envEndpoint
		explicitOrigin = endpoint != ""
	}
	if endpoint != "" && token != "" {
		a.secrets.Add(token)
		return api.NewClient(endpoint, token)
	}
	store, err := host.LoadContextStore("")
	if err != nil {
		return nil, err
	}
	if store.Current != "" {
		current, err := store.Get("")
		if err != nil {
			return nil, err
		}
		if endpoint == "" {
			endpoint = current.APIURL
		}
		if token == "" {
			if sameOrigin(endpoint, current.APIURL) {
				token = current.Token
			} else if explicitOrigin {
				return nil, contract.NewFault(contract.InputRequired, "the explicit API origin differs from the active context", "Supply EREGION_API_TOKEN for that origin or select the matching context with eregion context use.")
			}
		}
	}
	if endpoint == "" {
		endpoint = "http://127.0.0.1:8080"
	}
	a.secrets.Add(token)
	return api.NewClient(endpoint, token)
}

func sameOrigin(left, right string) bool {
	return strings.EqualFold(strings.TrimRight(strings.TrimSpace(left), "/"), strings.TrimRight(strings.TrimSpace(right), "/"))
}

func (a *app) contextSpecs() []spec {
	meta := func(summary, example string, mutating bool) CommandMeta {
		m := readMeta(summary, []string{summary + ". Never prints credentials; no interactive prompts."}, []string{example})
		m.Mutating = mutating
		if mutating {
			m.DryRunReason = "Local credential-store edits have no remote effect; inspect context show/list before changing them."
		}
		m.Environment = []string{"EREGION_CONTEXT_FILE", "EREGION_API_TOKEN", "EREGION_API_URL"}
		m.ErrorCodes = append(m.ErrorCodes, contract.ValidationFailed, contract.PreconditionFailed, contract.NotFound)
		return m
	}
	load := func(cmd *cobra.Command) (*host.ContextStore, error) {
		path, _ := cmd.Flags().GetString("context-file")
		return host.LoadContextStore(path)
	}
	return []spec{{use: "context", meta: meta("Manage private operator contexts", "eregion context list --json", false),
		flags: func(cmd *cobra.Command) {
			cmd.PersistentFlags().String("context-file", "", "Private context store (defaults to EREGION_CONTEXT_FILE or XDG config)")
		},
		run: func(cmd *cobra.Command, _ []string) (result, error) { return a.help(cmd), nil },
		children: []spec{
			{use: "login", meta: meta("Save and select an API context", "eregion context login --name production --api-url https://api.example.com --token-file /private/operator.token", true),
				flags: func(cmd *cobra.Command) {
					cmd.Flags().String("name", "default", "Context name")
					cmd.Flags().String("api-url", "", "API origin URL; HTTPS except literal loopback")
					cmd.Flags().String("token-file", "", "Owner-only token input; otherwise EREGION_API_TOKEN")
					cmd.Flags().StringSlice("scope", nil, "Informational API scopes (not a server-side grant)")
				}, run: func(cmd *cobra.Command, _ []string) (result, error) {
					name, _ := cmd.Flags().GetString("name")
					endpoint, _ := cmd.Flags().GetString("api-url")
					path, _ := cmd.Flags().GetString("token-file")
					scopes, _ := cmd.Flags().GetStringSlice("scope")
					token := os.Getenv("EREGION_API_TOKEN")
					if path != "" {
						var err error
						token, err = host.ReadTokenFile(path)
						if err != nil {
							return result{}, err
						}
					}
					a.secrets.Add(token)
					if endpoint == "" {
						endpoint = os.Getenv("EREGION_API_URL")
					}
					store, err := load(cmd)
					if err != nil {
						return result{}, err
					}
					view, err := store.Login(name, endpoint, token, scopes)
					return hostResult(view), err
				}},
			{use: "list", meta: meta("List contexts without tokens", "eregion context list --json", false), run: func(cmd *cobra.Command, _ []string) (result, error) {
				store, err := load(cmd)
				if err != nil {
					return result{}, err
				}
				return hostResult(store.List()), nil
			}},
			{use: "show", meta: meta("Show a context without its token", "eregion context show --name production --json", false), flags: contextNameFlag, run: func(cmd *cobra.Command, _ []string) (result, error) {
				store, err := load(cmd)
				if err != nil {
					return result{}, err
				}
				name, _ := cmd.Flags().GetString("name")
				view, err := store.Show(name)
				return hostResult(view), err
			}},
			{use: "use", meta: meta("Select the active context", "eregion context use --name production", true), flags: contextNameFlag, run: func(cmd *cobra.Command, _ []string) (result, error) {
				store, err := load(cmd)
				if err != nil {
					return result{}, err
				}
				name, _ := cmd.Flags().GetString("name")
				view, err := store.Use(name)
				return hostResult(view), err
			}},
			{use: "export", meta: meta("Export a token only to an explicit private file", "eregion context export --out /private/ci.token", true), flags: func(cmd *cobra.Command) {
				contextNameFlag(cmd)
				cmd.Flags().String("out", "", "Absolute owner-only token output file (never stdout)")
				cmd.Flags().Bool("replace", false, "Replace a different existing private token")
			}, run: func(cmd *cobra.Command, _ []string) (result, error) {
				store, err := load(cmd)
				if err != nil {
					return result{}, err
				}
				name, _ := cmd.Flags().GetString("name")
				path, _ := cmd.Flags().GetString("out")
				replace, _ := cmd.Flags().GetBool("replace")
				report, err := store.Export(name, path, replace)
				return hostResult(report), err
			}},
			a.contextCredentialSpec(meta),
		}}}
}

// contextCredentialSpec adds a scoped automation actor to an existing server
// auth file. It is a plan-bound mutation: dry-run returns the revision and
// token, and apply requires that exact token. The credential is written once to
// an owner-only file; neither the plan nor the result prints it.
func (a *app) contextCredentialSpec(meta func(string, string, bool) CommandMeta) spec {
	credentialMeta := meta("Add a scoped automation credential to the server auth file", "eregion context credential add --auth-file /etc/eregion/auth.json --subject ci-deploy --scope operations:submit --token-out /root/ci.token --dry-run --json", true)
	credentialMeta.Mutating, credentialMeta.DryRun, credentialMeta.Confirmation = true, true, "plan-bound"
	credentialMeta.DryRunReason = ""
	return spec{use: "credential", meta: meta("Manage scoped automation credentials", "eregion context credential add --dry-run --json", false),
		run: func(cmd *cobra.Command, _ []string) (result, error) { return a.help(cmd), nil },
		children: []spec{
			{use: "add", meta: credentialMeta, flags: func(cmd *cobra.Command) {
				cmd.Flags().String("auth-file", "/etc/eregion/auth.json", "Server auth file containing token hashes")
				cmd.Flags().String("subject", "", "Stable CI actor subject")
				cmd.Flags().StringSlice("scope", nil, "Explicit scopes (operations:submit, operations:read, operations:cancel, system:read)")
				cmd.Flags().String("token-out", "", "Absolute owner-only token output; never printed")
				cmd.Flags().Bool("dry-run", false, "Show the base revision and confirmation token without writing")
				cmd.Flags().String("confirm", "", "Exact plan-bound confirmation token")
			}, run: func(cmd *cobra.Command, _ []string) (result, error) {
				authFile, _ := cmd.Flags().GetString("auth-file")
				subject, _ := cmd.Flags().GetString("subject")
				scopes, _ := cmd.Flags().GetStringSlice("scope")
				tokenOut, _ := cmd.Flags().GetString("token-out")
				request := host.CredentialAddRequest{
					AuthFile: authFile, Actor: contract.Actor{Kind: "ci", Subject: subject},
					Scopes: scopes, TokenOut: tokenOut,
				}
				if err := request.Validate(); err != nil {
					return result{}, err
				}
				revision, err := host.AuthRevision(authFile)
				if err != nil {
					return result{}, err
				}
				now := time.Now()
				plan, err := request.Confirmation(now)
				if err != nil {
					return result{}, err
				}
				dryRun, _ := cmd.Flags().GetBool("dry-run")
				if dryRun {
					return hostResult(map[string]any{
						"auth_file": authFile, "actor_kind": "ci", "actor_subject": subject,
						"scopes": scopes, "base_revision": revision,
						"confirmation": plan, "dry_run": true,
					}), nil
				}
				confirmation, _ := cmd.Flags().GetString("confirm")
				if confirmation == "" {
					return result{}, contract.NewFault(contract.ConfirmationRequired, "--confirm is required to add a credential", "Run context credential add --dry-run and pass its confirmation token.")
				}
				report, err := request.Apply(confirmation, now)
				return hostResult(report), err
			}},
		}}
}

func contextNameFlag(cmd *cobra.Command) {
	cmd.Flags().String("name", "", "Context name; defaults to the active context")
}
