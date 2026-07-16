package builder

import (
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
)

// lendora.go is the per-contract ABI layer for the Lendora money markets — a
// Compound V2 fork of CErc20 markets governed by a single Comptroller — the seam
// the lendora_* tools sit on top of. Like bridge.go / uniswap.go it binds the one
// config-supplied singleton (the Comptroller) to its parsed ABI and owns nothing
// chain-aware (nonce/gas/fees live in EVMAssembler); it only turns typed
// arguments into calldata and decodes the reads the tools need.
//
// Three ABIs live here, all parsed once at construction:
//   - CErc20 (address-independent — every market answers the same methods, so the
//     cToken address is passed per call, like erc.go's unbound ERC-20).
//   - Comptroller (bound to the deployment's single Comptroller address).
//   - SVPChainPriceOracle (address resolved at runtime via comptroller.oracle();
//     the parsed ABI is shared).
//
// A function's 4-byte selector is keccak256 of its canonical signature, so the
// names + argument types below MUST match the deployed contracts verbatim. These
// match the Compound V2 CErc20 / Comptroller / SVPChainPriceOracle signatures
// (pragma ^0.8.10). Note the fork-specific shapes: markets() drops its internal
// mapping and returns a 3-tuple, and the oracle prices assets in native SVP.

// lendoraCTokenABI is the CErc20 slice the lendora_* tools need: the five write
// entrypoints (mint/redeem/redeemUnderlying/borrow/repayBorrow, each returning a
// uint error code) plus the read surface for market data + account snapshots.
// symbol() is the ERC-20 metadata getter (used for both the cToken and its
// underlying); decimals() is read via the shared erc.go ERC-20 ABI, not here.
const lendoraCTokenABI = `[
  {"name":"mint","type":"function","stateMutability":"nonpayable","inputs":[{"name":"mintAmount","type":"uint256"}],"outputs":[{"name":"","type":"uint256"}]},
  {"name":"redeem","type":"function","stateMutability":"nonpayable","inputs":[{"name":"redeemTokens","type":"uint256"}],"outputs":[{"name":"","type":"uint256"}]},
  {"name":"redeemUnderlying","type":"function","stateMutability":"nonpayable","inputs":[{"name":"redeemAmount","type":"uint256"}],"outputs":[{"name":"","type":"uint256"}]},
  {"name":"borrow","type":"function","stateMutability":"nonpayable","inputs":[{"name":"borrowAmount","type":"uint256"}],"outputs":[{"name":"","type":"uint256"}]},
  {"name":"repayBorrow","type":"function","stateMutability":"nonpayable","inputs":[{"name":"repayAmount","type":"uint256"}],"outputs":[{"name":"","type":"uint256"}]},
  {"name":"underlying","type":"function","stateMutability":"view","inputs":[],"outputs":[{"name":"","type":"address"}]},
  {"name":"symbol","type":"function","stateMutability":"view","inputs":[],"outputs":[{"name":"","type":"string"}]},
  {"name":"exchangeRateStored","type":"function","stateMutability":"view","inputs":[],"outputs":[{"name":"","type":"uint256"}]},
  {"name":"supplyRatePerBlock","type":"function","stateMutability":"view","inputs":[],"outputs":[{"name":"","type":"uint256"}]},
  {"name":"borrowRatePerBlock","type":"function","stateMutability":"view","inputs":[],"outputs":[{"name":"","type":"uint256"}]},
  {"name":"getCash","type":"function","stateMutability":"view","inputs":[],"outputs":[{"name":"","type":"uint256"}]},
  {"name":"totalBorrows","type":"function","stateMutability":"view","inputs":[],"outputs":[{"name":"","type":"uint256"}]},
  {"name":"totalReserves","type":"function","stateMutability":"view","inputs":[],"outputs":[{"name":"","type":"uint256"}]},
  {"name":"reserveFactorMantissa","type":"function","stateMutability":"view","inputs":[],"outputs":[{"name":"","type":"uint256"}]},
  {"name":"interestRateModel","type":"function","stateMutability":"view","inputs":[],"outputs":[{"name":"","type":"address"}]},
  {"name":"balanceOf","type":"function","stateMutability":"view","inputs":[{"name":"owner","type":"address"}],"outputs":[{"name":"","type":"uint256"}]},
  {"name":"getAccountSnapshot","type":"function","stateMutability":"view","inputs":[{"name":"account","type":"address"}],"outputs":[{"name":"error","type":"uint256"},{"name":"cTokenBalance","type":"uint256"},{"name":"borrowBalance","type":"uint256"},{"name":"exchangeRateMantissa","type":"uint256"}]}
]`

