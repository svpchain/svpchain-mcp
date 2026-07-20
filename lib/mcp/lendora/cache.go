// Package lendora holds the EVM-sourced market cache for the Lendora
// (Compound V2 fork) lending tools. It resolves a user-facing asset — an
// underlying symbol like "USDC" or a 0x cToken/underlying address — to the
// on-chain market metadata every lendora_* tool needs, so the tools never hard-
// code addresses. It mirrors lib/mcp/markets.Cache (periodic refresh, atomic
// swap, lock-free reads) but sources from eth_call against the Comptroller +
// cTokens rather than the chain's gRPC.
package lendora

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"cosmossdk.io/log"
	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"

	"github.com/svpchain/svpchain-mcp/lib/mcp/builder"
	"github.com/svpchain/svpchain-mcp/lib/mcp/chain"
)

// Market is the per-market constant set the lendora_* tools resolve an asset to.
// Symbol is the UNDERLYING token symbol (e.g. "USDC"), the name users pass; the
// cToken's own symbol is "cUSDC". IsCEther marks the native (cSVP) market, which
// has no ERC-20 underlying (Underlying is the zero address there).
type Market struct {
	Symbol             string
	CToken             common.Address
	Underlying         common.Address
	CTokenDecimals     int64
	UnderlyingDecimals int64
	IsCEther           bool
}

// Cache is a periodically-refreshed snapshot of the Lendora markets keyed by
// underlying symbol and by both addresses, plus the resolved PriceOracle address
// (read once per refresh from comptroller.oracle()). Reads take an RLock.
type Cache struct {
	client       chain.EVMClient
	lend         *builder.Lendora
	refreshEvery time.Duration
	logger       log.Logger

	mu        sync.RWMutex
	byKey     map[string]Market // lower-cased symbol AND lower-cased 0x addresses
	ordered   []Market          // stable getAllMarkets order, for enumeration
	oracle    common.Address
	oracleSet bool
}

// NewCache returns a Cache refreshing every `refresh` (defaults to 60s when
// zero). client and lend must be non-nil; production wiring passes the real EVM
// client + the Lendora ABI binding, tests pass a mock client.
func NewCache(client chain.EVMClient, lend *builder.Lendora, refresh time.Duration, logger log.Logger) *Cache {
	if refresh == 0 {
		refresh = 60 * time.Second
	}
	return &Cache{
		client:       client,
		lend:         lend,
		refreshEvery: refresh,
		logger:       logger,
		byKey:        make(map[string]Market),
	}
}

// Run blocks until ctx is done: one synchronous refresh on entry (so the server
// never starts with an empty cache), then a ticker. Only the initial refresh
// propagates its error; periodic failures are logged and the ticker continues.
func (c *Cache) Run(ctx context.Context) error {
	if err := c.Refresh(ctx); err != nil {
		return fmt.Errorf("initial lendora market cache refresh: %w", err)
	}
	t := time.NewTicker(c.refreshEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
			if err := c.Refresh(ctx); err != nil {
				c.logger.Error("lendora market cache refresh failed", "error", err)
			}
		}
	}
}

// call runs a read-only eth_call against `to` and returns the raw return bytes.
func (c *Cache) call(ctx context.Context, to common.Address, data []byte) ([]byte, error) {
	return c.client.CallContract(ctx, ethereum.CallMsg{To: &to, Data: data})
}

// Refresh enumerates the Comptroller's markets and joins each cToken's metadata,
// then atomically swaps the lookup table in. Per-market read failures drop that
// one market (logged) rather than failing the whole refresh; only enumeration or
// the oracle read can fail the refresh.
func (c *Cache) Refresh(ctx context.Context) error {
	comptroller := c.lend.Comptroller()

	oracleData, err := c.lend.PackOracle()
	if err != nil {
		return err
	}
	oracleOut, err := c.call(ctx, comptroller, oracleData)
	if err != nil {
		return fmt.Errorf("read comptroller.oracle(): %w", err)
	}
	oracleAddr, err := c.lend.UnpackOracle(oracleOut)
	if err != nil {
		return err
	}

	// cEtherAddress identifies the native market (which has no ERC-20 underlying);
	// a zero/absent value simply means there is no native market to special-case.
	cEther := c.resolveCEther(ctx, oracleAddr)

	marketsData, err := c.lend.PackGetAllMarkets()
	if err != nil {
		return err
	}
	marketsOut, err := c.call(ctx, comptroller, marketsData)
	if err != nil {
		return fmt.Errorf("read comptroller.getAllMarkets(): %w", err)
	}
	cTokens, err := c.lend.UnpackGetAllMarkets(marketsOut)
	if err != nil {
		return err
	}

	next := make(map[string]Market, len(cTokens)*3)
	ordered := make([]Market, 0, len(cTokens))
	var dropped []string
	for _, cToken := range cTokens {
		m, err := c.loadMarket(ctx, cToken, cEther)
		if err != nil {
			dropped = append(dropped, cToken.Hex())
			continue
		}
		ordered = append(ordered, m)
		next[strings.ToLower(m.Symbol)] = m
		next[strings.ToLower(cToken.Hex())] = m
		if !m.IsCEther {
			next[strings.ToLower(m.Underlying.Hex())] = m
		}
	}
	if len(dropped) > 0 {
		c.logger.Info("lendora market cache: dropped markets with unreadable metadata", "ctokens", dropped)
	}

	c.mu.Lock()
	c.byKey = next
	c.ordered = ordered
	c.oracle = oracleAddr
	c.oracleSet = true
	c.mu.Unlock()
	return nil
}

