package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	pricestypes "github.com/dydxprotocol/v4-chain/protocol/x/prices/types"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/indexer"
)

// -- list_markets -------------------------------------------------------

type ListMarketsInput struct{}

// ListMarketsOutput is a pass-through of the indexer's response.
type ListMarketsOutput struct {
	Markets map[string]indexer.PerpetualMarket `json:"markets" jsonschema:"map of ticker to perpetual market"`
}

func (h *Handlers) ListMarkets(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	_ ListMarketsInput,
) (*mcp.CallToolResult, ListMarketsOutput, error) {
	tc, ok := TenantFrom(ctx)
	if !ok {
		return nil, ListMarketsOutput{}, ErrNoTenant
	}
	if err := h.Deps.Policy.CheckTenant(tc.TenantID); err != nil {
		return nil, ListMarketsOutput{}, err
	}
	if !h.Deps.RateLimit.Allow("list_markets:" + tc.TenantID) {
		return nil, ListMarketsOutput{}, userErrf("rate limit exceeded")
	}
	resp, err := h.Deps.Indexer.ListPerpetualMarkets(ctx)
	if err != nil {
		return nil, ListMarketsOutput{}, err
	}
	return nil, ListMarketsOutput{Markets: resp.Markets}, nil
}

// -- get_market ---------------------------------------------------------

type GetMarketInput struct {
	Ticker string `json:"ticker" jsonschema:"perpetual market ticker, e.g. BTC-USD"`
}
type GetMarketOutput struct {
	Market indexer.PerpetualMarket `json:"market"`
}

func (h *Handlers) GetMarket(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	in GetMarketInput,
) (*mcp.CallToolResult, GetMarketOutput, error) {
	tc, ok := TenantFrom(ctx)
	if !ok {
		return nil, GetMarketOutput{}, ErrNoTenant
	}
	if err := h.Deps.Policy.CheckTenant(tc.TenantID); err != nil {
		return nil, GetMarketOutput{}, err
	}
	if !h.Deps.RateLimit.Allow("get_market:" + tc.TenantID) {
		return nil, GetMarketOutput{}, userErrf("rate limit exceeded")
	}
	m, err := h.Deps.Indexer.GetPerpetualMarket(ctx, in.Ticker)
	if err != nil {
		return nil, GetMarketOutput{}, err
	}
	return nil, GetMarketOutput{Market: *m}, nil
}

// -- get_orderbook ------------------------------------------------------

type GetOrderbookInput struct {
	Ticker string `json:"ticker" jsonschema:"perpetual market ticker"`
}
type GetOrderbookOutput struct {
	Orderbook indexer.Orderbook `json:"orderbook"`
}

func (h *Handlers) GetOrderbook(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	in GetOrderbookInput,
) (*mcp.CallToolResult, GetOrderbookOutput, error) {
	tc, ok := TenantFrom(ctx)
	if !ok {
		return nil, GetOrderbookOutput{}, ErrNoTenant
	}
	if err := h.Deps.Policy.CheckTenant(tc.TenantID); err != nil {
		return nil, GetOrderbookOutput{}, err
	}
	if !h.Deps.RateLimit.Allow("get_orderbook:" + tc.TenantID) {
		return nil, GetOrderbookOutput{}, userErrf("rate limit exceeded")
	}
	ob, err := h.Deps.Indexer.GetOrderbook(ctx, in.Ticker)
	if err != nil {
		return nil, GetOrderbookOutput{}, err
	}
	return nil, GetOrderbookOutput{Orderbook: *ob}, nil
}

// -- get_oracle_price ---------------------------------------------------

type GetOraclePriceInput struct {
	MarketID uint32 `json:"market_id" jsonschema:"prices module market id"`
}

// GetOraclePriceOutput surfaces the on-chain oracle price as both the raw
// (price * 10^exponent) form and as a derived float string.
type GetOraclePriceOutput struct {
	MarketID uint32 `json:"market_id"`
	Price    uint64 `json:"price"`
	Exponent int32  `json:"exponent"`
}

func (h *Handlers) GetOraclePrice(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	in GetOraclePriceInput,
) (*mcp.CallToolResult, GetOraclePriceOutput, error) {
	tc, ok := TenantFrom(ctx)
	if !ok {
		return nil, GetOraclePriceOutput{}, ErrNoTenant
	}
	if err := h.Deps.Policy.CheckTenant(tc.TenantID); err != nil {
		return nil, GetOraclePriceOutput{}, err
	}
	if !h.Deps.RateLimit.Allow("get_oracle_price:" + tc.TenantID) {
		return nil, GetOraclePriceOutput{}, userErrf("rate limit exceeded")
	}
	mp, err := h.Deps.Chain.PricesQuery.MarketPrice(ctx, in.MarketID)
	if err != nil {
		return nil, GetOraclePriceOutput{}, err
	}
	return nil, GetOraclePriceOutput{
		MarketID: mp.Id,
		Price:    mp.Price,
		Exponent: mp.Exponent,
	}, nil
}

// reference pricestypes import (kept for clarity; the type is used via
// the typed PricesQueryClient).
var _ = pricestypes.MarketPrice{}