// lendoraComptrollerABI is the Comptroller slice: the two collateral-management
// write entrypoints plus the risk/market reads. markets() returns a 3-tuple —
// the internal accountMembership mapping is omitted from the public getter.
const lendoraComptrollerABI = `[
  {"name":"enterMarkets","type":"function","stateMutability":"nonpayable","inputs":[{"name":"cTokens","type":"address[]"}],"outputs":[{"name":"","type":"uint256[]"}]},
  {"name":"exitMarket","type":"function","stateMutability":"nonpayable","inputs":[{"name":"cTokenAddress","type":"address"}],"outputs":[{"name":"","type":"uint256"}]},
  {"name":"getAllMarkets","type":"function","stateMutability":"view","inputs":[],"outputs":[{"name":"","type":"address[]"}]},
  {"name":"markets","type":"function","stateMutability":"view","inputs":[{"name":"cToken","type":"address"}],"outputs":[{"name":"isListed","type":"bool"},{"name":"collateralFactorMantissa","type":"uint256"},{"name":"isComped","type":"bool"}]},
  {"name":"getAccountLiquidity","type":"function","stateMutability":"view","inputs":[{"name":"account","type":"address"}],"outputs":[{"name":"error","type":"uint256"},{"name":"liquidity","type":"uint256"},{"name":"shortfall","type":"uint256"}]},
  {"name":"getHypotheticalAccountLiquidity","type":"function","stateMutability":"view","inputs":[{"name":"account","type":"address"},{"name":"cTokenModify","type":"address"},{"name":"redeemTokens","type":"uint256"},{"name":"borrowAmount","type":"uint256"}],"outputs":[{"name":"error","type":"uint256"},{"name":"liquidity","type":"uint256"},{"name":"shortfall","type":"uint256"}]},
  {"name":"getAssetsIn","type":"function","stateMutability":"view","inputs":[{"name":"account","type":"address"}],"outputs":[{"name":"","type":"address[]"}]},
  {"name":"closeFactorMantissa","type":"function","stateMutability":"view","inputs":[],"outputs":[{"name":"","type":"uint256"}]},
  {"name":"liquidationIncentiveMantissa","type":"function","stateMutability":"view","inputs":[],"outputs":[{"name":"","type":"uint256"}]},
  {"name":"oracle","type":"function","stateMutability":"view","inputs":[],"outputs":[{"name":"","type":"address"}]},
  {"name":"borrowCaps","type":"function","stateMutability":"view","inputs":[{"name":"cToken","type":"address"}],"outputs":[{"name":"","type":"uint256"}]},
  {"name":"mintGuardianPaused","type":"function","stateMutability":"view","inputs":[{"name":"cToken","type":"address"}],"outputs":[{"name":"","type":"bool"}]},
  {"name":"borrowGuardianPaused","type":"function","stateMutability":"view","inputs":[{"name":"cToken","type":"address"}],"outputs":[{"name":"","type":"bool"}]}
]`

// lendoraOracleABI is the SVPChainPriceOracle slice: the native-denominated price
// getter plus the per-asset USD feed lookups the read tools use to render USD
// values (cTokenToFeed → an 8-dec Chainlink feed; cEtherAddress identifies the
// native market whose feed is the SVP/USD denominator).
const lendoraOracleABI = `[
  {"name":"getUnderlyingPrice","type":"function","stateMutability":"view","inputs":[{"name":"cToken","type":"address"}],"outputs":[{"name":"","type":"uint256"}]},
  {"name":"cTokenToFeed","type":"function","stateMutability":"view","inputs":[{"name":"cToken","type":"address"}],"outputs":[{"name":"","type":"address"}]},
  {"name":"cEtherAddress","type":"function","stateMutability":"view","inputs":[],"outputs":[{"name":"","type":"address"}]},
  {"name":"underlyingDecimals","type":"function","stateMutability":"view","inputs":[{"name":"cToken","type":"address"}],"outputs":[{"name":"","type":"uint8"}]}
]`

