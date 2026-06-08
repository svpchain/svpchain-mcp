package builder

import (
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
)

// uniswap.go is the per-contract ABI layer for svpchain's UniswapV2Router02
// fork — the seam the swap tools sit on top of. It owns nothing chain-aware
// (nonce/gas/fees live in EVMAssembler); it only turns typed swap/approve
// arguments into calldata and decodes the router's read results.
//
// svpchain's router is a standard Uniswap V2 fork whose native-coin (SVP)
// entrypoints are renamed from the *ETH* originals (swapExactETHForTokens ->
// swapExactSVPForTokens, etc.). A function's 4-byte selector is keccak256 of
// its canonical signature, so the names below MUST match the deployed
// contract's names verbatim — a stray "ETH" here would compute a selector no
// pair on chain responds to.

// uniswapV2RouterABI is the minimal slice of UniswapV2Router02 the swap tools
// use: the read-only quote (getAmountsOut) plus the three exact-input swap
// entrypoints (one per native-side combination). Exact-output and
// fee-on-transfer variants are intentionally omitted until a tool needs them.
const uniswapV2RouterABI = `[
  {"name":"getAmountsOut","type":"function","stateMutability":"view","inputs":[{"name":"amountIn","type":"uint256"},{"name":"path","type":"address[]"}],"outputs":[{"name":"amounts","type":"uint256[]"}]},
  {"name":"swapExactTokensForTokens","type":"function","stateMutability":"nonpayable","inputs":[{"name":"amountIn","type":"uint256"},{"name":"amountOutMin","type":"uint256"},{"name":"path","type":"address[]"},{"name":"to","type":"address"},{"name":"deadline","type":"uint256"}],"outputs":[{"name":"amounts","type":"uint256[]"}]},
  {"name":"swapExactSVPForTokens","type":"function","stateMutability":"payable","inputs":[{"name":"amountOutMin","type":"uint256"},{"name":"path","type":"address[]"},{"name":"to","type":"address"},{"name":"deadline","type":"uint256"}],"outputs":[{"name":"amounts","type":"uint256[]"}]},
  {"name":"swapExactTokensForSVP","type":"function","stateMutability":"nonpayable","inputs":[{"name":"amountIn","type":"uint256"},{"name":"amountOutMin","type":"uint256"},{"name":"path","type":"address[]"},{"name":"to","type":"address"},{"name":"deadline","type":"uint256"}],"outputs":[{"name":"amounts","type":"uint256[]"}]}
]`

// erc20ABI is the slice of the ERC-20 standard the swap + balance flows need:
// approve the router to pull the input token, read the current allowance to
// decide whether an approval is even required, read decimals to convert human
// amounts, and read balanceOf for a holder's token balance (pure ERC-20s have
// no x/bank representation, so this is the only way to see them).
const erc20ABI = `[
  {"name":"approve","type":"function","stateMutability":"nonpayable","inputs":[{"name":"spender","type":"address"},{"name":"amount","type":"uint256"}],"outputs":[{"name":"","type":"bool"}]},
  {"name":"allowance","type":"function","stateMutability":"view","inputs":[{"name":"owner","type":"address"},{"name":"spender","type":"address"}],"outputs":[{"name":"","type":"uint256"}]},
  {"name":"decimals","type":"function","stateMutability":"view","inputs":[],"outputs":[{"name":"","type":"uint8"}]},
  {"name":"balanceOf","type":"function","stateMutability":"view","inputs":[{"name":"account","type":"address"}],"outputs":[{"name":"","type":"uint256"}]}
]`

// UniswapV2 binds the router + wrapped-native (WSVP) addresses of one
// deployment to the parsed ABIs. It is constructed once at wire time and shared
// by every swap tool call; all methods are read-only on it, so it is safe for
// concurrent use.
type UniswapV2 struct {
	router    common.Address
	wsvp      common.Address
	routerABI abi.ABI
	erc20ABI  abi.ABI
}

// NewUniswapV2 parses the embedded ABIs and binds them to a deployment's router
// and WSVP addresses. Returns an error only if the embedded ABI JSON fails to
// parse, which would be a build-time programming error.
func NewUniswapV2(router, wsvp common.Address) (*UniswapV2, error) {
	r, err := abi.JSON(strings.NewReader(uniswapV2RouterABI))
	if err != nil {
		return nil, fmt.Errorf("parse router abi: %w", err)
	}
	e, err := abi.JSON(strings.NewReader(erc20ABI))
	if err != nil {
		return nil, fmt.Errorf("parse erc20 abi: %w", err)
	}
	return &UniswapV2{router: router, wsvp: wsvp, routerABI: r, erc20ABI: e}, nil
}

// Router returns the router address swaps and quotes target.
func (u *UniswapV2) Router() common.Address { return u.router }

