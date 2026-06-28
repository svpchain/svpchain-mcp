package tools

import (
	"bytes"
	"context"
	"math/big"
	"path/filepath"
	"testing"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/require"

	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/bridge"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/builder"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/policy"
)

// mockForeignEVM is a chain.EVMClient standing in for a foreign chain's RPC:
// CallContract answers allowance() with a fixed value; the chain-state calls
// return fixed values (ChainID is the foreign chain id) so a real EVMAssembler
// can stamp a payload bound to that chain.
type mockForeignEVM struct {
	chainID   int64
	allowance *big.Int
}

func (m *mockForeignEVM) CallContract(_ context.Context, msg ethereum.CallMsg) ([]byte, error) {
	if bytes.Equal(msg.Data[:4], crypto.Keccak256([]byte("allowance(address,address)"))[:4]) {
		a := m.allowance
		if a == nil {
			a = big.NewInt(0)
		}
		return common.LeftPadBytes(a.Bytes(), 32), nil
	}
	return nil, nil
}
func (m *mockForeignEVM) PendingNonceAt(context.Context, common.Address) (uint64, error) {
	return 3, nil
}
func (m *mockForeignEVM) EstimateGas(context.Context, ethereum.CallMsg) (uint64, error) {
	return 80_000, nil
}
func (m *mockForeignEVM) SuggestGasTipCap(context.Context) (*big.Int, error) {
	return big.NewInt(1_000_000_000), nil
}
func (m *mockForeignEVM) BaseFee(context.Context) (*big.Int, error) {
	return big.NewInt(2_000_000_000), nil
}
func (m *mockForeignEVM) ChainID(context.Context) (*big.Int, error) {
	return big.NewInt(m.chainID), nil
}
func (m *mockForeignEVM) SendTransaction(context.Context, *ethtypes.Transaction) (string, error) {
	return "", nil
}
func (m *mockForeignEVM) TransactionReceipt(context.Context, common.Hash) (*ethtypes.Receipt, error) {
	return nil, nil
}

const (
	inboundHomeChainID = uint64(2517)
	arbBridgeAddr      = "0x1111111111111111111111111111111111111111"
	sepBridgeAddr      = "0x2222222222222222222222222222222222222222"
)

// inboundHandlers wires a *Handlers authorized for the inbound bridge, with the
// shared route registry and a foreign-chain bundle per (chainID -> mock client).
func inboundHandlers(t *testing.T, mocks map[uint64]*mockForeignEVM, bridgeAddrs map[uint64]string) (*Handlers, context.Context) {
	t.Helper()
	const tenantID = "t1"
	reg, err := bridge.LoadRegistry(filepath.Join("testdata", "inbound_routes.json"))
	require.NoError(t, err)

	foreigns := map[uint64]*ForeignChain{}
	for id, m := range mocks {
		br, err := builder.NewBridge(common.HexToAddress(bridgeAddrs[id]))
		require.NoError(t, err)
		foreigns[id] = &ForeignChain{
			Client:    m,
			Assembler: builder.NewEVMAssembler(m),
			Bridge:    br,
		}
	}
	h := &Handlers{Deps: Deps{
		EVM: EVMDeps{
			BridgeRoutes:  reg,
			HomeChainID:   inboundHomeChainID,
			ForeignChains: foreigns,
		},
		Policy:    policy.NewEngine([]policy.TenantPolicy{{TenantID: tenantID, Owner: testTxOwner}}),
		RateLimit: policy.NewRateLimiter(0, 0),
	}}
	ctx := WithTenant(context.Background(), TenantContext{TenantID: tenantID, Owner: testTxOwner})
	return h, ctx
}

