package tools

import (
	"context"
	"fmt"
	"math/big"
	"sort"

	"cosmossdk.io/math"
	"github.com/ethereum/go-ethereum/common"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/svpchain/svpchain-mcp/lib/mcp/lendora"
)

// lendora_read.go holds the seven read-only lendora_* query tools: market data
// (get_all_markets, get_market_details, get_protocol_dashboard), account views
// (get_account_summary, get_account_positions, get_balances), and the risk
// report (assess_risk). All are pure eth_call reads — no tx, no signing. USD
// figures come from each market's per-asset Chainlink feed; the health factor is
// the exact ratio from the risk engine (lendora_risk.go).

// -- shared market stats ------------------------------------------------

// marketStats is the per-market read set the market tools surface.
type marketStats struct {
	Symbol                string
	CToken                common.Address
	Underlying            common.Address
	UnderlyingDecimals    int64
	IsCEther              bool
	SupplyAPY             math.LegacyDec
	BorrowAPY             math.LegacyDec
	TotalSupplyUnderlying *big.Int
	TotalBorrowUnderlying *big.Int
	CashUnderlying        *big.Int
	ReservesUnderlying    *big.Int
	Utilization           math.LegacyDec
	CollateralFactor      math.LegacyDec
	ReserveFactor         math.LegacyDec
	MintPaused            bool
	BorrowPaused          bool
	FeedOK                bool
	SupplyUSD             math.LegacyDec
	BorrowUSD             math.LegacyDec
}

// loadMarketStats reads one market's rates, balances, risk params, and USD value.
func (h *Handlers) loadMarketStats(ctx context.Context, oracle common.Address, m lendora.Market) (marketStats, error) {
	lend := h.Deps.EVM.Lendora
	st := marketStats{
		Symbol:             m.Symbol,
		CToken:             m.CToken,
		Underlying:         m.Underlying,
		UnderlyingDecimals: m.UnderlyingDecimals,
		IsCEther:           m.IsCEther,
		SupplyAPY:          math.LegacyZeroDec(),
		BorrowAPY:          math.LegacyZeroDec(),
		Utilization:        math.LegacyZeroDec(),
		CollateralFactor:   math.LegacyZeroDec(),
		ReserveFactor:      math.LegacyZeroDec(),
		SupplyUSD:          math.LegacyZeroDec(),
		BorrowUSD:          math.LegacyZeroDec(),
	}

	cash, err := h.cTokenUint(ctx, m.CToken, lend.PackGetCash, "getCash")
	if err != nil {
		return marketStats{}, err
	}
	borrows, err := h.cTokenUint(ctx, m.CToken, lend.PackTotalBorrows, "totalBorrows")
	if err != nil {
		return marketStats{}, err
	}
	reserves, err := h.cTokenUint(ctx, m.CToken, lend.PackTotalReserves, "totalReserves")
	if err != nil {
		return marketStats{}, err
	}
	st.CashUnderlying, st.TotalBorrowUnderlying, st.ReservesUnderlying = cash, borrows, reserves
	// total supplied underlying = cash + borrows − reserves
	supply := new(big.Int).Add(cash, borrows)
	supply.Sub(supply, reserves)
	if supply.Sign() < 0 {
		supply = big.NewInt(0)
	}
	st.TotalSupplyUnderlying = supply
	// utilization = borrows / (cash + borrows − reserves)
	if supply.Sign() > 0 {
		st.Utilization = math.LegacyNewDecFromBigInt(borrows).Quo(math.LegacyNewDecFromBigInt(supply))
	}

	supplyRate, err := h.cTokenUint(ctx, m.CToken, lend.PackSupplyRatePerBlock, "supplyRatePerBlock")
	if err != nil {
		return marketStats{}, err
	}
	borrowRate, err := h.cTokenUint(ctx, m.CToken, lend.PackBorrowRatePerBlock, "borrowRatePerBlock")
	if err != nil {
		return marketStats{}, err
	}
	blocksPerYear, err := h.marketBlocksPerYear(ctx, m.CToken)
	if err != nil {
		return marketStats{}, err
	}
	st.SupplyAPY = apyFromRatePerBlock(supplyRate, blocksPerYear)
	st.BorrowAPY = apyFromRatePerBlock(borrowRate, blocksPerYear)

	cf, err := h.cTokenCollateralFactor(ctx, m.CToken)
	if err != nil {
		return marketStats{}, err
	}
	st.CollateralFactor = decFromMantissa(cf)
	if rf, err := h.cTokenUint(ctx, m.CToken, lend.PackReserveFactorMantissa, "reserveFactorMantissa"); err == nil {
		st.ReserveFactor = decFromMantissa(rf)
	}

	st.MintPaused, _ = h.comptrollerBool(ctx, lend.PackMintGuardianPaused, "mintGuardianPaused", m.CToken)
	st.BorrowPaused, _ = h.comptrollerBool(ctx, lend.PackBorrowGuardianPaused, "borrowGuardianPaused", m.CToken)

	if answer, feedDec, ok, err := h.underlyingUsdPrice(ctx, oracle, m.CToken); err != nil {
		return marketStats{}, err
	} else if ok {
		st.FeedOK = true
		st.SupplyUSD = usdFromFeed(supply, m.UnderlyingDecimals, answer, feedDec)
		st.BorrowUSD = usdFromFeed(borrows, m.UnderlyingDecimals, answer, feedDec)
	}
	return st, nil
}

