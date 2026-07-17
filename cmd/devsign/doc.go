// Command devsign is a development-only stand-in for the future
// local-signer MCP binary (cmd/mcp-signer/). It takes a TxPayload
// produced by the remote MCP server's build_* tools, signs it locally with
// a provided eth_secp256k1 private key, and emits a SignedTx ready to feed
// into broadcast_signed_tx.
//
// Why it exists: the design separates building (server) from signing
// (client) so the server never custodies a key. Until the real
// local-signer MCP binary lands, this small tool plays its role for
// manual e2e tests against localnet.
//
// Not shipped in any release. Lives under scripts/ to make that obvious.
package main
