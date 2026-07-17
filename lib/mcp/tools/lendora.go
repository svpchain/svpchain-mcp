package tools

import (
	"context"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/common"

	"github.com/svpchain/svpchain-mcp/lib/mcp/builder"
	"github.com/svpchain/svpchain-mcp/lib/mcp/lendora"
	"github.com/svpchain/svpchain-mcp/lib/mcp/policy"
)

// lendora.go is the shared base of the lendora_* family (a Compound V2 fork of
// CErc20 money markets governed by one Comptroller). The read tools live in
// lendora_read.go, the operation (build) tools in lendora_ops.go, the risk engine
// in lendora_risk.go, and the number formatters in lendora_format.go. This file
// holds the pieces they all share: the enablement guard, asset resolution via the
// markets cache, optional-owner auth, the allowance "approve first" step, and a
// couple of eth_call read helpers.
//
// Everything is on the EVM path: operation tools return an EVMTxPayload the caller
// signs (sign_evm_transaction) and submits (broadcast_evm_tx); read tools only
// eth_call. Amounts are human units, converted via on-chain decimals (the
// underlying's for supply/borrow/repay/withdraw, the cToken's for a raw redeem).

// requireLendora returns a clean user error if the server was started without the
// EVM + Comptroller config the lendora_* tools need.
func (h *Handlers) requireLendora() error {
	if err := h.requireEVM(); err != nil {
		return err
	}
	if h.Deps.EVM.Lendora == nil || h.Deps.LendoraMarkets == nil {
		return userErrf("Lendora is not enabled on this server (no evm_lendora_comptroller_addr configured)")
	}
	return nil
}

// resolveAsset maps a user-facing asset — an underlying symbol ("USDC") or a 0x
// cToken/underlying address — to its market metadata via the cache. A 0x address
// missing from the cache (a fresh listing, or a market dropped by a transient
// refresh error) is resolved on-demand so a valid cToken still works without
// waiting for the next refresh.
func (h *Handlers) resolveAsset(ctx context.Context, asset string) (lendora.Market, error) {
	if m, ok := h.Deps.LendoraMarkets.Resolve(asset); ok {
		return m, nil
	}
	if t := strings.TrimSpace(asset); common.IsHexAddress(t) {
		if m, ok := h.Deps.LendoraMarkets.LoadMarket(ctx, common.HexToAddress(t)); ok {
			return m, nil
		}
	}
	return lendora.Market{}, userErrf("unknown Lendora asset %q — pass an underlying symbol (e.g. \"USDC\") or a market 0x address", asset)
}

// lendoraOwner is the optional-owner auth prelude for the read tools: authorize,
// then resolve the target owner — the explicit bech32 address argument (checked
// against the tenant) or, when omitted, the session owner. Returns the tenant
// policy, the bech32 owner, and its 0x form (for the on-chain reads).
func (h *Handlers) lendoraOwner(ctx context.Context, tool, addrArg string) (*policy.TenantPolicy, string, common.Address, error) {
	tp, err := h.authorize(ctx, tool)
	if err != nil {
		return nil, "", common.Address{}, err
	}
	owner := strings.TrimSpace(addrArg)
	if owner == "" {
		owner = tp.Owner
	} else if err := h.Deps.Policy.CheckOwner(tp.TenantID, owner); err != nil {
		return nil, "", common.Address{}, err
	}
	eth, err := ownerEthAddress(owner)
	if err != nil {
		return nil, "", common.Address{}, err
	}
	return tp, owner, eth, nil
}

// lendoraOracle returns the resolved PriceOracle address (comptroller.oracle()),
// preferring the cache's value and falling back to a live read.
func (h *Handlers) lendoraOracle(ctx context.Context) (common.Address, error) {
	if addr, ok := h.Deps.LendoraMarkets.Oracle(); ok {
		return addr, nil
	}
	data, err := h.Deps.EVM.Lendora.PackOracle()
	if err != nil {
		return common.Address{}, err
	}
	out, err := h.evmCall(ctx, h.Deps.EVM.Lendora.Comptroller(), data)
	if err != nil {
		return common.Address{}, fmt.Errorf("read comptroller.oracle(): %w", err)
	}
	return h.Deps.EVM.Lendora.UnpackOracle(out)
}

// evmBlockNumber returns the latest EVM block height for the "at block #X"
// annotation, best-effort (0 on error — the annotation is informational).
func (h *Handlers) evmBlockNumber(ctx context.Context) uint64 {
	n, err := h.Deps.Chain.EVM.BlockNumber(ctx)
	if err != nil {
		return 0
	}
	return n
}

// -- read helpers on the Lendora binding -------------------------------

// cTokenUint reads a no-arg uint256 cToken view (exchangeRateStored,
// supplyRatePerBlock, …) via eth_call. `pack` is the matching builder packer and
// `method` labels errors.
func (h *Handlers) cTokenUint(ctx context.Context, cToken common.Address, pack func() ([]byte, error), method string) (*big.Int, error) {
	data, err := pack()
	if err != nil {
		return nil, err
	}
	out, err := h.evmCall(ctx, cToken, data)
	if err != nil {
		return nil, fmt.Errorf("read %s for %s: %w", method, cToken.Hex(), err)
	}
	return h.Deps.EVM.Lendora.UnpackCTokenUint(method, out)
}

