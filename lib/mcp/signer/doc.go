// Package signer turns a TxPayload produced by the remote MCP server's
// build_* tools into a SignedTx ready for broadcast_signed_tx. It owns
// the eth_secp256k1 + SIGN_MODE_DIRECT signing path and the cross-checks
// (signer address matches the loaded key, payload version matches the
// supported one).
//
// Two callers share this package today:
//
//   - cmd/mcp-signer/ — production stdio MCP server, exposes
//     sign_transaction + whoami over the wire.
//   - scripts/devsign/ — thin one-shot CLI kept for fullflow e2e parity
//     and ad-hoc dev use; reads a TxPayload JSON file, writes a SignedTx
//     JSON file.
//
// The package's init() sets the svp bech32 prefix so every sdk.AccAddress
// stringification (notably in DeriveAddress and the signer-address
// cross-check) matches the chain. Importing this package is sufficient —
// no caller needs its own blank import of app/config.
package signer
