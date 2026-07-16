package tools

import (
	"errors"
	"fmt"
)

// ErrNoTenant is returned when a protected tool is called without the auth
// middleware having resolved a TenantContext — i.e. the caller is not
// authenticated. In v0.3 the only tenant source is the auth_challenge →
// auth_verify handshake, so a missing tenant always means "authenticate
// first," never an internal misconfiguration. The message is therefore an
// actionable, client-facing instruction: the go-sdk surfaces it to the
// calling agent as CallToolResult{IsError:true} text, and auth_verify binds
// the minted bearer to the MCP session, so the agent can simply retry the
// original tool once the handshake completes.
var ErrNoTenant = errors.New(
	"authentication required: this tool needs a bearer token. Complete the handshake, then retry:\n" +
		"  1) call the svpchain-signer MCP server's whoami tool to get your svp1… owner address\n" +
		"  2) call auth_challenge with that owner address\n" +
		"  3) sign the returned `challenge` with the svpchain-signer MCP server's sign_challenge tool\n" +
		"  4) call auth_verify with the nonce and the base64 signature\n" +
		"The minted bearer binds to this MCP session automatically — after auth_verify, just call this tool again.",
)

// userErrf wraps a sentinel cause with a user-visible message. Callers
// should prefer this over raw fmt.Errorf for errors that surface to the
// MCP client.
func userErrf(format string, args ...any) error {
	return fmt.Errorf(format, args...)
}

// AuthRequired is the structured "authenticate first" step a tool returns as a
// SUCCESSFUL result (not an error) when the caller is unauthenticated. Returning
// it instead of ErrNoTenant keeps an agent loop alive: the go-sdk surfaces an
// error as CallToolResult{IsError:true}, which aborts agents that break out on a
// tool failure, whereas this is a normal result the agent can act on and retry.
type AuthRequired struct {
	Authenticated bool   `json:"authenticated"` // always false here
	Message       string `json:"message"`       // the handshake instructions (see ErrNoTenant)
	RetryTool     string `json:"retry_tool"`    // the tool to call again once the handshake completes
}

// authRequired builds the AuthRequired step for retryTool, reusing the canonical
// handshake instructions from ErrNoTenant so the wording stays in one place.
func authRequired(retryTool string) *AuthRequired {
	return &AuthRequired{
		Authenticated: false,
		Message:       ErrNoTenant.Error(),
		RetryTool:     retryTool,
	}
}
