package tools

import (
	"math/big"
	"strings"

	"cosmossdk.io/math"
)

// lendora_format.go holds the number formatters the lendora_* read/simulation
// output needs and the codebase does not already provide: USD with a "$" and
// thousands separators, a percentage, and the Compound per-block-rate → APY
// conversion. Everything is computed on math.LegacyDec (the precision-safe type
// used across the tools package) — never float64. Token amounts still use
// humanAmount (account.go); these cover the derived USD/percent/APY values.

// mantissaScale is 1e18 — the fixed-point scale of every Compound "mantissa"
// (rates, exchange rate, collateral factor).
var mantissaScale = int64(18)

// decFromBaseUnits converts an integer base-unit amount to a decimal in whole
// units for the given token decimals (e.g. 1_000_000 at 6 dp → 1.0).
func decFromBaseUnits(amount *big.Int, decimals int64) math.LegacyDec {
	if amount == nil {
		return math.LegacyZeroDec()
	}
	return math.LegacyNewDecFromBigIntWithPrec(new(big.Int).Set(amount), decimals)
}

// decFromMantissa converts a 1e18-scaled mantissa (rate, factor, price ratio) to
// its decimal value (e.g. 9e17 → 0.9).
func decFromMantissa(m *big.Int) math.LegacyDec {
	return decFromBaseUnits(m, mantissaScale)
}

// usdFromFeed values a base-unit token amount in USD given the token's decimals
// and a Chainlink USD feed answer scaled by feedDecimals (read from the feed, not
// assumed). Returns whole-dollar LegacyDec.
func usdFromFeed(amount *big.Int, tokenDecimals int64, feedAnswer *big.Int, feedDecimals int64) math.LegacyDec {
	amt := decFromBaseUnits(amount, tokenDecimals)
	price := decFromBaseUnits(feedAnswer, feedDecimals)
	return amt.Mul(price)
}

// apyFromRatePerBlock annualizes a Compound per-block rate mantissa into an APY
// fraction using the Compound frontend formula (daily compounding):
//
//	apy = (1 + ratePerBlock * blocksPerDay)^365 − 1
//
// where ratePerBlock = mantissa/1e18 and blocksPerDay = blocksPerYear/365.
// blocksPerYear is read per-market from the interest rate model. Returns a
// fraction (0.0523 = 5.23% APY); render with formatPercent.
func apyFromRatePerBlock(ratePerBlockMantissa, blocksPerYear *big.Int) math.LegacyDec {
	if ratePerBlockMantissa == nil || ratePerBlockMantissa.Sign() == 0 ||
		blocksPerYear == nil || blocksPerYear.Sign() == 0 {
		return math.LegacyZeroDec()
	}
	ratePerBlock := decFromMantissa(ratePerBlockMantissa)
	blocksPerDay := math.LegacyNewDecFromBigInt(new(big.Int).Set(blocksPerYear)).QuoInt64(365)
	dailyRate := ratePerBlock.Mul(blocksPerDay)
	base := math.LegacyOneDec().Add(dailyRate)
	return base.Power(365).Sub(math.LegacyOneDec())
}

// formatUSD renders a whole-dollar LegacyDec as "$1,234.56" (or "-$1,234.56"),
// rounding to cents with thousands separators.
func formatUSD(d math.LegacyDec) string {
	neg := d.IsNegative()
	cents := d.Abs().MulInt64(100).RoundInt() // math.Int, in cents
	s := cents.String()
	for len(s) < 3 {
		s = "0" + s
	}
	intPart := groupThousands(s[:len(s)-2])
	frac := s[len(s)-2:]
	out := "$" + intPart + "." + frac
	if neg {
		out = "-" + out
	}
	return out
}

// formatPercent renders a fraction (0.1234) as "12.34%", rounded to 2 dp.
func formatPercent(fraction math.LegacyDec) string {
	pct := fraction.MulInt64(100)
	neg := pct.IsNegative()
	hundredths := pct.Abs().MulInt64(100).RoundInt() // 2-dp precision
	s := hundredths.String()
	for len(s) < 3 {
		s = "0" + s
	}
	out := s[:len(s)-2] + "." + s[len(s)-2:] + "%"
	if neg {
		out = "-" + out
	}
	return out
}

// groupThousands inserts commas every three digits into a non-negative integer
// string (e.g. "1234567" → "1,234,567").
func groupThousands(intDigits string) string {
	n := len(intDigits)
	if n <= 3 {
		return intDigits
	}
	var b strings.Builder
	lead := n % 3
	if lead == 0 {
		lead = 3
	}
	b.WriteString(intDigits[:lead])
	for i := lead; i < n; i += 3 {
		b.WriteByte(',')
		b.WriteString(intDigits[i : i+3])
	}
	return b.String()
}
