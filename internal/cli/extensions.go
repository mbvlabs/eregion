package cli

import (
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/controlplane"
	"github.com/mbvlabs/eregion/internal/integrations"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/vault"
	"github.com/spf13/cobra"
)

// extensionEnvironment lists the variables that configure the extension
// packages. Both serve and worker read the shared ones, so any control plane can
// execute what any other accepted.
var extensionEnvironment = []string{
	"EREGION_VAULT_KEY_FILE",
	"EREGION_ACCESS_ADMINS",
	"EREGION_DNS_ADMINS",
}

// serveExtensionEnvironment lists the variables only the API listener reads.
var serveExtensionEnvironment = []string{
	"EREGION_PRIVATE_UPSTREAM",
}

// extensionRuntime is what the shared startup built for the extensions.
type extensionRuntime struct {
	Vault      *vault.Service
	Extensions *controlplane.Extensions
	DomainDNS  *integrations.DomainDNS
	Cloudflare *controlplane.CloudflareSetup
	Deliveries *controlplane.PostgresWebhookDeliveries
	Webhooks   *controlplane.PushWebhookResolver
}

// startExtensions builds the vault service and registers the extension actions.
// It returns the secret resolver to hand to controlplane.New; the resolver is
// installed even without a keyring so a deployment that references secrets fails
// with an instruction instead of silently deploying without them. A process
// without workers (executes false) registers the actions for validation but never
// touches Docker, so an API-only host needs no container runtime.
func (a *app) startExtensions(pool *pgxpool.Pool) (extensionRuntime, *vault.EnvironmentResolver, error) {
	var parts extensionRuntime
	if path := os.Getenv("EREGION_VAULT_KEY_FILE"); path != "" {
		service, err := controlplane.NewVaultService(pool, path)
		if err != nil {
			return parts, nil, err
		}
		parts.Vault = service
	}
	config := controlplane.ExtensionConfig{Pool: pool}
	config.AccessAdmins = splitList(os.Getenv("EREGION_ACCESS_ADMINS"))
	config.DNSAdmins = splitList(os.Getenv("EREGION_DNS_ADMINS"))
	var credentials *controlplane.CloudflareCredentialStore
	if parts.Vault != nil {
		credentials = controlplane.NewCloudflareCredentialStore(parts.Vault)
		config.DNSZones = credentials
		config.DNSCredentials = credentials
	}
	extensions, err := controlplane.RegisterExtensions(config)
	if err != nil {
		return parts, nil, err
	}
	parts.Extensions = extensions
	parts.DomainDNS = extensions.DomainDNS
	if parts.Vault != nil {
		parts.Cloudflare = controlplane.NewCloudflareSetup(parts.Vault, config.CloudflareBaseURL, config.CloudflareClient)
	}
	parts.Webhooks = controlplane.NewPushWebhookResolver(pool, parts.Vault)
	return parts, vault.NewEnvironmentResolver(parts.Vault), nil
}

// rewritePrivateRequest forwards only the path and the headers the private
// upstream needs. The upstream never sees the control-plane credential, a
// browser cookie for this origin, proxy credentials, or client-supplied
// forwarding headers.
func rewritePrivateRequest(target *url.URL, r *httputil.ProxyRequest) {
	r.SetURL(target)
	r.Out.URL.Path = "/" + strings.TrimPrefix(r.In.URL.Path, api.PrivatePrefix)
	r.Out.URL.RawPath = ""
	for _, header := range []string{
		"Authorization", "Cookie", "Proxy-Authorization",
		"X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto",
		"X-Real-IP", "Forwarded",
	} {
		r.Out.Header.Del(header)
	}
}

// serveFlag returns a flag value, falling back to its environment variable.
func serveFlag(cmd *cobra.Command, name, environment string) string {
	if value, _ := cmd.Flags().GetString(name); value != "" {
		return value
	}
	return os.Getenv(environment)
}

// buildMux composes the API: bearer-authenticated operations, the secret
// routes, the GitHub webhook, and (when configured) the private upstream.
// Every optional piece that is off is logged once so it is never silently absent.
func (a *app) buildMux(cmd *cobra.Command, logger *slog.Logger, auth *api.Auth, base http.Handler, service operations.Service, parts extensionRuntime) (http.Handler, error) {
	config := api.MuxConfig{Default: base}
	if parts.Vault != nil {
		config.Vault = api.NewVaultHandler(parts.Vault, api.AuthVaultAuthorizer(auth))
	} else {
		logger.Info("secret API disabled", "reason", "EREGION_VAULT_KEY_FILE is not set")
	}
	if parts.Webhooks != nil {
		push, err := api.NewPushWebhookHandler(api.PushWebhookConfig{
			Resolver: parts.Webhooks, Admissions: parts.Deliveries, Logger: logger,
		})
		if err != nil {
			return nil, err
		}
		config.PushWebhook = push
	}
	config.Deploy = api.NewCIDeployHandler(auth, service)
	upstream := serveFlag(cmd, "private-upstream", "EREGION_PRIVATE_UPSTREAM")
	switch {
	case upstream == "":
		logger.Info("private access enforcement disabled", "reason", "no private upstream configured")
	case parts.Extensions == nil || parts.Extensions.Access == nil:
		return nil, contract.NewFault(contract.InputRequired, "a private upstream needs access administrators", "Set EREGION_ACCESS_ADMINS so grants can be issued, or unset the private upstream.")
	default:
		target, err := url.Parse(upstream)
		if err != nil || (target.Scheme != "http" && target.Scheme != "https") || target.Host == "" {
			return nil, contract.NewFault(contract.ValidationFailed, "invalid private upstream URL", "Use an http(s) URL such as http://127.0.0.1:9000.")
		}
		proxy := &httputil.ReverseProxy{
			Rewrite: func(r *httputil.ProxyRequest) {
				rewritePrivateRequest(target, r)
			},
			ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
		}
		config.Private = api.PrivateAccess(parts.Extensions.Access, auth, proxy)
		logger.Info("private access enforcement enabled", "prefix", api.PrivatePrefix)
	}
	return api.NewMux(config), nil
}

// splitList parses a comma-separated environment value.
func splitList(text string) []string {
	var out []string
	for _, item := range strings.Split(text, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}
