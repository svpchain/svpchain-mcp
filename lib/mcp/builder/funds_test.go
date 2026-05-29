package builder_test

import (
	"testing"

	"github.com/stretchr/testify/require"

	// Blank import sets sdk bech32 prefix to "svp" via init() — required
	// for any test that runs Msg*.ValidateBasic on a bech32 address.
	_ "github.com/dydxprotocol/v4-chain/protocol/app/config"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/builder"
	assettypes "github.com/dydxprotocol/v4-chain/protocol/x/assets/types"
)

func newFundsAsm(t *testing.T) *builder.Assembler {
	t.Helper()
	return builder.NewAssembler("test-chain")
}

const fundsTestOwner = "svp199tqg4wdlnu4qjlxchpd7seg454937hjk505pe"

func TestBuildDepositToSubaccount_Happy(t *testing.T) {
	msg, p, err := builder.BuildDepositToSubaccount(builder.DepositToSubaccountInput{
		Owner:           fundsTestOwner,
		SubaccountNum:   0,
		HumanUSDC:       "100",
		PayloadClientID: "uuid-dep-1",
	}, newFundsAsm(t), 7, 17)
	require.NoError(t, err)
	require.Equal(t, fundsTestOwner, msg.Sender)
	require.Equal(t, fundsTestOwner, msg.Recipient.Owner)
	require.Equal(t, uint32(0), msg.Recipient.Number)
	require.Equal(t, assettypes.AssetUsdc.Id, msg.AssetId)
	require.EqualValues(t, 100_000_000, msg.Quantums) // 100 USDC * 10^6
	require.NotNil(t, p)
	require.Equal(t, "/dydxprotocol.sending.MsgDepositToSubaccount", p.Summary.MsgTypeURL)
	require.Equal(t, "100", p.Summary.AmountHuman)
	require.False(t, p.IsShortTermCLOB, "funds movements are not short-term CLOB")
}

func TestBuildDepositToSubaccount_FractionalUSDC(t *testing.T) {
	msg, _, err := builder.BuildDepositToSubaccount(builder.DepositToSubaccountInput{
		Owner:           fundsTestOwner,
		SubaccountNum:   0,
		HumanUSDC:       "1.500001",
		PayloadClientID: "uuid-dep-2",
	}, newFundsAsm(t), 7, 17)
	require.NoError(t, err)
	require.EqualValues(t, 1_500_001, msg.Quantums)
}

func TestBuildWithdrawFromSubaccount_Happy(t *testing.T) {
	msg, p, err := builder.BuildWithdrawFromSubaccount(builder.WithdrawFromSubaccountInput{
		Owner:           fundsTestOwner,
		SubaccountNum:   0,
		HumanUSDC:       "50",
		PayloadClientID: "uuid-wd-1",
	}, newFundsAsm(t), 7, 17)
	require.NoError(t, err)
	require.Equal(t, fundsTestOwner, msg.Sender.Owner)
	require.Equal(t, uint32(0), msg.Sender.Number)
	require.Equal(t, fundsTestOwner, msg.Recipient)
	require.Equal(t, assettypes.AssetUsdc.Id, msg.AssetId)
	require.EqualValues(t, 50_000_000, msg.Quantums)
	require.Equal(t, "/dydxprotocol.sending.MsgWithdrawFromSubaccount", p.Summary.MsgTypeURL)
}

func TestBuildWithdrawFromSubaccount_Rejects(t *testing.T) {
	_, _, err := builder.BuildWithdrawFromSubaccount(builder.WithdrawFromSubaccountInput{
		Owner:           fundsTestOwner,
		SubaccountNum:   0,
		HumanUSDC:       "0",
		PayloadClientID: "u",
	}, newFundsAsm(t), 1, 1)
	require.Error(t, err)
	require.Contains(t, err.Error(), "> 0")
}

func TestBuildTransferBetweenSubaccounts_Happy(t *testing.T) {
	msg, p, err := builder.BuildTransferBetweenSubaccounts(builder.TransferBetweenSubaccountsInput{
		Owner:                  fundsTestOwner,
		SenderSubaccountNum:    0,
		RecipientSubaccountNum: 1,
		HumanUSDC:              "10",
		PayloadClientID:        "uuid-xfer-1",
	}, newFundsAsm(t), 7, 17)
	require.NoError(t, err)
	require.Equal(t, fundsTestOwner, msg.Transfer.Sender.Owner)
	require.Equal(t, uint32(0), msg.Transfer.Sender.Number)
	require.Equal(t, fundsTestOwner, msg.Transfer.Recipient.Owner)
	require.Equal(t, uint32(1), msg.Transfer.Recipient.Number)
	require.EqualValues(t, 10_000_000, msg.Transfer.Amount)
	require.Equal(t, "/dydxprotocol.sending.MsgCreateTransfer", p.Summary.MsgTypeURL)
	require.Equal(t, fundsTestOwner, p.Summary.RecipientOwner)
	require.Equal(t, uint32(1), p.Summary.RecipientNum)
}

func TestBuildTransferBetweenSubaccounts_Rejects(t *testing.T) {
	cases := []struct {
		name    string
		in      builder.TransferBetweenSubaccountsInput
		wantErr string
	}{
		{
			name: "sender == recipient (same subaccount num)",
			in: builder.TransferBetweenSubaccountsInput{
				Owner: fundsTestOwner, SenderSubaccountNum: 0, RecipientSubaccountNum: 0,
				HumanUSDC: "1", PayloadClientID: "u",
			},
			wantErr: "must differ",
		},
		{
			name: "zero amount",
			in: builder.TransferBetweenSubaccountsInput{
				Owner: fundsTestOwner, SenderSubaccountNum: 0, RecipientSubaccountNum: 1,
				HumanUSDC: "0", PayloadClientID: "u",
			},
			wantErr: "> 0",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := builder.BuildTransferBetweenSubaccounts(tc.in, newFundsAsm(t), 1, 1)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

func TestBuildDepositToSubaccount_Rejects(t *testing.T) {
	cases := []struct {
		name    string
		in      builder.DepositToSubaccountInput
		wantErr string
	}{
		{
			name:    "zero amount",
			in:      builder.DepositToSubaccountInput{Owner: fundsTestOwner, HumanUSDC: "0", PayloadClientID: "u"},
			wantErr: "> 0",
		},
		{
			name:    "negative amount",
			in:      builder.DepositToSubaccountInput{Owner: fundsTestOwner, HumanUSDC: "-1", PayloadClientID: "u"},
			wantErr: "non-negative",
		},
		{
			name:    "too many decimals",
			in:      builder.DepositToSubaccountInput{Owner: fundsTestOwner, HumanUSDC: "1.1234567", PayloadClientID: "u"},
			wantErr: "more than 6",
		},
		{
			name:    "invalid owner bech32",
			in:      builder.DepositToSubaccountInput{Owner: "not-a-bech32", HumanUSDC: "1", PayloadClientID: "u"},
			wantErr: "ValidateBasic",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := builder.BuildDepositToSubaccount(tc.in, newFundsAsm(t), 1, 1)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.wantErr)
		})
	}
}
