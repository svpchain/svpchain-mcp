package tools

import "context"

// ctxKey is the context-key type for TenantContext. Using a private type
// (rather than a string) prevents collisions with other packages.
type ctxKey int

const tenantKey ctxKey = iota

// TenantContext carries the per-request tenant identity decoded by the
// auth middleware (cmd/mcp-server/auth.go) from the bearer token. The
// MCP SDK propagates the http.Request context into each tool handler's
// ctx, so handlers read this via TenantFrom(ctx).
type TenantContext struct {
	TenantID string
	Owner    string
}

// WithTenant returns ctx with tc attached.
func WithTenant(ctx context.Context, tc TenantContext) context.Context {
	return context.WithValue(ctx, tenantKey, tc)
}

// TenantFrom extracts the TenantContext set by middleware. ok == false
// means the request reached a handler without auth — handlers must treat
// this as an internal error and refuse to act.
func TenantFrom(ctx context.Context) (TenantContext, bool) {
	tc, ok := ctx.Value(tenantKey).(TenantContext)
	return tc, ok
}
