package tools

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"time"

	"cosmossdk.io/math"
	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/svpchain/svpchain-mcp/lib/mcp/builder"
	"github.com/svpchain/svpchain-mcp/lib/mcp/chain"
	"github.com/svpchain/svpchain-mcp/lib/mcp/payload"
)

// swap.go is the UniswapV2 swap tool family — the first per-contract EVM
// build_* tools. The write path mirrors the rest of the EVM family:
// build_swap / build_token_approval return an EVMTxPayload the caller signs on
// the local signer (sign_evm_transaction) and submits via broadcast_evm_tx.
// quote_swap is a read-only eth_call, no signing.
//
// Design choices (see also the swap tool descriptions in registry.go):
//   - Amounts are human units. The server reads each token's on-chain
//     decimals() (native SVP is 18) to convert — matching get_balance /
//     build_bank_send rather than forcing the agent to count wei.
//   - Output always goes to the caller's own EVM address (like faucet_claim);
//     there is no caller-supplied recipient in v1.
//   - Routing is direct/single-hop: [in,out], or [WSVP,out] / [in,WSVP] for a
//     native leg. Multi-hop (A->WSVP->B when no direct pair exists) is deferred.
//   - native SVP is selected by an empty / "native" / "svp" token, or the zero
//     address; any other value must be a 0x ERC-20 address.

const (
	// defaultSlippageBps caps how far below the quoted output the swap may
	// fill before reverting. 50 bps = 0.5%, a common DEX default.
	defaultSlippageBps = 50
	// defaultSwapDeadline bounds how long a built swap stays valid on chain
	// once signed. 20 minutes mirrors the Uniswap UI default.
	defaultSwapDeadline = 20 * time.Minute
	// bpsDenom is the basis-point denominator (100% = 10000 bps).
	bpsDenom = 10000
)

// maxUint256 is the conventional "infinite" ERC-20 allowance (2^256 - 1).
var maxUint256 = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))

// -- token / plan helpers (pure, unit-tested) --------------------------

// knownToken is one registered ERC-20 for this deployment: its address plus
// whether that balance is also represented by an x/bank denom.
//
// bankLinked tokens (e.g. USDC, the EVM side of the erc20/usdc trading
// collateral) already surface through get_balance's bank read, so they are NOT
// additionally contract-read there — that would double-count the same balance.
// Pure ERC-20s (USDV) have no bank denom and ARE contract-read. The distinction
// only affects get_balance; swap aliases and faucet labels use every entry.
type knownToken struct {
	address    common.Address
	bankLinked bool
}

// knownSwapTokens maps lower-case symbol aliases to this deployment's ERC-20s,
// so an agent can pass token_in/token_out="usdv" / "usdc" instead of the raw 0x
// address (native SVP is named separately, in parseSwapToken). These are
// convenience aliases only — a caller can always pass any 0x address, or
// discover faucet-dispensed tokens via list_faucet_tokens. Hardcoded like
// knownDenoms in account.go; decimals are still read on chain at call time. Also
// the source for labeling known ERC-20s by symbol in faucet output (faucet.go)
// and for the contract-read balances in get_balance (account.go).
var knownSwapTokens = map[string]knownToken{
	"usdv": {address: common.HexToAddress("0x013a61E622e6ABFCaB64F52D274C3Fc0aA37f951")},
	"usdc": {address: common.HexToAddress("0x732F6Ea7AfD5EdC02e7ba052075dd0780e285489"), bankLinked: true},
}

// knownTokenSymbol reverse-maps an ERC-20 address to its upper-cased symbol
// alias, if one is registered in knownSwapTokens.
func knownTokenSymbol(addr common.Address) (string, bool) {
	for sym, kt := range knownSwapTokens {
		if kt.address == addr {
			return strings.ToUpper(sym), true
		}
	}
	return "", false
}

