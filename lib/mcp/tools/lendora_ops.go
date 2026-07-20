package tools

import (
	"context"
	"fmt"
	"math/big"

	"cosmossdk.io/math"
	"github.com/ethereum/go-ethereum/common"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/svpchain/svpchain-mcp/lib/mcp/payload"
)

// lendora_ops.go holds the five operation tools — supply / withdraw / borrow /
// repay / collateral — that build a signable EVMTxPayload. Each first runs a
// simulation: it projects the post-action health factor and blocks the action
// (HF < 1.0) or warns (HF in [1.0, 1.2)) per the skill's guardrails. supply and
// repay pull the underlying via transferFrom, so a short allowance returns an
// "approve first" step (no payload) instead of a payload, exactly as
// build_bridge_deposit does.
//
// The returned payload is signed on the local signer (sign_evm_transaction) and
// submitted via broadcast_evm_tx — the same EVM path as every other build_* tool.

// LendoraSimulation is the projected impact shown before every operation.
type LendoraSimulation struct {
	BlockNumber           uint64   `json:"block_number"`
	Action                string   `json:"action"`
	Asset                 string   `json:"asset"`
	Amount                string   `json:"amount,omitempty"`
	CurrentHealthFactor   string   `json:"current_health_factor"`
	ProjectedHealthFactor string   `json:"projected_health_factor"`
	ProjectedRiskLevel    string   `json:"projected_risk_level"`
	ProjectedRiskEmoji    string   `json:"projected_risk_emoji"`
	Warnings              []string `json:"warnings,omitempty"`
}

// LendoraTxOutput is the uniform result of the operation tools: the simulation
// plus either a ready-to-sign payload or an approval step. Payload is present
// only when ApprovalRequired is nil.
type LendoraTxOutput struct {
	Simulation       LendoraSimulation     `json:"simulation"`
	Payload          *payload.EVMTxPayload `json:"payload,omitempty"`
	ApprovalRequired *LendoraApproval      `json:"approval_required,omitempty"`
}

// hfDangerLow / hfBlock frame the guardrail: block below 1.0, warn below 1.2.
var (
	hfBlock = math.LegacyOneDec()
	hfWarn  = math.LegacyMustNewDecFromStr("1.2")
)

// projectHF turns projected native collateral/borrow sums into a health-factor
// string, risk band, and (when in the warn zone) a warning. No debt → "∞"/Low.
func projectHF(postCollateral, postBorrow *big.Int) (hfStr, level, emoji string, warn string) {
	if postBorrow.Sign() <= 0 {
		return "∞", "Low", "🟢", ""
	}
	hf := math.LegacyNewDecFromBigInt(postCollateral).Quo(math.LegacyNewDecFromBigInt(postBorrow))
	shortfall := big.NewInt(0)
	if postCollateral.Cmp(postBorrow) < 0 {
		shortfall = big.NewInt(1)
	}
	level, emoji = riskBand(true, shortfall, hf)
	if hf.GTE(hfBlock) && hf.LT(hfWarn) {
		warn = fmt.Sprintf("projected health factor %s is close to the 1.0 liquidation threshold — leave a larger buffer", hf.String())
	}
	return hf.String(), level, emoji, warn
}

// nativeUnsupportedOps maps the lendora_build_* tools that cannot yet target the
// native (cSVP/cEther) market to the human verb used in their refusal message.
// CEther's mint()/repayBorrow() are payable and take no amount argument, so the
// amount-arg + ERC-20 allowance path these two ops use would build a reverting
// tx. The remaining ops (borrow/withdraw/collateral) are CEther-agnostic.
var nativeUnsupportedOps = map[string]string{
	"lendora_build_supply_tx": "supplying",
	"lendora_build_repay_tx":  "repaying",
}

