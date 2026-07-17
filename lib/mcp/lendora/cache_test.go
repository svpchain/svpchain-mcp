package lendora_test

import (
	"context"
	"math/big"
	"strings"
	"testing"

	"cosmossdk.io/log"
	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/require"

	"github.com/svpchain/svpchain-mcp/lib/mcp/builder"
	"github.com/svpchain/svpchain-mcp/lib/mcp/lendora"
)

var (
	comp   = common.HexToAddress("0x00000000000000000000000000000000000000c0")
	oracle = common.HexToAddress("0x000000000000000000000000000000000000000a")
	cUSDC  = common.HexToAddress("0x00000000000000000000000000000000000000c1")
	usdc   = common.HexToAddress("0x0000000000000000000000000000000000000011")
)

func sel(sig string) string { return common.Bytes2Hex(crypto.Keccak256([]byte(sig))[:4]) }

func pack(t *testing.T, types []string, vals ...any) []byte {
	t.Helper()
	var args abi.Arguments
	for _, ts := range types {
		ty, err := abi.NewType(ts, "", nil)
		require.NoError(t, err)
		args = append(args, abi.Argument{Type: ty})
	}
	b, err := args.Pack(vals...)
	require.NoError(t, err)
	return b
}

// mockEVM answers the reads a cache Refresh makes for one cUSDC market.
type mockEVM struct{ ret map[string][]byte }

func newMock(t *testing.T) *mockEVM {
	m := &mockEVM{ret: map[string][]byte{}}
	put := func(to common.Address, sig string, b []byte) {
		m.ret[strings.ToLower(to.Hex())+":"+sel(sig)] = b
	}
	put(comp, "oracle()", pack(t, []string{"address"}, oracle))
	put(comp, "getAllMarkets()", pack(t, []string{"address[]"}, []common.Address{cUSDC}))
	put(oracle, "cEtherAddress()", pack(t, []string{"address"}, common.Address{}))
	put(cUSDC, "decimals()", pack(t, []string{"uint8"}, uint8(8)))
	put(cUSDC, "symbol()", pack(t, []string{"string"}, "cUSDC"))
	put(cUSDC, "underlying()", pack(t, []string{"address"}, usdc))
	put(usdc, "decimals()", pack(t, []string{"uint8"}, uint8(6)))
	put(usdc, "symbol()", pack(t, []string{"string"}, "USDC"))
	return m
}

func (m *mockEVM) CallContract(_ context.Context, msg ethereum.CallMsg) ([]byte, error) {
	k := strings.ToLower(msg.To.Hex()) + ":" + common.Bytes2Hex(msg.Data[:4])
	if v, ok := m.ret[k]; ok {
		return v, nil
	}
	return nil, nil
}
func (m *mockEVM) PendingNonceAt(context.Context, common.Address) (uint64, error) { return 0, nil }
func (m *mockEVM) EstimateGas(context.Context, ethereum.CallMsg) (uint64, error)  { return 0, nil }
func (m *mockEVM) SuggestGasTipCap(context.Context) (*big.Int, error)             { return big.NewInt(0), nil }
func (m *mockEVM) BaseFee(context.Context) (*big.Int, error)                      { return big.NewInt(0), nil }
func (m *mockEVM) ChainID(context.Context) (*big.Int, error)                      { return big.NewInt(1), nil }
func (m *mockEVM) BlockNumber(context.Context) (uint64, error)                    { return 0, nil }
func (m *mockEVM) SendTransaction(context.Context, *ethtypes.Transaction) (string, error) {
	return "", nil
}
func (m *mockEVM) TransactionReceipt(context.Context, common.Hash) (*ethtypes.Receipt, error) {
	return nil, nil
}

func TestCache_RefreshAndResolve(t *testing.T) {
	lend, err := builder.NewLendora(comp)
	require.NoError(t, err)
	c := lendora.NewCache(newMock(t), lend, 0, log.NewNopLogger())
	require.NoError(t, c.Refresh(context.Background()))
	require.Equal(t, 1, c.Size())

	// resolvable by symbol (case-insensitive) and by both addresses
	for _, asset := range []string{"USDC", "usdc", cUSDC.Hex(), usdc.Hex()} {
		m, ok := c.Resolve(asset)
		require.True(t, ok, asset)
		require.Equal(t, "USDC", m.Symbol)
		require.Equal(t, cUSDC, m.CToken)
		require.Equal(t, usdc, m.Underlying)
		require.Equal(t, int64(8), m.CTokenDecimals)
		require.Equal(t, int64(6), m.UnderlyingDecimals)
		require.False(t, m.IsCEther)
	}

	_, ok := c.Resolve("WETH")
	require.False(t, ok)

	addr, ok := c.Oracle()
	require.True(t, ok)
	require.Equal(t, oracle, addr)
}
