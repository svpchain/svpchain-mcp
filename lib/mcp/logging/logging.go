package logging

import "cosmossdk.io/log"

// ModuleName is the structured-logging "module" key the MCP server uses so
// operators can grep its lines apart from the chain's own daemons.
const ModuleName = "mcp-server"

// NewLogger derives an MCP-server child logger from a parent. Add
// request-scoped fields per call site (e.g. tenant_id, tool, client_id)
// with logger.With(...).
func NewLogger(parent log.Logger) log.Logger {
	return parent.With(log.ModuleKey, ModuleName)
}
