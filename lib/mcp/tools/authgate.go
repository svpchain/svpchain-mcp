package tools

import (
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// authgate.go centralizes the "degrade gracefully when unauthenticated" policy.
// Nearly every tool requires a resolved tenant and, without one, its handler
// returns ErrNoTenant — which the go-sdk turns into CallToolResult{IsError:true}.
// An agent loop that aborts on a tool failure would break out on the first such
// call. Instead the auth middleware (cmd/mcp-server/mcp_auth.go) consults
// RequiresAuth and, for an unauthenticated tenant-scoped tool call, returns
// AuthRequiredResult — a SUCCESSFUL result carrying the handshake instructions,
// so the agent can authenticate and retry rather than fail.

// RequiresAuth reports whether a tool needs a resolved tenant. Only the
// self-service handshake tools (auth_challenge / auth_verify) run without one;
// every other tool authorizes and would otherwise hard-fail when unauthenticated.
func RequiresAuth(toolName string) bool {
	switch toolName {
	case "auth_challenge", "auth_verify":
		return false
	default:
		return true
	}
}

// AuthRequiredResult is the soft "authenticate first" tool result returned for an
// unauthenticated call to a tenant-scoped tool. It carries the same actionable
// handshake instructions as ErrNoTenant but as a non-error result (IsError is
// left false), so the caller's agent loop survives and can retry after the
// handshake. It is intentionally text-only: the sole change from the prior
// behavior is that IsError is false rather than true.
func AuthRequiredResult() *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: ErrNoTenant.Error()}},
	}
}
