package chain

import (
	"context"
	"fmt"

	pricestypes "github.com/dydxprotocol/v4-chain/protocol/x/prices/types"
	"google.golang.org/grpc"
)

// PricesQueryClient is the v0.1 surface over prices.Query that the MCP
// server uses to back the get_oracle_price tool (live oracle price for a
// market id) and to populate the markets cache with MarketParams.
type PricesQueryClient interface {
	MarketPrice(ctx context.Context, marketID uint32) (pricestypes.MarketPrice, error)
	AllMarketPrices(ctx context.Context) ([]pricestypes.MarketPrice, error)
}

type pricesQueryClient struct {
	inner pricestypes.QueryClient
}

func NewPricesQueryClient(conn *grpc.ClientConn) PricesQueryClient {
	return &pricesQueryClient{inner: pricestypes.NewQueryClient(conn)}
}

func (c *pricesQueryClient) MarketPrice(ctx context.Context, marketID uint32) (pricestypes.MarketPrice, error) {
	resp, err := c.inner.MarketPrice(ctx, &pricestypes.QueryMarketPriceRequest{Id: marketID})
	if err != nil {
		return pricestypes.MarketPrice{}, fmt.Errorf("prices.Query/MarketPrice %d: %w", marketID, err)
	}
	return resp.MarketPrice, nil
}

func (c *pricesQueryClient) AllMarketPrices(ctx context.Context) ([]pricestypes.MarketPrice, error) {
	resp, err := c.inner.AllMarketPrices(ctx, &pricestypes.QueryAllMarketPricesRequest{})
	if err != nil {
		return nil, fmt.Errorf("prices.Query/AllMarketPrices: %w", err)
	}
	return resp.MarketPrices, nil
}