// marketUSD reads only the TVL inputs (cash/borrows/reserves + the USD feed) a
// market contributes to the protocol dashboard — a light alternative to
// loadMarketStats, which also reads rates/pauses/factors the dashboard ignores.
func (h *Handlers) marketUSD(ctx context.Context, oracle common.Address, m lendora.Market) (supplyUSD, borrowUSD math.LegacyDec, err error) {
	lend := h.Deps.EVM.Lendora
	cash, err := h.cTokenUint(ctx, m.CToken, lend.PackGetCash, "getCash")
	if err != nil {
		return math.LegacyDec{}, math.LegacyDec{}, err
	}
	borrows, err := h.cTokenUint(ctx, m.CToken, lend.PackTotalBorrows, "totalBorrows")
	if err != nil {
		return math.LegacyDec{}, math.LegacyDec{}, err
	}
	reserves, err := h.cTokenUint(ctx, m.CToken, lend.PackTotalReserves, "totalReserves")
	if err != nil {
		return math.LegacyDec{}, math.LegacyDec{}, err
	}
	supply := new(big.Int).Sub(new(big.Int).Add(cash, borrows), reserves)
	if supply.Sign() < 0 {
		supply = big.NewInt(0)
	}
	supplyUSD, borrowUSD = math.LegacyZeroDec(), math.LegacyZeroDec()
	if answer, feedDec, ok, err := h.underlyingUsdPrice(ctx, oracle, m.CToken); err != nil {
		return math.LegacyDec{}, math.LegacyDec{}, err
	} else if ok {
		supplyUSD = usdFromFeed(supply, m.UnderlyingDecimals, answer, feedDec)
		borrowUSD = usdFromFeed(borrows, m.UnderlyingDecimals, answer, feedDec)
	}
	return supplyUSD, borrowUSD, nil
}

// marketBlocksPerYear reads the market's interest rate model then its
// blocksPerYear constant (the APY annualization factor).
func (h *Handlers) marketBlocksPerYear(ctx context.Context, cToken common.Address) (*big.Int, error) {
	lend := h.Deps.EVM.Lendora
	modelData, err := lend.PackInterestRateModel()
	if err != nil {
		return nil, err
	}
	modelOut, err := h.evmCall(ctx, cToken, modelData)
	if err != nil {
		return nil, fmt.Errorf("read interestRateModel for %s: %w", cToken.Hex(), err)
	}
	model, err := lend.UnpackInterestRateModel(modelOut)
	if err != nil {
		return nil, err
	}
	bpyData, err := lend.PackBlocksPerYear()
	if err != nil {
		return nil, err
	}
	bpyOut, err := h.evmCall(ctx, model, bpyData)
	if err != nil {
		return nil, fmt.Errorf("read blocksPerYear for model %s: %w", model.Hex(), err)
	}
	return lend.UnpackBlocksPerYear(bpyOut)
}

