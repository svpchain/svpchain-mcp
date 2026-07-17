package tools

import (
	"context"
	"fmt"

	"cosmossdk.io/math"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/svpchain/svpchain-mcp/lib/mcp/builder"
	"github.com/svpchain/svpchain-mcp/lib/mcp/indexer"
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
	if _, err := h.authorize(ctx, "list_markets"); err != nil {
		return nil, ListMarketsOutput{}, err
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
	if _, err := h.authorize(ctx, "get_market"); err != nil {
		return nil, GetMarketOutput{}, err
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
	if _, err := h.authorize(ctx, "get_orderbook"); err != nil {
		return nil, GetOrderbookOutput{}, err
	}
	ob, err := h.Deps.Indexer.GetOrderbook(ctx, in.Ticker)
	if err != nil {
		return nil, GetOrderbookOutput{}, err
	}
	return nil, GetOrderbookOutput{Orderbook: *ob}, nil
}

// -- get_oracle_price ---------------------------------------------------

// requireOracle returns the bound price feed, or a clean user error if the
// server was started without the EVM RPC + evm_oracle_addr config the
// get_oracle_price tool needs. Modeled on requireSwap (swap.go).
func (h *Handlers) requireOracle() (*builder.OracleFeed, error) {
	if h.Deps.Chain.EVM == nil {
		return nil, userErrf("EVM is not enabled on this server (no evm_rpc_url configured)")
	}
	if h.Deps.EVM.Oracle == nil {
		return nil, userErrf("oracle price feed is not enabled on this server (no evm_oracle_addr configured)")
	}
	return h.Deps.EVM.Oracle, nil
}

type GetOraclePriceInput struct{}

// GetOraclePriceOutput surfaces the EVM aggregator's latest price as both a
// decimal-adjusted human string and the raw int256, plus the feed metadata
// needed to interpret it.
type GetOraclePriceOutput struct {
	Oracle      string `json:"oracle"`       // 0x aggregator address read
	Description string `json:"description"`  // feed label, e.g. "BTC / USD"
	Decimals    int64  `json:"decimals"`     // feed decimals
	Price       string `json:"price"`        // decimal-adjusted answer
	PriceRaw    string `json:"price_raw"`    // raw int256 answer (base units)
	RoundID     string `json:"round_id"`     // latest round id
	UpdatedAt   int64  `json:"updated_at"`   // unix seconds the round was last updated
}

// GetOraclePrice reads the configured OffChainAggregator price feed via
// read-only eth_calls (description, decimals, latestRoundData) and returns the
// latest price. Read-only — no tx, no signing.
func (h *Handlers) GetOraclePrice(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	_ GetOraclePriceInput,
) (*mcp.CallToolResult, GetOraclePriceOutput, error) {
	if _, err := h.authorize(ctx, "get_oracle_price"); err != nil {
		return nil, GetOraclePriceOutput{}, err
	}
	oracle, err := h.requireOracle()
	if err != nil {
		return nil, GetOraclePriceOutput{}, err
	}
	feed := oracle.Address()

	descData, err := oracle.PackDescription()
	if err != nil {
		return nil, GetOraclePriceOutput{}, err
	}
	descOut, err := h.evmCall(ctx, feed, descData)
	if err != nil {
		return nil, GetOraclePriceOutput{}, fmt.Errorf("read description for %s (is it an aggregator?): %w", feed.Hex(), err)
	}
	desc, err := oracle.UnpackDescription(descOut)
	if err != nil {
		return nil, GetOraclePriceOutput{}, err
	}

	decData, err := oracle.PackDecimals()
	if err != nil {
		return nil, GetOraclePriceOutput{}, err
	}
	decOut, err := h.evmCall(ctx, feed, decData)
	if err != nil {
		return nil, GetOraclePriceOutput{}, fmt.Errorf("read decimals for %s: %w", feed.Hex(), err)
	}
	dec, err := oracle.UnpackDecimals(decOut)
	if err != nil {
		return nil, GetOraclePriceOutput{}, err
	}

	rdData, err := oracle.PackLatestRoundData()
	if err != nil {
		return nil, GetOraclePriceOutput{}, err
	}
	rdOut, err := h.evmCall(ctx, feed, rdData)
	if err != nil {
		return nil, GetOraclePriceOutput{}, fmt.Errorf("read latestRoundData for %s: %w", feed.Hex(), err)
	}
	rd, err := oracle.UnpackLatestRoundData(rdOut)
	if err != nil {
		return nil, GetOraclePriceOutput{}, err
	}

	return nil, GetOraclePriceOutput{
		Oracle:      feed.Hex(),
		Description: desc,
		Decimals:    int64(dec),
		Price:       humanAmount(math.NewIntFromBigInt(rd.Answer), int64(dec)),
		PriceRaw:    rd.Answer.String(),
		RoundID:     rd.RoundID.String(),
		UpdatedAt:   rd.UpdatedAt.Int64(),
	}, nil
}
