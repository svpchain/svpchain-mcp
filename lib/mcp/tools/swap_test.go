package tools

import (
	"math/big"
	"strings"
	"testing"

	"github.com/ethereum/go-ethereum/common"
	"github.com/stretchr/testify/require"
)

var (
	swapWSVP   = common.HexToAddress("0x2222222222222222222222222222222222222222")
	swapTokenA = common.HexToAddress("0xAAaAAAaaAAAAAAAaaAAAAaAAaAAaaAaaAaAAAAaa")
	swapTokenB = common.HexToAddress("0xBbBBBbbBbBbBBbBbBBBBBBbBbBbBBBBBbbBBBbbB")
)

func TestParseSwapToken_Native(t *testing.T) {
	for _, in := range []string{"", "native", "SVP", "svp", " native ", "0x0000000000000000000000000000000000000000"} {
		addr, native, err := parseSwapToken(in)
		require.NoError(t, err, "input %q", in)
		require.True(t, native, "input %q should be native", in)
		require.Equal(t, common.Address{}, addr)
	}
}

func TestParseSwapToken_ERC20(t *testing.T) {
	addr, native, err := parseSwapToken(swapTokenA.Hex())
	require.NoError(t, err)
	require.False(t, native)
	require.Equal(t, swapTokenA, addr)
}

func TestParseSwapToken_KnownSymbol(t *testing.T) {
	cases := map[string]common.Address{
		"usdv": common.HexToAddress("0x013a61E622e6ABFCaB64F52D274C3Fc0aA37f951"),
		"usdc": common.HexToAddress("0x732F6Ea7AfD5EdC02e7ba052075dd0780e285489"),
	}
	for sym, want := range cases {
		for _, in := range []string{sym, strings.ToUpper(sym), " " + sym + " "} {
			addr, native, err := parseSwapToken(in)
			require.NoError(t, err, "input %q", in)
			require.False(t, native, "input %q should resolve to an ERC-20", in)
			require.Equal(t, want, addr, "input %q", in)
		}
	}
}

func TestParseSwapToken_Invalid(t *testing.T) {
	for _, in := range []string{"0x123", "not-an-address", "0xZZZ"} {
		_, _, err := parseSwapToken(in)
		require.Error(t, err, "input %q should be rejected", in)
	}
}

func TestResolveSwapPlan(t *testing.T) {
	t.Run("erc20->erc20", func(t *testing.T) {
		p, err := resolveSwapPlan(swapTokenA, false, swapTokenB, false, swapWSVP)
		require.NoError(t, err)
		require.Equal(t, kindTokensForTokens, p.kind)
		require.Equal(t, []common.Address{swapTokenA, swapTokenB}, p.path)
	})
	t.Run("native->erc20 routes through WSVP", func(t *testing.T) {
		p, err := resolveSwapPlan(common.Address{}, true, swapTokenB, false, swapWSVP)
		require.NoError(t, err)
		require.Equal(t, kindSVPForTokens, p.kind)
		require.Equal(t, []common.Address{swapWSVP, swapTokenB}, p.path)
	})
	t.Run("erc20->native routes through WSVP", func(t *testing.T) {
		p, err := resolveSwapPlan(swapTokenA, false, common.Address{}, true, swapWSVP)
		require.NoError(t, err)
		require.Equal(t, kindTokensForSVP, p.kind)
		require.Equal(t, []common.Address{swapTokenA, swapWSVP}, p.path)
	})
	t.Run("native->native rejected", func(t *testing.T) {
		_, err := resolveSwapPlan(common.Address{}, true, common.Address{}, true, swapWSVP)
		require.Error(t, err)
	})
	t.Run("same token rejected", func(t *testing.T) {
		_, err := resolveSwapPlan(swapTokenA, false, swapTokenA, false, swapWSVP)
		require.Error(t, err)
	})
}

func TestApplySlippage(t *testing.T) {
	out := big.NewInt(1000)

	min, err := applySlippage(out, 50) // 0.5%
	require.NoError(t, err)
	require.Equal(t, "995", min.String())

	min, err = applySlippage(out, 0) // no slippage
	require.NoError(t, err)
	require.Equal(t, "1000", min.String())

	min, err = applySlippage(out, 10000-1) // 99.99%
	require.NoError(t, err)
	require.Equal(t, "0", min.String())

	_, err = applySlippage(out, 10000) // 100% is rejected (would allow zero-out)
	require.Error(t, err)
	_, err = applySlippage(out, -1)
	require.Error(t, err)
}

func TestTokenLabel(t *testing.T) {
	require.Equal(t, "native", tokenLabel(true, common.Address{}))
	require.Equal(t, swapTokenA.Hex(), tokenLabel(false, swapTokenA))
	// A known alias renders as its upper-cased symbol, not the raw address.
	usdv := common.HexToAddress("0x013a61E622e6ABFCaB64F52D274C3Fc0aA37f951")
	require.Equal(t, "USDV", tokenLabel(false, usdv))
	usdc := common.HexToAddress("0x732F6Ea7AfD5EdC02e7ba052075dd0780e285489")
	require.Equal(t, "USDC", tokenLabel(false, usdc))
}

func TestAddrsToHex(t *testing.T) {
	require.Equal(t,
		[]string{swapTokenA.Hex(), swapWSVP.Hex()},
		addrsToHex([]common.Address{swapTokenA, swapWSVP}),
	)
}