// parseSwapToken resolves a tool's token argument to either native SVP or an
// ERC-20 address. Empty, "native", "svp", or the zero address all mean native;
// a known symbol (see knownSwapTokens) resolves to its address; anything else
// must be a valid 0x address.
func parseSwapToken(s string) (addr common.Address, native bool, err error) {
	t := strings.TrimSpace(s)
	key := strings.ToLower(t)
	switch key {
	case "", "native", "svp":
		return common.Address{}, true, nil
	}
	if kt, ok := knownSwapTokens[key]; ok {
		return kt.address, false, nil
	}
	if !common.IsHexAddress(t) {
		return common.Address{}, false, fmt.Errorf(
			"invalid token %q: use a 0x address, a known symbol (usdv), or empty/\"native\"/\"svp\" for native SVP", s)
	}
	addr = common.HexToAddress(t)
	if addr == (common.Address{}) {
		return common.Address{}, true, nil // 0x0 is the native sentinel
	}
	return addr, false, nil
}

type swapKind int

const (
	kindTokensForTokens swapKind = iota // ERC-20 -> ERC-20
	kindSVPForTokens                    // native SVP -> ERC-20
	kindTokensForSVP                    // ERC-20 -> native SVP
)

// swapPlan is the resolved shape of a swap: which router entrypoint to call and
// the path to pass it. Derived purely from the in/out token kinds + WSVP.
type swapPlan struct {
	kind swapKind
	path []common.Address
}

// resolveSwapPlan maps (in, out) token kinds to a router entrypoint + path.
// Native legs go through WSVP as the path endpoint, exactly as the router's
// native swap functions require (path[0]==WSVP for SVP-in, path[last]==WSVP for
// SVP-out).
func resolveSwapPlan(inAddr common.Address, inNative bool, outAddr common.Address, outNative bool, wsvp common.Address) (swapPlan, error) {
	switch {
	case inNative && outNative:
		return swapPlan{}, fmt.Errorf("token_in and token_out are both native SVP — nothing to swap")
	case inNative:
		return swapPlan{kind: kindSVPForTokens, path: []common.Address{wsvp, outAddr}}, nil
	case outNative:
		return swapPlan{kind: kindTokensForSVP, path: []common.Address{inAddr, wsvp}}, nil
	default:
		if inAddr == outAddr {
			return swapPlan{}, fmt.Errorf("token_in and token_out are the same token")
		}
		return swapPlan{kind: kindTokensForTokens, path: []common.Address{inAddr, outAddr}}, nil
	}
}

// applySlippage returns the minimum acceptable output: out * (10000-bps)/10000.
// bps must be in [0, 10000).
func applySlippage(out *big.Int, bps int) (*big.Int, error) {
	if bps < 0 || bps >= bpsDenom {
		return nil, fmt.Errorf("slippage_bps must be in [0, %d), got %d", bpsDenom, bps)
	}
	min := new(big.Int).Mul(out, big.NewInt(int64(bpsDenom-bps)))
	return min.Div(min, big.NewInt(bpsDenom)), nil
}

// -- shared EVM read helpers -------------------------------------------

// requireSwap returns the swap binding + EVM client, or a clean user error if
// the server was started without the EVM/Uniswap config the swap tools need.
func (h *Handlers) requireSwap() (*builder.UniswapV2, error) {
	if h.Deps.Chain.EVM == nil || h.Deps.EVM.Assembler == nil {
		return nil, userErrf("EVM is not enabled on this server (no evm_rpc_url configured)")
	}
	if h.Deps.EVM.Uniswap == nil {
		return nil, userErrf("swaps are not enabled on this server (no evm_uniswap_router_addr / evm_wsvp_addr configured)")
	}
	return h.Deps.EVM.Uniswap, nil
}

// evmCall runs a read-only eth_call against to with the given calldata on the
// home (svpchain) EVM client.
func (h *Handlers) evmCall(ctx context.Context, to common.Address, data []byte) ([]byte, error) {
	return evmCallOn(ctx, h.Deps.Chain.EVM, to, data)
}

// evmCallOn is evmCall against an explicit client — used by inbound bridging to
// read state (e.g. an allowance) on a foreign chain's RPC rather than the home one.
func evmCallOn(ctx context.Context, client chain.EVMClient, to common.Address, data []byte) ([]byte, error) {
	return client.CallContract(ctx, ethereum.CallMsg{To: &to, Data: data})
}

