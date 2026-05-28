package indexer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

// CandlesResponse wraps Comlink's GET /v4/candles/perpetualMarkets/:ticker
// response. Individual candles are kept as json.RawMessage so the agent
// sees Comlink's schema verbatim and we don't have to track field drift.
type CandlesResponse struct {
	Candles []json.RawMessage `json:"candles"`
}

// GetCandlesArgs are the query params accepted by Comlink (all optional).
type GetCandlesArgs struct {
	Resolution string // e.g. "1MIN", "5MINS", "1HOUR", "1DAY"
	Limit      uint32
	FromISO    string // RFC3339
	ToISO      string // RFC3339
}

// GetCandles fetches GET /v4/candles/perpetualMarkets/:ticker.
func (c *Client) GetCandles(ctx context.Context, ticker string, args GetCandlesArgs) (*CandlesResponse, error) {
	q := url.Values{}
	if args.Resolution != "" {
		q.Set("resolution", args.Resolution)
	}
	if args.Limit > 0 {
		q.Set("limit", fmt.Sprintf("%d", args.Limit))
	}
	if args.FromISO != "" {
		q.Set("fromISO", args.FromISO)
	}
	if args.ToISO != "" {
		q.Set("toISO", args.ToISO)
	}
	var resp CandlesResponse
	if err := c.get(ctx, "/candles/perpetualMarkets/"+ticker, q, &resp); err != nil {
		return nil, fmt.Errorf("GetCandles %q: %w", ticker, err)
	}
	return &resp, nil
}
