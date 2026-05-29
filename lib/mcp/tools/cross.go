package tools

import (
	"context"
	"encoding/base64"
	"fmt"

	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	cryptotypes "github.com/cosmos/cosmos-sdk/crypto/types"
	sdk "github.com/cosmos/cosmos-sdk/types"
	txtypes "github.com/cosmos/cosmos-sdk/types/tx"
	"github.com/cosmos/gogoproto/proto"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/chain"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/limits"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/payload"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/policy"
	assettypes "github.com/dydxprotocol/v4-chain/protocol/x/assets/types"
	sendingtypes "github.com/dydxprotocol/v4-chain/protocol/x/sending/types"
)

// -- broadcast_signed_tx -----------------------------------------------

type BroadcastSignedTxInput struct {
	ClientID string           `json:"client_id" jsonschema:"payload-level idempotency uuid (must match payload.client_id)"`
	SignedTx payload.SignedTx `json:"signed_tx"`
}

type BroadcastSignedTxOutput struct {
	Result payload.BroadcastResult `json:"result"`
}

func (h *Handlers) BroadcastSignedTx(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	in BroadcastSignedTxInput,
) (*mcp.CallToolResult, BroadcastSignedTxOutput, error) {
	tc, ok := TenantFrom(ctx)
	if !ok {
		return nil, BroadcastSignedTxOutput{}, ErrNoTenant
	}
	if err := h.Deps.Policy.CheckTenant(tc.TenantID); err != nil {
		return nil, BroadcastSignedTxOutput{}, err
	}
	if !h.Deps.RateLimit.Allow("broadcast_signed_tx:" + tc.TenantID) {
		return nil, BroadcastSignedTxOutput{}, userErrf("rate limit exceeded")
	}
	if err := h.Deps.Idempotency.Claim(tc.TenantID, in.ClientID); err != nil {
		return nil, BroadcastSignedTxOutput{}, err
	}

	// Base64-decode the raw tx bytes (the field is a base64 string on the
	// wire — see the comment on payload.SignedTx).
	rawBytes, err := base64.StdEncoding.DecodeString(in.SignedTx.TxRawBytesB64)
	if err != nil {
		return nil, BroadcastSignedTxOutput{}, fmt.Errorf("decode tx_raw_bytes_b64: %w", err)
	}
	// Decode TxRaw + AuthInfo to verify the signer address matches the
	// tenant's configured owner. Without this check, a tenant could submit
	// a tx signed by some other key.
	signerAddr, err := h.signerAddressFromTxRaw(rawBytes)
	if err != nil {
		return nil, BroadcastSignedTxOutput{}, fmt.Errorf("decode signed tx: %w", err)
	}
	tp, err := h.Deps.Policy.Tenant(tc.TenantID)
	if err != nil {
		return nil, BroadcastSignedTxOutput{}, err
	}
	if signerAddr != tp.Owner {
		return nil, BroadcastSignedTxOutput{}, fmt.Errorf(
			"signer address %s does not match tenant owner %s",
			signerAddr, tp.Owner,
		)
	}

	// Dual-enforcement safety net: decode the TxBody, sum every USDC
	// withdraw across all messages, and re-Enforce against the per-tenant
	// daily ledger before we hit the wire. Catches the bypass where a
	// caller hand-crafts an unsigned tx to skip the build_* checks. Also
	// captures the per-tx delta so a successful broadcast records the
	// matching spend.
	withdrawQ, err := extractWithdrawQuantums(rawBytes, h.Deps.InterfaceRegistry)
	if err != nil {
		return nil, BroadcastSignedTxOutput{}, fmt.Errorf("inspect tx: %w", err)
	}
	if withdrawQ > 0 {
		if err := limits.Enforce(h.Deps.Limits, h.Deps.WithdrawLedger, tc.TenantID, limits.ToolWithdraw, withdrawQ); err != nil {
			return nil, BroadcastSignedTxOutput{}, err
		}
	}

	res, err := h.Deps.Chain.Broadcast.BroadcastSync(ctx, rawBytes)
	outcome := "broadcast"
	if err != nil {
		outcome = "chain_reject"
	} else if res.Code != 0 {
		outcome = "chain_reject"
	}
	_ = h.Deps.Auditor.Append(policy.AuditEntry{
		TenantID: tc.TenantID,
		Owner:    tp.Owner,
		Tool:     "broadcast_signed_tx",
		ClientID: in.ClientID,
		TxHash:   res.TxHash,
		Code:     res.Code,
		Outcome:  outcome,
		Reason:   res.RawLog,
	})
	if err != nil {
		return nil, BroadcastSignedTxOutput{}, err
	}
	// AIMD fail-fast: parse "insufficient fee" out of RawLog and surface
	// the chain's required min-gas-price as a typed error. The server
	// never retries; the caller decides whether to bump and retry.
	if res.Code != 0 {
		if pe := chain.ParseBroadcastError(res); pe != nil {
			return nil, BroadcastSignedTxOutput{}, pe
		}
	}
	// Spend only after the chain accepts the broadcast — a failed CheckTx
	// doesn't eat the tenant's daily cap.
	if res.Code == 0 && withdrawQ > 0 && h.Deps.WithdrawLedger != nil {
		h.Deps.WithdrawLedger.Record(tc.TenantID, withdrawQ)
	}
	return nil, BroadcastSignedTxOutput{Result: payload.BroadcastResult{
		TxHash: res.TxHash,
		Code:   res.Code,
		RawLog: res.RawLog,
	}}, nil
}

