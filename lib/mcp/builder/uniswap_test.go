package builder_test

import (
	"math/big"
	"testing"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/require"

	"github.com/svpchain/svpchain-mcp/lib/mcp/builder"
)

// encodeUint256Slice ABI-encodes a uint256[] exactly as a node would return it
// from getAmountsOut, so UnpackAmounts can be tested against real wire bytes.
func encodeUint256Slice(t *testing.T, vals []*big.Int) []byte {
	t.Helper()
	typ, err := abi.NewType("uint256[]", "", nil)
	require.NoError(t, err)
	args := abi.Arguments{{Type: typ}}
	out, err := args.Pack(vals)
	require.NoError(t, err)
	return out
}

var (
	testRouter = common.HexToAddress("0x1111111111111111111111111111111111111111")
	testWSVP   = common.HexToAddress("0x2222222222222222222222222222222222222222")
	tokenA     = common.HexToAddress("0xAAaAAAaaAAAAAAAaaAAAAaAAaAAaaAaaAaAAAAaa")
	tokenB     = common.HexToAddress("0xBbBBBbbBbBbBBbBbBBBBBBbBbBbBBBBBbbBBBbbB")
)

func newUni(t *testing.T) *builder.UniswapV2 {
	t.Helper()
	u, err := builder.NewUniswapV2(testRouter, testWSVP)
	require.NoError(t, err)
	return u
}

// selector is the first 4 bytes of keccak256(signature) — the on-chain method
// id a packed call must carry. We assert the renamed *SVP* functions compute
// the selectors of their exact deployed signatures, so an accidental "ETH"
// (the upstream name) would fail loudly instead of producing calldata no pair
// answers.
func selector(sig string) []byte { return crypto.Keccak256([]byte(sig))[:4] }

func TestUniswapV2_SwapSelectorsMatchDeployedNames(t *testing.T) {
	u := newUni(t)
	out := big.NewInt(1)
	in := big.NewInt(2)
	deadline := big.NewInt(3)
	to := tokenA
	path := []common.Address{tokenA, tokenB}

	cases := []struct {
		name string
		sig  string
		pack func() ([]byte, error)
	}{
		{
			"swapExactTokensForTokens",
			"swapExactTokensForTokens(uint256,uint256,address[],address,uint256)",
			func() ([]byte, error) { return u.PackSwapExactTokensForTokens(in, out, path, to, deadline) },
		},
		{
			"swapExactSVPForTokens",
			"swapExactSVPForTokens(uint256,address[],address,uint256)",
			func() ([]byte, error) { return u.PackSwapExactSVPForTokens(out, path, to, deadline) },
		},
		{
			"swapExactTokensForSVP",
			"swapExactTokensForSVP(uint256,uint256,address[],address,uint256)",
			func() ([]byte, error) { return u.PackSwapExactTokensForSVP(in, out, path, to, deadline) },
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := tc.pack()
			require.NoError(t, err)
			require.GreaterOrEqual(t, len(data), 4)
			require.Equal(t, selector(tc.sig), data[:4], "selector mismatch for %s", tc.sig)
		})
	}
}

func TestUniswapV2_ERC20Selectors(t *testing.T) {
	u := newUni(t)

	approve, err := u.PackApprove(testRouter, big.NewInt(5))
	require.NoError(t, err)
	require.Equal(t, selector("approve(address,uint256)"), approve[:4])

	allowance, err := u.PackAllowance(tokenA, testRouter)
	require.NoError(t, err)
	require.Equal(t, selector("allowance(address,address)"), allowance[:4])

	decimals, err := u.PackDecimals()
	require.NoError(t, err)
	require.Equal(t, selector("decimals()"), decimals[:4])

	balanceOf, err := u.PackBalanceOf(tokenA)
	require.NoError(t, err)
	require.Equal(t, selector("balanceOf(address)"), balanceOf[:4])
}

func TestUniswapV2_UnpackBalanceOf(t *testing.T) {
	u := newUni(t)
	data := common.LeftPadBytes(big.NewInt(2_500_000).Bytes(), 32)
	bal, err := u.UnpackBalanceOf(data)
	require.NoError(t, err)
	require.Equal(t, big.NewInt(2_500_000), bal)
}

// TestUniswapV2_UnpackAmounts roundtrips a uint256[] through the router ABI's
// getAmountsOut output encoding, proving UnpackAmounts decodes exactly what the
// node would return.
func TestUniswapV2_UnpackAmounts(t *testing.T) {
	u := newUni(t)
	want := []*big.Int{big.NewInt(1000), big.NewInt(996)}

	encoded := encodeUint256Slice(t, want)
	got, err := u.UnpackAmounts(encoded)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, want[0], got[0])
	require.Equal(t, want[1], got[1])
}

func TestUniswapV2_UnpackAllowanceAndDecimals(t *testing.T) {
	u := newUni(t)

	allowanceData := common.LeftPadBytes(big.NewInt(12345).Bytes(), 32)
	a, err := u.UnpackAllowance(allowanceData)
	require.NoError(t, err)
	require.Equal(t, big.NewInt(12345), a)

	decData := common.LeftPadBytes(big.NewInt(6).Bytes(), 32)
	d, err := u.UnpackDecimals(decData)
	require.NoError(t, err)
	require.Equal(t, uint8(6), d)
}

func TestUniswapV2_RouterAndWSVP(t *testing.T) {
	u := newUni(t)
	require.Equal(t, testRouter, u.Router())
	require.Equal(t, testWSVP, u.WSVP())
}