// lendoraIRMABI is the one interest-rate-model getter the APY math needs:
// blocksPerYear, read on the model resolved via cToken.interestRateModel().
const lendoraIRMABI = `[
  {"name":"blocksPerYear","type":"function","stateMutability":"view","inputs":[],"outputs":[{"name":"","type":"uint256"}]}
]`

// Lendora binds the deployment's single Comptroller address to the parsed
// Comptroller ABI and holds the address-independent CErc20 / oracle / IRM ABIs.
// It is constructed once at wire time and shared by every lendora_* call; all
// methods are read-only on it, so it is safe for concurrent use.
type Lendora struct {
	comptroller    common.Address
	ctokenABI      abi.ABI
	comptrollerABI abi.ABI
	oracleABI      abi.ABI
	irmABI         abi.ABI
}

// NewLendora parses the embedded ABIs and binds the Comptroller ABI to a
// deployment's Comptroller address. Returns an error only if an embedded ABI JSON
// fails to parse, which would be a build-time programming error (cf. NewBridge).
func NewLendora(comptroller common.Address) (*Lendora, error) {
	ct, err := abi.JSON(strings.NewReader(lendoraCTokenABI))
	if err != nil {
		return nil, fmt.Errorf("parse lendora ctoken abi: %w", err)
	}
	comp, err := abi.JSON(strings.NewReader(lendoraComptrollerABI))
	if err != nil {
		return nil, fmt.Errorf("parse lendora comptroller abi: %w", err)
	}
	orc, err := abi.JSON(strings.NewReader(lendoraOracleABI))
	if err != nil {
		return nil, fmt.Errorf("parse lendora oracle abi: %w", err)
	}
	irm, err := abi.JSON(strings.NewReader(lendoraIRMABI))
	if err != nil {
		return nil, fmt.Errorf("parse lendora irm abi: %w", err)
	}
	return &Lendora{comptroller: comptroller, ctokenABI: ct, comptrollerABI: comp, oracleABI: orc, irmABI: irm}, nil
}

// Comptroller returns the Comptroller address the enter/exit market + risk reads
// target (the tx / eth_call `to`).
func (l *Lendora) Comptroller() common.Address { return l.comptroller }

// -- shared ABI decode helpers -----------------------------------------
//
// The read surface is broad, so these typed helpers keep each public Unpack*
// terse while preserving the "unexpected shape" guard the rest of the builder
// package uses. Every helper wraps the method name into its error.

func abiUnpackUint(a abi.ABI, method string, data []byte) (*big.Int, error) {
	out, err := a.Unpack(method, data)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", method, err)
	}
	v, ok := out[0].(*big.Int)
	if !ok {
		return nil, fmt.Errorf("%s returned an unexpected shape", method)
	}
	return v, nil
}

func abiUnpackAddress(a abi.ABI, method string, data []byte) (common.Address, error) {
	out, err := a.Unpack(method, data)
	if err != nil {
		return common.Address{}, fmt.Errorf("decode %s: %w", method, err)
	}
	v, ok := out[0].(common.Address)
	if !ok {
		return common.Address{}, fmt.Errorf("%s returned an unexpected shape", method)
	}
	return v, nil
}

func abiUnpackAddresses(a abi.ABI, method string, data []byte) ([]common.Address, error) {
	out, err := a.Unpack(method, data)
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", method, err)
	}
	v, ok := out[0].([]common.Address)
	if !ok {
		return nil, fmt.Errorf("%s returned an unexpected shape", method)
	}
	return v, nil
}

func abiUnpackBool(a abi.ABI, method string, data []byte) (bool, error) {
	out, err := a.Unpack(method, data)
	if err != nil {
		return false, fmt.Errorf("decode %s: %w", method, err)
	}
	v, ok := out[0].(bool)
	if !ok {
		return false, fmt.Errorf("%s returned an unexpected shape", method)
	}
	return v, nil
}

