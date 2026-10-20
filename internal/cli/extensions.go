package cli

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/api"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/controlplane"
	"github.com/mbvlabs/eregion/internal/integrations"
	"github.com/mbvlabs/eregion/internal/operations"
	"github.com/mbvlabs/eregion/internal/resources"
	"github.com/mbvlabs/eregion/internal/vault"
	"github.com/spf13/cobra"
)

// extensionEnvironment lists the variables that configure the extension
// packages. Both serve and worker read the shared ones, so any control plane can
// execute what any other accepted.
var extensionEnvironment = []string{
	"EREGION_VAULT_KEY_FILE",
	"EREGION_RESOURCES_HOST",
	"EREGION_RESOURCES_STATE_DIR",
	"EREGION_RESOURCES_BACKUP_DIR",
	"EREGION_ACCESS_ADMINS",
	"EREGION_DNS_ADMINS",
	"EREGION_CLOUDFLARE_ZONES",
	"EREGION_CLOUDFLARE_TOKEN_FILE",
}

// serveExtensionEnvironment lists the variables only the API listener reads.
var serveExtensionEnvironment = []string{
	"EREGION_WEBHOOK_SECRET_FILE",
	"EREGION_WEBHOOK_ALLOW",
	"EREGION_PRIVATE_UPSTREAM",
}

// extensionRuntime is what the shared startup built for the extensions.
type extensionRuntime struct {
	Vault      *vault.Service
	Extensions *controlplane.Extensions
	Deliveries *controlplane.PostgresWebhookDeliveries
}

// startExtensions builds the vault service and registers the extension actions.
// It returns the secret resolver to hand to controlplane.New; the resolver is
// installed even without a keyring so a deployment that references secrets fails
// with an instruction instead of silently deploying without them. A process
// without workers (executes false) registers the actions for validation but never
// touches Docker, so an API-only host needs no container runtime.
func (a *app) startExtensions(pool *pgxpool.Pool, executes bool, nodes controlplane.NodeTransports) (extensionRuntime, *vault.EnvironmentResolver, error) {
	var parts extensionRuntime
	if path := os.Getenv("EREGION_VAULT_KEY_FILE"); path != "" {
		service, err := controlplane.NewVaultService(pool, path)
		if err != nil {
			return parts, nil, err
		}
		parts.Vault = service
	}
	config := controlplane.ExtensionConfig{Pool: pool, DatabaseURL: os.Getenv("EREGION_DATABASE_URL"), NodeTransports: nodes}
	if host := os.Getenv("EREGION_RESOURCES_HOST"); host != "" && executes {
		state := os.Getenv("EREGION_RESOURCES_STATE_DIR")
		if state == "" {
			return parts, nil, contract.NewFault(contract.InputRequired, "EREGION_RESOURCES_STATE_DIR is required with EREGION_RESOURCES_HOST", "Set a private directory for resource credentials and restore journals.")
		}
		config.Resources = &resources.Config{Host: host, StateDir: state, BackupDir: os.Getenv("EREGION_RESOURCES_BACKUP_DIR")}
	}
	config.AccessAdmins = splitList(os.Getenv("EREGION_ACCESS_ADMINS"))
	config.DNSAdmins = splitList(os.Getenv("EREGION_DNS_ADMINS"))
	zones, err := parseCloudflareZones(os.Getenv("EREGION_CLOUDFLARE_ZONES"), os.Getenv("EREGION_CLOUDFLARE_TOKEN_FILE"))
	if err != nil {
		return parts, nil, err
	}
	config.DNSZones = zones
	extensions, err := controlplane.RegisterExtensions(config)
	if err != nil {
		return parts, nil, err
	}
	parts.Extensions = extensions
	parts.Deliveries = controlplane.NewWebhookDeliveryStore(pool)
	return parts, vault.NewEnvironmentResolver(parts.Vault), nil
}

var cloudflareZoneIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// parseCloudflareZones parses EREGION_CLOUDFLARE_ZONES. Every entry is one
// explicit zone pair, for example 0123456789abcdef0123456789abcdef=example.com
// (either side of "=" or ":" may be the 32-character zone ID). A single name or
// ID is refused: the inventory and reconcile path need both facts, and this
// command never guesses or resolves a zone from the network.
func parseCloudflareZones(value, tokenFile string) ([]integrations.DNSZone, error) {
	entries := splitList(value)
	if len(entries) == 0 {
		return nil, nil
	}
	if strings.TrimSpace(tokenFile) == "" {
		return nil, contract.NewFault(contract.InputRequired, "EREGION_CLOUDFLARE_TOKEN_FILE is required with EREGION_CLOUDFLARE_ZONES", "Set the absolute path of a root-owned 0600 Cloudflare API token file.")
	}
	seen := map[string]bool{}
	zones := make([]integrations.DNSZone, 0, len(entries))
	for _, entry := range entries {
		left, right, ok := strings.Cut(entry, "=")
		if !ok {
			left, right, ok = strings.Cut(entry, ":")
		}
		if !ok {
			return nil, contract.NewFault(contract.ValidationFailed, "invalid EREGION_CLOUDFLARE_ZONES entry", "Use zoneID=zoneName pairs separated by commas; both the 32-character zone ID and the zone name are required.")
		}
		id, name := strings.TrimSpace(left), strings.TrimSpace(right)
		if cloudflareZoneIDPattern.MatchString(name) && !cloudflareZoneIDPattern.MatchString(id) {
			id, name = name, id
		}
		if !cloudflareZoneIDPattern.MatchString(id) {
			return nil, contract.NewFault(contract.ValidationFailed, "invalid Cloudflare zone ID in EREGION_CLOUDFLARE_ZONES", "Use the lowercase 32-character zone identifier.")
		}
		name = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(name), "."))
		if name == "" || !strings.Contains(name, ".") || strings.ContainsAny(name, " \t\r\n/") {
			return nil, contract.NewFault(contract.ValidationFailed, "invalid Cloudflare zone name in EREGION_CLOUDFLARE_ZONES", "Use a lowercase zone name such as example.com.")
		}
		if seen[id] {
			return nil, contract.NewFault(contract.ValidationFailed, "duplicate Cloudflare zone in EREGION_CLOUDFLARE_ZONES", "Configure each zone once.")
		}
		seen[id] = true
		zones = append(zones, integrations.DNSZone{ZoneID: id, ZoneName: name, TokenFile: strings.TrimSpace(tokenFile)})
	}
	return zones, nil
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
	webhook, err := a.webhookHandler(cmd, service, parts.Deliveries)
	if err != nil {
		return nil, err
	}
	if webhook != nil {
		config.Webhook = webhook
	} else {
		logger.Info("GitHub webhook disabled", "reason", "no webhook secret file and allow-list configured")
	}
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
				r.SetURL(target)
				r.Out.URL.Path = "/" + strings.TrimPrefix(r.In.URL.Path, api.PrivatePrefix)
				r.Out.URL.RawPath = ""
				r.Out.Header.Del("Authorization") // The upstream never sees the control-plane credential.
			},
			ErrorLog: slog.NewLogLogger(logger.Handler(), slog.LevelWarn),
		}
		config.Private = api.PrivateAccess(parts.Extensions.Access, auth, proxy)
		logger.Info("private access enforcement enabled", "prefix", api.PrivatePrefix)
	}
	return api.NewMux(config), nil
}

// webhookHandler builds the GitHub handler when a secret file and an allow-list
// are both configured, and returns nil when neither is. Half a configuration is
// an error: a webhook must never be mounted without its allow-list.
func (a *app) webhookHandler(cmd *cobra.Command, service operations.Service, deliveries api.WebhookDeliveryStore) (http.Handler, error) {
	secretFile := serveFlag(cmd, "webhook-secret-file", "EREGION_WEBHOOK_SECRET_FILE")
	allow, _ := cmd.Flags().GetStringArray("webhook-allow")
	if len(allow) == 0 {
		allow = splitList(os.Getenv("EREGION_WEBHOOK_ALLOW"))
	}
	if secretFile == "" && len(allow) == 0 {
		return nil, nil
	}
	if secretFile == "" || len(allow) == 0 {
		return nil, contract.NewFault(contract.InputRequired, "the GitHub webhook needs both a secret file and an allow-list", "Set EREGION_WEBHOOK_SECRET_FILE and EREGION_WEBHOOK_ALLOW (owner/repo@branch=app/environment, comma-separated), or neither.")
	}
	secret, err := readWebhookSecret(secretFile)
	if err != nil {
		return nil, err
	}
	a.secrets.Add(string(secret))
	var entries []api.AllowedPush
	for _, item := range allow {
		entry, err := parseAllowedPush(item)
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return api.NewWebhookHandler(api.WebhookConfig{Secret: secret, Allowed: entries, Deliveries: deliveries}, service)
}

// parseAllowedPush parses owner/repo@branch=app/environment.
func parseAllowedPush(text string) (api.AllowedPush, error) {
	bad := contract.NewFault(contract.ValidationFailed, "invalid webhook allow-list entry", "Use owner/repo@branch=app/environment.")
	left, placement, ok := strings.Cut(strings.TrimSpace(text), "=")
	if !ok {
		return api.AllowedPush{}, bad
	}
	repository, branch, ok := strings.Cut(left, "@")
	if !ok {
		return api.AllowedPush{}, bad
	}
	app, environment, ok := strings.Cut(placement, "/")
	if !ok || repository == "" || branch == "" || app == "" || environment == "" {
		return api.AllowedPush{}, bad
	}
	return api.AllowedPush{Repository: repository, Branch: branch, App: app, Environment: environment}, nil
}

func splitList(text string) []string {
	var out []string
	for _, item := range strings.Split(text, ",") {
		if item = strings.TrimSpace(item); item != "" {
			out = append(out, item)
		}
	}
	return out
}

// readWebhookSecret reads the GitHub shared secret from a private regular file.
// Permissions are checked on the opened file, never on the path.
func readWebhookSecret(path string) ([]byte, error) {
	unsafe := contract.NewFault(contract.ValidationFailed, "the webhook secret file is unsafe or unreadable", "Use a regular file readable only by its owner (0600 or 0400) holding the GitHub webhook secret.")
	file, err := os.Open(path)
	if err != nil {
		return nil, unsafe
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil || !stat.Mode().IsRegular() || stat.Mode().Perm()&0o077 != 0 {
		return nil, unsafe
	}
	raw, err := io.ReadAll(io.LimitReader(file, 1025))
	if err != nil || len(raw) > 1024 {
		return nil, unsafe
	}
	return []byte(strings.TrimRight(string(raw), "\r\n")), nil
}