// resolveCEther reads the price oracle's cEtherAddress(). A zero result (unset,
// unpackable, or a failed call) simply means no native market to special-case —
// every non-native cToken is then read as CErc20, which is correct.
func (c *Cache) resolveCEther(ctx context.Context, oracleAddr common.Address) common.Address {
	if oracleAddr == (common.Address{}) {
		return common.Address{}
	}
	data, err := c.lend.PackCEtherAddress()
	if err != nil {
		return common.Address{}
	}
	out, err := c.call(ctx, oracleAddr, data)
	if err != nil {
		return common.Address{}
	}
	cEther, _ := c.lend.UnpackOracleAddress("cEtherAddress", out)
	return cEther
}

// loadMarket reads one cToken's metadata. The native (cSVP) market has no
// underlying() — it is detected by matching cEther and given a synthetic
// underlying (zero address, 18 decimals, symbol = cToken symbol minus its "c").
func (c *Cache) loadMarket(ctx context.Context, cToken, cEther common.Address) (Market, error) {
	cTokenDec, err := c.readDecimals(ctx, cToken)
	if err != nil {
		return Market{}, err
	}
	cTokenSym, err := c.readSymbol(ctx, cToken)
	if err != nil {
		return Market{}, err
	}

	if cEther != (common.Address{}) && cToken == cEther {
		return Market{
			Symbol:             strings.ToUpper(strings.TrimPrefix(cTokenSym, "c")),
			CToken:             cToken,
			Underlying:         common.Address{},
			CTokenDecimals:     cTokenDec,
			UnderlyingDecimals: 18,
			IsCEther:           true,
		}, nil
	}

	underData, err := c.lend.PackUnderlying()
	if err != nil {
		return Market{}, err
	}
	underOut, err := c.call(ctx, cToken, underData)
	if err != nil {
		return Market{}, fmt.Errorf("read underlying() for %s: %w", cToken.Hex(), err)
	}
	underlying, err := c.lend.UnpackUnderlying(underOut)
	if err != nil {
		return Market{}, err
	}
	underDec, err := c.readDecimals(ctx, underlying)
	if err != nil {
		return Market{}, err
	}
	underSym, err := c.readSymbol(ctx, underlying)
	if err != nil {
		return Market{}, err
	}
	return Market{
		Symbol:             strings.ToUpper(underSym),
		CToken:             cToken,
		Underlying:         underlying,
		CTokenDecimals:     cTokenDec,
		UnderlyingDecimals: underDec,
		IsCEther:           false,
	}, nil
}

func (c *Cache) readDecimals(ctx context.Context, token common.Address) (int64, error) {
	data, err := builder.PackERC20Decimals()
	if err != nil {
		return 0, err
	}
	out, err := c.call(ctx, token, data)
	if err != nil {
		return 0, fmt.Errorf("read decimals() for %s: %w", token.Hex(), err)
	}
	dec, err := builder.UnpackERC20Decimals(out)
	if err != nil {
		return 0, err
	}
	return int64(dec), nil
}

func (c *Cache) readSymbol(ctx context.Context, token common.Address) (string, error) {
	data, err := c.lend.PackSymbol()
	if err != nil {
		return "", err
	}
	out, err := c.call(ctx, token, data)
	if err != nil {
		return "", fmt.Errorf("read symbol() for %s: %w", token.Hex(), err)
	}
	return c.lend.UnpackSymbol(out)
}

// Resolve returns the market for an asset given as an underlying symbol
// (case-insensitive) or a 0x cToken/underlying address.
func (c *Cache) Resolve(asset string) (Market, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	m, ok := c.byKey[strings.ToLower(strings.TrimSpace(asset))]
	return m, ok
}

// LoadMarket reads a single market's metadata on demand (bypassing the cache),
// for a cToken address the periodic refresh has not captured yet — a fresh
// listing or a market dropped by a transient refresh error. ok=false when the
// address is not a readable market. Native detection uses the last-refreshed
// oracle address, so the native (cSVP/cEther) cToken resolves correctly here too
// (it has no underlying() to read); the result is not cached.
func (c *Cache) LoadMarket(ctx context.Context, cToken common.Address) (Market, bool) {
	oracleAddr, _ := c.Oracle()
	m, err := c.loadMarket(ctx, cToken, c.resolveCEther(ctx, oracleAddr))
	if err != nil {
		return Market{}, false
	}
	return m, true
}

// All returns the cached markets in getAllMarkets order (for enumeration tools).
func (c *Cache) All() []Market {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Market, len(c.ordered))
	copy(out, c.ordered)
	return out
}

// Oracle returns the resolved PriceOracle address (comptroller.oracle()). ok is
// false until the first successful refresh.
func (c *Cache) Oracle() (common.Address, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.oracle, c.oracleSet
}

// Size reports how many markets are currently cached (for diagnostics).
func (c *Cache) Size() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.ordered)
}