// opCommon authorizes, guards, resolves the asset + session owner, and returns
// the current risk snapshot every operation starts from.
func (h *Handlers) opCommon(ctx context.Context, tool, asset string) (owner common.Address, ownerBech string, m lendoraMarketRef, r riskResult, err error) {
	tp, e := h.authorize(ctx, tool)
	if e != nil {
		return common.Address{}, "", lendoraMarketRef{}, riskResult{}, e
	}
	if e := h.requireLendora(); e != nil {
		return common.Address{}, "", lendoraMarketRef{}, riskResult{}, e
	}
	mk, e := h.resolveAsset(ctx, asset)
	if e != nil {
		return common.Address{}, "", lendoraMarketRef{}, riskResult{}, e
	}
	// Only supply/repay are CErc20-only: CEther's mint()/repayBorrow() are payable
	// and take no amount arg, so this file's amount-arg + ERC-20 allowance path
	// would build a reverting tx against the native market. borrow/redeem/
	// redeemUnderlying and the Comptroller's enterMarkets/exitMarket are identical
	// on CEther, so those ops work against the native market unchanged.
	if mk.IsCEther {
		if verb, blocked := nativeUnsupportedOps[tool]; blocked {
			return common.Address{}, "", lendoraMarketRef{}, riskResult{},
				userErrf("%s %s is not supported yet: %s is the native market and its cToken takes a value rather than an amount argument — borrowing, withdrawing, and enabling/disabling collateral do work on %s", verb, mk.Symbol, mk.Symbol, mk.Symbol)
		}
	}
	eth, e := ownerEthAddress(tp.Owner)
	if e != nil {
		return common.Address{}, "", lendoraMarketRef{}, riskResult{}, e
	}
	risk, e := h.computeRisk(ctx, eth)
	if e != nil {
		return common.Address{}, "", lendoraMarketRef{}, riskResult{}, e
	}
	return eth, tp.Owner, lendoraMarketRef{CToken: mk.CToken, Underlying: mk.Underlying, Symbol: mk.Symbol, UnderlyingDecimals: mk.UnderlyingDecimals, IsCEther: mk.IsCEther}, risk, nil
}

// lendoraMarketRef is the subset of a resolved market the ops need.
type lendoraMarketRef struct {
	CToken             common.Address
	Underlying         common.Address
	Symbol             string
	UnderlyingDecimals int64
	IsCEther           bool
}

// hypotheticalLiquidity reads getHypotheticalAccountLiquidity — the account's
// margin if it additionally redeemed redeemTokens and borrowed borrowAmount.
func (h *Handlers) hypotheticalLiquidity(ctx context.Context, owner, cToken common.Address, redeemTokens, borrowAmount *big.Int) (liq, short *big.Int, err error) {
	data, err := h.Deps.EVM.Lendora.PackGetHypotheticalAccountLiquidity(owner, cToken, redeemTokens, borrowAmount)
	if err != nil {
		return nil, nil, err
	}
	out, err := h.evmCall(ctx, h.Deps.EVM.Lendora.Comptroller(), data)
	if err != nil {
		return nil, nil, err
	}
	al, err := h.Deps.EVM.Lendora.UnpackAccountLiquidity("getHypotheticalAccountLiquidity", out)
	if err != nil {
		return nil, nil, err
	}
	return al.Liquidity, al.Shortfall, nil
}

// marketBorrowedNative / marketSuppliedNative pull a market's current borrowed /
// supplied native-weighted contribution from the risk snapshot (0 if no position).
func (r riskResult) marketPosition(cToken common.Address) (marketPosition, bool) {
	for _, p := range r.Positions {
		if p.CToken == cToken {
			return p, true
		}
	}
	return marketPosition{}, false
}

// -- lendora_build_supply_tx -------------------------------------------

type LendoraSupplyInput struct {
	Asset    string `json:"asset" jsonschema:"underlying symbol (e.g. \"USDC\") or a market/underlying 0x address to supply into"`
	Amount   string `json:"amount" jsonschema:"amount of the underlying to supply, human units (e.g. \"100\")"`
	ClientID string `json:"client_id" jsonschema:"broadcast-idempotency uuid (echo into broadcast_evm_tx.client_id)"`
}

