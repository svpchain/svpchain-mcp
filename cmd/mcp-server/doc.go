// Command mcp-server is the remote MCP server for svpchain.
//
// It exposes an MCP (Model Context Protocol) HTTP endpoint that LLM agents
// call to read market and account data and to *construct* (but never sign)
// trading and fund-movement transactions for the svpchain perpetuals DEX.
//
// Architecture (see protocol/docs/mcp-agent-service-design.md):
//
//   - This binary runs on operator infrastructure ("remote"); it NEVER
//     holds a private key.
//   - For trades, it constructs unsigned tx payloads via lib/mcp/build,
//     returns them to the MCP client. The client signs locally (typically
//     via a separate local-signer MCP server) and calls back into
//     broadcast_signed_tx, which the remote server then broadcasts to the
//     chain.
//   - Reads come from the indexer's Comlink REST API (lib/mcp/indexer) and
//     the chain gRPC Query services (lib/mcp/chain).
//   - Multi-tenant from v0.1: bearer-token → tenant identity mapping in
//     config; per-tenant owner + subaccount allowlist enforced server-side.
package main
