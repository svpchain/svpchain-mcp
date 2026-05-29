package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/builder"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/limits"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/payload"
)

// -- build_deposit_to_subaccount ---------------------------------------

type BuildDepositToSubaccountInput struct {
	SubaccountNumber uint32 `json:"subaccount_number"`
	HumanUSDC        string `json:"human_usdc" jsonschema:"USDC amount in human units, e.g. \"100\" or \"1.5\""`
	PayloadClientID  string `json:"payload_client_id" jsonschema:"broadcast-idempotency uuid"`
}

type BuildDepositToSubaccountOutput struct {
	Payload payload.TxPayload `json:"payload"`
}

func (h *Handlers) BuildDepositToSubaccount(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	in BuildDepositToSubaccountInput,
) (*mcp.CallToolResult, BuildDepositToSubaccountOutput, error) {
	tc, ok := TenantFrom(ctx)
	if !ok {
		return nil, BuildDepositToSubaccountOutput{}, ErrNoTenant
	}
	if err := h.Deps.Policy.CheckTenant(tc.TenantID); err != nil {
		return nil, BuildDepositToSubaccountOutput{}, err
	}
	tp, err := h.Deps.Policy.Tenant(tc.TenantID)
	if err != nil {
		return nil, BuildDepositToSubaccountOutput{}, err
	}
	if err := h.Deps.Policy.CheckSubaccount(tc.TenantID, in.SubaccountNumber); err != nil {
		return nil, BuildDepositToSubaccountOutput{}, err
	}
	if !h.Deps.RateLimit.Allow("build_deposit_to_subaccount:" + tc.TenantID) {
		return nil, BuildDepositToSubaccountOutput{}, userErrf("rate limit exceeded")
	}

	// Per-tx cap on deposit (no daily cap — money is coming in, not going out).
	// Caps live in quantums end-to-end so a 1.5 USDC tx with a 1 USDC cap
	// can't slip through via integer-USDC rounding.
	quantums, err := limits.HumanToQuantums(in.HumanUSDC)
	if err != nil {
		return nil, BuildDepositToSubaccountOutput{}, fmt.Errorf("human_usdc: %w", err)
	}
	if err := limits.CheckPerTx(h.Deps.Limits, limits.ToolDeposit, quantums); err != nil {
		return nil, BuildDepositToSubaccountOutput{}, err
	}

	acc, err := h.Deps.Chain.Account.Account(ctx, tp.Owner)
	if err != nil {
		return nil, BuildDepositToSubaccountOutput{}, fmt.Errorf("read account state: %w", err)
	}

	_, p, err := builder.BuildDepositToSubaccount(
		builder.DepositToSubaccountInput{
			Owner:           tp.Owner,
			SubaccountNum:   in.SubaccountNumber,
			HumanUSDC:       in.HumanUSDC,
			PayloadClientID: in.PayloadClientID,
		},
		h.Deps.Builder,
		acc.AccountNumber,
		acc.Sequence,
	)
	if err != nil {
		return nil, BuildDepositToSubaccountOutput{}, err
	}
	return nil, BuildDepositToSubaccountOutput{Payload: *p}, nil
}