// comptrollerBool reads a per-market Comptroller bool flag (mint/borrow paused).
func (h *Handlers) comptrollerBool(ctx context.Context, pack func(common.Address) ([]byte, error), method string, cToken common.Address) (bool, error) {
	data, err := pack(cToken)
	if err != nil {
		return false, err
	}
	out, err := h.evmCall(ctx, h.Deps.EVM.Lendora.Comptroller(), data)
	if err != nil {
		return false, err
	}
	return h.Deps.EVM.Lendora.UnpackComptrollerBool(method, out)
}

// -- DTOs ---------------------------------------------------------------

// MarketDTO is one market row in get_all_markets / get_market_details.
type MarketDTO struct {
	Symbol           string `json:"symbol"`
	CToken           string `json:"ctoken"`
	Underlying       string `json:"underlying"`
	SupplyAPY        string `json:"supply_apy"`        // e.g. "5.23%"
	BorrowAPY        string `json:"borrow_apy"`        // e.g. "8.10%"
	TotalSupply      string `json:"total_supply"`      // underlying, human units
	TotalBorrow      string `json:"total_borrow"`      // underlying, human units
	TotalSupplyUSD   string `json:"total_supply_usd"`  // "$…" or "" when no feed
	TotalBorrowUSD   string `json:"total_borrow_usd"`  // "$…" or ""
	Utilization      string `json:"utilization"`       // e.g. "62.00%"
	CollateralFactor string `json:"collateral_factor"` // e.g. "80.00%"
	MintPaused       bool   `json:"mint_paused"`
	BorrowPaused     bool   `json:"borrow_paused"`
	IsNative         bool   `json:"is_native"` // the native (cSVP) market; underlying is the zero address
}

func (h *Handlers) marketDTO(st marketStats) MarketDTO {
	dto := MarketDTO{
		Symbol:           st.Symbol,
		CToken:           st.CToken.Hex(),
		Underlying:       st.Underlying.Hex(),
		SupplyAPY:        formatPercent(st.SupplyAPY),
		BorrowAPY:        formatPercent(st.BorrowAPY),
		TotalSupply:      humanAmount(math.NewIntFromBigInt(st.TotalSupplyUnderlying), st.UnderlyingDecimals),
		TotalBorrow:      humanAmount(math.NewIntFromBigInt(st.TotalBorrowUnderlying), st.UnderlyingDecimals),
		Utilization:      formatPercent(st.Utilization),
		CollateralFactor: formatPercent(st.CollateralFactor),
		MintPaused:       st.MintPaused,
		BorrowPaused:     st.BorrowPaused,
		IsNative:         st.IsCEther,
	}
	if st.FeedOK {
		dto.TotalSupplyUSD = formatUSD(st.SupplyUSD)
		dto.TotalBorrowUSD = formatUSD(st.BorrowUSD)
	}
	return dto
}

// -- lendora_get_all_markets -------------------------------------------

type LendoraGetAllMarketsInput struct{}

type LendoraGetAllMarketsOutput struct {
	BlockNumber uint64      `json:"block_number"`
	Markets     []MarketDTO `json:"markets"`
}

// LendoraGetAllMarkets lists every Lendora market with supply/borrow APY, TVL
// (USD when a feed is configured), utilization, collateral factor, and pause
// flags. Read-only.
func (h *Handlers) LendoraGetAllMarkets(
	ctx context.Context, _ *mcp.CallToolRequest, _ LendoraGetAllMarketsInput,
) (*mcp.CallToolResult, LendoraGetAllMarketsOutput, error) {
	if _, err := h.authorize(ctx, "lendora_get_all_markets"); err != nil {
		return nil, LendoraGetAllMarketsOutput{}, err
	}
	if err := h.requireLendora(); err != nil {
		return nil, LendoraGetAllMarketsOutput{}, err
	}
	oracle, err := h.lendoraOracle(ctx)
	if err != nil {
		return nil, LendoraGetAllMarketsOutput{}, err
	}
	// Non-nil so an empty result marshals to [] (a nil slice marshals to null,
	// which fails the tool's "type":"array" output schema).
	out := LendoraGetAllMarketsOutput{BlockNumber: h.evmBlockNumber(ctx), Markets: []MarketDTO{}}
	for _, m := range h.Deps.LendoraMarkets.All() {
		st, err := h.loadMarketStats(ctx, oracle, m)
		if err != nil {
			return nil, LendoraGetAllMarketsOutput{}, err
		}
		out.Markets = append(out.Markets, h.marketDTO(st))
	}
	return nil, out, nil
}

