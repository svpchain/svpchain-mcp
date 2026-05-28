package markets

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"cosmossdk.io/log"

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
type Cache struct {
	indexer      *indexer.Client
	refreshEvery time.Duration
	logger       log.Logger

	mu       sync.RWMutex
	byTicker map[string]MarketMeta
}

// NewCache returns a Cache that refreshes every `refresh` (defaults to 60s
// when zero).
func NewCache(idx *indexer.Client, refresh time.Duration, logger log.Logger) *Cache {
	if refresh == 0 {
		refresh = 60 * time.Second
	}
	return &Cache{
		indexer:      idx,
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

// Refresh fetches all markets from the indexer and atomically swaps the
// in-memory table.
func (c *Cache) Refresh(ctx context.Context) error {
	resp, err := c.indexer.ListPerpetualMarkets(ctx)
	if err != nil {
		return fmt.Errorf("ListPerpetualMarkets: %w", err)
	}
	next := make(map[string]MarketMeta, len(resp.Markets))
	for ticker, m := range resp.Markets {
		// ClobPairID is a uint32 on-chain but the indexer returns it as a
		// string for JS-precision safety.
		clobPairID, err := strconv.ParseUint(m.ClobPairID, 10, 32)
		if err != nil {
			c.logger.Error("market has invalid clobPairId; skipping",
				"ticker", ticker, "value", m.ClobPairID)
			continue
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
