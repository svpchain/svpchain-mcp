package tools

import (
	"context"
	"encoding/json"
	"math/big"
	"strings"
	"testing"

	"cosmossdk.io/log"
	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	ethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/stretchr/testify/require"

	"github.com/svpchain/svpchain-mcp/lib/mcp/builder"
	"github.com/svpchain/svpchain-mcp/lib/mcp/lendora"
	"github.com/svpchain/svpchain-mcp/lib/mcp/payload"
	"github.com/svpchain/svpchain-mcp/lib/mcp/policy"
)

// lendSel is the 4-byte selector for a canonical signature.
func lendSel(sig string) []byte { return crypto.Keccak256([]byte(sig))[:4] }

// Fixed scenario addresses (one cUSDC market).
var (
	lendComp   = common.HexToAddress("0x00000000000000000000000000000000000000c0")
	lendOracle = common.HexToAddress("0x000000000000000000000000000000000000000a")
	lendFeed   = common.HexToAddress("0x00000000000000000000000000000000000000fe")
	lendIRM    = common.HexToAddress("0x0000000000000000000000000000000000000012")
	lendCUSDC  = common.HexToAddress("0x00000000000000000000000000000000000000c1")
	lendUSDC   = common.HexToAddress("0x0000000000000000000000000000000000000011")
	lendCSVP   = common.HexToAddress("0x00000000000000000000000000000000000000c5") // native (cSVP) market
)

// mustPack ABI-encodes vals against the given solidity type list — used to build
// the mock's eth_call return data exactly as the contracts would. Panics on a bad
// type/value (a test-authoring error).
func mustPack(types []string, vals ...any) []byte {
	var args abi.Arguments
	for _, ts := range types {
		ty, err := abi.NewType(ts, "", nil)
		if err != nil {
			panic(err)
		}
		args = append(args, abi.Argument{Type: ty})
	}
	b, err := args.Pack(vals...)
	if err != nil {
		panic(err)
	}
	return b
}

// abiPackTypes is mustPack with a *testing.T for symmetry in setup code.
func abiPackTypes(t *testing.T, types []string, vals ...any) []byte {
	t.Helper()
	return mustPack(types, vals...)
}

// lendScenario is a (to,selector)-keyed mock EVM answering every read the lendora
// tools make for a single cUSDC market. Amounts: USDC 6-dec, cToken 8-dec.
type lendScenario struct {
	ret       map[string][]byte
	allowance *big.Int
	hypoShort *big.Int
	hypoLiq   *big.Int
	borrowBal *big.Int // getAccountSnapshot borrow balance (underlying units)
	cTokenBal *big.Int // getAccountSnapshot cToken balance (cToken units)
	entered   bool     // whether getAssetsIn includes cUSDC
}

func key(to common.Address, sig string) string {
	return strings.ToLower(to.Hex()) + ":" + common.Bytes2Hex(lendSel(sig))
}

