package controlplane

import (
	"net/http"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/integrations"
	"github.com/mbvlabs/eregion/internal/platform"
	"github.com/mbvlabs/eregion/internal/vault"
)

// ExtensionConfig is everything the extension packages need from the process
// that hosts them. serve and worker pass the same value shape, so an action
// accepted by one control plane can be executed by any other.
type ExtensionConfig struct {
	Pool *pgxpool.Pool
	// AccessAdmins are the actor subjects allowed to administer private-device
	// grants. Empty leaves access actions registered but fail-closed.
	AccessAdmins []string
	// DNSAdmins grants the unrelated generic integration DNS action. The
	// domain-scoped flow uses the operator-only provider authorizer instead and
	// never falls back to these subjects or AccessAdmins.
	DNSAdmins []string
	// DNSZones and DNSCredentials are the authoritative PostgreSQL provider
	// inventory/credential resolver. Nil fails provider access closed. There is
	// no environment or file fallback.
	DNSZones       integrations.DNSZoneSource
	DNSCredentials integrations.DNSCredentialSource
	// CloudflareBaseURL is a test seam; empty uses the real API.
	CloudflareBaseURL string
	// CloudflareClient is a test seam; empty uses a bounded default client.
	CloudflareClient *http.Client
}

// Extensions is what RegisterExtensions built, for the caller to mount.
type Extensions struct {
	// Access is the private-access controller, nil when no admins are configured.
	Access *integrations.AccessController
	// DomainDNS is the domain-scoped Cloudflare planner/executor. It is present
	// even without configured zones and fails closed on use.
	DomainDNS *integrations.DomainDNS
}

var extensionMu sync.Mutex

// RegisterExtensions wires the integrations package into the process-wide
// platform registry. Call it exactly once per process, before New and before
// the API listens; serve and worker both call it.
//
// Registration is idempotent per process so a test (or an embedder) that runs
// several control planes in one process does not trip the registry's duplicate
// check; the first call's database pool and configuration win for the registry.
func RegisterExtensions(config ExtensionConfig) (*Extensions, error) {
	if config.Pool == nil {
		return nil, contract.NewFault(contract.Internal, "extensions need the control-plane database", "Open the database before registering extensions.")
	}
	extensionMu.Lock()
	defer extensionMu.Unlock()

	registry := platform.Default()
	grants, err := integrations.NewPostgresGrantStore(config.Pool)
	if err != nil {
		return nil, err
	}
	audit, err := integrations.NewPostgresAudit(config.Pool)
	if err != nil {
		return nil, err
	}
	extensions := &Extensions{}
	runtime := integrations.RuntimeConfig{
		Audit: audit, CloudflareBaseURL: config.CloudflareBaseURL, CloudflareClient: config.CloudflareClient,
		Zones: config.DNSZones, Credentials: config.DNSCredentials,
	}
	if len(config.DNSAdmins) > 0 {
		allow := map[string][]string{}
		for _, subject := range config.DNSAdmins {
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
	domainDNS, err := integrations.NewDomainDNS(integrations.DomainDNSConfig{
		Zones: config.DNSZones, Credentials: config.DNSCredentials,
		BaseURL: config.CloudflareBaseURL, HTTPClient: config.CloudflareClient,
		Authorizer: integrations.NewOperatorDNSAuthorizer(), Audit: audit,
	})
	if err != nil {
		return nil, err
	}
	extensions.DomainDNS = domainDNS
	if _, registered := registry.Lookup(integrations.KindAccessGrant); !registered {
		if err := integrations.Configure(registry, runtime); err != nil {
			return nil, err
		}
	}
	return extensions, nil
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