// LendoraBuildSupplyTx builds a mint (supply) tx after simulating the position.
// Returns an approval step when the market's underlying allowance is short.
func (h *Handlers) LendoraBuildSupplyTx(
	ctx context.Context, _ *mcp.CallToolRequest, in LendoraSupplyInput,
) (*mcp.CallToolResult, LendoraTxOutput, error) {
	owner, ownerBech, m, r, err := h.opCommon(ctx, "lendora_build_supply_tx", in.Asset)
	if err != nil {
		return nil, LendoraTxOutput{}, err
	}
	amount, err := humanToBaseUnits(in.Amount, m.UnderlyingDecimals)
	if err != nil {
		return nil, LendoraTxOutput{}, fmt.Errorf("amount: %w", err)
	}
	// Supply raises collateral ONLY for a market already entered as collateral —
	// Compound's mint does not auto-enter, so supplying to a non-entered market
	// leaves the health factor unchanged (the user must enable it first).
	added := big.NewInt(0)
	var supplyWarn string
	if pos, ok := r.marketPosition(m.CToken); ok && pos.CollateralEnabled {
		price, err := h.cTokenUnderlyingPriceNativeVia(ctx, m.CToken)
		if err != nil {
			return nil, LendoraTxOutput{}, err
		}
		cf, err := h.cTokenCollateralFactor(ctx, m.CToken)
		if err != nil {
			return nil, LendoraTxOutput{}, err
		}
		added = weightedNative(price, amount.BigInt(), cf)
	} else {
		supplyWarn = "this market is not enabled as collateral, so supplying does not change your health factor — enable it with lendora_build_collateral_tx (action=enable)"
	}
	postCollateral := new(big.Int).Add(r.SumCollateralNat, added)
	sim := h.simulation("supply", m.Symbol, in.Amount, r, postCollateral, r.SumBorrowNat)
	if supplyWarn != "" {
		sim.Warnings = append(sim.Warnings, supplyWarn)
	}

	// Allowance gate (approve first when short).
	approval, err := h.checkLendoraAllowance(ctx, m.Underlying, owner, m.CToken, amount.BigInt(), in.Amount, "lendora_build_supply_tx")
	if err != nil {
		return nil, LendoraTxOutput{}, err
	}
	if approval != nil {
		return nil, LendoraTxOutput{Simulation: sim, ApprovalRequired: approval}, nil
	}
	data, err := h.Deps.EVM.Lendora.PackMint(amount.BigInt())
	if err != nil {
		return nil, LendoraTxOutput{}, err
	}
	return h.finishOp(ctx, ownerBech, m.CToken, data, in.ClientID, "lendora_build_supply_tx",
		fmt.Sprintf("supply %s %s to Lendora", in.Amount, m.Symbol), sim)
}

// -- lendora_build_withdraw_tx -----------------------------------------

type LendoraWithdrawInput struct {
	Asset    string `json:"asset" jsonschema:"underlying symbol or 0x address to withdraw from"`
	Amount   string `json:"amount,omitempty" jsonschema:"amount of the underlying to withdraw, human units; omit when full=true"`
	Full     bool   `json:"full,omitempty" jsonschema:"withdraw the entire supplied balance (redeems all cTokens, leaving no dust); ignores amount"`
	ClientID string `json:"client_id" jsonschema:"broadcast-idempotency uuid (echo into broadcast_evm_tx.client_id)"`
}