func newLendScenario(t *testing.T) *lendScenario {
	t.Helper()
	s := &lendScenario{
		ret:       map[string][]byte{},
		allowance: new(big.Int).Exp(big.NewInt(10), big.NewInt(30), nil), // ample by default
		hypoShort: big.NewInt(0),
		hypoLiq:   mustBig("50000000000000000000"), // 5e19
		borrowBal: big.NewInt(100_000_000),         // 100 USDC debt
		cTokenBal: mustBig("500000000000"),         // 5000 cUSDC supplied
		entered:   true,
	}
	u := func(to common.Address, sig, typ string, v any) {
		s.ret[key(to, sig)] = abiPackTypes(t, []string{typ}, v)
	}

	// Comptroller
	u(lendComp, "oracle()", "address", lendOracle)
	s.ret[key(lendComp, "getAllMarkets()")] = abiPackTypes(t, []string{"address[]"}, []common.Address{lendCUSDC})
	s.ret[key(lendComp, "markets(address)")] = abiPackTypes(t, []string{"bool", "uint256", "bool"}, true, mustBig("800000000000000000"), false) // cf 0.8
	s.ret[key(lendComp, "getAccountLiquidity(address)")] = abiPackTypes(t, []string{"uint256", "uint256", "uint256"},
		big.NewInt(0), mustBig("100000000000000000000"), big.NewInt(0)) // liq 1e20, short 0
	u(lendComp, "mintGuardianPaused(address)", "bool", false)
	u(lendComp, "borrowGuardianPaused(address)", "bool", false)
	u(lendComp, "closeFactorMantissa()", "uint256", mustBig("500000000000000000"))           // 0.5
	u(lendComp, "liquidationIncentiveMantissa()", "uint256", mustBig("1080000000000000000")) // 1.08
	u(lendComp, "borrowCaps(address)", "uint256", big.NewInt(0))                             // uncapped

	// cToken (cUSDC)
	u(lendCUSDC, "decimals()", "uint8", uint8(8))
	u(lendCUSDC, "symbol()", "string", "cUSDC")
	u(lendCUSDC, "underlying()", "address", lendUSDC)
	u(lendCUSDC, "exchangeRateStored()", "uint256", mustBig("20000000000000000")) // 2e16
	u(lendCUSDC, "supplyRatePerBlock()", "uint256", big.NewInt(1_000_000_000))
	u(lendCUSDC, "borrowRatePerBlock()", "uint256", big.NewInt(2_000_000_000))
	u(lendCUSDC, "getCash()", "uint256", big.NewInt(1_000_000_000))    // 1000 USDC
	u(lendCUSDC, "totalBorrows()", "uint256", big.NewInt(100_000_000)) // 100 USDC
	u(lendCUSDC, "totalReserves()", "uint256", big.NewInt(0))
	u(lendCUSDC, "reserveFactorMantissa()", "uint256", mustBig("100000000000000000")) // 0.1
	u(lendCUSDC, "interestRateModel()", "address", lendIRM)
	u(lendCUSDC, "balanceOf(address)", "uint256", mustBig("500000000000"))

	// Underlying (USDC)
	u(lendUSDC, "decimals()", "uint8", uint8(6))
	u(lendUSDC, "symbol()", "string", "USDC")
	u(lendUSDC, "balanceOf(address)", "uint256", big.NewInt(1_000_000_000))

	// Oracle
	u(lendOracle, "getUnderlyingPrice(address)", "uint256", mustBig("1000000000000000000000000000000")) // 1e30
	u(lendOracle, "cTokenToFeed(address)", "address", lendFeed)
	u(lendOracle, "cEtherAddress()", "address", common.Address{})

	// Chainlink USD feed ($1.00, 8-dec)
	u(lendFeed, "decimals()", "uint8", uint8(8))
	s.ret[key(lendFeed, "latestRoundData()")] = abiPackTypes(t,
		[]string{"uint80", "int256", "uint256", "uint256", "uint80"},
		big.NewInt(1), big.NewInt(100_000_000), big.NewInt(0), big.NewInt(1_000_000_000), big.NewInt(1))

	// Interest rate model
	u(lendIRM, "blocksPerYear()", "uint256", big.NewInt(31_536_000))

	return s
}

func mustBig(s string) *big.Int {
	v, ok := new(big.Int).SetString(s, 10)
	if !ok {
		panic("bad big: " + s)
	}
	return v
}

// withNativeMarket adds a native (cSVP) market to a scenario: getAllMarkets grows
// to [cUSDC, cSVP], the oracle's cEtherAddress() points at cSVP, and cSVP answers
// the per-cToken reads — but deliberately NOT underlying(), which reverts on a
// real CEther and must never be read for the native market. Returns s for chaining.
func withNativeMarket(t *testing.T, s *lendScenario) *lendScenario {
	t.Helper()
	u := func(to common.Address, sig, typ string, v any) {
		s.ret[key(to, sig)] = abiPackTypes(t, []string{typ}, v)
	}
	s.ret[key(lendComp, "getAllMarkets()")] = abiPackTypes(t, []string{"address[]"}, []common.Address{lendCUSDC, lendCSVP})
	u(lendOracle, "cEtherAddress()", "address", lendCSVP)

	u(lendCSVP, "decimals()", "uint8", uint8(8))
	u(lendCSVP, "symbol()", "string", "cSVP")
	u(lendCSVP, "exchangeRateStored()", "uint256", mustBig("20000000000000000")) // 2e16
	u(lendCSVP, "supplyRatePerBlock()", "uint256", big.NewInt(1_000_000_000))
	u(lendCSVP, "borrowRatePerBlock()", "uint256", big.NewInt(2_000_000_000))
	u(lendCSVP, "getCash()", "uint256", mustBig("1000000000000000000000")) // 1000 SVP
	u(lendCSVP, "totalBorrows()", "uint256", mustBig("100000000000000000000"))
	u(lendCSVP, "totalReserves()", "uint256", big.NewInt(0))
	u(lendCSVP, "reserveFactorMantissa()", "uint256", mustBig("100000000000000000"))
	u(lendCSVP, "interestRateModel()", "address", lendIRM)
	u(lendCSVP, "balanceOf(address)", "uint256", mustBig("500000000000"))
	return s
}