func abiUnpackString(a abi.ABI, method string, data []byte) (string, error) {
	out, err := a.Unpack(method, data)
	if err != nil {
		return "", fmt.Errorf("decode %s: %w", method, err)
	}
	v, ok := out[0].(string)
	if !ok {
		return "", fmt.Errorf("%s returned an unexpected shape", method)
	}
	return v, nil
}

// -- CErc20 write calls (tx `to` = the cToken, supplied per call) ------

// PackMint encodes mint(mintAmount) — supply underlying, receive cTokens. The
// caller must have approved the cToken to spend `amount` of the underlying first.
// amount is in underlying base units.
func (l *Lendora) PackMint(amount *big.Int) ([]byte, error) {
	return l.ctokenABI.Pack("mint", amount)
}

// PackRedeem encodes redeem(redeemTokens) — burn cTokens for underlying.
// redeemTokens is in cToken base units.
func (l *Lendora) PackRedeem(tokens *big.Int) ([]byte, error) {
	return l.ctokenABI.Pack("redeem", tokens)
}

// PackRedeemUnderlying encodes redeemUnderlying(redeemAmount) — withdraw a
// specified amount of underlying. amount is in underlying base units.
func (l *Lendora) PackRedeemUnderlying(amount *big.Int) ([]byte, error) {
	return l.ctokenABI.Pack("redeemUnderlying", amount)
}

// PackBorrow encodes borrow(borrowAmount). amount is in underlying base units.
func (l *Lendora) PackBorrow(amount *big.Int) ([]byte, error) {
	return l.ctokenABI.Pack("borrow", amount)
}

// PackRepayBorrow encodes repayBorrow(repayAmount) — repay the sender's own
// borrow. Pass 2^256-1 to repay the full outstanding balance. amount is in
// underlying base units.
func (l *Lendora) PackRepayBorrow(amount *big.Int) ([]byte, error) {
	return l.ctokenABI.Pack("repayBorrow", amount)
}

// -- CErc20 reads (eth_call `to` = the cToken) -------------------------

// PackUnderlying encodes the underlying() getter. Resolves a cToken's underlying
// ERC-20 (for its decimals + the mint/repay allowance check + symbol).
func (l *Lendora) PackUnderlying() ([]byte, error) { return l.ctokenABI.Pack("underlying") }

// UnpackUnderlying decodes the address returned by underlying().
func (l *Lendora) UnpackUnderlying(data []byte) (common.Address, error) {
	return abiUnpackAddress(l.ctokenABI, "underlying", data)
}

// PackSymbol encodes symbol() (ERC-20 metadata; works for a cToken or its
// underlying). UnpackSymbol decodes the string.
func (l *Lendora) PackSymbol() ([]byte, error) { return l.ctokenABI.Pack("symbol") }
func (l *Lendora) UnpackSymbol(data []byte) (string, error) {
	return abiUnpackString(l.ctokenABI, "symbol", data)
}

// PackExchangeRateStored / PackSupplyRatePerBlock / PackBorrowRatePerBlock /
// PackGetCash / PackTotalBorrows / PackTotalReserves / PackReserveFactorMantissa
// encode the no-arg uint256 market reads. Each has a matching UnpackUint256
// decode via UnpackCTokenUint.
func (l *Lendora) PackExchangeRateStored() ([]byte, error) {
	return l.ctokenABI.Pack("exchangeRateStored")
}
func (l *Lendora) PackSupplyRatePerBlock() ([]byte, error) {
	return l.ctokenABI.Pack("supplyRatePerBlock")
}
func (l *Lendora) PackBorrowRatePerBlock() ([]byte, error) {
	return l.ctokenABI.Pack("borrowRatePerBlock")
}
func (l *Lendora) PackGetCash() ([]byte, error)       { return l.ctokenABI.Pack("getCash") }
func (l *Lendora) PackTotalBorrows() ([]byte, error)  { return l.ctokenABI.Pack("totalBorrows") }
func (l *Lendora) PackTotalReserves() ([]byte, error) { return l.ctokenABI.Pack("totalReserves") }
func (l *Lendora) PackReserveFactorMantissa() ([]byte, error) {
	return l.ctokenABI.Pack("reserveFactorMantissa")
}

