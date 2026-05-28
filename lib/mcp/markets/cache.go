package markets

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"sync"
	"time"

	"cosmossdk.io/log"

	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/chain"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/indexer"
)

// MarketMeta is the per-market constant set that every build_* tool path
// needs: clobPairId for the on-chain Order field, plus the conversion
// constants for the units package to translate human size/price into
// quantums/subticks. Pulled from the indexer's PerpetualMarketResponseObject.
type MarketMeta struct {
	Ticker                    string
	ClobPairID                uint32
	AtomicResolution          int32
	QuantumConversionExponent int32
	StepBaseQuantums          uint64
	SubticksPerTick           uint32
}

// Cache is a periodically-refreshed in-memory snapshot of market metadata
// keyed by ticker. Reads (ResolveTicker) are lock-free in the common
// case via sync.RWMutex.
//
// Refresh joins two sources:
//   - chain.ClobQueryClient.ClobPairAll — authoritative set of valid
//     clob_pair_id values. Any indexer entry whose clob_pair_id is not in
//     this set is dropped (with a logged warning), so the MCP server is
//     resilient to indexer ticker→clobPairId drift.
//   - indexer.Client.ListPerpetualMarkets — human ticker strings + the
//     unit-conversion constants (atomicResolution, stepBaseQuantums,
//     subticksPerTick, quantumConversionExponent). The chain has these
//     too, but they're spread across clob/perpetuals/prices and need
//     more queries; the indexer pre-joins them.
type Cache struct {
	indexer      *indexer.Client
	clobQuery    chain.ClobQueryClient
	refreshEvery time.Duration
	logger       log.Logger

	mu       sync.RWMutex
	byTicker map[string]MarketMeta
}

// NewCache returns a Cache that refreshes every `refresh` (defaults to 60s
// when zero). clobQuery may be nil for tests that exercise only the
// indexer path; production callers (cmd/mcp-server/wire.go) always pass
// a real chain client.
func NewCache(idx *indexer.Client, clobQuery chain.ClobQueryClient, refresh time.Duration, logger log.Logger) *Cache {
	if refresh == 0 {
		refresh = 60 * time.Second
	}
	return &Cache{
		indexer:      idx,
		clobQuery:    clobQuery,
		refreshEvery: refresh,
		logger:       logger,
		byTicker:     make(map[string]MarketMeta),
	}
}

// Run blocks until ctx is done. It does one synchronous refresh on entry
// (so the server is not started with an empty cache) and then ticks.
// Errors from periodic refreshes are logged and the ticker continues —
// only the initial refresh propagates an error to the caller.
func (c *Cache) Run(ctx context.Context) error {
	if err := c.Refresh(ctx); err != nil {
		return fmt.Errorf("initial market cache refresh: %w", err)
	}
	t := time.NewTicker(c.refreshEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if err := c.Refresh(ctx); err != nil {
				c.logger.Error("market cache refresh failed", "error", err)
			}
		}
	}
}

// Refresh joins the chain's authoritative ClobPair set with the indexer's
// ticker + conversion metadata, then atomically swaps the in-memory table.
//
// Entries the indexer knows about but the chain does not (stale indexer
// state — what bit us in the first end-to-end run) are dropped, with the
// dropped tickers logged for diagnostics.
func (c *Cache) Refresh(ctx context.Context) error {
	// Source of truth for valid clob_pair_id values. nil clobQuery is
	// permitted for tests; we just skip the filter in that mode.
	var validIDs map[uint32]struct{}
	if c.clobQuery != nil {
		chainPairs, err := c.clobQuery.ClobPairAll(ctx)
		if err != nil {
			return fmt.Errorf("clob.Query/ClobPairAll: %w", err)
		}
		validIDs = make(map[uint32]struct{}, len(chainPairs))
		for _, p := range chainPairs {
			validIDs[p.Id] = struct{}{}
		}
	}

	resp, err := c.indexer.ListPerpetualMarkets(ctx)
	if err != nil {
		return fmt.Errorf("ListPerpetualMarkets: %w", err)
	}

	next := make(map[string]MarketMeta, len(resp.Markets))
	var dropped []string
	for ticker, m := range resp.Markets {
		// ClobPairID is a uint32 on-chain but the indexer returns it as a
		// string for JS-precision safety.
		clobPairID, err := strconv.ParseUint(m.ClobPairID, 10, 32)
		if err != nil {
			c.logger.Error("market has invalid clobPairId; skipping",
				"ticker", ticker, "value", m.ClobPairID)
			continue
		}
		if validIDs != nil {
			if _, ok := validIDs[uint32(clobPairID)]; !ok {
				dropped = append(dropped, ticker)
				continue
			}
		}
		next[ticker] = MarketMeta{
			Ticker:                    ticker,
			ClobPairID:                uint32(clobPairID),
			AtomicResolution:          m.AtomicResolution,
			QuantumConversionExponent: m.QuantumConversionExponent,
			StepBaseQuantums:          m.StepBaseQuantums,
			SubticksPerTick:           m.SubticksPerTick,
		}
	}
	if len(dropped) > 0 {
		sort.Strings(dropped) // stable log output
		c.logger.Info("markets cache: dropped indexer entries not present on chain",
			"tickers", dropped)
	}

	c.mu.Lock()
	c.byTicker = next
	c.mu.Unlock()
	return nil
}

// ResolveTicker returns the cached MarketMeta for ticker, if known. It is
// the single entry point used by build_* paths so the ticker→clobPairId
// + conversion constants lookup happens in exactly one place.
func (c *Cache) ResolveTicker(ticker string) (MarketMeta, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	m, ok := c.byTicker[ticker]
	return m, ok
}

// Size reports how many markets are currently cached (for diagnostics).
func (c *Cache) Size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.byTicker)
}
