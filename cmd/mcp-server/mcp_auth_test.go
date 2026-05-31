package main

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/auth"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/tools"
)

// fakeLookup is a tenantLookup test double — bearer "good" resolves
// to a fixed tenant, everything else fails.
type fakeLookup struct{ tc tools.TenantContext }

func (f fakeLookup) LookupTenantByToken(token string) (tools.TenantContext, bool) {
	if token == "good" {
		return f.tc, true
	}
	return tools.TenantContext{}, false
}

// buildReq returns a CallToolRequest with the supplied headers — the
// middleware only ever touches req.Extra.Header, so a zero-valued
// Session and Params suffice.
func buildReq(h http.Header) *mcp.CallToolRequest {
	return &mcp.CallToolRequest{Extra: &mcp.RequestExtra{Header: h}}
}

// captureNext returns an mcp.MethodHandler that records the ctx it was
// invoked with. Lets the tests assert what the middleware set on ctx
// before delegating downstream.
func captureNext() (mcp.MethodHandler, *context.Context) {
	var seen context.Context
	h := func(ctx context.Context, _ string, _ mcp.Request) (mcp.Result, error) {
		seen = ctx
		return nil, nil
	}
	return h, &seen
}

func TestAuthMiddleware_ExplicitBearer_ResolvesTenant(t *testing.T) {
	lookup := fakeLookup{tc: tools.TenantContext{TenantID: "tenant-1", Owner: "svp1abc"}}
	sessions := auth.NewSessionBearers(time.Hour, nil)
	mw := authReceivingMiddleware(lookup, sessions)

	next, seen := captureNext()
	wrapped := mw(next)

	req := buildReq(http.Header{
		"Authorization":  {"Bearer good"},
		"Mcp-Session-Id": {"sess-A"},
		ipHeader:         {"203.0.113.7"},
	})
	_, err := wrapped(context.Background(), "tools/call", req)
	require.NoError(t, err)

	tc, ok := tools.TenantFrom(*seen)
	require.True(t, ok, "tenant should be resolved")
	require.Equal(t, "tenant-1", tc.TenantID)
	require.Equal(t, "svp1abc", tc.Owner)
	require.Equal(t, "203.0.113.7", tools.IPFrom(*seen))
	require.Equal(t, "sess-A", tools.SessionIDFrom(*seen))
}

func TestAuthMiddleware_SessionBoundBearer_ResolvesTenant(t *testing.T) {
	// Simulate the v0.3 flow: a prior auth_verify call bound bearer
	// "good" to session "sess-A". A subsequent request omits
	// Authorization but echoes Mcp-Session-Id, and must still resolve
	// to the bound tenant via path 2.
	lookup := fakeLookup{tc: tools.TenantContext{TenantID: "tenant-1", Owner: "svp1abc"}}
	sessions := auth.NewSessionBearers(time.Hour, nil)
	sessions.Bind("sess-A", "good")

	mw := authReceivingMiddleware(lookup, sessions)
	next, seen := captureNext()
	wrapped := mw(next)

	req := buildReq(http.Header{"Mcp-Session-Id": {"sess-A"}})
	_, err := wrapped(context.Background(), "tools/call", req)
	require.NoError(t, err)

	tc, ok := tools.TenantFrom(*seen)
	require.True(t, ok, "session-bound tenant should resolve")
	require.Equal(t, "tenant-1", tc.TenantID)
}

func TestAuthMiddleware_NoCredentials_PassesThroughWithoutTenant(t *testing.T) {
	// The auth_challenge / auth_verify tools themselves run without
	// a TenantContext — every other tool errors via ErrNoTenant in
	// the handler. The middleware must pass through, not reject.
	lookup := fakeLookup{}
	sessions := auth.NewSessionBearers(time.Hour, nil)
	mw := authReceivingMiddleware(lookup, sessions)
	next, seen := captureNext()
	wrapped := mw(next)

	req := buildReq(http.Header{})
	_, err := wrapped(context.Background(), "tools/call", req)
	require.NoError(t, err)

	_, ok := tools.TenantFrom(*seen)
	require.False(t, ok)
}

func TestAuthMiddleware_NilExtra_PassesThrough(t *testing.T) {
	// Defensive: a request that arrives without RequestExtra (which
	// would only happen via internal call paths, not HTTP) must not
	// panic — just pass through with no ctx values.
	mw := authReceivingMiddleware(fakeLookup{}, auth.NewSessionBearers(time.Hour, nil))
	next, seen := captureNext()
	wrapped := mw(next)

	_, err := wrapped(context.Background(), "tools/call", &mcp.CallToolRequest{Extra: nil})
	require.NoError(t, err)
	require.Empty(t, tools.IPFrom(*seen))
}