// tokenDecimals returns a token's decimals — 18 for native SVP, otherwise the
// on-chain decimals() getter. Used to convert human amounts at the boundary.
func (h *Handlers) tokenDecimals(ctx context.Context, native bool, token common.Address) (int64, error) {
	if native {
		return 18, nil
	}
	uni := h.Deps.EVM.Uniswap
	data, err := uni.PackDecimals()
	if err != nil {
		return 0, err
	}
	out, err := h.evmCall(ctx, token, data)
	if err != nil {
		return 0, fmt.Errorf("read decimals for %s (is it an ERC-20?): %w", token.Hex(), err)
	}
	dec, err := uni.UnpackDecimals(out)
	if err != nil {
		return 0, fmt.Errorf("read decimals for %s: %w", token.Hex(), err)
	}
	return int64(dec), nil
}

// erc20Balance reads balanceOf(account) for an ERC-20 token off chain.
func (h *Handlers) erc20Balance(ctx context.Context, token, account common.Address) (*big.Int, error) {
	uni := h.Deps.EVM.Uniswap
	data, err := uni.PackBalanceOf(account)
	if err != nil {
		return nil, err
	}
	out, err := h.evmCall(ctx, token, data)
	if err != nil {
		return nil, fmt.Errorf("read balanceOf for %s: %w", token.Hex(), err)
	}
	return uni.UnpackBalanceOf(out)
}

// quoteAmountsOut reads getAmountsOut(amountIn, path) off chain and returns the
// full amounts array (amounts[0]==amountIn, amounts[last]==output).
func (h *Handlers) quoteAmountsOut(ctx context.Context, uni *builder.UniswapV2, amountIn *big.Int, path []common.Address) ([]*big.Int, error) {
	data, err := uni.PackGetAmountsOut(amountIn, path)
	if err != nil {
		return nil, err
	}
	out, err := h.evmCall(ctx, uni.Router(), data)
	if err != nil {
		return nil, fmt.Errorf("quote getAmountsOut (no liquidity for this path?): %w", err)
	}
	return uni.UnpackAmounts(out)
}

// -- quote_swap --------------------------------------------------------

type QuoteSwapInput struct {
	TokenIn  string `json:"token_in" jsonschema:"input token: a 0x ERC-20 address, a known symbol (\"usdv\"), or empty/\"native\"/\"svp\" for native SVP"`
	TokenOut string `json:"token_out" jsonschema:"output token: a 0x ERC-20 address, a known symbol (\"usdv\"), or empty/\"native\"/\"svp\" for native SVP"`
	AmountIn string `json:"amount_in" jsonschema:"input amount in human units, e.g. \"1.5\""`
}

type QuoteSwapOutput struct {
	TokenIn         string   `json:"token_in"`          // 0x address, or "native" for SVP
	TokenOut        string   `json:"token_out"`         // 0x address, or "native" for SVP
	Path            []string `json:"path"`              // resolved router path (0x addresses)
	AmountIn        string   `json:"amount_in"`         // human, echoed
	AmountInBase    string   `json:"amount_in_base"`    // base units (integer)
	ExpectedOut     string   `json:"expected_out"`      // human, at current reserves (pre-slippage)
	ExpectedOutBase string   `json:"expected_out_base"` // base units (integer)
}

