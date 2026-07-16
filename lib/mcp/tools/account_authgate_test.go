package tools

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestGetSubaccount_UnauthenticatedReturnsAuthRequired verifies that an
// unauthenticated call returns a SUCCESSFUL result carrying auth_required (so an
// agent loop keeps running), not ErrNoTenant (which the go-sdk would surface as
// CallToolResult{IsError:true} and abort the loop).
func TestGetSubaccount_UnauthenticatedReturnsAuthRequired(t *testing.T) {
	h := &Handlers{}            // no deps needed — the auth gate returns before touching them
	ctx := context.Background() // no TenantContext == unauthenticated

	res, out, err := h.GetSubaccount(ctx, nil, GetSubaccountInput{Address: "svp1abc", SubaccountNumber: 0})
	require.NoError(t, err) // not an error: the loop survives
	require.Nil(t, res)
	require.Nil(t, out.Subaccount)
	require.NotNil(t, out.AuthRequired)
	require.False(t, out.AuthRequired.Authenticated)
	require.Equal(t, "get_subaccount", out.AuthRequired.RetryTool)
	require.Contains(t, out.AuthRequired.Message, "auth_challenge")

	// The output marshals cleanly for schema validation: the nil Subaccount is
	// omitted (not a null object), and auth_required is present.
	b, err := json.Marshal(out)
	require.NoError(t, err)
	require.NotContains(t, string(b), `"subaccount"`)
	require.Contains(t, string(b), `"auth_required"`)
}