// extractWithdrawQuantums sums the USDC quantums of every
// MsgWithdrawFromSubaccount in the tx body. Returns 0 if there are no
// withdraws. Non-USDC withdraws are ignored — the chain rejects them in
// ValidateBasic, and the cap is denominated in USDC anyway.
func extractWithdrawQuantums(rawTxBytes []byte, reg codectypes.InterfaceRegistry) (uint64, error) {
	var txRaw txtypes.TxRaw
	if err := proto.Unmarshal(rawTxBytes, &txRaw); err != nil {
		return 0, fmt.Errorf("unmarshal TxRaw: %w", err)
	}
	var body txtypes.TxBody
	if err := proto.Unmarshal(txRaw.BodyBytes, &body); err != nil {
		return 0, fmt.Errorf("unmarshal TxBody: %w", err)
	}
	var total uint64
	for _, anyMsg := range body.Messages {
		var msg sdk.Msg
		if err := reg.UnpackAny(anyMsg, &msg); err != nil {
			// Unknown msg type — not a funds msg we care about.
			continue
		}
		w, ok := msg.(*sendingtypes.MsgWithdrawFromSubaccount)
		if !ok {
			continue
		}
		if w.AssetId != assettypes.AssetUsdc.Id {
			continue
		}
		sum := total + w.Quantums
		if sum < total {
			return 0, fmt.Errorf("withdraw quantums total overflows uint64")
		}
		total = sum
	}
	return total, nil
}

// signerAddressFromTxRaw decodes a TxRaw bytes blob, extracts the first
// SignerInfo's public key, unpacks it via the InterfaceRegistry, and
// returns the bech32 string address. Returns an error if there is not
// exactly one signer (v0.1 only supports single-signer txs).
func (h *Handlers) signerAddressFromTxRaw(raw []byte) (string, error) {
	var txRaw txtypes.TxRaw
	if err := proto.Unmarshal(raw, &txRaw); err != nil {
		return "", fmt.Errorf("unmarshal TxRaw: %w", err)
	}
	var ai txtypes.AuthInfo
	if err := proto.Unmarshal(txRaw.AuthInfoBytes, &ai); err != nil {
		return "", fmt.Errorf("unmarshal AuthInfo: %w", err)
	}
	if len(ai.SignerInfos) != 1 {
		return "", fmt.Errorf("expected exactly 1 signer, got %d", len(ai.SignerInfos))
	}
	pkAny := ai.SignerInfos[0].PublicKey
	if pkAny == nil {
		return "", fmt.Errorf("signer info has no public key")
	}
	var pk cryptotypes.PubKey
	if err := h.Deps.InterfaceRegistry.UnpackAny(pkAny, &pk); err != nil {
		return "", fmt.Errorf("unpack signer pubkey: %w", err)
	}
	return sdk.AccAddress(pk.Address()).String(), nil
}

// -- get_tx_status -----------------------------------------------------

type GetTxStatusInput struct {
	TxHash string `json:"tx_hash" jsonschema:"hex tx hash returned by broadcast_signed_tx"`
}
type GetTxStatusOutput struct {
	TxHash string `json:"tx_hash"`
	Height int64  `json:"height"`
	Code   uint32 `json:"code"`
	RawLog string `json:"raw_log,omitempty"`
}

func (h *Handlers) GetTxStatus(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	in GetTxStatusInput,
) (*mcp.CallToolResult, GetTxStatusOutput, error) {
	tc, ok := TenantFrom(ctx)
	if !ok {
		return nil, GetTxStatusOutput{}, ErrNoTenant
	}
	if err := h.Deps.Policy.CheckTenant(tc.TenantID); err != nil {
		return nil, GetTxStatusOutput{}, err
	}
	if !h.Deps.RateLimit.Allow("get_tx_status:" + tc.TenantID) {
		return nil, GetTxStatusOutput{}, userErrf("rate limit exceeded")
	}
	st, err := h.Deps.Chain.CometBft.TxStatus(ctx, in.TxHash)
	if err != nil {
		return nil, GetTxStatusOutput{}, err
	}
	return nil, GetTxStatusOutput{
		TxHash: st.TxHash,
		Height: st.Height,
		Code:   st.Code,
		RawLog: st.RawLog,
	}, nil
}