// WSVP returns the wrapped-native token address used as the path endpoint for
// native-SVP legs.
func (u *UniswapV2) WSVP() common.Address { return u.wsvp }

// -- router: quote -----------------------------------------------------

// PackGetAmountsOut encodes a getAmountsOut(amountIn, path) eth_call. The
// returned amounts[len-1] is the output the swap would yield at the current
// reserves (before slippage).
func (u *UniswapV2) PackGetAmountsOut(amountIn *big.Int, path []common.Address) ([]byte, error) {
	return u.routerABI.Pack("getAmountsOut", amountIn, path)
}

// UnpackAmounts decodes the uint256[] returned by getAmountsOut.
func (u *UniswapV2) UnpackAmounts(data []byte) ([]*big.Int, error) {
	out, err := u.routerABI.Unpack("getAmountsOut", data)
	if err != nil {
		return nil, fmt.Errorf("decode getAmountsOut: %w", err)
	}
	amounts, ok := out[0].([]*big.Int)
	if !ok || len(amounts) == 0 {
		return nil, fmt.Errorf("getAmountsOut returned an unexpected shape")
	}
	return amounts, nil
}

// -- router: swaps -----------------------------------------------------

// PackSwapExactTokensForTokens encodes an ERC-20 -> ERC-20 exact-input swap.
func (u *UniswapV2) PackSwapExactTokensForTokens(amountIn, amountOutMin *big.Int, path []common.Address, to common.Address, deadline *big.Int) ([]byte, error) {
	return u.routerABI.Pack("swapExactTokensForTokens", amountIn, amountOutMin, path, to, deadline)
}

// PackSwapExactSVPForTokens encodes a native-SVP -> ERC-20 exact-input swap.
// The input amount rides as the tx value, so it is not an ABI argument.
func (u *UniswapV2) PackSwapExactSVPForTokens(amountOutMin *big.Int, path []common.Address, to common.Address, deadline *big.Int) ([]byte, error) {
	return u.routerABI.Pack("swapExactSVPForTokens", amountOutMin, path, to, deadline)
}

// PackSwapExactTokensForSVP encodes an ERC-20 -> native-SVP exact-input swap.
func (u *UniswapV2) PackSwapExactTokensForSVP(amountIn, amountOutMin *big.Int, path []common.Address, to common.Address, deadline *big.Int) ([]byte, error) {
	return u.routerABI.Pack("swapExactTokensForSVP", amountIn, amountOutMin, path, to, deadline)
}

// -- erc20 -------------------------------------------------------------

// PackApprove encodes approve(spender, amount) on the input token.
func (u *UniswapV2) PackApprove(spender common.Address, amount *big.Int) ([]byte, error) {
	return u.erc20ABI.Pack("approve", spender, amount)
}

// PackAllowance encodes allowance(owner, spender) on the input token.
func (u *UniswapV2) PackAllowance(owner, spender common.Address) ([]byte, error) {
	return u.erc20ABI.Pack("allowance", owner, spender)
}

// UnpackAllowance decodes the uint256 returned by allowance.
func (u *UniswapV2) UnpackAllowance(data []byte) (*big.Int, error) {
	out, err := u.erc20ABI.Unpack("allowance", data)
	if err != nil {
		return nil, fmt.Errorf("decode allowance: %w", err)
	}
	v, ok := out[0].(*big.Int)
	if !ok {
		return nil, fmt.Errorf("allowance returned an unexpected shape")
	}
	return v, nil
}

// PackBalanceOf encodes balanceOf(account) on a token.
func (u *UniswapV2) PackBalanceOf(account common.Address) ([]byte, error) {
	return u.erc20ABI.Pack("balanceOf", account)
}

// UnpackBalanceOf decodes the uint256 returned by balanceOf.
func (u *UniswapV2) UnpackBalanceOf(data []byte) (*big.Int, error) {
	out, err := u.erc20ABI.Unpack("balanceOf", data)
	if err != nil {
		return nil, fmt.Errorf("decode balanceOf: %w", err)
	}
	v, ok := out[0].(*big.Int)
	if !ok {
		return nil, fmt.Errorf("balanceOf returned an unexpected shape")
	}
	return v, nil
}

// PackDecimals encodes decimals() on a token.
func (u *UniswapV2) PackDecimals() ([]byte, error) {
	return u.erc20ABI.Pack("decimals")
}

// UnpackDecimals decodes the uint8 returned by decimals().
func (u *UniswapV2) UnpackDecimals(data []byte) (uint8, error) {
	out, err := u.erc20ABI.Unpack("decimals", data)
	if err != nil {
		return 0, fmt.Errorf("decode decimals: %w", err)
	}
	v, ok := out[0].(uint8)
	if !ok {
		return 0, fmt.Errorf("decimals returned an unexpected shape")
	}
	return v, nil
}