func TestBuildBridgeDepositInbound_ERC20(t *testing.T) {
	// arbitrum_sepolia -> svpchain SVP is an ERC-20 source with ample allowance.
	mocks := map[uint64]*mockForeignEVM{
		421614: {chainID: 421614, allowance: new(big.Int).Exp(big.NewInt(10), big.NewInt(30), nil)},
	}
	h, ctx := inboundHandlers(t, mocks, map[uint64]string{421614: arbBridgeAddr})

	_, out, err := h.BuildBridgeDepositInbound(ctx, nil, BuildBridgeDepositInboundInput{
		SourceChain: "arbitrum_sepolia",
		Token:       "SVP",
		Amount:      "1.5",
		ClientID:    "cid-1",
	})
	require.NoError(t, err)

	require.Equal(t, uint64(421614), out.SourceChainID)
	require.Equal(t, inboundHomeChainID, out.DestChainID)
	require.Equal(t, "SVP", out.Symbol)
	require.Equal(t, "native", out.TargetToken) // releases native SVP on svpchain
	require.Equal(t, "1500000000000000000", out.AmountBase)

	// Payload is stamped with the FOREIGN chain id and targets the foreign bridge.
	require.Equal(t, "421614", out.Payload.EVMChainID)
	require.Equal(t, common.HexToAddress(arbBridgeAddr).Hex(), out.Payload.To)
	require.Equal(t, "0", out.Payload.Value) // ERC-20 deposit carries no value
	require.Equal(t, "3", out.Payload.Nonce)

	// Recipient defaults to the owner's own 0x address on svpchain.
	wantFrom, err := ownerEthAddress(testTxOwner)
	require.NoError(t, err)
	require.Equal(t, wantFrom.Hex(), out.Recipient)
}

func TestBuildBridgeDepositInbound_AllowanceShort(t *testing.T) {
	mocks := map[uint64]*mockForeignEVM{
		421614: {chainID: 421614, allowance: big.NewInt(0)},
	}
	h, ctx := inboundHandlers(t, mocks, map[uint64]string{421614: arbBridgeAddr})

	_, _, err := h.BuildBridgeDepositInbound(ctx, nil, BuildBridgeDepositInboundInput{
		SourceChain: "421614",
		Token:       "SVP",
		Amount:      "1.0",
		ClientID:    "cid-2",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "build_erc20_approve")
	// The spender called out is the FOREIGN bridge, not the home one.
	require.Contains(t, err.Error(), common.HexToAddress(arbBridgeAddr).Hex())
}

func TestBuildBridgeDepositInbound_Native(t *testing.T) {
	// sepolia -> svpchain native (ETH source) rides as the tx value, no allowance.
	mocks := map[uint64]*mockForeignEVM{
		11155111: {chainID: 11155111},
	}
	h, ctx := inboundHandlers(t, mocks, map[uint64]string{11155111: sepBridgeAddr})

	_, out, err := h.BuildBridgeDepositInbound(ctx, nil, BuildBridgeDepositInboundInput{
		SourceChain: "sepolia",
		Token:       "native",
		Amount:      "2",
		Recipient:   "0x3333333333333333333333333333333333333333",
		ClientID:    "cid-3",
	})
	require.NoError(t, err)

	require.Equal(t, uint64(11155111), out.SourceChainID)
	require.Equal(t, "native", out.SourceToken)
	require.Equal(t, "11155111", out.Payload.EVMChainID)
	require.Equal(t, "2000000000000000000", out.Payload.Value) // value-carried
	require.Equal(t, common.HexToAddress(sepBridgeAddr).Hex(), out.Payload.To)
	require.Equal(t, "0x3333333333333333333333333333333333333333", out.Recipient)
}

func TestBuildBridgeDepositInbound_NotConfigured(t *testing.T) {
	// Bridge routes present but no foreign chains wired.
	reg, err := bridge.LoadRegistry(filepath.Join("testdata", "inbound_routes.json"))
	require.NoError(t, err)
	h := &Handlers{Deps: Deps{
		EVM:       EVMDeps{BridgeRoutes: reg, HomeChainID: inboundHomeChainID},
		Policy:    policy.NewEngine([]policy.TenantPolicy{{TenantID: "t1", Owner: testTxOwner}}),
		RateLimit: policy.NewRateLimiter(0, 0),
	}}
	ctx := WithTenant(context.Background(), TenantContext{TenantID: "t1", Owner: testTxOwner})
	_, _, err = h.BuildBridgeDepositInbound(ctx, nil, BuildBridgeDepositInboundInput{
		SourceChain: "arbitrum_sepolia", Token: "SVP", Amount: "1", ClientID: "c",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "inbound bridging is not enabled")
}

func TestBuildBridgeDepositInbound_UnknownSource(t *testing.T) {
	mocks := map[uint64]*mockForeignEVM{
		421614: {chainID: 421614, allowance: big.NewInt(0)},
	}
	h, ctx := inboundHandlers(t, mocks, map[uint64]string{421614: arbBridgeAddr})

	// "optimism" is not in the registry at all → unknown source chain.
	_, _, err := h.BuildBridgeDepositInbound(ctx, nil, BuildBridgeDepositInboundInput{
		SourceChain: "optimism", Token: "SVP", Amount: "1", ClientID: "c",
	})
	require.Error(t, err)
	require.Contains(t, err.Error(), "unknown source chain")
}