// QuoteSwap returns the output a swap would yield at the current reserves
// (before slippage). Read-only — no tx, no signing. Use it to preview a swap
// or to size amount_in; build_swap re-quotes at build time for freshness.
func (h *Handlers) QuoteSwap(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	in QuoteSwapInput,
) (*mcp.CallToolResult, QuoteSwapOutput, error) {
	if _, err := h.authorize(ctx, "quote_swap"); err != nil {
		return nil, QuoteSwapOutput{}, err
	}
	uni, err := h.requireSwap()
	if err != nil {
		return nil, QuoteSwapOutput{}, err
	}

	inAddr, inNative, err := parseSwapToken(in.TokenIn)
	if err != nil {
		return nil, QuoteSwapOutput{}, fmt.Errorf("token_in: %w", err)
	}
	outAddr, outNative, err := parseSwapToken(in.TokenOut)
	if err != nil {
		return nil, QuoteSwapOutput{}, fmt.Errorf("token_out: %w", err)
	}
	plan, err := resolveSwapPlan(inAddr, inNative, outAddr, outNative, uni.WSVP())
	if err != nil {
		return nil, QuoteSwapOutput{}, err
	}

	decIn, err := h.tokenDecimals(ctx, inNative, inAddr)
	if err != nil {
		return nil, QuoteSwapOutput{}, err
	}
	amountInBase, err := humanToBaseUnits(in.AmountIn, decIn)
	if err != nil {
		return nil, QuoteSwapOutput{}, fmt.Errorf("amount_in: %w", err)
	}

	amounts, err := h.quoteAmountsOut(ctx, uni, amountInBase.BigInt(), plan.path)
	if err != nil {
		return nil, QuoteSwapOutput{}, err
	}
	expectedOut := amounts[len(amounts)-1]

	decOut, err := h.tokenDecimals(ctx, outNative, outAddr)
	if err != nil {
		return nil, QuoteSwapOutput{}, err
	}

	return nil, QuoteSwapOutput{
		TokenIn:         tokenLabel(inNative, inAddr),
		TokenOut:        tokenLabel(outNative, outAddr),
		Path:            addrsToHex(plan.path),
		AmountIn:        in.AmountIn,
		AmountInBase:    amountInBase.String(),
		ExpectedOut:     humanAmount(math.NewIntFromBigInt(expectedOut), decOut),
		ExpectedOutBase: expectedOut.String(),
	}, nil
}

// -- build_token_approval ----------------------------------------------

type BuildTokenApprovalInput struct {
	Token     string `json:"token" jsonschema:"ERC-20 to approve the router to spend (the input token of a swap): a 0x address or a known symbol (\"usdv\")"`
	Amount    string `json:"amount,omitempty" jsonschema:"human amount to approve, e.g. \"100\"; omit when unlimited=true"`
	Unlimited bool   `json:"unlimited,omitempty" jsonschema:"approve the maximum (2^256-1) so future swaps of this token need no further approval; ignores amount"`
	ClientID  string `json:"client_id" jsonschema:"broadcast-idempotency uuid (echo into broadcast_evm_tx.client_id)"`
}

type BuildTokenApprovalOutput struct {
	Payload payload.EVMTxPayload `json:"payload"`
}

// BuildTokenApproval constructs an ERC-20 approve(router, amount) tx so the
// router can pull the input token during a token-input swap. Native-SVP-input
// swaps do not need this (the SVP rides as the tx value). Returns an
// EVMTxPayload — sign with sign_evm_transaction then broadcast_evm_tx.
func (h *Handlers) BuildTokenApproval(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	in BuildTokenApprovalInput,
) (*mcp.CallToolResult, BuildTokenApprovalOutput, error) {
	tp, err := h.authorize(ctx, "build_token_approval")
	if err != nil {
		return nil, BuildTokenApprovalOutput{}, err
	}
	uni, err := h.requireSwap()
	if err != nil {
		return nil, BuildTokenApprovalOutput{}, err
	}

	addr, native, err := parseSwapToken(in.Token)
	if err != nil {
		return nil, BuildTokenApprovalOutput{}, fmt.Errorf("token: %w", err)
	}
	if native {
		return nil, BuildTokenApprovalOutput{}, userErrf("native SVP needs no approval; only ERC-20 input tokens do")
	}

	amount := maxUint256
	amountLabel := "unlimited"
	if !in.Unlimited {
		dec, err := h.tokenDecimals(ctx, false, addr)
		if err != nil {
			return nil, BuildTokenApprovalOutput{}, err
		}
		base, err := humanToBaseUnits(in.Amount, dec)
		if err != nil {
			return nil, BuildTokenApprovalOutput{}, fmt.Errorf("amount: %w", err)
		}
		amount = base.BigInt()
		amountLabel = in.Amount
	}

	data, err := uni.PackApprove(uni.Router(), amount)
	if err != nil {
		return nil, BuildTokenApprovalOutput{}, err
	}

	from, err := ownerEthAddress(tp.Owner)
	if err != nil {
		return nil, BuildTokenApprovalOutput{}, err
	}

	p, err := h.Deps.EVM.Assembler.Assemble(ctx, builder.EVMArgs{
		ClientID: in.ClientID,
		From:     from,
		To:       addr,
		Data:     data,
		Summary: payload.EVMSummary{
			ToolName:    "build_token_approval",
			Description: fmt.Sprintf("approve router %s to spend %s of %s", uni.Router().Hex(), amountLabel, addr.Hex()),
		},
	})
	if err != nil {
		return nil, BuildTokenApprovalOutput{}, err
	}
	return nil, BuildTokenApprovalOutput{Payload: *p}, nil
}

