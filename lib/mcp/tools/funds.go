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
	tp, err := h.authorizeSubaccount(ctx, "build_deposit_to_subaccount", in.SubaccountNumber)
	if err != nil {
		return nil, BuildDepositToSubaccountOutput{}, err
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

// -- build_withdraw_from_subaccount ------------------------------------

type BuildWithdrawFromSubaccountInput struct {
	SubaccountNumber uint32 `json:"subaccount_number"`
	HumanUSDC        string `json:"human_usdc" jsonschema:"USDC amount in human units, e.g. \"100\" or \"1.5\""`
	PayloadClientID  string `json:"payload_client_id" jsonschema:"broadcast-idempotency uuid"`
}

type BuildWithdrawFromSubaccountOutput struct {
	Payload payload.TxPayload `json:"payload"`
}

func (h *Handlers) BuildWithdrawFromSubaccount(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	in BuildWithdrawFromSubaccountInput,
) (*mcp.CallToolResult, BuildWithdrawFromSubaccountOutput, error) {
	tp, err := h.authorizeSubaccount(ctx, "build_withdraw_from_subaccount", in.SubaccountNumber)
	if err != nil {
		return nil, BuildWithdrawFromSubaccountOutput{}, err
	}

	// Build-time enforcement is best-effort UX — it gives the caller a fast
	// structured rejection without burning a sign. The real safety net runs
	// at broadcast_signed_tx, where we decode the tx and re-Enforce against
	// the ledger that may have advanced between build and broadcast.
	quantums, err := limits.HumanToQuantums(in.HumanUSDC)
	if err != nil {
		return nil, BuildWithdrawFromSubaccountOutput{}, fmt.Errorf("human_usdc: %w", err)
	}
	if err := limits.Enforce(h.Deps.Limits, h.Deps.WithdrawLedger, tp.TenantID, limits.ToolWithdraw, quantums); err != nil {
		return nil, BuildWithdrawFromSubaccountOutput{}, err
	}

	acc, err := h.Deps.Chain.Account.Account(ctx, tp.Owner)
	if err != nil {
		return nil, BuildWithdrawFromSubaccountOutput{}, fmt.Errorf("read account state: %w", err)
	}

	_, p, err := builder.BuildWithdrawFromSubaccount(
		builder.WithdrawFromSubaccountInput{
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
		return nil, BuildWithdrawFromSubaccountOutput{}, err
	}
	return nil, BuildWithdrawFromSubaccountOutput{Payload: *p}, nil
}

// -- build_transfer_between_subaccounts (same-owner) -------------------

type BuildTransferBetweenSubaccountsInput struct {
	SenderSubaccountNumber    uint32 `json:"sender_subaccount_number"`
	RecipientSubaccountNumber uint32 `json:"recipient_subaccount_number"`
	HumanUSDC                 string `json:"human_usdc" jsonschema:"USDC amount in human units"`
	PayloadClientID           string `json:"payload_client_id" jsonschema:"broadcast-idempotency uuid"`
}

type BuildTransferBetweenSubaccountsOutput struct {
	Payload payload.TxPayload `json:"payload"`
}

func (h *Handlers) BuildTransferBetweenSubaccounts(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	in BuildTransferBetweenSubaccountsInput,
) (*mcp.CallToolResult, BuildTransferBetweenSubaccountsOutput, error) {
	// Same-owner transfer needs CheckSubaccount on BOTH sender and recipient
	// with distinct error prefixes — too custom for authorizeSubaccount.
	// Use the base authorize and inline the two subaccount checks.
	tp, err := h.authorize(ctx, "build_transfer_between_subaccounts")
	if err != nil {
		return nil, BuildTransferBetweenSubaccountsOutput{}, err
	}
	if err := h.Deps.Policy.CheckSubaccount(tp.TenantID, in.SenderSubaccountNumber); err != nil {
		return nil, BuildTransferBetweenSubaccountsOutput{}, fmt.Errorf("sender: %w", err)
	}
	if err := h.Deps.Policy.CheckSubaccount(tp.TenantID, in.RecipientSubaccountNumber); err != nil {
		return nil, BuildTransferBetweenSubaccountsOutput{}, fmt.Errorf("recipient: %w", err)
	}

	// Per-tx cap only (no daily cap — funds stay in the tenant's books).
	quantums, err := limits.HumanToQuantums(in.HumanUSDC)
	if err != nil {
		return nil, BuildTransferBetweenSubaccountsOutput{}, fmt.Errorf("human_usdc: %w", err)
	}
	if err := limits.CheckPerTx(h.Deps.Limits, limits.ToolTransfer, quantums); err != nil {
		return nil, BuildTransferBetweenSubaccountsOutput{}, err
	}

	acc, err := h.Deps.Chain.Account.Account(ctx, tp.Owner)
	if err != nil {
		return nil, BuildTransferBetweenSubaccountsOutput{}, fmt.Errorf("read account state: %w", err)
	}

	_, p, err := builder.BuildTransferBetweenSubaccounts(
		builder.TransferBetweenSubaccountsInput{
			Owner:                  tp.Owner,
			SenderSubaccountNum:    in.SenderSubaccountNumber,
			RecipientSubaccountNum: in.RecipientSubaccountNumber,
			HumanUSDC:              in.HumanUSDC,
			PayloadClientID:        in.PayloadClientID,
		},
		h.Deps.Builder,
		acc.AccountNumber,
		acc.Sequence,
	)
	if err != nil {
		return nil, BuildTransferBetweenSubaccountsOutput{}, err
	}
	return nil, BuildTransferBetweenSubaccountsOutput{Payload: *p}, nil
}
