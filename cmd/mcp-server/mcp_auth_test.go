package main

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/svpchain/svpchain-mcp/lib/mcp/auth"
	"github.com/svpchain/svpchain-mcp/lib/mcp/tools"
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

// callReq builds a tools/call request for a named tool with the given headers.
func callReq(name string, h http.Header) *mcp.CallToolRequest {
	return &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{Name: name},
		Extra:  &mcp.RequestExtra{Header: h},
	}
}

// TestAuthGate_UnauthenticatedGatedTool_ReturnsSoftAuthRequired verifies that an
// unauthenticated tools/call to a tenant-scoped tool short-circuits into a
// NON-error result (so an agent loop survives) and never reaches the handler.
func TestAuthGate_UnauthenticatedGatedTool_ReturnsSoftAuthRequired(t *testing.T) {
	mw := authReceivingMiddleware(fakeLookup{}, auth.NewSessionBearers(time.Hour, nil))
	next, seen := captureNext()
	res, err := mw(next)(context.Background(), "tools/call", callReq("get_subaccount", http.Header{}))

	require.NoError(t, err)
	require.Nil(t, *seen, "handler must NOT be reached when unauthenticated")
	ctr, ok := res.(*mcp.CallToolResult)
	require.True(t, ok)
	require.False(t, ctr.IsError, "must be a soft (non-error) result so the agent loop continues")
	require.Len(t, ctr.Content, 1)
	txt, ok := ctr.Content[0].(*mcp.TextContent)
	require.True(t, ok)
	require.Contains(t, txt.Text, "authentication required")
	require.Contains(t, txt.Text, "auth_challenge")
}

// TestAuthGate_AuthenticatedGatedTool_PassesThrough verifies an authenticated
// call to a gated tool reaches the handler untouched.
func TestAuthGate_AuthenticatedGatedTool_PassesThrough(t *testing.T) {
	lookup := fakeLookup{tc: tools.TenantContext{TenantID: "t1", Owner: "svp1abc"}}
	mw := authReceivingMiddleware(lookup, auth.NewSessionBearers(time.Hour, nil))
	next, seen := captureNext()
	h := http.Header{"Authorization": {"Bearer good"}}
	_, err := mw(next)(context.Background(), "tools/call", callReq("get_subaccount", h))

	require.NoError(t, err)
	require.NotNil(t, *seen, "handler must run for an authenticated call")
	tc, ok := tools.TenantFrom(*seen)
	require.True(t, ok)
	require.Equal(t, "t1", tc.TenantID)
}

// TestAuthGate_HandshakeToolsExempt verifies the auth handshake tools are NOT
// gated even when unauthenticated (otherwise you could never log in).
func TestAuthGate_HandshakeToolsExempt(t *testing.T) {
	mw := authReceivingMiddleware(fakeLookup{}, auth.NewSessionBearers(time.Hour, nil))
	for _, tool := range []string{"auth_challenge", "auth_verify"} {
		next, seen := captureNext()
		_, err := mw(next)(context.Background(), "tools/call", callReq(tool, http.Header{}))
		require.NoError(t, err)
		require.NotNil(t, *seen, "%s must reach the handler unauthenticated", tool)
	}
}

// TestAuthGate_NonToolCallMethodsNotGated verifies non-tools/call methods (e.g.
// tools/list) are never gated, so an agent can discover tools before auth.
func TestAuthGate_NonToolCallMethodsNotGated(t *testing.T) {
	mw := authReceivingMiddleware(fakeLookup{}, auth.NewSessionBearers(time.Hour, nil))
	next, seen := captureNext()
	_, err := mw(next)(context.Background(), "tools/list", buildReq(http.Header{}))
	require.NoError(t, err)
	require.NotNil(t, *seen, "tools/list must pass through unauthenticated")
}
