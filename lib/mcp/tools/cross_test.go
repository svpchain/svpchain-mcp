package tools

import (
	"testing"

	codectypes "github.com/cosmos/cosmos-sdk/codec/types"
	txtypes "github.com/cosmos/cosmos-sdk/types/tx"
	"github.com/cosmos/gogoproto/proto"
	"github.com/stretchr/testify/require"

	"github.com/dydxprotocol/v4-chain/protocol/app"
	assettypes "github.com/dydxprotocol/v4-chain/protocol/x/assets/types"
	sendingtypes "github.com/dydxprotocol/v4-chain/protocol/x/sending/types"
	satypes "github.com/dydxprotocol/v4-chain/protocol/x/subaccounts/types"
)

// Owner is reused across the test cases below; bech32 validity matters
// only insofar as the encoder accepts the embedded SubaccountId.
const testTxOwner = "svp199tqg4wdlnu4qjlxchpd7seg454937hjk505pe"

// txRawWith builds a TxRaw envelope wrapping the given Cosmos msgs and
// returns the marshalled bytes (the same shape that arrives at
// broadcast_signed_tx via payload.SignedTx.TxRawBytesB64).
func txRawWith(t *testing.T, msgs ...proto.Message) []byte {
	t.Helper()
	anyMsgs := make([]*codectypes.Any, 0, len(msgs))
	for _, m := range msgs {
		a, err := codectypes.NewAnyWithValue(m)
		require.NoError(t, err)
		anyMsgs = append(anyMsgs, a)
	}
	body := &txtypes.TxBody{Messages: anyMsgs}
	bodyBytes, err := proto.Marshal(body)
	require.NoError(t, err)
	rawBytes, err := proto.Marshal(&txtypes.TxRaw{BodyBytes: bodyBytes})
	require.NoError(t, err)
	return rawBytes
}

func TestExtractWithdrawQuantums(t *testing.T) {
	reg := app.GetEncodingConfig().InterfaceRegistry

	t.Run("no withdraw — total is 0", func(t *testing.T) {
		// An empty TxBody should produce no withdraws.
		total, err := extractWithdrawQuantums(txRawWith(t), reg)
		require.NoError(t, err)
		require.EqualValues(t, 0, total)
	})

	t.Run("single withdraw — total matches", func(t *testing.T) {
		w := sendingtypes.NewMsgWithdrawFromSubaccount(
			satypes.SubaccountId{Owner: testTxOwner, Number: 0},
			testTxOwner,
			assettypes.AssetUsdc.Id,
			5_000_000, // 5 USDC
		)
		total, err := extractWithdrawQuantums(txRawWith(t, w), reg)
		require.NoError(t, err)
		require.EqualValues(t, 5_000_000, total)
	})

	t.Run("two withdraws — summed", func(t *testing.T) {
		w1 := sendingtypes.NewMsgWithdrawFromSubaccount(
			satypes.SubaccountId{Owner: testTxOwner, Number: 0},
			testTxOwner, assettypes.AssetUsdc.Id, 1_000_000,
		)
		w2 := sendingtypes.NewMsgWithdrawFromSubaccount(
			satypes.SubaccountId{Owner: testTxOwner, Number: 1},
			testTxOwner, assettypes.AssetUsdc.Id, 2_500_000,
		)
		total, err := extractWithdrawQuantums(txRawWith(t, w1, w2), reg)
		require.NoError(t, err)
		require.EqualValues(t, 3_500_000, total)
	})

	t.Run("withdraw + deposit — only withdraw counts", func(t *testing.T) {
		w := sendingtypes.NewMsgWithdrawFromSubaccount(
			satypes.SubaccountId{Owner: testTxOwner, Number: 0},
			testTxOwner, assettypes.AssetUsdc.Id, 7_500_000,
		)
		d := sendingtypes.NewMsgDepositToSubaccount(
			testTxOwner,
			satypes.SubaccountId{Owner: testTxOwner, Number: 0},
			assettypes.AssetUsdc.Id, 9_999_999,
		)
		total, err := extractWithdrawQuantums(txRawWith(t, w, d), reg)
		require.NoError(t, err)
		require.EqualValues(t, 7_500_000, total)
	})

	t.Run("malformed bytes — error not panic", func(t *testing.T) {
		_, err := extractWithdrawQuantums([]byte{0xff, 0xff, 0xff}, reg)
		require.Error(t, err)
	})
}
