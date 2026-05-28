package markets_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"cosmossdk.io/log"
	"github.com/stretchr/testify/require"

	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/indexer"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/markets"
)

// stubMarkets returns an httptest.Server that answers GET
// /v4/perpetualMarkets with `markets`, plus exposes a *count that records
// how many times the endpoint was hit so the cache's Refresh behavior can
// be observed.
func stubMarkets(t *testing.T, markets map[string]any, hits *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v4/perpetualMarkets" {
			http.NotFound(w, r)
			return
		}
		if hits != nil {
			*hits++
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"markets": markets})
	}))
}

func goodMarket() map[string]any {
	return map[string]any{
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
}

func TestCache_RefreshAndResolve(t *testing.T) {
	hits := 0
	srv := stubMarkets(t, map[string]any{"BTC-USD": goodMarket()}, &hits)
	defer srv.Close()

	idx := indexer.NewClient(srv.URL, indexer.Options{})
	c := markets.NewCache(idx, time.Hour, log.NewNopLogger())

	require.NoError(t, c.Refresh(context.Background()))
	require.Equal(t, 1, c.Size())
	require.Equal(t, 1, hits, "Refresh must hit the indexer exactly once")

	meta, ok := c.ResolveTicker("BTC-USD")
	require.True(t, ok)
	require.Equal(t, "BTC-USD", meta.Ticker)
	require.Equal(t, uint32(0), meta.ClobPairID)
	require.Equal(t, int32(-10), meta.AtomicResolution)
	require.Equal(t, int32(-9), meta.QuantumConversionExponent)
	require.Equal(t, uint64(1_000_000), meta.StepBaseQuantums)
	require.Equal(t, uint32(1_000), meta.SubticksPerTick)

	_, ok = c.ResolveTicker("ETH-USD")
	require.False(t, ok, "unknown ticker must miss")
}

func TestCache_RefreshAtomicSwap(t *testing.T) {
	// Two markets in round 1; one in round 2. The cache must atomically
	// replace its table (no half-populated state visible) and a previously-
	// resolvable ticker must vanish if it's no longer in the refresh.
	hits := 0
	round := 1
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		body := map[string]any{"markets": map[string]any{"BTC-USD": goodMarket(), "ETH-USD": goodMarket()}}
		if round == 2 {
			body = map[string]any{"markets": map[string]any{"BTC-USD": goodMarket()}}
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer srv.Close()

	idx := indexer.NewClient(srv.URL, indexer.Options{})
	c := markets.NewCache(idx, time.Hour, log.NewNopLogger())

	require.NoError(t, c.Refresh(context.Background()))
	require.Equal(t, 2, c.Size())
	_, ok := c.ResolveTicker("ETH-USD")
	require.True(t, ok)

	round = 2
	require.NoError(t, c.Refresh(context.Background()))
	require.Equal(t, 1, c.Size())
	_, ok = c.ResolveTicker("ETH-USD")
	require.False(t, ok, "ticker dropped from upstream must disappear after refresh")
	_, ok = c.ResolveTicker("BTC-USD")
	require.True(t, ok)
}

func TestCache_SkipsBadClobPairID(t *testing.T) {
	bad := goodMarket()
	bad["clobPairId"] = "not-a-number"
	srv := stubMarkets(t, map[string]any{
		"BTC-USD": goodMarket(),
		"BAD":     bad,
	}, nil)
	defer srv.Close()

	idx := indexer.NewClient(srv.URL, indexer.Options{})
	c := markets.NewCache(idx, time.Hour, log.NewNopLogger())

	require.NoError(t, c.Refresh(context.Background()))
	require.Equal(t, 1, c.Size(), "entries with unparseable clobPairId must be skipped, others preserved")
	_, ok := c.ResolveTicker("BTC-USD")
	require.True(t, ok)
	_, ok = c.ResolveTicker("BAD")
	require.False(t, ok)
}

func TestCache_RefreshError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()

	idx := indexer.NewClient(srv.URL, indexer.Options{})
	c := markets.NewCache(idx, time.Hour, log.NewNopLogger())
	require.Error(t, c.Refresh(context.Background()))
	require.Equal(t, 0, c.Size())
}
