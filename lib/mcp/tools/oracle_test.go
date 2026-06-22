package tools

import (
	"bytes"
	"context"
	"math/big"
	"testing"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/require"

	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/builder"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/policy"
)

// mockOracleEVM is a chain.EVMClient whose CallContract answers the three
// aggregator getters (description / decimals / latestRoundData) with ABI-encoded
// bytes keyed by 4-byte selector; the chain-state methods are unused by the
// read-only get_oracle_price path.
type mockOracleEVM struct {
	decimals  uint8
	desc      string
	roundID   *big.Int
	answer    *big.Int
	updatedAt *big.Int
}

func sel(sig string) []byte { return crypto.Keccak256([]byte(sig))[:4] }

func (m *mockOracleEVM) CallContract(_ context.Context, msg ethereum.CallMsg) ([]byte, error) {
	switch {
	case bytes.Equal(msg.Data[:4], sel("decimals()")):
		return common.LeftPadBytes(big.NewInt(int64(m.decimals)).Bytes(), 32), nil
	case bytes.Equal(msg.Data[:4], sel("description()")):
		typ, _ := abi.NewType("string", "", nil)
		out, _ := abi.Arguments{{Type: typ}}.Pack(m.desc)
		return out, nil
	case bytes.Equal(msg.Data[:4], sel("latestRoundData()")):
		u80, _ := abi.NewType("uint80", "", nil)
		i256, _ := abi.NewType("int256", "", nil)
		u256, _ := abi.NewType("uint256", "", nil)
		args := abi.Arguments{{Type: u80}, {Type: i256}, {Type: u256}, {Type: u256}, {Type: u80}}
		out, _ := args.Pack(m.roundID, m.answer, big.NewInt(0), m.updatedAt, m.roundID)
		return out, nil
	}
	return nil, nil
}
func (m *mockOracleEVM) PendingNonceAt(context.Context, common.Address) (uint64, error) {
	return 0, nil
}
func (m *mockOracleEVM) EstimateGas(context.Context, ethereum.CallMsg) (uint64, error) { return 0, nil }
func (m *mockOracleEVM) SuggestGasTipCap(context.Context) (*big.Int, error)            { return nil, nil }
func (m *mockOracleEVM) BaseFee(context.Context) (*big.Int, error)                     { return nil, nil }
func (m *mockOracleEVM) ChainID(context.Context) (*big.Int, error)                     { return nil, nil }
func (m *mockOracleEVM) SendTransaction(context.Context, *ethtypes.Transaction) (string, error) {
	return "", nil
}
func (m *mockOracleEVM) TransactionReceipt(context.Context, common.Hash) (*ethtypes.Receipt, error) {
	return nil, nil
}

// oracleHandlers wires a *Handlers with a tenant authorized for the oracle read,
// plus the bound feed (when feed != nil) over the given mock EVM client.
func oracleHandlers(t *testing.T, evm *mockOracleEVM, feed *builder.OracleFeed) (*Handlers, context.Context) {
	t.Helper()
	const tenantID = "t1"
	engine := policy.NewEngine([]policy.TenantPolicy{{TenantID: tenantID, Owner: testTxOwner}})
	h := &Handlers{Deps: Deps{
		Chain:     ChainDeps{EVM: evm},
		EVM:       EVMDeps{Oracle: feed},
		Policy:    engine,
		RateLimit: policy.NewRateLimiter(0, 0),
	}}
	ctx := WithTenant(context.Background(), TenantContext{TenantID: tenantID, Owner: testTxOwner})
	return h, ctx
}

func TestGetOraclePrice(t *testing.T) {
	feed, err := builder.NewOracleFeed(common.HexToAddress("0xAE351F2dF66DF1A7d2eB0D7574BcDb909E680B56"))
	require.NoError(t, err)
	evm := &mockOracleEVM{
		decimals:  8,
		desc:      "BTC / USD",
		roundID:   big.NewInt(42),
		answer:    big.NewInt(6_523_412_000_000), // 65234.12 at 8 decimals
		updatedAt: big.NewInt(1_700_000_000),
	}
	h, ctx := oracleHandlers(t, evm, feed)

	_, out, err := h.GetOraclePrice(ctx, nil, GetOraclePriceInput{})
	require.NoError(t, err)
	require.Equal(t, "0xAE351F2dF66DF1A7d2eB0D7574BcDb909E680B56", out.Oracle)
	require.Equal(t, "BTC / USD", out.Description)
	require.Equal(t, int64(8), out.Decimals)
	require.Equal(t, "65234.12", out.Price)
	require.Equal(t, "6523412000000", out.PriceRaw)
	require.Equal(t, "42", out.RoundID)
	require.Equal(t, int64(1_700_000_000), out.UpdatedAt)
}

func TestGetOraclePrice_NotConfigured(t *testing.T) {
	// EVM client present but no oracle feed bound (evm_oracle_addr unset).
	h, ctx := oracleHandlers(t, &mockOracleEVM{}, nil)
	_, _, err := h.GetOraclePrice(ctx, nil, GetOraclePriceInput{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "evm_oracle_addr")

	// EVM disabled entirely.
	h2 := &Handlers{Deps: Deps{
		Policy:    policy.NewEngine([]policy.TenantPolicy{{TenantID: "t1", Owner: testTxOwner}}),
		RateLimit: policy.NewRateLimiter(0, 0),
	}}
	ctx2 := WithTenant(context.Background(), TenantContext{TenantID: "t1", Owner: testTxOwner})
	_, _, err = h2.GetOraclePrice(ctx2, nil, GetOraclePriceInput{})
	require.Error(t, err)
	require.Contains(t, err.Error(), "EVM is not enabled")
}
