// Package logging produces a cosmossdk.io/log child logger named
// "mcp-server" with structured fields (tenant_id, tool, client_id,
// owner) injected per request, matching the convention used elsewhere
// in the protocol (cmd/svpchaind, daemons/*).
package logging
