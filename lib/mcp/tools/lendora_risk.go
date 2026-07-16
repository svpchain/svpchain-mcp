package tools

import (
	"context"
	"math/big"

	"cosmossdk.io/math"
	"github.com/ethereum/go-ethereum/common"
)

// lendora_risk.go is the Lendora risk engine shared by lendora_assess_risk,
// lendora_get_account_summary, and the operation simulations. It reads an
// account's per-market snapshots and prices via eth_call and derives an exact,
// numeraire-independent health factor plus USD-valued totals.
//
// The health factor is computed the same way the Comptroller does, but split so
// we recover the ratio (not just the margin the chain returns):
//   - net = liquidity − shortfall             (from getAccountLiquidity, native 1e18)
//   - sumBorrow = Σ price_i · borrowBal_i / 1e18   (native 1e18)
//   - sumCollateral = net + sumBorrow
//   - HF = sumCollateral / sumBorrow           (native prices cancel; exact)
// USD display uses each market's per-asset Chainlink feed (underlyingUsdPrice).

// e18 is 1e18, the shared Compound mantissa scale.
var e18 = new(big.Int).Exp(big.NewInt(10), big.NewInt(18), nil)

// Risk bands by health factor (only meaningful when the account has debt).
const (
	hfHighUpper   = "1.2" // HF < 1.2 → High
	hfMediumUpper = "1.5" // HF < 1.5 → Medium
)

// marketPosition is one market's contribution to an account's position.
type marketPosition struct {
	Symbol             string
	CToken             common.Address
	UnderlyingDecimals int64
	CollateralEnabled  bool
	SuppliedUnderlying *big.Int // underlying base units
	BorrowedUnderlying *big.Int // underlying base units
	PriceNative        *big.Int // getUnderlyingPrice, 1e(36-underlyingDec)
	CollateralFactor   *big.Int // mantissa 1e18
	SuppliedUSD        math.LegacyDec
	BorrowedUSD        math.LegacyDec
	FeedOK             bool
}

// riskResult is the full analysed position for an account.
type riskResult struct {
	BlockNumber      uint64
	Positions        []marketPosition
	HasDebt          bool
	HealthFactor     math.LegacyDec // valid only when HasDebt
	SumCollateralNat *big.Int       // native 1e18
	SumBorrowNat     *big.Int       // native 1e18
	LiquidityNat     *big.Int       // native 1e18 (borrowing power remaining)
	ShortfallNat     *big.Int       // native 1e18 (> 0 ⇒ underwater)
	TotalSuppliedUSD math.LegacyDec
	TotalBorrowedUSD math.LegacyDec
	MaxBorrowableUSD math.LegacyDec // liquidity converted to USD, when a native feed resolves
	MaxBorrowableOK  bool
	RiskLevel        string // "Low" | "Medium" | "High" | "Critical"
	RiskEmoji        string // 🟢 | 🟡 | 🟠 | 🔴
}