// -- lendora_get_market_details ----------------------------------------

type LendoraGetMarketDetailsInput struct {
	Asset string `json:"asset" jsonschema:"the market's underlying symbol (e.g. \"USDC\") or a market/underlying 0x address"`
}

type LendoraGetMarketDetailsOutput struct {
	BlockNumber          uint64    `json:"block_number"`
	Market               MarketDTO `json:"market"`
	Cash                 string    `json:"cash"`                  // underlying, human units
	Reserves             string    `json:"reserves"`              // underlying, human units
	ReserveFactor        string    `json:"reserve_factor"`        // e.g. "10.00%"
	CloseFactor          string    `json:"close_factor"`          // max fraction of a borrow a liquidator may repay
	LiquidationIncentive string    `json:"liquidation_incentive"` // e.g. "108.00%" (8% bonus)
	BorrowCap            string    `json:"borrow_cap"`            // underlying, human units; "0" = uncapped
}

// LendoraGetMarketDetails returns one market's full stats. Read-only.
func (h *Handlers) LendoraGetMarketDetails(
	ctx context.Context, _ *mcp.CallToolRequest, in LendoraGetMarketDetailsInput,
) (*mcp.CallToolResult, LendoraGetMarketDetailsOutput, error) {
	if _, err := h.authorize(ctx, "lendora_get_market_details"); err != nil {
		return nil, LendoraGetMarketDetailsOutput{}, err
	}
	if err := h.requireLendora(); err != nil {
		return nil, LendoraGetMarketDetailsOutput{}, err
	}
	m, err := h.resolveAsset(ctx, in.Asset)
	if err != nil {
		return nil, LendoraGetMarketDetailsOutput{}, err
	}
	oracle, err := h.lendoraOracle(ctx)
	if err != nil {
		return nil, LendoraGetMarketDetailsOutput{}, err
	}
	st, err := h.loadMarketStats(ctx, oracle, m)
	if err != nil {
		return nil, LendoraGetMarketDetailsOutput{}, err
	}
	lend := h.Deps.EVM.Lendora
	closeFactor, err := h.comptrollerUint(ctx, lend.PackCloseFactorMantissa, "closeFactorMantissa")
	if err != nil {
		return nil, LendoraGetMarketDetailsOutput{}, err
	}
	liqIncentive, err := h.comptrollerUint(ctx, lend.PackLiquidationIncentiveMantissa, "liquidationIncentiveMantissa")
	if err != nil {
		return nil, LendoraGetMarketDetailsOutput{}, err
	}
	capData, err := lend.PackBorrowCaps(m.CToken)
	if err != nil {
		return nil, LendoraGetMarketDetailsOutput{}, err
	}
	capOut, err := h.evmCall(ctx, lend.Comptroller(), capData)
	if err != nil {
		return nil, LendoraGetMarketDetailsOutput{}, err
	}
	borrowCap, err := lend.UnpackComptrollerUint("borrowCaps", capOut)
	if err != nil {
		return nil, LendoraGetMarketDetailsOutput{}, err
	}
	return nil, LendoraGetMarketDetailsOutput{
		BlockNumber:          h.evmBlockNumber(ctx),
		Market:               h.marketDTO(st),
		Cash:                 humanAmount(math.NewIntFromBigInt(st.CashUnderlying), st.UnderlyingDecimals),
		Reserves:             humanAmount(math.NewIntFromBigInt(st.ReservesUnderlying), st.UnderlyingDecimals),
		ReserveFactor:        formatPercent(st.ReserveFactor),
		CloseFactor:          formatPercent(decFromMantissa(closeFactor)),
		LiquidationIncentive: formatPercent(decFromMantissa(liqIncentive)),
		BorrowCap:            humanAmount(math.NewIntFromBigInt(borrowCap), m.UnderlyingDecimals),
	}, nil
}