// UnpackCTokenUint decodes any of the no-arg uint256 cToken reads above. `method`
// is the read's name (e.g. "exchangeRateStored"), used only for the error label.
func (l *Lendora) UnpackCTokenUint(method string, data []byte) (*big.Int, error) {
	return abiUnpackUint(l.ctokenABI, method, data)
}

// PackInterestRateModel encodes interestRateModel(); UnpackInterestRateModel
// decodes the model address (whose blocksPerYear the APY math reads).
func (l *Lendora) PackInterestRateModel() ([]byte, error) {
	return l.ctokenABI.Pack("interestRateModel")
}
func (l *Lendora) UnpackInterestRateModel(data []byte) (common.Address, error) {
	return abiUnpackAddress(l.ctokenABI, "interestRateModel", data)
}

// PackBalanceOf encodes balanceOf(account) — the per-account cToken (or, reused
// for the underlying ERC-20, wallet) balance read.
func (l *Lendora) PackBalanceOf(account common.Address) ([]byte, error) {
	return l.ctokenABI.Pack("balanceOf", account)
}
func (l *Lendora) UnpackBalanceOf(data []byte) (*big.Int, error) {
	return abiUnpackUint(l.ctokenABI, "balanceOf", data)
}

// AccountSnapshot is the decoded getAccountSnapshot tuple (the error code is
// checked and dropped). CTokenBalance is in cToken units; BorrowBalance in
// underlying units; ExchangeRate is the 1e(18+underlyingDec-8) mantissa.
type AccountSnapshot struct {
	CTokenBalance *big.Int
	BorrowBalance *big.Int
	ExchangeRate  *big.Int
}

// PackGetAccountSnapshot encodes getAccountSnapshot(account).
func (l *Lendora) PackGetAccountSnapshot(account common.Address) ([]byte, error) {
	return l.ctokenABI.Pack("getAccountSnapshot", account)
}

// UnpackGetAccountSnapshot decodes the (error, cTokenBalance, borrowBalance,
// exchangeRateMantissa) tuple, returning an error if the contract error code is
// nonzero.
func (l *Lendora) UnpackGetAccountSnapshot(data []byte) (AccountSnapshot, error) {
	out, err := l.ctokenABI.Unpack("getAccountSnapshot", data)
	if err != nil {
		return AccountSnapshot{}, fmt.Errorf("decode getAccountSnapshot: %w", err)
	}
	if len(out) != 4 {
		return AccountSnapshot{}, fmt.Errorf("getAccountSnapshot returned an unexpected shape")
	}
	code, _ := out[0].(*big.Int)
	bal, ok1 := out[1].(*big.Int)
	borrow, ok2 := out[2].(*big.Int)
	rate, ok3 := out[3].(*big.Int)
	if code == nil || !ok1 || !ok2 || !ok3 {
		return AccountSnapshot{}, fmt.Errorf("getAccountSnapshot returned an unexpected shape")
	}
	if code.Sign() != 0 {
		return AccountSnapshot{}, fmt.Errorf("getAccountSnapshot returned error code %s", code)
	}
	return AccountSnapshot{CTokenBalance: bal, BorrowBalance: borrow, ExchangeRate: rate}, nil
}

// -- Comptroller reads (eth_call `to` = the Comptroller) ---------------

// PackGetAllMarkets encodes getAllMarkets(); UnpackGetAllMarkets decodes the
// cToken address list.
func (l *Lendora) PackGetAllMarkets() ([]byte, error) { return l.comptrollerABI.Pack("getAllMarkets") }
func (l *Lendora) UnpackGetAllMarkets(data []byte) ([]common.Address, error) {
	return abiUnpackAddresses(l.comptrollerABI, "getAllMarkets", data)
}

// MarketInfo is the decoded markets() 3-tuple (accountMembership is omitted by
// the public getter). CollateralFactorMantissa is 1e18-scaled (0..1).
type MarketInfo struct {
	IsListed                 bool
	CollateralFactorMantissa *big.Int
	IsComped                 bool
}

