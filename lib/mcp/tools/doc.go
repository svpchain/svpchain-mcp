// Package tools wires the MCP tool handlers exposed to LLM agents.
//
// registry.go is the single place that registers every tool with the MCP
// server (via mcp.AddTool generic, with reflection-derived input/output
// JSON schemas from struct tags). market.go / account.go / trade.go /
// funds.go / cross.go group handlers by the four capability areas + cross-
// cutting (broadcast_signed_tx, simulate, get_tx_status, whoami).
//
// Each handler:
//  1. Extracts TenantContext from the request context (set by auth middleware).
//  2. Calls policy.Engine.Check.
//  3. Dispatches to lib/mcp/chain, lib/mcp/indexer, or lib/mcp/build.
//  4. Maps backend errors to user-visible MCP errors (policy reject →
//     plain text; chain reject → Code + RawLog).
package tools