// comptrollerUint reads a no-arg uint256 Comptroller getter (closeFactor, …).
func (h *Handlers) comptrollerUint(ctx context.Context, pack func() ([]byte, error), method string) (*big.Int, error) {
	data, err := pack()
	if err != nil {
		return nil, err
	}
	out, err := h.evmCall(ctx, h.Deps.EVM.Lendora.Comptroller(), data)
	if err != nil {
		return nil, err
	}
	return h.Deps.EVM.Lendora.UnpackComptrollerUint(method, out)
}

// -- lendora_get_protocol_dashboard ------------------------------------

type LendoraGetProtocolDashboardInput struct{}

type LendoraGetProtocolDashboardOutput struct {
	BlockNumber       uint64 `json:"block_number"`
	MarketCount       int    `json:"market_count"`
	TotalSupplyUSD    string `json:"total_supply_usd"`
	TotalBorrowUSD    string `json:"total_borrow_usd"`
	TotalLiquidityUSD string `json:"total_liquidity_usd"` // supply − borrow
}

// LendoraGetProtocolDashboard aggregates TVL across all markets (USD). Read-only.
func (h *Handlers) LendoraGetProtocolDashboard(
	ctx context.Context, _ *mcp.CallToolRequest, _ LendoraGetProtocolDashboardInput,
) (*mcp.CallToolResult, LendoraGetProtocolDashboardOutput, error) {
	if _, err := h.authorize(ctx, "lendora_get_protocol_dashboard"); err != nil {
		return nil, LendoraGetProtocolDashboardOutput{}, err
	}
	if err := h.requireLendora(); err != nil {
		return nil, LendoraGetProtocolDashboardOutput{}, err
	}
	oracle, err := h.lendoraOracle(ctx)
	if err != nil {
		return nil, LendoraGetProtocolDashboardOutput{}, err
	}
	markets := h.Deps.LendoraMarkets.All()
	supplyUSD, borrowUSD := math.LegacyZeroDec(), math.LegacyZeroDec()
	for _, m := range markets {
		s, b, err := h.marketUSD(ctx, oracle, m)
		if err != nil {
			return nil, LendoraGetProtocolDashboardOutput{}, err
		}
		supplyUSD = supplyUSD.Add(s)
		borrowUSD = borrowUSD.Add(b)
	}
	return nil, LendoraGetProtocolDashboardOutput{
		BlockNumber:       h.evmBlockNumber(ctx),
		MarketCount:       len(markets),
		TotalSupplyUSD:    formatUSD(supplyUSD),
		TotalBorrowUSD:    formatUSD(borrowUSD),
		TotalLiquidityUSD: formatUSD(supplyUSD.Sub(borrowUSD)),
	}, nil
}

// -- account views ------------------------------------------------------

// PositionDTO is one market's account position row.
type PositionDTO struct {
	Symbol            string `json:"symbol"`
	CToken            string `json:"ctoken"`
	Supplied          string `json:"supplied"`     // underlying, human units
	Borrowed          string `json:"borrowed"`     // underlying, human units
	SuppliedUSD       string `json:"supplied_usd"` // "$…" or ""
	BorrowedUSD       string `json:"borrowed_usd"` // "$…" or ""
	CollateralEnabled bool   `json:"collateral_enabled"`
	IsNative          bool   `json:"is_native"` // the native (cSVP) market
}

func positionDTO(p marketPosition) PositionDTO {
	dto := PositionDTO{
		Symbol:            p.Symbol,
		CToken:            p.CToken.Hex(),
		Supplied:          humanAmount(math.NewIntFromBigInt(p.SuppliedUnderlying), p.UnderlyingDecimals),
		Borrowed:          humanAmount(math.NewIntFromBigInt(p.BorrowedUnderlying), p.UnderlyingDecimals),
		CollateralEnabled: p.CollateralEnabled,
		IsNative:          p.IsCEther,
	}
	if p.FeedOK {
		dto.SuppliedUSD = formatUSD(p.SuppliedUSD)
		dto.BorrowedUSD = formatUSD(p.BorrowedUSD)
	}
	return dto
}