// underlyingUsdPrice reads a market's underlying USD price from its per-asset
// Chainlink feed (cTokenToFeed → latestRoundData) together with the feed's own
// decimals(). Returns (answer, feedDecimals, ok); ok=false when the market has no
// configured feed, so callers can omit USD rather than fail. The native (cSVP)
// market's feed doubles as the SVP/USD denominator.
func (h *Handlers) underlyingUsdPrice(ctx context.Context, oracle, cToken common.Address) (*big.Int, int64, bool, error) {
	feedData, err := h.Deps.EVM.Lendora.PackCTokenToFeed(cToken)
	if err != nil {
		return nil, 0, false, err
	}
	feedOut, err := h.evmCall(ctx, oracle, feedData)
	if err != nil {
		return nil, 0, false, fmt.Errorf("read cTokenToFeed(%s): %w", cToken.Hex(), err)
	}
	feed, err := h.Deps.EVM.Lendora.UnpackOracleAddress("cTokenToFeed", feedOut)
	if err != nil {
		return nil, 0, false, err
	}
	if feed == (common.Address{}) {
		return nil, 0, false, nil
	}
	// Reuse the AggregatorV3 read layer (decimals()/latestRoundData()) rather than
	// re-embedding the feed ABI. Read the feed's own decimals rather than assuming
	// a fixed scale — Chainlink feeds are usually 8-dec USD but not guaranteed.
	agg, err := builder.NewOracleFeed(feed)
	if err != nil {
		return nil, 0, false, err
	}
	decData, err := agg.PackDecimals()
	if err != nil {
		return nil, 0, false, err
	}
	decOut, err := h.evmCall(ctx, feed, decData)
	if err != nil {
		return nil, 0, false, fmt.Errorf("read decimals for feed %s: %w", feed.Hex(), err)
	}
	feedDec, err := agg.UnpackDecimals(decOut)
	if err != nil {
		return nil, 0, false, err
	}
	rdData, err := agg.PackLatestRoundData()
	if err != nil {
		return nil, 0, false, err
	}
	rdOut, err := h.evmCall(ctx, feed, rdData)
	if err != nil {
		return nil, 0, false, fmt.Errorf("read latestRoundData for feed %s: %w", feed.Hex(), err)
	}
	rd, err := agg.UnpackLatestRoundData(rdOut)
	if err != nil {
		return nil, 0, false, err
	}
	if rd.Answer == nil || rd.Answer.Sign() <= 0 {
		return nil, 0, false, nil
	}
	return rd.Answer, int64(feedDec), true, nil
}

// -- approval "approve first" step (supply / repay) --------------------

// LendoraApproval is the structured "approve first" step returned (instead of an
// error) when the cToken's allowance on the underlying does not cover a supply /
// repay. It is the exact set of arguments to feed build_erc20_approve, plus the
// tool to retry once the approval is signed and broadcast. Mirrors BridgeApproval.
type LendoraApproval struct {
	Tool      string `json:"tool"`       // always "build_erc20_approve"
	Token     string `json:"token"`      // 0x underlying token to approve
	Spender   string `json:"spender"`    // 0x cToken to approve as spender
	MinAmount string `json:"min_amount"` // human-units amount the approval must be >=
	RetryTool string `json:"retry_tool"` // lendora tool to call again after approving
	Message   string `json:"message"`    // human-readable instruction
}

// checkLendoraAllowance reads the cToken's allowance on the underlying and returns
// a structured "approve first" step pointing at retryTool when it does not cover
// amount (nil when sufficient). A short allowance is NOT an error — only a genuine
// RPC/decode failure is — so callers surface the approval step in a successful
// result. Mirrors checkBridgeAllowance.
func (h *Handlers) checkLendoraAllowance(
	ctx context.Context, underlying, owner, cToken common.Address, amount *big.Int, amountHuman, retryTool string,
) (*LendoraApproval, error) {
	data, err := builder.PackERC20Allowance(owner, cToken)
	if err != nil {
		return nil, err
	}
	out, err := h.evmCall(ctx, underlying, data)
	if err != nil {
		return nil, fmt.Errorf("read allowance for %s: %w", underlying.Hex(), err)
	}
	allowance, err := builder.UnpackERC20Allowance(out)
	if err != nil {
		return nil, err
	}
	if allowance.Cmp(amount) < 0 {
		return &LendoraApproval{
			Tool:      "build_erc20_approve",
			Token:     underlying.Hex(),
			Spender:   cToken.Hex(),
			MinAmount: amountHuman,
			RetryTool: retryTool,
			Message: fmt.Sprintf(
				"the market's allowance on %s is insufficient — call build_erc20_approve (token %s, spender %s, amount >= %s) first, then retry %s",
				underlying.Hex(), underlying.Hex(), cToken.Hex(), amountHuman, retryTool),
		}, nil
	}
	return nil, nil
}
