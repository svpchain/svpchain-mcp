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

// packReturn ABI-encodes return values against a solidity type list, to feed the
// Unpack* decoders exactly what the contracts would return.
func packReturn(t *testing.T, types []string, vals ...any) []byte {
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

var (
	lendComptroller = common.HexToAddress("0x1111111111111111111111111111111111111111")
	lendCToken      = common.HexToAddress("0x2222222222222222222222222222222222222222")
	lendUnderlying  = common.HexToAddress("0x3333333333333333333333333333333333333333")
)

// sel4 returns the canonical 4-byte selector (keccak256 of the signature) as
// lower-case hex, so tests assert Pack output against the signature independently
// of the embedded ABI JSON (catching a typo in that JSON).
func sel4(sig string) string {
	return hex4(crypto.Keccak256([]byte(sig))[:4])
}

func newLendora(t *testing.T) *builder.Lendora {
	t.Helper()
	l, err := builder.NewLendora(lendComptroller)
	require.NoError(t, err)
	require.Equal(t, lendComptroller, l.Comptroller())
	return l
}

func TestLendora_CTokenSelectors(t *testing.T) {
	l := newLendora(t)

	mint, err := l.PackMint(big.NewInt(1_000_000))
	require.NoError(t, err)
	require.Equal(t, sel4("mint(uint256)"), hex4(mint))
	require.Len(t, mint, 4+32)
	require.Equal(t, big.NewInt(1_000_000), new(big.Int).SetBytes(mint[4:4+32]))

	redeem, err := l.PackRedeem(big.NewInt(42))
	require.NoError(t, err)
	require.Equal(t, sel4("redeem(uint256)"), hex4(redeem))

	redeemU, err := l.PackRedeemUnderlying(big.NewInt(43))
	require.NoError(t, err)
	require.Equal(t, sel4("redeemUnderlying(uint256)"), hex4(redeemU))

	borrow, err := l.PackBorrow(big.NewInt(44))
	require.NoError(t, err)
	require.Equal(t, sel4("borrow(uint256)"), hex4(borrow))

	repay, err := l.PackRepayBorrow(big.NewInt(45))
	require.NoError(t, err)
	require.Equal(t, sel4("repayBorrow(uint256)"), hex4(repay))
}

func TestLendora_ComptrollerSelectors(t *testing.T) {
	l := newLendora(t)

	enter, err := l.PackEnterMarkets([]common.Address{lendCToken})
	require.NoError(t, err)
	require.Equal(t, sel4("enterMarkets(address[])"), hex4(enter))

	exit, err := l.PackExitMarket(lendCToken)
	require.NoError(t, err)
	require.Equal(t, sel4("exitMarket(address)"), hex4(exit))
	require.Equal(t, lendCToken, common.BytesToAddress(exit[4:4+32]))
}

func TestLendora_UnderlyingRoundTrip(t *testing.T) {
	l := newLendora(t)

	data, err := l.PackUnderlying()
	require.NoError(t, err)
	require.Equal(t, sel4("underlying()"), hex4(data))

	// Encode an on-chain underlying() return (a left-padded address word) and
	// decode it back.
	out := common.LeftPadBytes(lendUnderlying.Bytes(), 32)
	got, err := l.UnpackUnderlying(out)
	require.NoError(t, err)
	require.Equal(t, lendUnderlying, got)
}

func TestLendora_ReadSelectors(t *testing.T) {
	l := newLendora(t)
	cases := []struct {
		sig  string
		pack func() ([]byte, error)
	}{
		{"exchangeRateStored()", l.PackExchangeRateStored},
		{"supplyRatePerBlock()", l.PackSupplyRatePerBlock},
		{"borrowRatePerBlock()", l.PackBorrowRatePerBlock},
		{"getCash()", l.PackGetCash},
		{"totalBorrows()", l.PackTotalBorrows},
		{"totalReserves()", l.PackTotalReserves},
		{"reserveFactorMantissa()", l.PackReserveFactorMantissa},
		{"borrowCaps(address)", func() ([]byte, error) { return l.PackBorrowCaps(lendCToken) }},
		{"interestRateModel()", l.PackInterestRateModel},
		{"symbol()", l.PackSymbol},
		{"getAllMarkets()", l.PackGetAllMarkets},
		{"closeFactorMantissa()", l.PackCloseFactorMantissa},
		{"liquidationIncentiveMantissa()", l.PackLiquidationIncentiveMantissa},
		{"oracle()", l.PackOracle},
		{"cEtherAddress()", l.PackCEtherAddress},
		{"blocksPerYear()", l.PackBlocksPerYear},
	}
	for _, c := range cases {
		data, err := c.pack()
		require.NoError(t, err, c.sig)
		require.Equal(t, sel4(c.sig), hex4(data), c.sig)
	}

	// arg-taking reads
	acc, err := l.PackGetAccountSnapshot(lendCToken)
	require.NoError(t, err)
	require.Equal(t, sel4("getAccountSnapshot(address)"), hex4(acc))

	mk, err := l.PackMarkets(lendCToken)
	require.NoError(t, err)
	require.Equal(t, sel4("markets(address)"), hex4(mk))

	al, err := l.PackGetAccountLiquidity(lendCToken)
	require.NoError(t, err)
	require.Equal(t, sel4("getAccountLiquidity(address)"), hex4(al))

	hyp, err := l.PackGetHypotheticalAccountLiquidity(lendCToken, lendCToken, big.NewInt(1), big.NewInt(2))
	require.NoError(t, err)
	require.Equal(t, sel4("getHypotheticalAccountLiquidity(address,address,uint256,uint256)"), hex4(hyp))

	price, err := l.PackGetUnderlyingPrice(lendCToken)
	require.NoError(t, err)
	require.Equal(t, sel4("getUnderlyingPrice(address)"), hex4(price))

	feed, err := l.PackCTokenToFeed(lendCToken)
	require.NoError(t, err)
	require.Equal(t, sel4("cTokenToFeed(address)"), hex4(feed))
}

func TestLendora_ReadDecoders(t *testing.T) {
	l := newLendora(t)

	// markets() 3-tuple
	mk := packReturn(t, []string{"bool", "uint256", "bool"}, true, big.NewInt(800), false)
	info, err := l.UnpackMarkets(mk)
	require.NoError(t, err)
	require.True(t, info.IsListed)
	require.Equal(t, big.NewInt(800), info.CollateralFactorMantissa)

	// getAccountSnapshot 4-tuple (error 0)
	snap := packReturn(t, []string{"uint256", "uint256", "uint256", "uint256"},
		big.NewInt(0), big.NewInt(11), big.NewInt(22), big.NewInt(33))
	as, err := l.UnpackGetAccountSnapshot(snap)
	require.NoError(t, err)
	require.Equal(t, big.NewInt(11), as.CTokenBalance)
	require.Equal(t, big.NewInt(22), as.BorrowBalance)
	require.Equal(t, big.NewInt(33), as.ExchangeRate)

	// nonzero error code rejected
	bad := packReturn(t, []string{"uint256", "uint256", "uint256", "uint256"},
		big.NewInt(9), big.NewInt(0), big.NewInt(0), big.NewInt(0))
	_, err = l.UnpackGetAccountSnapshot(bad)
	require.Error(t, err)

	// getAccountLiquidity 3-tuple
	liq := packReturn(t, []string{"uint256", "uint256", "uint256"}, big.NewInt(0), big.NewInt(100), big.NewInt(0))
	al, err := l.UnpackAccountLiquidity("getAccountLiquidity", liq)
	require.NoError(t, err)
	require.Equal(t, big.NewInt(100), al.Liquidity)
	require.Equal(t, 0, al.Shortfall.Sign())

	// getAllMarkets address[]
	arr := packReturn(t, []string{"address[]"}, []common.Address{lendCToken, lendUnderlying})
	addrs, err := l.UnpackGetAllMarkets(arr)
	require.NoError(t, err)
	require.Equal(t, []common.Address{lendCToken, lendUnderlying}, addrs)

	// blocksPerYear uint256
	bpy := packReturn(t, []string{"uint256"}, big.NewInt(31_536_000))
	v, err := l.UnpackBlocksPerYear(bpy)
	require.NoError(t, err)
	require.Equal(t, big.NewInt(31_536_000), v)
}