// computeRisk analyses owner's Lendora position: per-market supplied/borrowed
// (underlying + USD), the exact health factor, and the risk band. Requires
// requireLendora to have passed.
func (h *Handlers) computeRisk(ctx context.Context, owner common.Address) (riskResult, error) {
	lend := h.Deps.EVM.Lendora
	res := riskResult{
		BlockNumber:      h.evmBlockNumber(ctx),
		HealthFactor:     math.LegacyZeroDec(),
		SumCollateralNat: big.NewInt(0),
		SumBorrowNat:     big.NewInt(0),
		TotalSuppliedUSD: math.LegacyZeroDec(),
		TotalBorrowedUSD: math.LegacyZeroDec(),
		MaxBorrowableUSD: math.LegacyZeroDec(),
	}

	oracle, err := h.lendoraOracle(ctx)
	if err != nil {
		return riskResult{}, err
	}

	// Account margin (native 1e18) straight from the Comptroller.
	liqData, err := lend.PackGetAccountLiquidity(owner)
	if err != nil {
		return riskResult{}, err
	}
	liqOut, err := h.evmCall(ctx, lend.Comptroller(), liqData)
	if err != nil {
		return riskResult{}, err
	}
	liq, err := lend.UnpackAccountLiquidity("getAccountLiquidity", liqOut)
	if err != nil {
		return riskResult{}, err
	}
	res.LiquidityNat = liq.Liquidity
	res.ShortfallNat = liq.Shortfall

	// Which markets the account has entered as collateral.
	entered, err := h.lendoraAssetsInSet(ctx, owner)
	if err != nil {
		return riskResult{}, err
	}

	for _, m := range h.Deps.LendoraMarkets.All() {
		snapData, err := lend.PackGetAccountSnapshot(owner)
		if err != nil {
			return riskResult{}, err
		}
		snapOut, err := h.evmCall(ctx, m.CToken, snapData)
		if err != nil {
			return riskResult{}, err
		}
		snap, err := lend.UnpackGetAccountSnapshot(snapOut)
		if err != nil {
			return riskResult{}, err
		}
		if snap.CTokenBalance.Sign() == 0 && snap.BorrowBalance.Sign() == 0 {
			continue // no position in this market
		}

		// supplied underlying = cTokenBalance * exchangeRate / 1e18
		suppliedUnderlying := new(big.Int).Mul(snap.CTokenBalance, snap.ExchangeRate)
		suppliedUnderlying.Div(suppliedUnderlying, e18)
		borrowedUnderlying := snap.BorrowBalance

		price, err := h.cTokenUnderlyingPriceNative(ctx, oracle, m.CToken)
		if err != nil {
			return riskResult{}, err
		}
		// native borrow value = price * borrowBalance / 1e18
		if borrowedUnderlying.Sign() > 0 {
			bv := new(big.Int).Mul(price, borrowedUnderlying)
			bv.Div(bv, e18)
			res.SumBorrowNat.Add(res.SumBorrowNat, bv)
		}

		cf, err := h.cTokenCollateralFactor(ctx, m.CToken)
		if err != nil {
			return riskResult{}, err
		}

		pos := marketPosition{
			Symbol:             m.Symbol,
			CToken:             m.CToken,
			UnderlyingDecimals: m.UnderlyingDecimals,
			CollateralEnabled:  entered[m.CToken],
			SuppliedUnderlying: suppliedUnderlying,
			BorrowedUnderlying: borrowedUnderlying,
			PriceNative:        price,
			CollateralFactor:   cf,
			SuppliedUSD:        math.LegacyZeroDec(),
			BorrowedUSD:        math.LegacyZeroDec(),
		}
		if answer, feedDec, ok, err := h.underlyingUsdPrice(ctx, oracle, m.CToken); err != nil {
			return riskResult{}, err
		} else if ok {
			pos.FeedOK = true
			pos.SuppliedUSD = usdFromFeed(suppliedUnderlying, m.UnderlyingDecimals, answer, feedDec)
			pos.BorrowedUSD = usdFromFeed(borrowedUnderlying, m.UnderlyingDecimals, answer, feedDec)
			res.TotalSuppliedUSD = res.TotalSuppliedUSD.Add(pos.SuppliedUSD)
			res.TotalBorrowedUSD = res.TotalBorrowedUSD.Add(pos.BorrowedUSD)
		}
		res.Positions = append(res.Positions, pos)
	}

	// sumCollateral = (liquidity − shortfall) + sumBorrow
	net := new(big.Int).Sub(res.LiquidityNat, res.ShortfallNat)
	res.SumCollateralNat = new(big.Int).Add(net, res.SumBorrowNat)
	res.HasDebt = res.SumBorrowNat.Sign() > 0
	if res.HasDebt {
		res.HealthFactor = math.LegacyNewDecFromBigInt(res.SumCollateralNat).
			Quo(math.LegacyNewDecFromBigInt(res.SumBorrowNat))
	}
	res.RiskLevel, res.RiskEmoji = riskBand(res.HasDebt, res.ShortfallNat, res.HealthFactor)

	// Max borrowable = remaining liquidity, valued in USD via the native SVP feed.
	if usd, ok, err := h.nativeLiquidityUSD(ctx, oracle, res.LiquidityNat); err != nil {
		return riskResult{}, err
	} else if ok {
		res.MaxBorrowableUSD = usd
		res.MaxBorrowableOK = true
	}

	return res, nil
}

