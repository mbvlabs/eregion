package controlplane

import (
	"net/url"
	"strings"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/integrations"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/resources"
	"github.com/mbvlabs/eregion/internal/telemetry"
	"github.com/mbvlabs/eregion/internal/vault"
)

// ExtensionConfig is everything the extension packages need from the process
// that hosts them. serve and worker pass the same value shape, so an action
// accepted by one control plane can be executed by any other.
type ExtensionConfig struct {
	Pool *pgxpool.Pool
	// DatabaseURL is the control-plane DSN. Only its database name is used: the
	// resources extension refuses to destroy or back up that database.
	DatabaseURL string
	// Resources configures one application host. Nil leaves resource actions
	// registered but failing with "resource host is not configured".
	Resources *resources.Config
	// AccessAdmins are the actor subjects allowed to administer private-device
	// grants. Empty leaves access actions registered but fail-closed.
	AccessAdmins []string
	// DNSAdmins are the actor subjects allowed to reconcile managed DNS. When
	// empty they fall back to AccessAdmins, preserving the earlier single-admin
	// configuration. DNS authorization is otherwise independent of private
	// access grants.
	DNSAdmins []string
	// DNSZones is the explicit read-only Cloudflare inventory configuration.
	// Reconcile actions must name one of these zones; with no zones configured,
	// DNS actions fail closed instead of accepting an arbitrary zone.
	DNSZones []integrations.DNSZone
	// CloudflareBaseURL is a test seam; empty uses the real API.
	CloudflareBaseURL string
	// NodeTransports resolves the authenticated transport for each target node.
	// Telemetry installs and agents need it because one worker process may target
	// several nodes. Nil leaves telemetry actions registered but fail-closed.
	NodeTransports telemetry.TransportResolver
}

// Extensions is what RegisterExtensions built, for the caller to mount.
type Extensions struct {
	// Access is the private-access controller, nil when no admins are configured.
	Access *integrations.AccessController
}

var extensionMu sync.Mutex

// RegisterExtensions wires the resources and integrations packages into the
// process-wide platform registry. Call it exactly once per process, before
// New and before the API listens; serve and worker both call it.
//
// Registration is idempotent per process so a test (or an embedder) that runs
// several control planes in one process does not trip the registry's duplicate
// check; the first call's database pool and host configuration win for the
// registry, while resource host services are always (re)configured.
func RegisterExtensions(config ExtensionConfig) (*Extensions, error) {
	if config.Pool == nil {
		return nil, contract.NewFault(contract.Internal, "extensions need the control-plane database", "Open the database before registering extensions.")
	}
	extensionMu.Lock()
	defer extensionMu.Unlock()

	if config.Resources != nil {
		resourceConfig := *config.Resources
		if resourceConfig.ControlPlaneDatabase == "" {
			resourceConfig.ControlPlaneDatabase = databaseName(config.DatabaseURL)
		}
		if err := resources.Configure(resourceConfig); err != nil {
			return nil, err
		}
	}
	registry := platform.Default()
	if _, registered := registry.Lookup(resources.KindResourceCreate); !registered {
		if err := resources.RegisterDefault(); err != nil {
			return nil, err
		}
	}

	grants, err := integrations.NewPostgresGrantStore(config.Pool)
	if err != nil {
		return nil, err
	}
	audit, err := integrations.NewPostgresAudit(config.Pool)
	if err != nil {
		return nil, err
	}
	extensions := &Extensions{}
	runtime := integrations.RuntimeConfig{Audit: audit, CloudflareBaseURL: config.CloudflareBaseURL, DNSZones: config.DNSZones}
	dnsAdmins := config.DNSAdmins
	if len(dnsAdmins) == 0 {
		dnsAdmins = config.AccessAdmins
	}
	if len(dnsAdmins) > 0 {
		allow := map[string][]string{}
		for _, subject := range dnsAdmins {
			allow[subject] = []string{"*"}
		}
		authorizer, err := integrations.NewStaticAuthorizer(allow)
		if err != nil {
			return nil, err
		}
		runtime.Authorizer = authorizer
	}
	if len(config.AccessAdmins) > 0 {
		access, err := integrations.NewAccessController(integrations.AccessConfig{AdminSubjects: config.AccessAdmins, Audit: audit, Store: grants})
		if err != nil {
			return nil, err
		}
		runtime.Access, extensions.Access = access, access
	}
	if _, registered := registry.Lookup(integrations.KindAccessGrant); !registered {
		if err := integrations.Configure(registry, runtime); err != nil {
			return nil, err
		}
	}

	if _, registered := registry.Lookup(telemetry.KindInstall); !registered {
		if config.NodeTransports != nil {
			if err := telemetry.ConfigureProductionWithResolver(registry, config.Pool, config.NodeTransports); err != nil {
				return nil, err
			}
		} else {
			// Tests and local embedders call RegisterExtensions without a worker
			// transport. Registration still succeeds, and every dispatch attempt
			// fails with an explicit unavailable fault rather than a local install.
			if err := telemetry.Configure(registry, telemetry.RuntimeConfig{}); err != nil {
				return nil, err
			}
		}
	}
	return extensions, nil
}

// databaseName extracts the database from a control-plane DSN without echoing
// any other part of it.
func databaseName(dsn string) string {
	parsed, err := url.Parse(dsn)
	if err != nil {
		return ""
	}
	return strings.TrimPrefix(parsed.Path, "/")
}

// NewVaultService builds the secret service for a pool and a protected
// keyring file. The keyring never enters the database.
func NewVaultService(pool *pgxpool.Pool, keyringPath string) (*vault.Service, error) {
	ring, err := vault.LoadKeyRing(keyringPath)
	if err != nil {
		return nil, err
	}
	return vault.NewService(pool, ring)
}
