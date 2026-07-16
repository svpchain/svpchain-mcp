package tools

import (
	"bytes"
	"context"
	"math/big"
	"testing"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/require"

	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/builder"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/policy"
)

// mockERCEVM is a full chain.EVMClient for the ERC build path: CallContract
// answers decimals(); the chain-state calls return fixed values so a real
// EVMAssembler can stamp a payload.
type mockERCEVM struct {
	decimals uint8
}

func (m *mockERCEVM) CallContract(_ context.Context, msg ethereum.CallMsg) ([]byte, error) {
	if bytes.Equal(msg.Data[:4], crypto.Keccak256([]byte("decimals()"))[:4]) {
		return common.LeftPadBytes(big.NewInt(int64(m.decimals)).Bytes(), 32), nil
	}
	return nil, nil
}
func (m *mockERCEVM) PendingNonceAt(context.Context, common.Address) (uint64, error) { return 7, nil }
func (m *mockERCEVM) EstimateGas(context.Context, ethereum.CallMsg) (uint64, error) {
	return 50_000, nil
}
func (m *mockERCEVM) SuggestGasTipCap(context.Context) (*big.Int, error) {
	return big.NewInt(1_000_000_000), nil
}
func (m *mockERCEVM) BaseFee(context.Context) (*big.Int, error) {
	return big.NewInt(2_000_000_000), nil
}
func (m *mockERCEVM) ChainID(context.Context) (*big.Int, error) { return big.NewInt(262144), nil }
func (m *mockERCEVM) BlockNumber(context.Context) (uint64, error) { return 0, nil }
func (m *mockERCEVM) SendTransaction(context.Context, *ethtypes.Transaction) (string, error) {
	return "", nil
}
func (m *mockERCEVM) TransactionReceipt(context.Context, common.Hash) (*ethtypes.Receipt, error) {
	return nil, nil
}

func ercHandlers(evm *mockERCEVM) *Handlers {
	return &Handlers{Deps: Deps{
		Chain: ChainDeps{EVM: evm},
		EVM:   EVMDeps{Assembler: builder.NewEVMAssembler(evm)},
	}}
}

func TestParseEVMAddress(t *testing.T) {
	addr, err := parseEVMAddress("0x2222222222222222222222222222222222222222", "to")
	require.NoError(t, err)
	require.Equal(t, common.HexToAddress("0x2222222222222222222222222222222222222222"), addr)

	for _, bad := range []string{"", "0x123", "not-an-address"} {
		_, err := parseEVMAddress(bad, "to")
		require.Error(t, err, "input %q", bad)
	}
}

func TestParseTokenID(t *testing.T) {
	id, err := parseTokenID("42")
	require.NoError(t, err)
	require.Equal(t, big.NewInt(42), id)

	for _, bad := range []string{"", "12.5", "-1", "abc"} {
		_, err := parseTokenID(bad)
		require.Error(t, err, "input %q", bad)
	}
}

func TestERC20Decimals(t *testing.T) {
	h := ercHandlers(&mockERCEVM{decimals: 6})
	dec, err := h.erc20Decimals(context.Background(), common.HexToAddress("0x1111111111111111111111111111111111111111"))
	require.NoError(t, err)
	require.Equal(t, int64(6), dec)
}

// TestBuildERC20Approve_ChainRouting covers the inbound-bridge fix: an approval
// targets the home chain by default, but a chain_id routes it to that chain's
// assembler + client so it is stamped with the foreign chain id (and decimals
// are read there) — required to approve a foreign bridge before an inbound deposit.
func TestBuildERC20Approve_ChainRouting(t *testing.T) {
	const home = uint64(262144)
	homeEVM := &mockERCEVM{decimals: 6}                         // ChainID 262144, nonce 7
	foreignEVM := &mockForeignEVM{chainID: 421614, decimals: 6} // nonce 3
	h := &Handlers{Deps: Deps{
		Chain: ChainDeps{EVM: homeEVM},
		EVM: EVMDeps{
			Assembler:   builder.NewEVMAssembler(homeEVM),
			HomeChainID: home,
			ForeignChains: map[uint64]*ForeignChain{
				421614: {Client: foreignEVM, Assembler: builder.NewEVMAssembler(foreignEVM)},
			},
		},
		Policy:    policy.NewEngine([]policy.TenantPolicy{{TenantID: "t1", Owner: testTxOwner}}),
		RateLimit: policy.NewRateLimiter(0, 0),
	}}
	ctx := WithTenant(context.Background(), TenantContext{TenantID: "t1", Owner: testTxOwner})
	const token = "0x1111111111111111111111111111111111111111"
	const spender = "0x2222222222222222222222222222222222222222"

	t.Run("foreign chain_id stamps the foreign chain", func(t *testing.T) {
		_, out, err := h.BuildERC20Approve(ctx, nil, BuildERC20ApproveInput{
			Token: token, Spender: spender, Amount: "100", ChainID: 421614, ClientID: "c1",
		})
		require.NoError(t, err)
		require.Equal(t, "421614", out.Payload.EVMChainID)
		require.Equal(t, "3", out.Payload.Nonce) // assembled against the foreign client
	})

	t.Run("omitted chain_id stays home", func(t *testing.T) {
		_, out, err := h.BuildERC20Approve(ctx, nil, BuildERC20ApproveInput{
			Token: token, Spender: spender, Unlimited: true, ClientID: "c2",
		})
		require.NoError(t, err)
		require.Equal(t, "262144", out.Payload.EVMChainID)
		require.Equal(t, "7", out.Payload.Nonce)
	})

	t.Run("unknown chain_id is rejected", func(t *testing.T) {
		_, _, err := h.BuildERC20Approve(ctx, nil, BuildERC20ApproveInput{
			Token: token, Spender: spender, Unlimited: true, ChainID: 999, ClientID: "c3",
		})
		require.Error(t, err)
		require.Contains(t, err.Error(), "not configured")
	})
}

func TestAssembleERC_StampsPayload(t *testing.T) {
	h := ercHandlers(&mockERCEVM{decimals: 6})
	token := common.HexToAddress("0x1111111111111111111111111111111111111111")
	to := common.HexToAddress("0x2222222222222222222222222222222222222222")

	data, err := builder.PackERC20Transfer(to, big.NewInt(1_000_000))
	require.NoError(t, err)

	p, err := h.assembleERC(context.Background(), h.Deps.EVM.Assembler, testTxOwner, token, data, "cid", "build_erc20_transfer", "desc")
	require.NoError(t, err)

	require.Equal(t, token.Hex(), p.To)
	require.Equal(t, "0", p.Value) // value-0 contract call
	require.Equal(t, "262144", p.EVMChainID)
	require.Equal(t, "7", p.Nonce)
	require.Equal(t, "build_erc20_transfer", p.Summary.ToolName)

	// SignerAddress is the owner's 0x form, and Data is the packed transfer.
	wantFrom, err := ownerEthAddress(testTxOwner)
	require.NoError(t, err)
	require.Equal(t, wantFrom.Hex(), p.SignerAddress)
	require.Equal(t, common.HexToAddress("0x"+p.Data[10:74]), to) // arg word 1 = recipient
}