// riskBand maps a health factor to a level + emoji. Shortfall > 0 (HF < 1) is the
// definitive underwater signal; with no debt the account is unconditionally safe.
func riskBand(hasDebt bool, shortfall *big.Int, hf math.LegacyDec) (string, string) {
	if !hasDebt {
		return "Low", "🟢"
	}
	if shortfall != nil && shortfall.Sign() > 0 {
		return "Critical", "🔴"
	}
	high := math.LegacyMustNewDecFromStr(hfHighUpper)
	medium := math.LegacyMustNewDecFromStr(hfMediumUpper)
	switch {
	case hf.LT(high):
		return "High", "🟠"
	case hf.LT(medium):
		return "Medium", "🟡"
	default:
		return "Low", "🟢"
	}
}

// lendoraAssetsInSet reads getAssetsIn(owner) into a set for membership tests.
func (h *Handlers) lendoraAssetsInSet(ctx context.Context, owner common.Address) (map[common.Address]bool, error) {
	data, err := h.Deps.EVM.Lendora.PackGetAssetsIn(owner)
	if err != nil {
		return nil, err
	}
	out, err := h.evmCall(ctx, h.Deps.EVM.Lendora.Comptroller(), data)
	if err != nil {
		return nil, err
	}
	addrs, err := h.Deps.EVM.Lendora.UnpackGetAssetsIn(out)
	if err != nil {
		return nil, err
	}
	set := make(map[common.Address]bool, len(addrs))
	for _, a := range addrs {
		set[a] = true
	}
	return set, nil
}

// cTokenUnderlyingPriceNative reads getUnderlyingPrice(cToken) (native 1e(36-dec)).
func (h *Handlers) cTokenUnderlyingPriceNative(ctx context.Context, oracle, cToken common.Address) (*big.Int, error) {
	data, err := h.Deps.EVM.Lendora.PackGetUnderlyingPrice(cToken)
	if err != nil {
		return nil, err
	}
	out, err := h.evmCall(ctx, oracle, data)
	if err != nil {
		return nil, err
	}
	return h.Deps.EVM.Lendora.UnpackOracleUint("getUnderlyingPrice", out)
}

// cTokenCollateralFactor reads markets(cToken).collateralFactorMantissa (1e18).
func (h *Handlers) cTokenCollateralFactor(ctx context.Context, cToken common.Address) (*big.Int, error) {
	data, err := h.Deps.EVM.Lendora.PackMarkets(cToken)
	if err != nil {
		return nil, err
	}
	out, err := h.evmCall(ctx, h.Deps.EVM.Lendora.Comptroller(), data)
	if err != nil {
		return nil, err
	}
	info, err := h.Deps.EVM.Lendora.UnpackMarkets(out)
	if err != nil {
		return nil, err
	}
	return info.CollateralFactorMantissa, nil
}

// nativeLiquidityUSD values a native-1e18 amount (e.g. account liquidity) in USD
// via the native (cSVP) market's Chainlink feed. ok=false when no native market
// or feed resolves, so callers can omit the USD figure.
func (h *Handlers) nativeLiquidityUSD(ctx context.Context, oracle common.Address, liquidityNat *big.Int) (math.LegacyDec, bool, error) {
	var cEther common.Address
	for _, m := range h.Deps.LendoraMarkets.All() {
		if m.IsCEther {
			cEther = m.CToken
			break
		}
	}
	if cEther == (common.Address{}) {
		return math.LegacyZeroDec(), false, nil
	}
	answer, feedDec, ok, err := h.underlyingUsdPrice(ctx, oracle, cEther)
	if err != nil || !ok {
		return math.LegacyZeroDec(), false, err
	}
	// liquidityNat is native-SVP scaled 1e18; value it like an 18-dec token amount.
	return usdFromFeed(liquidityNat, 18, answer, feedDec), true, nil
}
