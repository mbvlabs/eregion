package integrations

import (
	"encoding/json"
	"net/http"
)

// IdentityResolver extracts an already-authenticated subject and private device
// identity from a request. The resolver must be backed by the control plane's
// verified bearer/session authentication. This middleware never trusts raw
// headers by itself.
type IdentityResolver func(*http.Request) (subject, deviceID string, err error)

// RouteResolver maps an authenticated request to the exact private route that a
// grant must authorize. It must not use wildcards or client-supplied route
// strings. Returning an error rejects the request before any access decision.
type RouteResolver func(*http.Request) (route string, err error)

// Middleware is the enforcement point for private access. Mount it in front of
// the private handler, after the normal authentication middleware:
//
//	protected := access.Middleware(resolveIdentity, resolveRoute)(privateHandler)
//	mux.Handle("/private/", protected)
//
// It returns 401 when identity cannot be established, 400 when the route cannot
// be resolved, 403 for a missing/expired/revoked grant, and 503 when the grant
// store or audit sink is unavailable. Only an allowed request reaches next.
func (c *AccessController) Middleware(identity IdentityResolver, route RouteResolver) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		if next == nil {
			next = http.NotFoundHandler()
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Cache-Control", "no-store")
			if identity == nil || route == nil {
				writeAccessError(w, http.StatusServiceUnavailable, "access enforcement is not configured")
				return
			}
			subject, deviceID, err := identity(r)
			if err != nil || subject == "" || deviceID == "" {
				writeAccessError(w, http.StatusUnauthorized, "authentication required")
				return
			}
			exactRoute, err := route(r)
			if err != nil || exactRoute == "" {
				writeAccessError(w, http.StatusBadRequest, "access route cannot be resolved")
				return
			}
			decision, err := c.Authorize(r.Context(), subject, deviceID, exactRoute)
			if err != nil {
				writeAccessError(w, http.StatusServiceUnavailable, "access decision is unavailable")
				return
			}
			if !decision.Allowed {
				writeAccessError(w, http.StatusForbidden, "private access is not granted")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func writeAccessError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}