// PackMarkets encodes markets(cToken).
func (l *Lendora) PackMarkets(cToken common.Address) ([]byte, error) {
	return l.comptrollerABI.Pack("markets", cToken)
}

// UnpackMarkets decodes the (isListed, collateralFactorMantissa, isComped) tuple.
func (l *Lendora) UnpackMarkets(data []byte) (MarketInfo, error) {
	out, err := l.comptrollerABI.Unpack("markets", data)
	if err != nil {
		return MarketInfo{}, fmt.Errorf("decode markets: %w", err)
	}
	if len(out) != 3 {
		return MarketInfo{}, fmt.Errorf("markets returned an unexpected shape")
	}
	listed, ok1 := out[0].(bool)
	cf, ok2 := out[1].(*big.Int)
	comped, ok3 := out[2].(bool)
	if !ok1 || !ok2 || !ok3 {
		return MarketInfo{}, fmt.Errorf("markets returned an unexpected shape")
	}
	return MarketInfo{IsListed: listed, CollateralFactorMantissa: cf, IsComped: comped}, nil
}

// AccountLiquidity is the decoded (liquidity, shortfall) margin (native-SVP,
// 1e18). Healthy iff Shortfall == 0. The error code is checked and dropped.
type AccountLiquidity struct {
	Liquidity *big.Int
	Shortfall *big.Int
}

// PackGetAccountLiquidity encodes getAccountLiquidity(account).
func (l *Lendora) PackGetAccountLiquidity(account common.Address) ([]byte, error) {
	return l.comptrollerABI.Pack("getAccountLiquidity", account)
}

// PackGetHypotheticalAccountLiquidity encodes the what-if variant used by the op
// simulations: the account's liquidity if it additionally redeemed redeemTokens
// (cToken units) and borrowed borrowAmount (underlying units) of cTokenModify.
func (l *Lendora) PackGetHypotheticalAccountLiquidity(account, cTokenModify common.Address, redeemTokens, borrowAmount *big.Int) ([]byte, error) {
	return l.comptrollerABI.Pack("getHypotheticalAccountLiquidity", account, cTokenModify, redeemTokens, borrowAmount)
}

// UnpackAccountLiquidity decodes the (error, liquidity, shortfall) tuple returned
// by both getAccountLiquidity and its hypothetical variant. `method` labels errors.
func (l *Lendora) UnpackAccountLiquidity(method string, data []byte) (AccountLiquidity, error) {
	out, err := l.comptrollerABI.Unpack(method, data)
	if err != nil {
		return AccountLiquidity{}, fmt.Errorf("decode %s: %w", method, err)
	}
	if len(out) != 3 {
		return AccountLiquidity{}, fmt.Errorf("%s returned an unexpected shape", method)
	}
	code, _ := out[0].(*big.Int)
	liq, ok1 := out[1].(*big.Int)
	short, ok2 := out[2].(*big.Int)
	if code == nil || !ok1 || !ok2 {
		return AccountLiquidity{}, fmt.Errorf("%s returned an unexpected shape", method)
	}
	if code.Sign() != 0 {
		return AccountLiquidity{}, fmt.Errorf("%s returned error code %s", method, code)
	}
	return AccountLiquidity{Liquidity: liq, Shortfall: short}, nil
}

// PackGetAssetsIn encodes getAssetsIn(account); UnpackGetAssetsIn decodes the
// list of markets the account has entered as collateral.
func (l *Lendora) PackGetAssetsIn(account common.Address) ([]byte, error) {
	return l.comptrollerABI.Pack("getAssetsIn", account)
}
func (l *Lendora) UnpackGetAssetsIn(data []byte) ([]common.Address, error) {
	return abiUnpackAddresses(l.comptrollerABI, "getAssetsIn", data)
}

