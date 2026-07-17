package main

import (
	"context"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/auth"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/tools"
)

// authReceivingMiddleware resolves tenant identity per request. It runs
// inside the MCP SDK's "receiving" middleware chain — meaning it sees
// the per-call HTTP headers via req.GetExtra().Header, not via the
// handler ctx.
//
// The HTTP-layer wrapper (auth.go) cannot do this directly: the go-sdk
// captures the request ctx only at the initialize-time server.Connect
// call. Subsequent requests' r.Context() mutations never reach handler
// ctx (the connection holds a detached ctx from the initialize call).
// So we resolve tenant here, where each call carries its own headers
// in req.Extra, and we set tools.WithTenant / WithIP / WithSessionID on
// the ctx that we forward to the next handler — that one IS the ctx
// each handler receives.
//
// Three resolution paths, tried in order:
//
//  1. Explicit Authorization: Bearer <token>. Looked up in the dynamic
//     tenant store.
//  2. Mcp-Session-Id with a previously-bound bearer (binding set up by
//     auth_verify on a prior call in the same session).
//  3. Neither resolves — pass through with no TenantContext. The
//     auth_challenge / auth_verify tools run anyway; every other tool
//     errors via authorize*.
func authReceivingMiddleware(
	lookup tenantLookup,
	sessions *auth.SessionBearers,
) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if extra := req.GetExtra(); extra != nil && extra.Header != nil {
				h := extra.Header

				if ip := h.Get(ipHeader); ip != "" {
					ctx = tools.WithIP(ctx, ip)
				}
				sessionID := h.Get("Mcp-Session-Id")
				if sessionID != "" {
					ctx = tools.WithSessionID(ctx, sessionID)
				}

				// Path 1: explicit bearer.
				var resolved bool
				if token, found := strings.CutPrefix(h.Get("Authorization"), "Bearer "); found {
					if tc, ok := lookup.LookupTenantByToken(token); ok {
						ctx = tools.WithTenant(ctx, tc)
						resolved = true
					}
				}
				// Path 2: fall back to session-bound bearer.
				if !resolved && sessionID != "" && sessions != nil {
					if bearer := sessions.Lookup(sessionID); bearer != "" {
						if tc, ok := lookup.LookupTenantByToken(bearer); ok {
							ctx = tools.WithTenant(ctx, tc)
						}
					}
				}
			}

			// Auth gate: for an unauthenticated call to a tenant-scoped tool, return
			// a soft auth_required result (a NON-error success carrying the handshake
			// instructions) instead of letting the handler return ErrNoTenant. The
			// go-sdk turns a handler error into CallToolResult{IsError:true}, which
			// aborts agent loops that break out on tool failures; the soft result lets
			// the agent authenticate and retry. Only tools/call is gated (tools/list,
			// initialize, etc. stay open); the handshake tools are exempt via
			// RequiresAuth. Runs after tenant resolution above, so an authenticated
			// call (tenant on ctx) passes straight through to the handler.
			if method == "tools/call" {
				if ctr, ok := req.(*mcp.CallToolRequest); ok && ctr.Params != nil && tools.RequiresAuth(ctr.Params.Name) {
					if _, authed := tools.TenantFrom(ctx); !authed {
						return tools.AuthRequiredResult(), nil
					}
				}
			}
			return next(ctx, method, req)
		}
	}
}

// tenantLookup is the interface used by authReceivingMiddleware to
// resolve a bearer token to a tenant identity. Implemented by Server.
type tenantLookup interface {
	LookupTenantByToken(token string) (tools.TenantContext, bool)
}
