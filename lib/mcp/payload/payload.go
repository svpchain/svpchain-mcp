package payload

import (
	"time"
)

// CurrentVersion is the only supported wire version of TxPayload / SignedTx
// in v0.1. Bumped if the on-wire shape changes incompatibly.
const CurrentVersion = 1

// TxPayload is the on-wire envelope returned by every build_* MCP tool and
// later supplied (alongside SignedTx) to broadcast_signed_tx.
//
// The remote MCP server fills every field except the signature; the local
// signer signs the precomputed SignBytes (SIGN_MODE_DIRECT) and round-trips
// the whole TxPayload back inside a BroadcastSignedTxArgs so the server can
// re-verify the bytes match before broadcasting.
//
// Cosmos uint64 fields are JSON-encoded as strings (matching standard
// Cosmos SDK JSON conventions) so JS-based MCP clients do not lose
// precision.
type TxPayload struct {
	Version int `json:"version"`

	// ClientID is the broadcast-idempotency key (uuid). Distinct from the
	// per-order Order.ClientId (uint32) carried inside Summary, which goes
	// on-chain — see protocol/x/clob/types/order_id.go:24.
	ClientID string `json:"client_id"`

	ChainID        string `json:"chain_id"`
	SignerAddress  string `json:"signer_address"`
	AccountNumber  string `json:"account_number"`
	Sequence       string `json:"sequence"`
	IsShortTermCLOB bool  `json:"is_short_term_clob"`

	// Encoded transaction parts. Marshaled to base64 by encoding/json when
	// serialized; the field-name suffix _b64 makes the wire shape explicit.
	//
	// TxBodyBytesB64 is always set. AuthInfoBytesB64 and SignBytesB64 are
	// pre-computed by the server only when the tenant config carries the
	// signer's public key (v0.2+); when absent (v0.1), the local signer
	// builds AuthInfo and computes sign-bytes itself using the chain id,
	// account number, sequence, and fee carried in this payload.
	TxBodyBytesB64   []byte `json:"tx_body_bytes_b64"`
	AuthInfoBytesB64 []byte `json:"auth_info_bytes_b64,omitempty"`
	SignBytesB64     []byte `json:"sign_bytes_b64,omitempty"`

	Fee     Fee     `json:"fee"`
	Summary Summary `json:"summary"`

	// ExpiresAt is the server-side TTL on payload validity; broadcast_signed_tx
	// rejects payloads past this point so a stale long-pending sign-then-broadcast
	// cycle does not produce surprise on-chain actions.
	ExpiresAt time.Time `json:"expires_at"`
}

// Fee mirrors cosmos.tx.v1beta1.Fee on the wire. CLOB txs in svpchain pay
// zero fee (Amount stays empty) and GasLimit is a comfortable constant —
// see cmd/dex-bench/cosmos_signing.go:42-43 and the plan's open item #6.
type Fee struct {
	GasLimit string `json:"gas_limit"`
	Amount   []Coin `json:"amount"`
}

// Coin is the JSON form of sdk.Coin (amount kept as string to preserve
// uint128/uint64 precision).
type Coin struct {
	Denom  string `json:"denom"`
	Amount string `json:"amount"`
}

// SubaccountRef is the JSON form of subaccounts.SubaccountId
// (proto/dydxprotocol/subaccounts/subaccount.proto:11).
type SubaccountRef struct {
	Owner  string `json:"owner"`
	Number uint32 `json:"number"`
}

// Summary is the human-readable description of a build_* result. The
// remote server fills it from tool args; clients display it for user
// approval. The server is the authority on what was actually signed (via
// TxBodyBytesB64) — Summary is informational only and never re-validated
// at broadcast time except for sanity comparisons.
type Summary struct {
	ToolName   string        `json:"tool_name"`
	MsgTypeURL string        `json:"msg_type_url"`
	Subaccount SubaccountRef `json:"subaccount"`

	// Trading-specific fields (omitted for non-trading tools).
	Ticker        string `json:"ticker,omitempty"`
	Side          string `json:"side,omitempty"`
	SizeHuman     string `json:"size_human,omitempty"`
	PriceHuman    string `json:"price_human,omitempty"`
	NotionalUSD   string `json:"notional_usd,omitempty"`
	GoodTil       string `json:"good_til,omitempty"`
	ReduceOnly    bool   `json:"reduce_only,omitempty"`
	OrderClientID uint32 `json:"order_client_id,omitempty"`

	// Fund-movement fields (v0.2; left here so the shape is stable).
	AssetID        uint32 `json:"asset_id,omitempty"`
	AmountHuman    string `json:"amount_human,omitempty"`
	RecipientOwner string `json:"recipient_owner,omitempty"`
	RecipientNum   uint32 `json:"recipient_subaccount,omitempty"`
}

// SignedTx is what broadcast_signed_tx receives from the MCP client. The
// remote server decodes TxRawBytesB64 and verifies it matches the
// TxPayload it originally built before broadcasting.
type SignedTx struct {
	TxRawBytesB64 []byte `json:"tx_raw_bytes_b64"`
	SignatureB64  []byte `json:"signature_b64"`
	PubKeyB64     []byte `json:"pub_key_b64"`
}

// BroadcastResult is the return shape of broadcast_signed_tx after a
// successful BroadcastSync. Code 0 means accepted into mempool;
// non-zero is a CheckTx reject with RawLog explaining why.
type BroadcastResult struct {
	TxHash string `json:"tx_hash"` // hex
	Code   uint32 `json:"code"`
	RawLog string `json:"raw_log,omitempty"`
}
