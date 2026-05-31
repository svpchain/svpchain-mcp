package main

import (
	"net"
	"net/http"
)

// ipHeader is a synthetic request header the HTTP middleware writes so
// the MCP receiving middleware (mcp_auth.go) can read the client IP
// from req.GetExtra().Header. r.RemoteAddr lives on http.Request only;
// once the request crosses into the go-sdk's StreamableHTTP transport
// the per-call handler ctx sees nothing from r.RemoteAddr — only the
// header map is propagated through to req.Extra.Header. Folding the
// IP into a header is the simplest way to thread it across.
// Kept in canonical MIME form (http.CanonicalHeaderKey) so it round-trips
// through http.Header.Set/Get regardless of how callers spell it. "IP"
// canonicalizes to "Ip" — the test path constructs http.Header literals
// that bypass canonicalization, so we use the canonical form everywhere
// to avoid lookup misses.
const ipHeader = "X-Mcp-Client-Ip"

// ipMiddleware stamps the client IP into ipHeader so the MCP-side
// receiving middleware can use it for per-IP rate limiting. That's the
// only thing this HTTP-layer wrapper does in v0.3 — Authorization and
// Mcp-Session-Id flow through as-is, and the MCP receiving middleware
// is where tenant resolution actually happens (see mcp_auth.go for why
// the per-request ctx values set here would not reach the handler).
//
// Production deployments behind a load balancer should also map
// X-Forwarded-For onto ipHeader before this runs (deferred).
func ipMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ip := clientIP(r); ip != "" {
			// Overwrite any client-supplied X-Mcp-Client-IP so a hostile
			// client can't spoof the IP rate limiter.
			r.Header.Set(ipHeader, ip)
		} else {
			r.Header.Del(ipHeader)
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP extracts an IP from r.RemoteAddr (host:port → host). Returns
// an empty string when parsing fails; the rate limiter treats empty as
// "no IP available" and allows.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return ""
	}
	return host
}
