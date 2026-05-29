package builder

import (
	"fmt"

	sdk "github.com/cosmos/cosmos-sdk/types"

	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/limits"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/payload"
	assettypes "github.com/dydxprotocol/v4-chain/protocol/x/assets/types"
	sendingtypes "github.com/dydxprotocol/v4-chain/protocol/x/sending/types"
	satypes "github.com/dydxprotocol/v4-chain/protocol/x/subaccounts/types"
)

// -- build_deposit_to_subaccount ---------------------------------------

// DepositToSubaccountInput drives build_deposit_to_subaccount. svpchain's
// sending module only accepts USDC (see ErrNonUsdcAssetTransferNotImplemented
// in x/sending/types), so the asset is implicit — the input carries only the
// human USDC amount.
type DepositToSubaccountInput struct {
	// Owner is the bech32 bank-account sender. The recipient subaccount
	// belongs to the same owner; per-tenant subaccount allowlist enforcement
	// happens in the tool handler, not here.
	Owner         string
	SubaccountNum uint32

	HumanUSDC string // e.g. "100", "1.5"

	PayloadClientID string
}

// BuildDepositToSubaccount turns a DepositToSubaccountInput into a
// MsgDepositToSubaccount and an envelope TxPayload. The human USDC string is
// converted to quantums via limits.HumanToQuantums (10^6 = 1 USDC).
//
// Cap enforcement is the tool handler's responsibility — keeping the builder
// cap-agnostic mirrors how policy / subaccount checks live in handlers
// (see tools/trade.go).
func BuildDepositToSubaccount(
	in DepositToSubaccountInput,
	asm *Assembler,
	accountNumber, sequence uint64,
) (*sendingtypes.MsgDepositToSubaccount, *payload.TxPayload, error) {
	quantums, err := limits.HumanToQuantums(in.HumanUSDC)
	if err != nil {
		return nil, nil, fmt.Errorf("human_usdc: %w", err)
	}
	if quantums == 0 {
		return nil, nil, fmt.Errorf("human_usdc must be > 0")
	}

	msg := sendingtypes.NewMsgDepositToSubaccount(
		in.Owner,
		satypes.SubaccountId{Owner: in.Owner, Number: in.SubaccountNum},
		assettypes.AssetUsdc.Id,
		quantums,
	)
	if err := msg.ValidateBasic(); err != nil {
		return nil, nil, fmt.Errorf("MsgDepositToSubaccount.ValidateBasic: %w", err)
	}

	summary := payload.Summary{
		ToolName:    "build_deposit_to_subaccount",
		MsgTypeURL:  "/dydxprotocol.sending.MsgDepositToSubaccount",
		Subaccount:  payload.SubaccountRef{Owner: in.Owner, Number: in.SubaccountNum},
		AssetID:     assettypes.AssetUsdc.Id,
		AmountHuman: in.HumanUSDC,
	}
	txPayload, err := asm.Assemble(Args{
		Msgs:          []sdk.Msg{msg},
		SignerAddress: in.Owner,
		AccountNumber: accountNumber,
		Sequence:      sequence,
		ClientID:      in.PayloadClientID,
		Summary:       summary,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("assemble: %w", err)
	}
	return msg, txPayload, nil
}