// lendHandlersNative wires a *Handlers whose cache holds both the cUSDC and the
// native cSVP market.
func lendHandlersNative(t *testing.T, s *lendScenario) (*Handlers, context.Context) {
	t.Helper()
	lend, err := builder.NewLendora(lendComp)
	require.NoError(t, err)
	cache := lendora.NewCache(s, lend, 0, log.NewNopLogger())
	require.NoError(t, cache.Refresh(context.Background()))
	require.Equal(t, 2, cache.Size())

	h := &Handlers{Deps: Deps{
		Chain:          ChainDeps{EVM: s},
		EVM:            EVMDeps{Assembler: builder.NewEVMAssembler(s), Lendora: lend},
		LendoraMarkets: cache,
		Policy:         policy.NewEngine([]policy.TenantPolicy{{TenantID: "t1", Owner: testTxOwner}}),
		RateLimit:      policy.NewRateLimiter(0, 0),
	}}
	ctx := WithTenant(context.Background(), TenantContext{TenantID: "t1", Owner: testTxOwner})
	return h, ctx
}

func (s *lendScenario) CallContract(_ context.Context, msg ethereum.CallMsg) ([]byte, error) {
	to := *msg.To
	sel := common.Bytes2Hex(msg.Data[:4])
	k := strings.ToLower(to.Hex()) + ":" + sel
	// Argument-dependent reads handled explicitly:
	switch {
	case sel == common.Bytes2Hex(lendSel("allowance(address,address)")):
		return mustPack([]string{"uint256"}, s.allowance), nil
	case sel == common.Bytes2Hex(lendSel("getHypotheticalAccountLiquidity(address,address,uint256,uint256)")):
		return mustPack([]string{"uint256", "uint256", "uint256"}, big.NewInt(0), s.hypoLiq, s.hypoShort), nil
	case sel == common.Bytes2Hex(lendSel("getAccountSnapshot(address)")):
		// (error, cTokenBalance, borrowBalance, exchangeRate 2e16)
		return mustPack([]string{"uint256", "uint256", "uint256", "uint256"},
			big.NewInt(0), s.cTokenBal, s.borrowBal, mustBig("20000000000000000")), nil
	case sel == common.Bytes2Hex(lendSel("getAssetsIn(address)")):
		if s.entered {
			return mustPack([]string{"address[]"}, []common.Address{lendCUSDC}), nil
		}
		return mustPack([]string{"address[]"}, []common.Address{}), nil
	}
	if v, ok := s.ret[k]; ok {
		return v, nil
	}
	return nil, nil
}

func (s *lendScenario) PendingNonceAt(context.Context, common.Address) (uint64, error) { return 7, nil }
func (s *lendScenario) EstimateGas(context.Context, ethereum.CallMsg) (uint64, error) {
	return 200_000, nil
}
func (s *lendScenario) SuggestGasTipCap(context.Context) (*big.Int, error) {
	return big.NewInt(1_000_000_000), nil
}
func (s *lendScenario) BaseFee(context.Context) (*big.Int, error) {
	return big.NewInt(2_000_000_000), nil
}
func (s *lendScenario) ChainID(context.Context) (*big.Int, error)   { return big.NewInt(262144), nil }
func (s *lendScenario) BlockNumber(context.Context) (uint64, error) { return 12_345, nil }
func (s *lendScenario) SendTransaction(context.Context, *ethtypes.Transaction) (string, error) {
	return "", nil
}
func (s *lendScenario) TransactionReceipt(context.Context, common.Hash) (*ethtypes.Receipt, error) {
	return nil, nil
}

