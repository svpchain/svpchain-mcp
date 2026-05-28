package indexer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

// PnlResponse wraps Comlink's GET /v4/pnl response.
type PnlResponse struct {
	HistoricalPnl []json.RawMessage `json:"historicalPnl"`
}

// HistoricalPnlResponse wraps Comlink's HistoricalPnlResponse (paginated).
type HistoricalPnlResponse struct {
	HistoricalPnl []json.RawMessage `json:"historicalPnl"`
	PageSize      uint32            `json:"pageSize,omitempty"`
	TotalResults  uint32            `json:"totalResults,omitempty"`
	Offset        uint32            `json:"offset,omitempty"`
}

// GetPnl fetches GET /v4/pnl?address=&subaccountNumber=
func (c *Client) GetPnl(ctx context.Context, address string, subaccountNumber uint32) (*PnlResponse, error) {
	q := url.Values{}
	q.Set("address", address)
	q.Set("subaccountNumber", fmt.Sprintf("%d", subaccountNumber))
	var resp PnlResponse
	if err := c.get(ctx, "/pnl", q, &resp); err != nil {
		return nil, fmt.Errorf("GetPnl %s/%d: %w", address, subaccountNumber, err)
	}
	return &resp, nil
}

// GetHistoricalPnl fetches GET /v4/historical-pnl?address=&subaccountNumber=
func (c *Client) GetHistoricalPnl(ctx context.Context, address string, subaccountNumber uint32) (*HistoricalPnlResponse, error) {
	q := url.Values{}
	q.Set("address", address)
	q.Set("subaccountNumber", fmt.Sprintf("%d", subaccountNumber))
	var resp HistoricalPnlResponse
	if err := c.get(ctx, "/historical-pnl", q, &resp); err != nil {
		return nil, fmt.Errorf("GetHistoricalPnl %s/%d: %w", address, subaccountNumber, err)
	}
	return &resp, nil
}
