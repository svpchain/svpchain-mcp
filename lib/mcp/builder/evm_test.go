package builder_test

import (
	"context"
	"encoding/json"
	"math/big"
	"testing"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/stretchr/testify/require"

	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/builder"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/payload"
)

// mockEVMClient is a minimal chain.EVMClient for assembler unit tests. Only
// the read/state calls Assemble uses return meaningful values.
type mockEVMClient struct {
	nonce   uint64
	gas     uint64
	tip     *big.Int
	baseFee *big.Int
	chainID *big.Int

	gotCall ethereum.CallMsg // captured EstimateGas input
}

func (m *mockEVMClient) PendingNonceAt(context.Context, common.Address) (uint64, error) {
	return m.nonce, nil
}
func (m *mockEVMClient) EstimateGas(_ context.Context, msg ethereum.CallMsg) (uint64, error) {
	m.gotCall = msg
	return m.gas, nil
}
func (m *mockEVMClient) SuggestGasTipCap(context.Context) (*big.Int, error) { return m.tip, nil }
func (m *mockEVMClient) BaseFee(context.Context) (*big.Int, error)          { return m.baseFee, nil }
func (m *mockEVMClient) ChainID(context.Context) (*big.Int, error)          { return m.chainID, nil }
func (m *mockEVMClient) CallContract(context.Context, ethereum.CallMsg) ([]byte, error) {
	return nil, nil
}
func (m *mockEVMClient) SendTransaction(context.Context, *ethtypes.Transaction) (string, error) {
	return "", nil
}
func (m *mockEVMClient) TransactionReceipt(context.Context, common.Hash) (*ethtypes.Receipt, error) {
	return nil, nil
}

func TestEVMAssembler_Assemble(t *testing.T) {
	mock := &mockEVMClient{
		nonce:   7,
		gas:     100_000,
		tip:     big.NewInt(1_000_000_000), // 1 gwei
		baseFee: big.NewInt(2_000_000_000), // 2 gwei
		chainID: big.NewInt(262144),
	}
	a := builder.NewEVMAssembler(mock)

	from := common.HexToAddress("0x1111111111111111111111111111111111111111")
	to := common.HexToAddress("0x2222222222222222222222222222222222222222")
	data := []byte{0x4e, 0x71, 0xd9, 0x2d} // claim()

	p, err := a.Assemble(context.Background(), builder.EVMArgs{
		ClientID: "cid",
		From:     from,
		To:       to,
		Data:     data,
		Summary:  payload.EVMSummary{ToolName: "build_faucet_claim"},
	})
	require.NoError(t, err)

	require.Equal(t, payload.CurrentVersion, p.Version)
	require.Equal(t, "cid", p.ClientID)
	require.Equal(t, "262144", p.EVMChainID)
	require.Equal(t, payload.EVMTxTypeEIP1559, p.TxType)
	require.Equal(t, from.Hex(), p.SignerAddress)
	require.Equal(t, to.Hex(), p.To)
	require.Equal(t, "7", p.Nonce)
	// gas is padded 1.25x: 100000 * 5 / 4 = 125000.
	require.Equal(t, "125000", p.Gas)
	// maxFee = baseFee*2 + tip = 4e9 + 1e9 = 5e9.
	require.Equal(t, "5000000000", p.MaxFeePerGas)
	require.Equal(t, "1000000000", p.MaxPriorityFeePerGas)
	require.Equal(t, "0", p.Value) // nil value -> zero
	require.Equal(t, "0x4e71d92d", p.Data)
	require.False(t, p.ExpiresAt.IsZero())

	// EstimateGas was called with the EIP-1559 caps + the real calldata.
	require.Equal(t, &to, mock.gotCall.To)
	require.Equal(t, data, mock.gotCall.Data)
	require.Equal(t, mock.tip, mock.gotCall.GasTipCap)
}

// TestEVMTxPayload_WireShapeMatchesSigner locks the JSON shape the remote emits
// to the field names the svpchain-signer-mcp signer reads (its
// payload.EvmTxPayload). If a rename drifts these apart, the signer would
// silently see zero-valued fields — this guard fails loudly instead.
func TestEVMTxPayload_WireShapeMatchesSigner(t *testing.T) {
	p := payload.EVMTxPayload{
		Version:              payload.CurrentVersion,
		ClientID:             "cid",
		EVMChainID:           "262144",
		SignerAddress:        "0x1111111111111111111111111111111111111111",
		TxType:               payload.EVMTxTypeEIP1559,
		To:                   "0x2222222222222222222222222222222222222222",
		Nonce:                "7",
		Gas:                  "125000",
		MaxFeePerGas:         "5000000000",
		MaxPriorityFeePerGas: "1000000000",
		Value:                "0",
		Data:                 "0x4e71d92d",
		Summary:              payload.EVMSummary{ToolName: "build_faucet_claim", Description: "faucet claim()"},
	}
	b, err := json.Marshal(p)
	require.NoError(t, err)
	var m map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(b, &m))

	// Keys the signer's EvmTxPayload decodes (evm.go in svpchain-signer-mcp).
	for _, k := range []string{
		"version", "evm_chain_id", "signer_address", "tx_type", "to",
		"nonce", "gas", "max_fee_per_gas", "max_priority_fee_per_gas",
		"value", "data", "summary",
	} {
		_, ok := m[k]
		require.Truef(t, ok, "missing signer-required JSON key %q in %s", k, b)
	}
	// Must NOT emit the old pre-alignment names.
	for _, k := range []string{"chain_id", "gas_limit", "data_hex"} {
		_, ok := m[k]
		require.Falsef(t, ok, "unexpected legacy JSON key %q (drifted from signer)", k)
	}
}

func TestBuildFaucetClaim_PacksSelector(t *testing.T) {
	contract := common.HexToAddress("0x2222222222222222222222222222222222222222")
	to, data, err := builder.BuildFaucetClaim(builder.FaucetClaimArgs{Contract: contract})
	require.NoError(t, err)
	require.Equal(t, contract, to)
	// claim() takes no args, so calldata is exactly the 4-byte selector.
	require.Equal(t, builder.FaucetABI.Methods["claim"].ID, data)
	require.Len(t, data, 4)
}
