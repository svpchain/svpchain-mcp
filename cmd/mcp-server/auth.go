package main

import (
	"net/http"
	"strings"

	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/tools"
)

// tenantLookup is the interface the auth middleware uses to resolve a
// bearer token to a tenant identity. Implemented by Server.
type tenantLookup interface {
	LookupTenantByToken(token string) (tools.TenantContext, bool)
}

// bearerAuthMiddleware extracts the bearer token from the Authorization
// header, resolves it via lookup, and injects a tools.TenantContext into
// the request context for downstream handlers. Unknown / missing tokens
// are rejected with 401 without echoing the token back (no info leak).
//
// v0.1 uses a plain map lookup; bearer tokens should be random
// high-entropy strings so timing attacks are not a meaningful threat.
// v0.2 may switch to constant-time-compare + JWT.
func bearerAuthMiddleware(lookup tenantLookup, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header.Get("Authorization")
		if !strings.HasPrefix(authHeader, "Bearer ") {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		token := strings.TrimPrefix(authHeader, "Bearer ")
		tc, ok := lookup.LookupTenantByToken(token)
		if !ok {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		ctx := tools.WithTenant(r.Context(), tc)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
