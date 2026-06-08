package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/faucet"
)

// faucet.go holds the HTTP faucet tools. Unlike the EVM build_* tools, these
// do not construct an on-chain transaction for the caller to sign: the faucet
// backend (faucet_base_url) runs its own operator that signs and submits the
// claim. The tools are thin wrappers over lib/mcp/faucet.Client — claim funds
// in one call, no signing / EVM RPC / contract address on the client side.

// nativeTokenAddress is the faucet's sentinel for the chain's native token
// (SVP). The faucet treats the zero address as "native" in /api/claim and
// /api/enabledTokens; an ERC-20 claim passes the token's real 0x address.
const nativeTokenAddress = "0x0000000000000000000000000000000000000000"

// -- list_faucet_tokens ------------------------------------------------

type ListFaucetTokensInput struct{}

type ListFaucetTokensOutput struct {
	Tokens []faucet.TokenInfo `json:"tokens" jsonschema:"tokens the faucet will dispense, each with its per-claim amount (base units)"`
}

func (h *Handlers) ListFaucetTokens(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	_ ListFaucetTokensInput,
) (*mcp.CallToolResult, ListFaucetTokensOutput, error) {
	if _, err := h.authorize(ctx, "list_faucet_tokens"); err != nil {
		return nil, ListFaucetTokensOutput{}, err
	}
	if h.Deps.Faucet == nil {
		return nil, ListFaucetTokensOutput{}, userErrf("faucet is not enabled on this server (no faucet_base_url configured)")
	}
	tokens, err := h.Deps.Faucet.EnabledTokens(ctx)
	if err != nil {
		return nil, ListFaucetTokensOutput{}, err
	}
	return nil, ListFaucetTokensOutput{Tokens: tokens}, nil
}

// -- faucet_claim ------------------------------------------------------

type FaucetClaimInput struct {
	// Token is the 0x token address to claim. Empty (the default) claims the
	// native token (SVP). Use list_faucet_tokens to discover claimable ERC-20
	// addresses.
	Token string `json:"token,omitempty" jsonschema:"0x token address to claim; omit (or 0x0) for the native token (SVP). See list_faucet_tokens."`
}

type FaucetClaimOutput struct {
	TxHash  string `json:"tx_hash"` // on-chain tx the faucet operator submitted
	Amount  string `json:"amount"`  // amount dispensed, base units
	Token   string `json:"token"`   // token that was claimed
	Address string `json:"address"` // recipient (the caller's own EVM address)
}

func (h *Handlers) FaucetClaim(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	in FaucetClaimInput,
) (*mcp.CallToolResult, FaucetClaimOutput, error) {
	tp, err := h.authorize(ctx, "faucet_claim")
	if err != nil {
		return nil, FaucetClaimOutput{}, err
	}
	if h.Deps.Faucet == nil {
		return nil, FaucetClaimOutput{}, userErrf("faucet is not enabled on this server (no faucet_base_url configured)")
	}

	// Recipient is always the caller's own EVM address (the same 20 bytes as
	// the bech32 owner the auth handshake recovered) — never a caller-supplied
	// address.
	addr, err := ownerEthAddress(tp.Owner)
	if err != nil {
		return nil, FaucetClaimOutput{}, err
	}
	address := addr.Hex()

	token := in.Token
	if token == "" {
		token = nativeTokenAddress
	}

	res, err := h.Deps.Faucet.Claim(ctx, token, address)
	if err != nil {
		return nil, FaucetClaimOutput{}, err
	}
	return nil, FaucetClaimOutput{
		TxHash:  res.TxHash,
		Amount:  res.Amount,
		Token:   res.Token,
		Address: address,
	}, nil
}
