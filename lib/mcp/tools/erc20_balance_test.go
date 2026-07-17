package tools

import (
	"bytes"
	"context"
	"fmt"
	"math/big"
	"testing"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/require"

	"github.com/svpchain/svpchain-mcp/lib/mcp/builder"
)

// mockBalanceEVM is a chain.EVMClient stub for the erc20Balances path: only
// CallContract is meaningful, dispatching on the 4-byte selector to return a
// balanceOf / decimals result (or a forced error).
type mockBalanceEVM struct {
	balance  *big.Int
	decimals uint8
	callErr  error
}

func (m *mockBalanceEVM) CallContract(_ context.Context, msg ethereum.CallMsg) ([]byte, error) {
	if m.callErr != nil {
		return nil, m.callErr
	}
	switch {
	case bytes.Equal(msg.Data[:4], crypto.Keccak256([]byte("balanceOf(address)"))[:4]):
		return common.LeftPadBytes(m.balance.Bytes(), 32), nil
	case bytes.Equal(msg.Data[:4], crypto.Keccak256([]byte("decimals()"))[:4]):
		return common.LeftPadBytes(big.NewInt(int64(m.decimals)).Bytes(), 32), nil
	}
	return nil, fmt.Errorf("unexpected call %x", msg.Data[:4])
}

func (m *mockBalanceEVM) PendingNonceAt(context.Context, common.Address) (uint64, error) {
	return 0, nil
}
func (m *mockBalanceEVM) EstimateGas(context.Context, ethereum.CallMsg) (uint64, error) {
	return 0, nil
}
func (m *mockBalanceEVM) SuggestGasTipCap(context.Context) (*big.Int, error) { return nil, nil }
func (m *mockBalanceEVM) BaseFee(context.Context) (*big.Int, error)          { return nil, nil }
func (m *mockBalanceEVM) ChainID(context.Context) (*big.Int, error)          { return nil, nil }
func (m *mockBalanceEVM) BlockNumber(context.Context) (uint64, error)        { return 0, nil }
func (m *mockBalanceEVM) SendTransaction(context.Context, *ethtypes.Transaction) (string, error) {
	return "", nil
}
func (m *mockBalanceEVM) TransactionReceipt(context.Context, common.Hash) (*ethtypes.Receipt, error) {
	return nil, nil
}

func handlersWithEVM(t *testing.T, evm *mockBalanceEVM) *Handlers {
	t.Helper()
	uni, err := builder.NewUniswapV2(
		common.HexToAddress("0x1111111111111111111111111111111111111111"),
		common.HexToAddress("0x2222222222222222222222222222222222222222"),
	)
	require.NoError(t, err)
	return &Handlers{Deps: Deps{
		Chain: ChainDeps{EVM: evm},
		EVM:   EVMDeps{Uniswap: uni},
	}}
}

func TestErc20Balances_KnownTokenAppears(t *testing.T) {
	// The mock returns a positive balance for ANY balanceOf, so if the
	// bank-linked USDC weren't skipped it would also appear (len 2). Only the
	// pure-ERC-20 USDV is contract-read here.
	h := handlersWithEVM(t, &mockBalanceEVM{balance: big.NewInt(2_500_000), decimals: 6})

	got := h.erc20Balances(context.Background(), testTxOwner)
	require.Len(t, got, 1)
	usdv := got[0]
	require.Equal(t, "USDV", usdv.Symbol)
	require.Equal(t, "erc20", usdv.Source)
	require.Equal(t, "2500000", usdv.Amount)
	require.Equal(t, "2.5", usdv.Display) // 2_500_000 at 6 dp
	// Denom is the checksummed contract address.
	require.Equal(t, common.HexToAddress("0x013a61E622e6ABFCaB64F52D274C3Fc0aA37f951").Hex(), usdv.Denom)
}

func TestErc20Balances_BankLinkedTokenExcluded(t *testing.T) {
	// USDC is bank-linked: get_balance surfaces it via the x/bank read, so
	// erc20Balances must never contract-read it (that would double-count).
	h := handlersWithEVM(t, &mockBalanceEVM{balance: big.NewInt(2_500_000), decimals: 6})
	usdc := common.HexToAddress("0x732F6Ea7AfD5EdC02e7ba052075dd0780e285489").Hex()
	for _, b := range h.erc20Balances(context.Background(), testTxOwner) {
		require.NotEqual(t, "USDC", b.Symbol)
		require.NotEqual(t, usdc, b.Denom)
	}
}

func TestErc20Balances_ZeroBalanceOmitted(t *testing.T) {
	h := handlersWithEVM(t, &mockBalanceEVM{balance: big.NewInt(0), decimals: 6})
	require.Empty(t, h.erc20Balances(context.Background(), testTxOwner))
}

func TestErc20Balances_ReadErrorSkipped(t *testing.T) {
	// A failed contract read must not surface — get_balance still returns bank
	// balances. erc20Balances swallows it and yields no ERC-20 entry.
	h := handlersWithEVM(t, &mockBalanceEVM{callErr: fmt.Errorf("rpc down")})
	require.Empty(t, h.erc20Balances(context.Background(), testTxOwner))
}

func TestErc20Balances_EVMDisabledReturnsNil(t *testing.T) {
	// No EVM client wired (non-EVM deployment): no ERC-20 balances, no panic.
	h := &Handlers{Deps: Deps{}}
	require.Nil(t, h.erc20Balances(context.Background(), testTxOwner))
}
