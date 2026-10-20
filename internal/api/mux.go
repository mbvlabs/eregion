package api

import (
	"net/http"
	"strings"

	"github.com/mbvlabs/eregion/internal/contract"
	"github.com/mbvlabs/eregion/internal/integrations"
)

// PrivatePrefix is where the private-access upstream is mounted.
const PrivatePrefix = "/private/"

// MuxConfig lists the handlers `serve` mounts around the bearer-authenticated
// operations API. Each optional handler that is nil leaves its route to fall
// through to Default, which answers 404 in the standard error envelope.
type MuxConfig struct {
	// Default serves everything not claimed below (NewHandler).
	Default http.Handler
	// Vault serves /v1/secrets*. It authenticates itself (NewVaultHandler).
	Vault http.Handler
	// Webhook serves POST /v1/webhooks/github. It must bypass bearer auth: GitHub
	// cannot send a token, and the handler verifies its own HMAC signature.
	Webhook http.Handler
	// Private serves PrivatePrefix. It must already be wrapped in the access
	// enforcement middleware; nil leaves private access disabled.
	Private http.Handler
}

// NewMux routes by exact path class and never by a client-supplied route table.
func NewMux(config MuxConfig) http.Handler {
	fallback := config.Default
	if fallback == nil {
		fallback = http.NotFoundHandler()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case config.Vault != nil && (path == "/v1/secrets" || strings.HasPrefix(path, "/v1/secrets/")):
			config.Vault.ServeHTTP(w, r)
		case config.Webhook != nil && path == "/v1/webhooks/github":
			config.Webhook.ServeHTTP(w, r)
		case config.Private != nil && strings.HasPrefix(path, PrivatePrefix):
			config.Private.ServeHTTP(w, r)
		default:
			fallback.ServeHTTP(w, r)
		}
	})
}

// PrivateAccess wraps an upstream in the integrations access middleware. The
// identity is the bearer-authenticated principal (scope operations:read) and the
// device is its X-Eregion-Device header; a grant binds the pair, so a stolen
// token alone does not reach a private route. The route a grant must name is the
// lower-cased request path below PrivatePrefix, matched exactly.
func PrivateAccess(access *integrations.AccessController, auth *Auth, upstream http.Handler) http.Handler {
	identity := func(r *http.Request) (string, string, error) {
		if len(r.Header.Values("Authorization")) != 1 {
			return "", "", contract.NewFault(contract.Unauthenticated, "exactly one authorization header is required", "Supply one Bearer token.")
		}
		actor, err := auth.authenticate(r.Header.Get("Authorization"), "operations:read")
		if err != nil {
			return "", "", err
		}
		return actor.Subject, r.Header.Get("X-Eregion-Device"), nil
	}
	route := func(r *http.Request) (string, error) {
		return strings.ToLower(strings.TrimPrefix(r.URL.Path, PrivatePrefix)), nil
	}
	return access.Middleware(identity, route)(upstream)
}