// healthFactorString renders the HF as a 2-dp string, or "∞" when there is no debt.
func healthFactorString(r riskResult) string {
	if !r.HasDebt {
		return "∞"
	}
	return r.HealthFactor.String()
}

type LendoraGetAccountSummaryInput struct {
	Address string `json:"address,omitempty" jsonschema:"the account's bech32 (svp1…) address; omit for the session account"`
}

type LendoraGetAccountSummaryOutput struct {
	BlockNumber         uint64 `json:"block_number"`
	Owner               string `json:"owner"`
	TotalSuppliedUSD    string `json:"total_supplied_usd"`
	TotalBorrowedUSD    string `json:"total_borrowed_usd"`
	HealthFactor        string `json:"health_factor"`
	RiskLevel           string `json:"risk_level"`
	RiskEmoji           string `json:"risk_emoji"`
	MaxBorrowableUSD    string `json:"max_borrowable_usd,omitempty"`
	MarketsWithPosition int    `json:"markets_with_position"`
}

// LendoraGetAccountSummary returns an account's aggregate supplied/borrowed USD,
// health factor, and risk level. Read-only.
func (h *Handlers) LendoraGetAccountSummary(
	ctx context.Context, _ *mcp.CallToolRequest, in LendoraGetAccountSummaryInput,
) (*mcp.CallToolResult, LendoraGetAccountSummaryOutput, error) {
	_, owner, ownerEth, err := h.lendoraOwner(ctx, "lendora_get_account_summary", in.Address)
	if err != nil {
		return nil, LendoraGetAccountSummaryOutput{}, err
	}
	if err := h.requireLendora(); err != nil {
		return nil, LendoraGetAccountSummaryOutput{}, err
	}
	r, err := h.computeRisk(ctx, ownerEth)
	if err != nil {
		return nil, LendoraGetAccountSummaryOutput{}, err
	}
	out := LendoraGetAccountSummaryOutput{
		BlockNumber:         r.BlockNumber,
		Owner:               owner,
		TotalSuppliedUSD:    formatUSD(r.TotalSuppliedUSD),
		TotalBorrowedUSD:    formatUSD(r.TotalBorrowedUSD),
		HealthFactor:        healthFactorString(r),
		RiskLevel:           r.RiskLevel,
		RiskEmoji:           r.RiskEmoji,
		MarketsWithPosition: len(r.Positions),
	}
	if r.MaxBorrowableOK {
		out.MaxBorrowableUSD = formatUSD(r.MaxBorrowableUSD)
	}
	return nil, out, nil
}

type LendoraGetAccountPositionsInput struct {
	Address string `json:"address,omitempty" jsonschema:"the account's bech32 (svp1…) address; omit for the session account"`
}

type LendoraGetAccountPositionsOutput struct {
	BlockNumber uint64        `json:"block_number"`
	Owner       string        `json:"owner"`
	Positions   []PositionDTO `json:"positions"`
}

// LendoraGetAccountPositions returns the account's per-market supplied/borrowed
// amounts (underlying + USD) and which markets are enabled as collateral.
func (h *Handlers) LendoraGetAccountPositions(
	ctx context.Context, _ *mcp.CallToolRequest, in LendoraGetAccountPositionsInput,
) (*mcp.CallToolResult, LendoraGetAccountPositionsOutput, error) {
	_, owner, ownerEth, err := h.lendoraOwner(ctx, "lendora_get_account_positions", in.Address)
	if err != nil {
		return nil, LendoraGetAccountPositionsOutput{}, err
	}
	if err := h.requireLendora(); err != nil {
		return nil, LendoraGetAccountPositionsOutput{}, err
	}
	r, err := h.computeRisk(ctx, ownerEth)
	if err != nil {
		return nil, LendoraGetAccountPositionsOutput{}, err
	}
	out := LendoraGetAccountPositionsOutput{BlockNumber: r.BlockNumber, Owner: owner, Positions: []PositionDTO{}}
	for _, p := range r.Positions {
		out.Positions = append(out.Positions, positionDTO(p))
	}
	return nil, out, nil
}