// lendHandlers wires a *Handlers with the scenario mock and a refreshed cache.
func lendHandlers(t *testing.T, s *lendScenario) (*Handlers, context.Context) {
	t.Helper()
	lend, err := builder.NewLendora(lendComp)
	require.NoError(t, err)
	cache := lendora.NewCache(s, lend, 0, log.NewNopLogger())
	require.NoError(t, cache.Refresh(context.Background()))
	require.Equal(t, 1, cache.Size())

	h := &Handlers{Deps: Deps{
		Chain:          ChainDeps{EVM: s},
		EVM:            EVMDeps{Assembler: builder.NewEVMAssembler(s), Lendora: lend},
		LendoraMarkets: cache,
		Policy:         policy.NewEngine([]policy.TenantPolicy{{TenantID: "t1", Owner: testTxOwner}}),
		RateLimit:      policy.NewRateLimiter(0, 0),
	}}
	ctx := WithTenant(context.Background(), TenantContext{TenantID: "t1", Owner: testTxOwner})
	return h, ctx
}

func TestLendoraGetAllMarkets(t *testing.T) {
	h, ctx := lendHandlers(t, newLendScenario(t))
	_, out, err := h.LendoraGetAllMarkets(ctx, nil, LendoraGetAllMarketsInput{})
	require.NoError(t, err)
	require.Equal(t, uint64(12_345), out.BlockNumber)
	require.Len(t, out.Markets, 1)
	m := out.Markets[0]
	require.Equal(t, "USDC", m.Symbol)
	require.Equal(t, lendCUSDC.Hex(), m.CToken)
	require.Contains(t, m.SupplyAPY, "%")
	require.Contains(t, m.BorrowAPY, "%")
	require.Equal(t, "80.00%", m.CollateralFactor)
	// total supply underlying = cash + borrows − reserves = 1000 + 100 = 1100 USDC → $1,100.00
	require.Equal(t, "$1,100.00", m.TotalSupplyUSD)
	require.Equal(t, "$100.00", m.TotalBorrowUSD)
	require.False(t, m.MintPaused)
}

func TestLendoraAssessRisk_Healthy(t *testing.T) {
	h, ctx := lendHandlers(t, newLendScenario(t))
	_, out, err := h.LendoraAssessRisk(ctx, nil, LendoraAssessRiskInput{})
	require.NoError(t, err)
	require.True(t, out.HasDebt)
	require.False(t, out.Shortfall)
	// sumBorrow = price(1e30)*borrow(100e6)/1e18 = 1e20; net = liq(1e20); HF = (1e20+1e20)/1e20 = 2
	require.Equal(t, "2.000000000000000000", out.HealthFactor)
	require.Equal(t, "Low", out.RiskLevel)
	require.Equal(t, "🟢", out.RiskEmoji)
	require.Equal(t, "$100.00", out.TotalBorrowedUSD)
	require.Len(t, out.Positions, 1)
	require.True(t, out.Positions[0].CollateralEnabled)
}

func TestLendoraBuildSupplyTx_ApprovalRequired(t *testing.T) {
	s := newLendScenario(t)
	s.allowance = big.NewInt(0)
	h, ctx := lendHandlers(t, s)
	_, out, err := h.LendoraBuildSupplyTx(ctx, nil, LendoraSupplyInput{Asset: "USDC", Amount: "100", ClientID: "c1"})
	require.NoError(t, err)
	require.Nil(t, out.Payload)
	require.NotNil(t, out.ApprovalRequired)
	require.Equal(t, "build_erc20_approve", out.ApprovalRequired.Tool)
	require.Equal(t, lendUSDC.Hex(), out.ApprovalRequired.Token)
	require.Equal(t, lendCUSDC.Hex(), out.ApprovalRequired.Spender)
	require.Equal(t, "lendora_build_supply_tx", out.ApprovalRequired.RetryTool)
	// The simulation is still populated.
	require.Equal(t, "supply", out.Simulation.Action)
}

func TestLendoraBuildSupplyTx_Payload(t *testing.T) {
	h, ctx := lendHandlers(t, newLendScenario(t)) // ample allowance
	_, out, err := h.LendoraBuildSupplyTx(ctx, nil, LendoraSupplyInput{Asset: "USDC", Amount: "100", ClientID: "c2"})
	require.NoError(t, err)
	require.Nil(t, out.ApprovalRequired)
	require.NotNil(t, out.Payload)
	require.Equal(t, lendCUSDC.Hex(), out.Payload.To)
	require.Equal(t, common.Bytes2Hex(lendSel("mint(uint256)")), common.Bytes2Hex(common.FromHex(out.Payload.Data)[:4]))
}

