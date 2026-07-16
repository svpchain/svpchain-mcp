package tools

import (
	"math/big"
	"testing"

	"cosmossdk.io/math"
	"github.com/stretchr/testify/require"
)

func TestFormatUSD(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"0", "$0.00"},
		{"1", "$1.00"},
		{"1200.5", "$1,200.50"},
		{"1234567.891", "$1,234,567.89"}, // rounds to cents
		{"-42.5", "-$42.50"},
		{"1000000", "$1,000,000.00"},
	}
	for _, c := range cases {
		require.Equal(t, c.want, formatUSD(math.LegacyMustNewDecFromStr(c.in)), c.in)
	}
}

func TestFormatPercent(t *testing.T) {
	require.Equal(t, "12.34%", formatPercent(math.LegacyMustNewDecFromStr("0.1234")))
	require.Equal(t, "0.00%", formatPercent(math.LegacyZeroDec()))
	require.Equal(t, "100.00%", formatPercent(math.LegacyOneDec()))
	require.Equal(t, "5.23%", formatPercent(math.LegacyMustNewDecFromStr("0.0523")))
}

func TestGroupThousands(t *testing.T) {
	require.Equal(t, "0", groupThousands("0"))
	require.Equal(t, "123", groupThousands("123"))
	require.Equal(t, "1,234", groupThousands("1234"))
	require.Equal(t, "12,345", groupThousands("12345"))
	require.Equal(t, "1,234,567", groupThousands("1234567"))
}

func TestApyFromRatePerBlock(t *testing.T) {
	// Zero rate → zero APY.
	require.True(t, apyFromRatePerBlock(big.NewInt(0), big.NewInt(31_536_000)).IsZero())

	// A positive per-block rate yields a positive, bounded APY.
	apy := apyFromRatePerBlock(big.NewInt(1_000_000_000), big.NewInt(31_536_000))
	require.True(t, apy.IsPositive())
	require.True(t, apy.LT(math.LegacyOneDec()), "expected a sane sub-100%% APY, got %s", apy)
}

func TestUsdFromFeed(t *testing.T) {
	// 100 USDC (6-dec) at $1.00 (8-dec feed) = $100.
	got := usdFromFeed(big.NewInt(100_000_000), 6, big.NewInt(100_000_000), 8)
	require.Equal(t, "$100.00", formatUSD(got))
	// Same price on an 18-dec feed must yield the same USD.
	got18 := usdFromFeed(big.NewInt(100_000_000), 6, mustBig("1000000000000000000"), 18)
	require.Equal(t, "$100.00", formatUSD(got18))
}