// -- lendora_get_balances ----------------------------------------------

// WalletBalanceDTO is one token's wallet + supplied balance.
type WalletBalanceDTO struct {
	Symbol    string `json:"symbol"`
	Wallet    string `json:"wallet"`     // underlying held in wallet, human units
	Supplied  string `json:"supplied"`   // supplied underlying (cToken balance × exchange rate), human units
	WalletUSD string `json:"wallet_usd"` // "$…" or ""
}

type LendoraGetBalancesInput struct {
	Address string `json:"address,omitempty" jsonschema:"the account's bech32 (svp1…) address; omit for the session account"`
}

type LendoraGetBalancesOutput struct {
	BlockNumber uint64             `json:"block_number"`
	Owner       string             `json:"owner"`
	GasToken    string             `json:"gas_token"` // native SVP balance, human units (for fees)
	Balances    []WalletBalanceDTO `json:"balances"`
}

// LendoraGetBalances reports the account's per-market wallet (underlying) balance
// and supplied (cToken) balance, plus the native SVP gas balance. Read-only.
func (h *Handlers) LendoraGetBalances(
	ctx context.Context, _ *mcp.CallToolRequest, in LendoraGetBalancesInput,
) (*mcp.CallToolResult, LendoraGetBalancesOutput, error) {
	_, owner, ownerEth, err := h.lendoraOwner(ctx, "lendora_get_balances", in.Address)
	if err != nil {
		return nil, LendoraGetBalancesOutput{}, err
	}
	if err := h.requireLendora(); err != nil {
		return nil, LendoraGetBalancesOutput{}, err
	}
	oracle, err := h.lendoraOracle(ctx)
	if err != nil {
		return nil, LendoraGetBalancesOutput{}, err
	}
	lend := h.Deps.EVM.Lendora
	out := LendoraGetBalancesOutput{BlockNumber: h.evmBlockNumber(ctx), Owner: owner, GasToken: h.nativeGasBalance(ctx, owner), Balances: []WalletBalanceDTO{}}

	markets := h.Deps.LendoraMarkets.All()
	sort.Slice(markets, func(i, j int) bool { return markets[i].Symbol < markets[j].Symbol })
	for _, m := range markets {
		if m.IsCEther {
			continue // native handled as GasToken
		}
		walletBal, err := h.erc20BalanceOf(ctx, m.Underlying, ownerEth)
		if err != nil {
			return nil, LendoraGetBalancesOutput{}, err
		}
		cBalData, err := lend.PackBalanceOf(ownerEth)
		if err != nil {
			return nil, LendoraGetBalancesOutput{}, err
		}
		cBalOut, err := h.evmCall(ctx, m.CToken, cBalData)
		if err != nil {
			return nil, LendoraGetBalancesOutput{}, err
		}
		cBal, err := lend.UnpackBalanceOf(cBalOut)
		if err != nil {
			return nil, LendoraGetBalancesOutput{}, err
		}
		if walletBal.Sign() == 0 && cBal.Sign() == 0 {
			continue
		}
		// Convert the cToken balance to supplied underlying via the exchange rate —
		// the raw cToken balance is not the supplied amount.
		suppliedUnderlying := big.NewInt(0)
		if cBal.Sign() > 0 {
			exRate, err := h.cTokenUint(ctx, m.CToken, lend.PackExchangeRateStored, "exchangeRateStored")
			if err != nil {
				return nil, LendoraGetBalancesOutput{}, err
			}
			suppliedUnderlying = new(big.Int).Div(new(big.Int).Mul(cBal, exRate), e18)
		}
		dto := WalletBalanceDTO{
			Symbol:   m.Symbol,
			Wallet:   humanAmount(math.NewIntFromBigInt(walletBal), m.UnderlyingDecimals),
			Supplied: humanAmount(math.NewIntFromBigInt(suppliedUnderlying), m.UnderlyingDecimals),
		}
		if answer, feedDec, ok, err := h.underlyingUsdPrice(ctx, oracle, m.CToken); err == nil && ok {
			dto.WalletUSD = formatUSD(usdFromFeed(walletBal, m.UnderlyingDecimals, answer, feedDec))
		}
		out.Balances = append(out.Balances, dto)
	}
	return nil, out, nil
}