func TestLendoraBuildBorrowTx_BlockedOnShortfall(t *testing.T) {
	s := newLendScenario(t)
	s.hypoShort = mustBig("1000000000000000000") // 1e18 shortfall
	h, ctx := lendHandlers(t, s)
	_, _, err := h.LendoraBuildBorrowTx(ctx, nil, LendoraBorrowInput{Asset: "USDC", Amount: "10000", ClientID: "c3"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "undercollateralized")
}

func TestLendoraBuildBorrowTx_OK(t *testing.T) {
	h, ctx := lendHandlers(t, newLendScenario(t)) // hypoShort 0
	_, out, err := h.LendoraBuildBorrowTx(ctx, nil, LendoraBorrowInput{Asset: "USDC", Amount: "10", ClientID: "c4"})
	require.NoError(t, err)
	require.NotNil(t, out.Payload)
	require.Equal(t, common.Bytes2Hex(lendSel("borrow(uint256)")), common.Bytes2Hex(common.FromHex(out.Payload.Data)[:4]))
	require.Equal(t, "borrow", out.Simulation.Action)
}

func TestLendoraBuildCollateralTx_EnterTargetsComptroller(t *testing.T) {
	h, ctx := lendHandlers(t, newLendScenario(t))
	_, out, err := h.LendoraBuildCollateralTx(ctx, nil, LendoraCollateralInput{Asset: "USDC", Action: "enable", ClientID: "c5"})
	require.NoError(t, err)
	require.NotNil(t, out.Payload)
	require.Equal(t, lendComp.Hex(), out.Payload.To)
	require.Equal(t, common.Bytes2Hex(lendSel("enterMarkets(address[])")), common.Bytes2Hex(common.FromHex(out.Payload.Data)[:4]))
}

func TestLendora_ResolveByAddress(t *testing.T) {
	h, ctx := lendHandlers(t, newLendScenario(t))
	// Resolve by cToken address instead of symbol.
	_, out, err := h.LendoraGetMarketDetails(ctx, nil, LendoraGetMarketDetailsInput{Asset: lendCUSDC.Hex()})
	require.NoError(t, err)
	require.Equal(t, "USDC", out.Market.Symbol)
	require.Equal(t, "10.00%", out.ReserveFactor)
}

func TestLendora_NotConfigured(t *testing.T) {
	h := &Handlers{Deps: Deps{
		Chain:     ChainDeps{EVM: newLendScenario(t)},
		EVM:       EVMDeps{Assembler: builder.NewEVMAssembler(newLendScenario(t))}, // Lendora + cache nil
		Policy:    policy.NewEngine([]policy.TenantPolicy{{TenantID: "t1", Owner: testTxOwner}}),
		RateLimit: policy.NewRateLimiter(0, 0),
	}}
	ctx := WithTenant(context.Background(), TenantContext{TenantID: "t1", Owner: testTxOwner})
	_, _, err := h.LendoraGetAllMarkets(ctx, nil, LendoraGetAllMarketsInput{})
	require.Error(t, err)
}

func TestLendoraBuildRepayTx_OverCapUsesMaxSentinel(t *testing.T) {
	// Debt is 100 USDC; asking to repay 200 must encode the repay-max sentinel
	// (2^256-1) rather than 200e6 (which would revert on-chain).
	h, ctx := lendHandlers(t, newLendScenario(t))
	_, out, err := h.LendoraBuildRepayTx(ctx, nil, LendoraRepayInput{Asset: "USDC", Amount: "200", ClientID: "r1"})
	require.NoError(t, err)
	require.NotNil(t, out.Payload)
	b := common.FromHex(out.Payload.Data)
	require.Equal(t, common.Bytes2Hex(lendSel("repayBorrow(uint256)")), common.Bytes2Hex(b[:4]))
	require.Equal(t, maxUint256, new(big.Int).SetBytes(b[4:4+32]))
	require.Equal(t, "full", out.Simulation.Amount)
}

func TestLendoraBuildRepayTx_PartialEncodesAmount(t *testing.T) {
	h, ctx := lendHandlers(t, newLendScenario(t))
	_, out, err := h.LendoraBuildRepayTx(ctx, nil, LendoraRepayInput{Asset: "USDC", Amount: "40", ClientID: "r2"})
	require.NoError(t, err)
	require.NotNil(t, out.Payload)
	b := common.FromHex(out.Payload.Data)
	require.Equal(t, big.NewInt(40_000_000), new(big.Int).SetBytes(b[4:4+32]))
}

func TestLendoraBuildRepayTx_FullNoDebt(t *testing.T) {
	s := newLendScenario(t)
	s.borrowBal = big.NewInt(0)
	h, ctx := lendHandlers(t, s)
	_, _, err := h.LendoraBuildRepayTx(ctx, nil, LendoraRepayInput{Asset: "USDC", Full: true, ClientID: "r3"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "no outstanding")
}

func TestLendoraBuildWithdrawTx_FullRedeemsCTokens(t *testing.T) {
	h, ctx := lendHandlers(t, newLendScenario(t))
	_, out, err := h.LendoraBuildWithdrawTx(ctx, nil, LendoraWithdrawInput{Asset: "USDC", Full: true, ClientID: "w1"})
	require.NoError(t, err)
	require.NotNil(t, out.Payload)
	b := common.FromHex(out.Payload.Data)
	require.Equal(t, common.Bytes2Hex(lendSel("redeem(uint256)")), common.Bytes2Hex(b[:4]))
	// redeems the full cToken balance (5000e8)
	require.Equal(t, mustBig("500000000000"), new(big.Int).SetBytes(b[4:4+32]))
}

func TestLendoraBuildSupplyTx_NotEnteredWarns(t *testing.T) {
	s := newLendScenario(t)
	s.entered = false // supplied but not collateral-enabled
	h, ctx := lendHandlers(t, s)
	_, out, err := h.LendoraBuildSupplyTx(ctx, nil, LendoraSupplyInput{Asset: "USDC", Amount: "100", ClientID: "s1"})
	require.NoError(t, err)
	require.NotNil(t, out.Payload)
	require.NotEmpty(t, out.Simulation.Warnings)
	require.Contains(t, out.Simulation.Warnings[0], "not enabled as collateral")
}

func TestLendoraBuildCollateralTx_DisableWithBorrowBlocked(t *testing.T) {
	h, ctx := lendHandlers(t, newLendScenario(t)) // has a 100 USDC borrow in cUSDC
	_, _, err := h.LendoraBuildCollateralTx(ctx, nil, LendoraCollateralInput{Asset: "USDC", Action: "disable", ClientID: "d1"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "outstanding borrow")
}

// TestLendoraReads_EmptyCollectionsAreNonNil guards the null-slice output-schema
// bug: a nil slice marshals to JSON null, which fails the go-sdk's "type":"array"
// output schema. An account/market set with no rows must marshal to [] not null.
func TestLendoraReads_EmptyPositionsMarshalToArray(t *testing.T) {
	s := newLendScenario(t)
	s.cTokenBal = big.NewInt(0) // no supply
	s.borrowBal = big.NewInt(0) // no borrow -> no position in any market
	s.entered = false
	// Also zero the wallet + cToken balances so get_balances is genuinely empty
	// (it reports wallet holdings independent of Lendora positions).
	s.ret[key(lendUSDC, "balanceOf(address)")] = mustPack([]string{"uint256"}, big.NewInt(0))
	s.ret[key(lendCUSDC, "balanceOf(address)")] = mustPack([]string{"uint256"}, big.NewInt(0))
	h, ctx := lendHandlers(t, s)

	_, out, err := h.LendoraGetAccountPositions(ctx, nil, LendoraGetAccountPositionsInput{})
	require.NoError(t, err)
	require.NotNil(t, out.Positions, "must be non-nil so it marshals to [] not null")
	require.Len(t, out.Positions, 0)
	b, err := json.Marshal(out)
	require.NoError(t, err)
	require.Contains(t, string(b), `"positions":[]`)
	require.NotContains(t, string(b), `"positions":null`)

	_, risk, err := h.LendoraAssessRisk(ctx, nil, LendoraAssessRiskInput{})
	require.NoError(t, err)
	rb, _ := json.Marshal(risk)
	require.Contains(t, string(rb), `"positions":[]`)

	_, bal, err := h.LendoraGetBalances(ctx, nil, LendoraGetBalancesInput{})
	require.NoError(t, err)
	require.NotNil(t, bal.Balances)
	bb, _ := json.Marshal(bal)
	require.Contains(t, string(bb), `"balances":[]`)
}

// -- native (cSVP) market -----------------------------------------------

// firstSel returns the 4-byte selector of a built payload's calldata.
func firstSel(t *testing.T, p *payload.EVMTxPayload) string {
	t.Helper()
	require.NotNil(t, p)
	return common.Bytes2Hex(common.FromHex(p.Data)[:4])
}

// The native market surfaces its is_native flag so a client can tell it apart
// from a CErc20 market whose only other distinguishing hint is a zero underlying.
func TestLendoraGetAllMarkets_NativeFlag(t *testing.T) {
	h, ctx := lendHandlersNative(t, withNativeMarket(t, newLendScenario(t)))
	_, out, err := h.LendoraGetAllMarkets(ctx, nil, LendoraGetAllMarketsInput{})
	require.NoError(t, err)
	var svp, usdc *MarketDTO
	for i := range out.Markets {
		switch out.Markets[i].Symbol {
		case "SVP":
			svp = &out.Markets[i]
		case "USDC":
			usdc = &out.Markets[i]
		}
	}
	require.NotNil(t, svp, "native SVP market present")
	require.True(t, svp.IsNative)
	require.NotNil(t, usdc)
	require.False(t, usdc.IsNative)
}

// borrow/withdraw/collateral use CEther-identical calldata and no msg.value, so
// they build against the native market unchanged once the gate is narrowed.
func TestLendoraBuildBorrowTx_Native(t *testing.T) {
	h, ctx := lendHandlersNative(t, withNativeMarket(t, newLendScenario(t)))
	_, out, err := h.LendoraBuildBorrowTx(ctx, nil, LendoraBorrowInput{Asset: "SVP", Amount: "1", ClientID: "nb1"})
	require.NoError(t, err)
	require.Equal(t, lendCSVP.Hex(), out.Payload.To)
	require.Equal(t, common.Bytes2Hex(lendSel("borrow(uint256)")), firstSel(t, out.Payload))
	require.Equal(t, "0", out.Payload.Value, "borrow is not payable")
}

func TestLendoraBuildWithdrawTx_Native(t *testing.T) {
	h, ctx := lendHandlersNative(t, withNativeMarket(t, newLendScenario(t)))
	_, out, err := h.LendoraBuildWithdrawTx(ctx, nil, LendoraWithdrawInput{Asset: "SVP", Full: true, ClientID: "nw1"})
	require.NoError(t, err)
	require.Equal(t, lendCSVP.Hex(), out.Payload.To)
	require.Equal(t, common.Bytes2Hex(lendSel("redeem(uint256)")), firstSel(t, out.Payload))
	require.Equal(t, "0", out.Payload.Value, "withdraw is not payable")
}

func TestLendoraBuildCollateralTx_Native(t *testing.T) {
	h, ctx := lendHandlersNative(t, withNativeMarket(t, newLendScenario(t)))
	_, out, err := h.LendoraBuildCollateralTx(ctx, nil, LendoraCollateralInput{Asset: "SVP", Action: "enable", ClientID: "nc1"})
	require.NoError(t, err)
	require.Equal(t, lendComp.Hex(), out.Payload.To)
	require.Equal(t, common.Bytes2Hex(lendSel("enterMarkets(address[])")), firstSel(t, out.Payload))
	require.Equal(t, "0", out.Payload.Value)
}

// supply/repay still refuse the native market — their payable no-arg CEther forms
// are out of scope — with a message naming the market, not the whole family.
func TestLendoraBuildSupplyTx_NativeRefused(t *testing.T) {
	h, ctx := lendHandlersNative(t, withNativeMarket(t, newLendScenario(t)))
	_, _, err := h.LendoraBuildSupplyTx(ctx, nil, LendoraSupplyInput{Asset: "SVP", Amount: "1", ClientID: "ns1"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "SVP")
	require.Contains(t, err.Error(), "not supported yet")
}

func TestLendoraBuildRepayTx_NativeRefused(t *testing.T) {
	h, ctx := lendHandlersNative(t, withNativeMarket(t, newLendScenario(t)))
	_, _, err := h.LendoraBuildRepayTx(ctx, nil, LendoraRepayInput{Asset: "SVP", Amount: "1", ClientID: "nr1"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "SVP")
	require.Contains(t, err.Error(), "not supported yet")
}
