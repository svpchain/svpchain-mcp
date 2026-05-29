package builder_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"cosmossdk.io/log"
	"github.com/stretchr/testify/require"

	// testOwner, newTestAsm, and the app/config blank-import live in
	// testutil_test.go (same package).
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/builder"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/indexer"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/markets"
	clobtypes "github.com/dydxprotocol/v4-chain/protocol/x/clob/types"
)

// stubClobQuery returns a fixed set of ClobPair ids — needed so the cache's
// chain/indexer join keeps our seeded BTC-USD entry.
type stubClobQuery struct{ ids []uint32 }

func (s *stubClobQuery) ClobPairAll(_ context.Context) ([]clobtypes.ClobPair, error) {
	out := make([]clobtypes.ClobPair, 0, len(s.ids))
	for _, id := range s.ids {
		out = append(out, clobtypes.ClobPair{Id: id})
	}
	return out, nil
}

func newBtcCache(t *testing.T) *markets.Cache {
	t.Helper()
	mkt := map[string]any{
		"clobPairId":                "0",
		"ticker":                    "BTC-USD",
		"status":                    "ACTIVE",
		"oraclePrice":               "65000.00",
		"priceChange24H":            "0",
		"volume24H":                 "0",
		"trades24H":                 0,
		"nextFundingRate":           "0",
		"initialMarginFraction":     "0.05",
		"maintenanceMarginFraction": "0.03",
		"openInterest":              "0",
		"atomicResolution":          -10,
		"quantumConversionExponent": -9,
		"tickSize":                  "0.01",
		"stepSize":                  "0.000001",
		"stepBaseQuantums":          1_000_000,
		"subticksPerTick":           1_000,
		"marketType":                "CROSS",
		"baseOpenInterest":          "0",
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"markets": map[string]any{"BTC-USD": mkt},
		})
	}))
	t.Cleanup(srv.Close)

	idx := indexer.NewClient(srv.URL, indexer.Options{})
	c := markets.NewCache(idx, &stubClobQuery{ids: []uint32{0}}, time.Hour, log.NewNopLogger())
	require.NoError(t, c.Refresh(context.Background()))
	return c
}

func TestBuildPlaceLimitOrder_Happy(t *testing.T) {
	cache := newBtcCache(t)
	msg, p, err := builder.BuildPlaceLimitOrder(builder.PlaceLimitOrderInput{
		Owner:           testOwner,
		Ticker:          "BTC-USD",
		Side:            "BUY",
		HumanSize:       "0.05",
		HumanPrice:      "65000.00",
		TimeInForce:     "",
		GoodTilBlock:    100,
		OrderClientID:   1,
		PayloadClientID: "uuid-limit",
	}, cache, newTestAsm(t), 7, 17)
	require.NoError(t, err)
	require.Equal(t, clobtypes.Order_SIDE_BUY, msg.Order.Side)
	require.Equal(t, clobtypes.OrderIdFlags_ShortTerm, msg.Order.OrderId.OrderFlags)
	require.Equal(t, uint32(100), msg.Order.GetGoodTilBlock())
	require.True(t, p.IsShortTermCLOB)
}

func TestBuildPlaceMarketOrder_ExplicitWorstPrice(t *testing.T) {
	cache := newBtcCache(t)
	msg, p, err := builder.BuildPlaceMarketOrder(builder.PlaceMarketOrderInput{
		Owner:           testOwner,
		Ticker:          "BTC-USD",
		Side:            "BUY",
		HumanSize:       "0.05",
		WorstPrice:      "66000.00",
		GoodTilBlock:    100,
		OrderClientID:   2,
		PayloadClientID: "uuid-market-1",
	}, cache, newTestAsm(t), 7, 17)
	require.NoError(t, err)
	require.Equal(t, clobtypes.Order_TIME_IN_FORCE_IOC, msg.Order.TimeInForce, "market = IOC limit")
	require.True(t, p.IsShortTermCLOB)
	// 66000.00 → 6_600_000_000 subticks, already a multiple of SubticksPerTick=1000.
	require.Equal(t, uint64(6_600_000_000), msg.Order.Subticks)
}