// LendoraBuildWithdrawTx builds a withdraw tx: redeemUnderlying(amount) for a
// concrete amount, or redeem(cTokenBalance) for a full withdrawal (no dust).
// Blocks when the withdrawal would leave the account undercollateralized (HF<1.0).
func (h *Handlers) LendoraBuildWithdrawTx(
	ctx context.Context, _ *mcp.CallToolRequest, in LendoraWithdrawInput,
) (*mcp.CallToolResult, LendoraTxOutput, error) {
	owner, ownerBech, m, r, err := h.opCommon(ctx, "lendora_build_withdraw_tx", in.Asset)
	if err != nil {
		return nil, LendoraTxOutput{}, err
	}

	var data []byte
	var redeemTokens *big.Int // cToken quantity burned (for the hypothetical)
	amountLabel := in.Amount
	full := in.Full || in.Amount == ""
	if full {
		// Redeem the entire cToken balance — cleanly empties the position without the
		// stale-exchange-rate dust that a redeemUnderlying(estimate) would leave.
		cBal, err := h.erc20BalanceOf(ctx, m.CToken, owner)
		if err != nil {
			return nil, LendoraTxOutput{}, err
		}
		if cBal.Sign() == 0 {
			return nil, LendoraTxOutput{}, userErrf("you have no supplied %s to withdraw", m.Symbol)
		}
		redeemTokens = cBal
		amountLabel = "full"
		if data, err = h.Deps.EVM.Lendora.PackRedeem(cBal); err != nil {
			return nil, LendoraTxOutput{}, err
		}
	} else {
		amount, err := humanToBaseUnits(in.Amount, m.UnderlyingDecimals)
		if err != nil {
			return nil, LendoraTxOutput{}, fmt.Errorf("amount: %w", err)
		}
		// redeemTokens = amount * 1e18 / exchangeRate — the cToken quantity burned.
		exRate, err := h.cTokenUint(ctx, m.CToken, h.Deps.EVM.Lendora.PackExchangeRateStored, "exchangeRateStored")
		if err != nil {
			return nil, LendoraTxOutput{}, err
		}
		if exRate.Sign() == 0 {
			return nil, LendoraTxOutput{}, userErrf("market %s has a zero exchange rate", m.Symbol)
		}
		redeemTokens = new(big.Int).Div(new(big.Int).Mul(amount.BigInt(), e18), exRate)
		if data, err = h.Deps.EVM.Lendora.PackRedeemUnderlying(amount.BigInt()); err != nil {
			return nil, LendoraTxOutput{}, err
		}
	}

	liq, short, err := h.hypotheticalLiquidity(ctx, owner, m.CToken, redeemTokens, big.NewInt(0))
	if err != nil {
		return nil, LendoraTxOutput{}, err
	}
	if short.Sign() > 0 {
		return nil, LendoraTxOutput{}, userErrf(
			"withdrawing %s %s would make your position undercollateralized (health factor below 1.0) — withdraw less or repay debt first",
			amountLabel, m.Symbol)
	}
	postCollateral := new(big.Int).Add(new(big.Int).Sub(liq, short), r.SumBorrowNat)
	sim := h.simulation("withdraw", m.Symbol, amountLabel, r, postCollateral, r.SumBorrowNat)

	return h.finishOp(ctx, ownerBech, m.CToken, data, in.ClientID, "lendora_build_withdraw_tx",
		fmt.Sprintf("withdraw %s %s from Lendora", amountLabel, m.Symbol), sim)
}

// -- lendora_build_borrow_tx -------------------------------------------

type LendoraBorrowInput struct {
	Asset    string `json:"asset" jsonschema:"underlying symbol or 0x address to borrow"`
	Amount   string `json:"amount" jsonschema:"amount of the underlying to borrow, human units"`
	ClientID string `json:"client_id" jsonschema:"broadcast-idempotency uuid (echo into broadcast_evm_tx.client_id)"`
}