// erc20BalanceOf reads balanceOf(owner) on any ERC-20 (reuses the cToken ABI's
// balanceOf, which is the standard selector).
func (h *Handlers) erc20BalanceOf(ctx context.Context, token, owner common.Address) (*big.Int, error) {
	data, err := h.Deps.EVM.Lendora.PackBalanceOf(owner)
	if err != nil {
		return nil, err
	}
	out, err := h.evmCall(ctx, token, data)
	if err != nil {
		return nil, fmt.Errorf("read balanceOf for %s: %w", token.Hex(), err)
	}
	return h.Deps.EVM.Lendora.UnpackBalanceOf(out)
}

// nativeGasBalance reads the account's native SVP (gas) balance from x/bank,
// best-effort ("0" when unavailable).
func (h *Handlers) nativeGasBalance(ctx context.Context, owner string) string {
	if h.Deps.Chain.BankQuery == nil {
		return "0"
	}
	coins, err := h.Deps.Chain.BankQuery.AllBalances(ctx, owner)
	if err != nil {
		return "0"
	}
	for _, c := range coins {
		if c.Denom == "asvp" {
			return humanAmount(c.Amount, 18)
		}
	}
	return "0"
}

// -- lendora_assess_risk -----------------------------------------------

type LendoraAssessRiskInput struct {
	Address string `json:"address,omitempty" jsonschema:"the account's bech32 (svp1…) address; omit for the session account"`
}

type LendoraAssessRiskOutput struct {
	BlockNumber      uint64        `json:"block_number"`
	Owner            string        `json:"owner"`
	HealthFactor     string        `json:"health_factor"`
	RiskLevel        string        `json:"risk_level"`
	RiskEmoji        string        `json:"risk_emoji"`
	HasDebt          bool          `json:"has_debt"`
	TotalSuppliedUSD string        `json:"total_supplied_usd"`
	TotalBorrowedUSD string        `json:"total_borrowed_usd"`
	MaxBorrowableUSD string        `json:"max_borrowable_usd,omitempty"`
	Shortfall        bool          `json:"shortfall"`
	Positions        []PositionDTO `json:"positions"`
}

// LendoraAssessRisk returns the account's health factor, risk level, and per-
// market breakdown — the risk-first report the skill drives its warnings from.
func (h *Handlers) LendoraAssessRisk(
	ctx context.Context, _ *mcp.CallToolRequest, in LendoraAssessRiskInput,
) (*mcp.CallToolResult, LendoraAssessRiskOutput, error) {
	_, owner, ownerEth, err := h.lendoraOwner(ctx, "lendora_assess_risk", in.Address)
	if err != nil {
		return nil, LendoraAssessRiskOutput{}, err
	}
	if err := h.requireLendora(); err != nil {
		return nil, LendoraAssessRiskOutput{}, err
	}
	r, err := h.computeRisk(ctx, ownerEth)
	if err != nil {
		return nil, LendoraAssessRiskOutput{}, err
	}
	out := LendoraAssessRiskOutput{
		BlockNumber:      r.BlockNumber,
		Owner:            owner,
		HealthFactor:     healthFactorString(r),
		RiskLevel:        r.RiskLevel,
		RiskEmoji:        r.RiskEmoji,
		HasDebt:          r.HasDebt,
		TotalSuppliedUSD: formatUSD(r.TotalSuppliedUSD),
		TotalBorrowedUSD: formatUSD(r.TotalBorrowedUSD),
		Shortfall:        r.ShortfallNat != nil && r.ShortfallNat.Sign() > 0,
		Positions:        []PositionDTO{}, // non-nil so an empty result marshals to [] not null
	}
	if r.MaxBorrowableOK {
		out.MaxBorrowableUSD = formatUSD(r.MaxBorrowableUSD)
	}
	for _, p := range r.Positions {
		out.Positions = append(out.Positions, positionDTO(p))
	}
	return nil, out, nil
}