func TestBuildPlaceMarketOrder_OraclePlusSlippage(t *testing.T) {
	cache := newBtcCache(t)
	msg, _, err := builder.BuildPlaceMarketOrder(builder.PlaceMarketOrderInput{
		Owner:           testOwner,
		Ticker:          "BTC-USD",
		Side:            "BUY",
		HumanSize:       "0.05",
		OraclePrice:     "65000.00",
		SlippageBps:     100, // 1%
		GoodTilBlock:    100,
		OrderClientID:   3,
		PayloadClientID: "uuid-market-2",
	}, cache, newTestAsm(t), 7, 17)
	require.NoError(t, err)
	// 65000 * 1.01 = 65650 → 6_565_000_000 subticks (tick-snap exact).
	require.Equal(t, uint64(6_565_000_000), msg.Order.Subticks)
	require.Equal(t, clobtypes.Order_TIME_IN_FORCE_IOC, msg.Order.TimeInForce)
}

func TestBuildPlaceMarketOrder_RequiresPriceStrategy(t *testing.T) {
	cache := newBtcCache(t)
	_, _, err := builder.BuildPlaceMarketOrder(builder.PlaceMarketOrderInput{
		Owner:           testOwner,
		Ticker:          "BTC-USD",
		Side:            "BUY",
		HumanSize:       "0.05",
		GoodTilBlock:    100,
		OrderClientID:   4,
		PayloadClientID: "uuid-market-3",
	}, cache, newTestAsm(t), 7, 17)
	require.Error(t, err)
	require.Contains(t, err.Error(), "worst_price")
}

func TestBuildPlaceConditionalOrder_StopLoss(t *testing.T) {
	cache := newBtcCache(t)
	msg, p, err := builder.BuildPlaceConditionalOrder(builder.PlaceConditionalOrderInput{
		Owner:            testOwner,
		Ticker:           "BTC-USD",
		Side:             "SELL",
		HumanSize:        "0.05",
		HumanPrice:       "60000.00", // limit when triggered
		ConditionType:    "STOP_LOSS",
		TriggerPrice:     "60500.00",
		GoodTilBlockTime: 1780000000,
		OrderClientID:    5,
		PayloadClientID:  "uuid-cond",
	}, cache, newTestAsm(t), 7, 17)
	require.NoError(t, err)
	require.Equal(t, clobtypes.OrderIdFlags_Conditional, msg.Order.OrderId.OrderFlags)
	require.Equal(t, clobtypes.Order_CONDITION_TYPE_STOP_LOSS, msg.Order.ConditionType)
	require.Equal(t, uint64(6_050_000_000), msg.Order.ConditionalOrderTriggerSubticks)
	require.Equal(t, uint32(1780000000), msg.Order.GetGoodTilBlockTime())
	require.False(t, p.IsShortTermCLOB, "conditional orders are stateful")
}

func TestBuildPlaceConditionalOrder_BadConditionType(t *testing.T) {
	cache := newBtcCache(t)
	_, _, err := builder.BuildPlaceConditionalOrder(builder.PlaceConditionalOrderInput{
		Owner:            testOwner,
		Ticker:           "BTC-USD",
		Side:             "SELL",
		HumanSize:        "0.05",
		HumanPrice:       "60000.00",
		ConditionType:    "TRAILING_STOP",
		TriggerPrice:     "60500.00",
		GoodTilBlockTime: 1780000000,
		OrderClientID:    6,
		PayloadClientID:  "uuid-cond-bad",
	}, cache, newTestAsm(t), 7, 17)
	require.Error(t, err)
	require.Contains(t, err.Error(), "condition_type")
}
