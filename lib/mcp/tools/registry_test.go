package tools_test

import (
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/tools"
)

// TestRegister_NoSchemaPanic catches the failure mode where a jsonschema
// struct tag on a tool input is malformed in a way that AddTool only
// notices at registration time — e.g. a value beginning with "WORD=" that
// the go-sdk's tag parser treats as a directive and panics on. Unit tests
// of individual handlers won't hit this path because they never call
// AddTool; only this Register-level smoke does.
//
// Handlers may be zero-valued: the SDK reflects on the handler method's
// input type and never invokes the handler at registration time.
func TestRegister_NoSchemaPanic(t *testing.T) {
	srv := mcp.NewServer(&mcp.Implementation{
		Name: "test", Version: "v0.0.0",
	}, nil)
	require.NotPanics(t, func() {
		tools.Register(srv, &tools.Handlers{})
	})
}