// LendoraBuildBorrowTx builds a borrow tx. Blocks when the borrow would leave the
// account undercollateralized (HF < 1.0).
func (h *Handlers) LendoraBuildBorrowTx(
	ctx context.Context, _ *mcp.CallToolRequest, in LendoraBorrowInput,
) (*mcp.CallToolResult, LendoraTxOutput, error) {
	owner, ownerBech, m, r, err := h.opCommon(ctx, "lendora_build_borrow_tx", in.Asset)
	if err != nil {
		return nil, LendoraTxOutput{}, err
	}
	amount, err := humanToBaseUnits(in.Amount, m.UnderlyingDecimals)
	if err != nil {
		return nil, LendoraTxOutput{}, fmt.Errorf("amount: %w", err)
	}
	liq, short, err := h.hypotheticalLiquidity(ctx, owner, m.CToken, big.NewInt(0), amount.BigInt())
	if err != nil {
		return nil, LendoraTxOutput{}, err
	}
	if short.Sign() > 0 {
		return nil, LendoraTxOutput{}, userErrf(
			"borrowing %s %s would make your position undercollateralized (health factor below 1.0) — borrow less or supply more collateral",
			in.Amount, m.Symbol)
	}
	price, err := h.cTokenUnderlyingPriceNativeVia(ctx, m.CToken)
	if err != nil {
		return nil, LendoraTxOutput{}, err
	}
	postBorrow := new(big.Int).Add(r.SumBorrowNat, nativeValue(price, amount.BigInt()))
	postCollateral := new(big.Int).Add(new(big.Int).Sub(liq, short), postBorrow)
	sim := h.simulation("borrow", m.Symbol, in.Amount, r, postCollateral, postBorrow)

	data, err := h.Deps.EVM.Lendora.PackBorrow(amount.BigInt())
	if err != nil {
		return nil, LendoraTxOutput{}, err
	}
	return h.finishOp(ctx, ownerBech, m.CToken, data, in.ClientID, "lendora_build_borrow_tx",
		fmt.Sprintf("borrow %s %s from Lendora", in.Amount, m.Symbol), sim)
}

// -- lendora_build_repay_tx --------------------------------------------

type LendoraRepayInput struct {
	Asset    string `json:"asset" jsonschema:"underlying symbol or 0x address to repay"`
	Amount   string `json:"amount,omitempty" jsonschema:"amount of the underlying to repay, human units; omit or pass a value >= your debt (or full=true) to repay the entire outstanding balance"`
	Full     bool   `json:"full,omitempty" jsonschema:"repay the entire outstanding debt exactly (uses the repay-max sentinel, covering accrued interest with no dust); ignores amount"`
	ClientID string `json:"client_id" jsonschema:"broadcast-idempotency uuid (echo into broadcast_evm_tx.client_id)"`
}

