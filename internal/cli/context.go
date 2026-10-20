package cli

import (
	"encoding/json"
	"os"
	"strings"

	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/host"
	"github.com/spf13/cobra"
)

func hostResult(data any) result {
	raw, _ := json.MarshalIndent(data, "", "  ")
	return result{data: data, plain: string(raw)}
}

// contextClient is the integration hook for a.client in runtime.go: replace
// its body with `return a.contextClient(cmd)`. Explicit flags/environment win;
// missing endpoint/token values fall back to the active saved context.
func (a *app) contextClient(cmd *cobra.Command) (*api.Client, error) {
	endpoint, _ := cmd.Flags().GetString("api-url")
	if endpoint == "" {
		endpoint = os.Getenv("EREGION_API_URL")
	}
	token := os.Getenv("EREGION_API_TOKEN")
	if endpoint == "" || token == "" {
		store, err := host.LoadContextStore("")
		if err != nil {
			return nil, err
		}
		if store.Current != "" {
			current, err := store.Get("")
			if err != nil {
				return nil, err
			}
			// Never send a saved credential to an explicitly different origin.
			if endpoint != "" && strings.TrimRight(endpoint, "/") != current.APIURL && token == "" {
				return nil, contract.NewFault(contract.InputRequired, "the explicit API origin differs from the active context", "Supply EREGION_API_TOKEN for that origin or select the matching context.")
			}
			if endpoint == "" {
				endpoint = current.APIURL
			}
			if token == "" {
				token = current.Token
			}
		}
	}
	if endpoint == "" {
		endpoint = "http://127.0.0.1:8080"
	}
	a.secrets.Add(token)
	return api.NewClient(endpoint, token)
}

func (a *app) contextSpecs() []spec {
	meta := func(summary, example string, mutating bool) CommandMeta {
		m := readMeta(summary, []string{summary + ". Never prints credentials; no interactive prompts."}, []string{example})
		m.Mutating = mutating
		if mutating {
			m.DryRunReason = "Local credential-store edits have no remote effect; inspect context show/list before changing them."
		}
		m.Environment = []string{"EREGION_CONTEXT_FILE", "EREGION_API_TOKEN"}
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
		}}}
}

func contextNameFlag(cmd *cobra.Command) {
	cmd.Flags().String("name", "", "Context name; defaults to the active context")
}