// PackCloseFactorMantissa / PackLiquidationIncentiveMantissa encode the global
// risk-param getters; UnpackComptrollerUint decodes them.
func (l *Lendora) PackCloseFactorMantissa() ([]byte, error) {
	return l.comptrollerABI.Pack("closeFactorMantissa")
}
func (l *Lendora) PackLiquidationIncentiveMantissa() ([]byte, error) {
	return l.comptrollerABI.Pack("liquidationIncentiveMantissa")
}
func (l *Lendora) PackBorrowCaps(cToken common.Address) ([]byte, error) {
	return l.comptrollerABI.Pack("borrowCaps", cToken)
}
func (l *Lendora) UnpackComptrollerUint(method string, data []byte) (*big.Int, error) {
	return abiUnpackUint(l.comptrollerABI, method, data)
}

// PackOracle encodes oracle(); UnpackOracle decodes the PriceOracle address.
func (l *Lendora) PackOracle() ([]byte, error) { return l.comptrollerABI.Pack("oracle") }
func (l *Lendora) UnpackOracle(data []byte) (common.Address, error) {
	return abiUnpackAddress(l.comptrollerABI, "oracle", data)
}

// PackMintGuardianPaused / PackBorrowGuardianPaused encode the per-market pause
// flags; UnpackComptrollerBool decodes them.
func (l *Lendora) PackMintGuardianPaused(cToken common.Address) ([]byte, error) {
	return l.comptrollerABI.Pack("mintGuardianPaused", cToken)
}
func (l *Lendora) PackBorrowGuardianPaused(cToken common.Address) ([]byte, error) {
	return l.comptrollerABI.Pack("borrowGuardianPaused", cToken)
}
func (l *Lendora) UnpackComptrollerBool(method string, data []byte) (bool, error) {
	return abiUnpackBool(l.comptrollerABI, method, data)
}

// -- Comptroller write calls -------------------------------------------

// PackEnterMarkets encodes enterMarkets(cTokens) — enable the listed markets as
// collateral for the sender.
func (l *Lendora) PackEnterMarkets(cTokens []common.Address) ([]byte, error) {
	return l.comptrollerABI.Pack("enterMarkets", cTokens)
}

// PackExitMarket encodes exitMarket(cTokenAddress) — remove a market from the
// sender's collateral.
func (l *Lendora) PackExitMarket(cToken common.Address) ([]byte, error) {
	return l.comptrollerABI.Pack("exitMarket", cToken)
}

// -- SVPChainPriceOracle reads (eth_call `to` = the oracle address) ----

// PackGetUnderlyingPrice encodes getUnderlyingPrice(cToken) — native-SVP price,
// scaled 1e(36-underlyingDecimals). UnpackOracleUint decodes it.
func (l *Lendora) PackGetUnderlyingPrice(cToken common.Address) ([]byte, error) {
	return l.oracleABI.Pack("getUnderlyingPrice", cToken)
}

// PackCTokenToFeed encodes cTokenToFeed(cToken) — the per-asset 8-dec USD
// Chainlink feed address (for USD display). UnpackOracleAddress decodes it.
func (l *Lendora) PackCTokenToFeed(cToken common.Address) ([]byte, error) {
	return l.oracleABI.Pack("cTokenToFeed", cToken)
}

// PackCEtherAddress encodes cEtherAddress() — the native (cSVP) market whose feed
// is the SVP/USD denominator.
func (l *Lendora) PackCEtherAddress() ([]byte, error) { return l.oracleABI.Pack("cEtherAddress") }

func (l *Lendora) UnpackOracleUint(method string, data []byte) (*big.Int, error) {
	return abiUnpackUint(l.oracleABI, method, data)
}
func (l *Lendora) UnpackOracleAddress(method string, data []byte) (common.Address, error) {
	return abiUnpackAddress(l.oracleABI, method, data)
}

// -- interest rate model read (eth_call `to` = the model address) ------

// PackBlocksPerYear encodes blocksPerYear(); UnpackBlocksPerYear decodes the
// annualization constant used by the APY math.
func (l *Lendora) PackBlocksPerYear() ([]byte, error) { return l.irmABI.Pack("blocksPerYear") }
func (l *Lendora) UnpackBlocksPerYear(data []byte) (*big.Int, error) {
	return abiUnpackUint(l.irmABI, "blocksPerYear", data)
}