// LendoraBuildRepayTx builds a repayBorrow tx after simulating the improved
// position. Returns an approval step when the underlying allowance is short.
func (h *Handlers) LendoraBuildRepayTx(
	ctx context.Context, _ *mcp.CallToolRequest, in LendoraRepayInput,
) (*mcp.CallToolResult, LendoraTxOutput, error) {
	owner, ownerBech, m, r, err := h.opCommon(ctx, "lendora_build_repay_tx", in.Asset)
	if err != nil {
		return nil, LendoraTxOutput{}, err
	}
	pos, hasPos := r.marketPosition(m.CToken)
	debt := big.NewInt(0)
	if hasPos {
		debt = pos.BorrowedUnderlying
	}

	// Determine the amount to encode (packAmount) and the actual underlying pulled
	// (pull, for the allowance check + HF projection). A full repay — full=true, an
	// empty amount, or an amount >= the debt — encodes the repay-max sentinel
	// (2^256-1) so the contract clears exactly the live debt (incl. accrued
	// interest) with no dust and no over-repay revert. A partial repay encodes the
	// concrete amount, which must not exceed the debt.
	var packAmount, pull *big.Int
	amountLabel := in.Amount
	full := in.Full || in.Amount == ""
	if !full {
		amount, err := humanToBaseUnits(in.Amount, m.UnderlyingDecimals)
		if err != nil {
			return nil, LendoraTxOutput{}, fmt.Errorf("amount: %w", err)
		}
		if amount.BigInt().Cmp(debt) >= 0 {
			full = true
		} else {
			packAmount, pull = amount.BigInt(), amount.BigInt()
		}
	}
	if full {
		if debt.Sign() == 0 {
			return nil, LendoraTxOutput{}, userErrf("you have no outstanding %s borrow to repay", m.Symbol)
		}
		packAmount, pull = maxUint256, debt
		amountLabel = "full"
	}

	price, err := h.cTokenUnderlyingPriceNativeVia(ctx, m.CToken)
	if err != nil {
		return nil, LendoraTxOutput{}, err
	}
	postBorrow := new(big.Int).Sub(r.SumBorrowNat, nativeValue(price, pull))
	if postBorrow.Sign() < 0 {
		postBorrow = big.NewInt(0)
	}
	sim := h.simulation("repay", m.Symbol, amountLabel, r, r.SumCollateralNat, postBorrow)

	// Allowance must cover the actual pull (the debt for a full repay, else amount).
	approval, err := h.checkLendoraAllowance(ctx, m.Underlying, owner, m.CToken, pull, amountLabel, "lendora_build_repay_tx")
	if err != nil {
		return nil, LendoraTxOutput{}, err
	}
	if approval != nil {
		return nil, LendoraTxOutput{Simulation: sim, ApprovalRequired: approval}, nil
	}
	data, err := h.Deps.EVM.Lendora.PackRepayBorrow(packAmount)
	if err != nil {
		return nil, LendoraTxOutput{}, err
	}
	return h.finishOp(ctx, ownerBech, m.CToken, data, in.ClientID, "lendora_build_repay_tx",
		fmt.Sprintf("repay %s %s to Lendora", amountLabel, m.Symbol), sim)
}

// -- lendora_build_collateral_tx ---------------------------------------

type LendoraCollateralInput struct {
	Asset    string `json:"asset" jsonschema:"underlying symbol or 0x address whose market to enable/disable as collateral"`
	Action   string `json:"action" jsonschema:"\"enable\" (enterMarkets) or \"disable\" (exitMarket)"`
	ClientID string `json:"client_id" jsonschema:"broadcast-idempotency uuid (echo into broadcast_evm_tx.client_id)"`
}

// LendoraBuildCollateralTx builds an enterMarkets/exitMarket tx on the
// Comptroller. Disabling collateral that would undercollateralize the account is
// blocked (HF < 1.0).
func (h *Handlers) LendoraBuildCollateralTx(
	ctx context.Context, _ *mcp.CallToolRequest, in LendoraCollateralInput,
) (*mcp.CallToolResult, LendoraTxOutput, error) {
	_, ownerBech, m, r, err := h.opCommon(ctx, "lendora_build_collateral_tx", in.Asset)
	if err != nil {
		return nil, LendoraTxOutput{}, err
	}
	pos, hasPos := r.marketPosition(m.CToken)
	alreadyEnabled := hasPos && pos.CollateralEnabled
	// The market's own collateral contribution (cf-weighted supplied native) — the
	// amount enabling adds / disabling removes. Zero when the account has no supply.
	contribution := big.NewInt(0)
	if hasPos {
		price, err := h.cTokenUnderlyingPriceNativeVia(ctx, m.CToken)
		if err != nil {
			return nil, LendoraTxOutput{}, err
		}
		contribution = weightedNative(price, pos.SuppliedUnderlying, pos.CollateralFactor)
	}

	var data []byte
	postCollateral := new(big.Int).Set(r.SumCollateralNat)
	switch in.Action {
	case "enable":
		// Entering an already-entered market is a no-op — don't double-count.
		if !alreadyEnabled {
			postCollateral.Add(postCollateral, contribution)
		}
		data, err = h.Deps.EVM.Lendora.PackEnterMarkets([]common.Address{m.CToken})
	case "disable":
		// exitMarket reverts if the account still borrows from this market.
		if hasPos && pos.BorrowedUnderlying.Sign() > 0 {
			return nil, LendoraTxOutput{}, userErrf(
				"cannot disable %s as collateral while it has an outstanding borrow — repay it first", m.Symbol)
		}
		// Only a currently-entered market contributes collateral to remove; exiting
		// a non-entered market is a harmless no-op.
		if alreadyEnabled {
			postCollateral.Sub(postCollateral, contribution)
			if r.HasDebt && postCollateral.Cmp(r.SumBorrowNat) < 0 {
				return nil, LendoraTxOutput{}, userErrf(
					"disabling %s as collateral would make your position undercollateralized (health factor below 1.0) — repay debt or disable a different market",
					m.Symbol)
			}
		}
		data, err = h.Deps.EVM.Lendora.PackExitMarket(m.CToken)
	default:
		return nil, LendoraTxOutput{}, userErrf("action must be \"enable\" or \"disable\", got %q", in.Action)
	}
	if err != nil {
		return nil, LendoraTxOutput{}, err
	}
	action := "enable_collateral"
	if in.Action == "disable" {
		action = "disable_collateral"
	}
	sim := h.simulation(action, m.Symbol, "", r, postCollateral, r.SumBorrowNat)
	return h.finishOp(ctx, ownerBech, h.Deps.EVM.Lendora.Comptroller(), data, in.ClientID, "lendora_build_collateral_tx",
		fmt.Sprintf("%s %s as Lendora collateral", in.Action, m.Symbol), sim)
}