// -- build_swap --------------------------------------------------------

type BuildSwapInput struct {
	TokenIn     string `json:"token_in" jsonschema:"input token: a 0x ERC-20 address, a known symbol (\"usdv\"), or empty/\"native\"/\"svp\" for native SVP"`
	TokenOut    string `json:"token_out" jsonschema:"output token: a 0x ERC-20 address, a known symbol (\"usdv\"), or empty/\"native\"/\"svp\" for native SVP"`
	AmountIn    string `json:"amount_in" jsonschema:"exact input amount in human units, e.g. \"1.5\""`
	SlippageBps int    `json:"slippage_bps,omitempty" jsonschema:"max slippage in basis points (50 = 0.5%); defaults to 50. The swap reverts if it would fill worse than quote*(1-slippage)"`
	DeadlineSec int64  `json:"deadline_seconds,omitempty" jsonschema:"seconds from now the swap stays valid once signed; defaults to 1200 (20m)"`
	ClientID    string `json:"client_id" jsonschema:"broadcast-idempotency uuid (echo into broadcast_evm_tx.client_id)"`
}

type BuildSwapOutput struct {
	Payload     payload.EVMTxPayload `json:"payload"`
	ExpectedOut string               `json:"expected_out"` // human, at build-time reserves
	MinOut      string               `json:"min_out"`      // human, the amountOutMin enforced on chain
	MinOutBase  string               `json:"min_out_base"` // base units (integer)
	SlippageBps int                  `json:"slippage_bps"` // effective slippage applied
}

