package bridge_test

import (
	"path/filepath"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/require"

	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/bridge"
)

const svpChainID = uint64(2517)

func load(t *testing.T) *bridge.Registry {
	t.Helper()
	reg, err := bridge.LoadRegistry(filepath.Join("testdata", "routes.json"))
	require.NoError(t, err)
	return reg
}

func TestLoadRegistry_BadPath(t *testing.T) {
	_, err := bridge.LoadRegistry(filepath.Join("testdata", "does-not-exist.json"))
	require.Error(t, err)
}

func TestResolveDestChain(t *testing.T) {
	reg := load(t)

	// by name
	id, err := reg.ResolveDestChain("sepolia", svpChainID)
	require.NoError(t, err)
	require.Equal(t, uint64(11155111), id)

	// by numeric id
	id, err = reg.ResolveDestChain("421614", svpChainID)
	require.NoError(t, err)
	require.Equal(t, uint64(421614), id)

	// unknown chain
	_, err = reg.ResolveDestChain("optimism", svpChainID)
	require.ErrorContains(t, err, "unknown destination chain")

	// self
	_, err = reg.ResolveDestChain("2517", svpChainID)
	require.ErrorContains(t, err, "source chain")

	// a numeric id that parses but has no route from svpchain → empty-route guard.
	_, err = reg.ResolveDestChain("999999", svpChainID)
	require.ErrorContains(t, err, "no bridge route")
}

func TestLookup_Symbol(t *testing.T) {
	reg := load(t)
	dest, err := reg.ResolveDestChain("sepolia", svpChainID)
	require.NoError(t, err)

	rt, err := reg.Lookup(svpChainID, dest, "usdc") // case-insensitive
	require.NoError(t, err)
	require.Equal(t, "USDC", rt.Symbol)
	require.Equal(t, int64(6), rt.Decimals)
	require.False(t, rt.NativeSource())
	require.Equal(t, common.HexToAddress("0x732f6ea7afd5edc02e7ba052075dd0780e285489"), rt.SrcToken)
	require.Equal(t, common.HexToAddress("0x7af80a20da5a4000175eb8babcab73da6ed01f9d"), rt.TargetToken)
}

func TestLookup_Native(t *testing.T) {
	reg := load(t)
	dest, err := reg.ResolveDestChain("sepolia", svpChainID)
	require.NoError(t, err)

	for _, q := range []string{"", "native", "svp", "0x0000000000000000000000000000000000000000"} {
		rt, err := reg.Lookup(svpChainID, dest, q)
		require.NoErrorf(t, err, "query %q", q)
		require.True(t, rt.NativeSource(), "query %q should resolve native", q)
		require.Equal(t, "SVP", rt.Symbol)
		require.Equal(t, common.HexToAddress("0x16B065D7519D5C1c53eff6ed5AE732E90d602A00"), rt.TargetToken)
	}
}

func TestLookup_ByAddress(t *testing.T) {
	reg := load(t)
	dest, err := reg.ResolveDestChain("sepolia", svpChainID)
	require.NoError(t, err)

	rt, err := reg.Lookup(svpChainID, dest, "0x1c12dbda863900c680a3836c53d408feaf63f0ba")
	require.NoError(t, err)
	require.Equal(t, "WETH", rt.Symbol)
	require.True(t, rt.TargetToken == (common.Address{}), "WETH releases native on sepolia")
}

func TestLookup_Unknown(t *testing.T) {
	reg := load(t)
	dest, err := reg.ResolveDestChain("sepolia", svpChainID)
	require.NoError(t, err)

	_, err = reg.Lookup(svpChainID, dest, "DOGE")
	require.ErrorContains(t, err, "not bridgeable")
}

func TestAvailableTargets(t *testing.T) {
	reg := load(t)
	targets := reg.AvailableTargets(svpChainID)
	require.Len(t, targets, 2)
	// sorted ascending by id: arbitrum_sepolia (421614) < sepolia (11155111).
	require.Equal(t, uint64(421614), targets[0].ID)
	require.Equal(t, uint64(11155111), targets[1].ID)
	require.True(t, reg.HasSource(svpChainID))
	require.False(t, reg.HasSource(999999))
}