// -- shared op tails ----------------------------------------------------

// simulation assembles a LendoraSimulation from current + projected sums.
func (h *Handlers) simulation(action, asset, amount string, r riskResult, postCollateral, postBorrow *big.Int) LendoraSimulation {
	hfStr, level, emoji, warn := projectHF(postCollateral, postBorrow)
	sim := LendoraSimulation{
		BlockNumber:           r.BlockNumber,
		Action:                action,
		Asset:                 asset,
		Amount:                amount,
		CurrentHealthFactor:   healthFactorString(r),
		ProjectedHealthFactor: hfStr,
		ProjectedRiskLevel:    level,
		ProjectedRiskEmoji:    emoji,
	}
	if warn != "" {
		sim.Warnings = append(sim.Warnings, warn)
	}
	return sim
}

// finishOp assembles the EVMTxPayload and wraps it with the simulation.
func (h *Handlers) finishOp(ctx context.Context, ownerBech string, to common.Address, data []byte, clientID, tool, desc string, sim LendoraSimulation) (*mcp.CallToolResult, LendoraTxOutput, error) {
	p, err := h.assembleERC(ctx, h.Deps.EVM.Assembler, ownerBech, to, data, clientID, tool, desc)
	if err != nil {
		return nil, LendoraTxOutput{}, err
	}
	return nil, LendoraTxOutput{Simulation: sim, Payload: p}, nil
}

// cTokenUnderlyingPriceNativeVia reads getUnderlyingPrice via the resolved oracle.
func (h *Handlers) cTokenUnderlyingPriceNativeVia(ctx context.Context, cToken common.Address) (*big.Int, error) {
	oracle, err := h.lendoraOracle(ctx)
	if err != nil {
		return nil, err
	}
	return h.cTokenUnderlyingPriceNative(ctx, oracle, cToken)
}

// nativeValue = price * amount / 1e18 (native-1e18 value of an underlying amount).
func nativeValue(price, amount *big.Int) *big.Int {
	v := new(big.Int).Mul(price, amount)
	return v.Div(v, e18)
}

// weightedNative = collateralFactor * (price * amount / 1e18) / 1e18 — the
// cf-weighted native collateral value of an underlying amount.
func weightedNative(price, amount, cfMantissa *big.Int) *big.Int {
	v := nativeValue(price, amount)
	v.Mul(v, cfMantissa)
	return v.Div(v, e18)
}