// BuildSwap constructs an exact-input swap on the UniswapV2 router. It
// re-quotes at build time, applies slippage to set amountOutMin, picks the
// right entrypoint for the native/ERC-20 combination, and (for token-input
// swaps) checks the router's allowance first — returning a clear "approve
// first" error rather than a tx that would revert. Returns an EVMTxPayload —
// sign with sign_evm_transaction then broadcast_evm_tx.
func (h *Handlers) BuildSwap(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	in BuildSwapInput,
) (*mcp.CallToolResult, BuildSwapOutput, error) {
	tp, err := h.authorize(ctx, "build_swap")
	if err != nil {
		return nil, BuildSwapOutput{}, err
	}
	uni, err := h.requireSwap()
	if err != nil {
		return nil, BuildSwapOutput{}, err
	}

	inAddr, inNative, err := parseSwapToken(in.TokenIn)
	if err != nil {
		return nil, BuildSwapOutput{}, fmt.Errorf("token_in: %w", err)
	}
	outAddr, outNative, err := parseSwapToken(in.TokenOut)
	if err != nil {
		return nil, BuildSwapOutput{}, fmt.Errorf("token_out: %w", err)
	}
	plan, err := resolveSwapPlan(inAddr, inNative, outAddr, outNative, uni.WSVP())
	if err != nil {
		return nil, BuildSwapOutput{}, err
	}

	slippage := in.SlippageBps
	if slippage == 0 {
		slippage = defaultSlippageBps
	}

	from, err := ownerEthAddress(tp.Owner)
	if err != nil {
		return nil, BuildSwapOutput{}, err
	}

	decIn, err := h.tokenDecimals(ctx, inNative, inAddr)
	if err != nil {
		return nil, BuildSwapOutput{}, err
	}
	amountIn, err := humanToBaseUnits(in.AmountIn, decIn)
	if err != nil {
		return nil, BuildSwapOutput{}, fmt.Errorf("amount_in: %w", err)
	}
	amountInBase := amountIn.BigInt()

	// Token-input swaps pull via transferFrom — verify the router allowance
	// covers this swap before building, so the agent gets a structured
	// "approve first" instead of an on-chain revert after signing.
	if !inNative {
		if err := h.checkAllowance(ctx, uni, inAddr, from, amountInBase, in.AmountIn); err != nil {
			return nil, BuildSwapOutput{}, err
		}
	}

	amounts, err := h.quoteAmountsOut(ctx, uni, amountInBase, plan.path)
	if err != nil {
		return nil, BuildSwapOutput{}, err
	}
	expectedOut := amounts[len(amounts)-1]
	minOut, err := applySlippage(expectedOut, slippage)
	if err != nil {
		return nil, BuildSwapOutput{}, err
	}
	if minOut.Sign() <= 0 {
		return nil, BuildSwapOutput{}, userErrf("quoted output rounds to zero — amount_in is too small for this path")
	}

	deadlineSec := in.DeadlineSec
	if deadlineSec <= 0 {
		deadlineSec = int64(defaultSwapDeadline.Seconds())
	}
	deadline := big.NewInt(time.Now().Add(time.Duration(deadlineSec) * time.Second).Unix())

	var data []byte
	value := big.NewInt(0)
	switch plan.kind {
	case kindTokensForTokens:
		data, err = uni.PackSwapExactTokensForTokens(amountInBase, minOut, plan.path, from, deadline)
	case kindSVPForTokens:
		data, err = uni.PackSwapExactSVPForTokens(minOut, plan.path, from, deadline)
		value = amountInBase // native SVP rides as the tx value
	case kindTokensForSVP:
		data, err = uni.PackSwapExactTokensForSVP(amountInBase, minOut, plan.path, from, deadline)
	}
	if err != nil {
		return nil, BuildSwapOutput{}, err
	}

	decOut, err := h.tokenDecimals(ctx, outNative, outAddr)
	if err != nil {
		return nil, BuildSwapOutput{}, err
	}

	p, err := h.Deps.EVM.Assembler.Assemble(ctx, builder.EVMArgs{
		ClientID: in.ClientID,
		From:     from,
		To:       uni.Router(),
		Data:     data,
		Value:    value,
		Summary: payload.EVMSummary{
			ToolName: "build_swap",
			Description: fmt.Sprintf("swap %s %s -> >=%s %s (%.2f%% max slippage)",
				in.AmountIn, tokenLabel(inNative, inAddr),
				humanAmount(math.NewIntFromBigInt(minOut), decOut), tokenLabel(outNative, outAddr),
				float64(slippage)/100),
		},
	})
	if err != nil {
		return nil, BuildSwapOutput{}, err
	}
	return nil, BuildSwapOutput{
		Payload:     *p,
		ExpectedOut: humanAmount(math.NewIntFromBigInt(expectedOut), decOut),
		MinOut:      humanAmount(math.NewIntFromBigInt(minOut), decOut),
		MinOutBase:  minOut.String(),
		SlippageBps: slippage,
	}, nil
}

// checkAllowance reads the router's allowance on the input token and returns a
// user-facing "approve first" error if it does not cover amountIn.
func (h *Handlers) checkAllowance(ctx context.Context, uni *builder.UniswapV2, token, owner common.Address, amountIn *big.Int, amountHuman string) error {
	data, err := uni.PackAllowance(owner, uni.Router())
	if err != nil {
		return err
	}
	out, err := h.evmCall(ctx, token, data)
	if err != nil {
		return fmt.Errorf("read allowance for %s: %w", token.Hex(), err)
	}
	allowance, err := uni.UnpackAllowance(out)
	if err != nil {
		return err
	}
	if allowance.Cmp(amountIn) < 0 {
		return userErrf(
			"router allowance for %s is insufficient — call build_token_approval (token %s, amount >= %s) first, then retry build_swap",
			token.Hex(), token.Hex(), amountHuman)
	}
	return nil
}

// -- small shared helpers ----------------------------------------------

// tokenLabel renders a token for human-facing output: "native" for SVP, its
// upper-cased symbol for a known alias (e.g. "USDV"), else the 0x address.
func tokenLabel(native bool, addr common.Address) string {
	if native {
		return "native"
	}
	if sym, ok := knownTokenSymbol(addr); ok {
		return sym
	}
	return addr.Hex()
}

// addrsToHex renders a path as 0x strings for JSON output.
func addrsToHex(path []common.Address) []string {
	out := make([]string, len(path))
	for i, a := range path {
		out[i] = a.Hex()
	}
	return out
}
