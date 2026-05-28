package indexer

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
)

// HistoricalFundingResponse wraps Comlink's HistoricalFundingResponse.
type HistoricalFundingResponse struct {
	HistoricalFunding []json.RawMessage `json:"historicalFunding"`
}

// FundingPaymentsResponse wraps Comlink's FundingPaymentResponse.
type FundingPaymentsResponse struct {
	FundingPayments []json.RawMessage `json:"fundingPayments"`
}

// GetHistoricalFunding fetches GET /v4/historicalFunding/:ticker.
func (c *Client) GetHistoricalFunding(ctx context.Context, ticker string) (*HistoricalFundingResponse, error) {
	var resp HistoricalFundingResponse
	if err := c.get(ctx, "/historicalFunding/"+ticker, nil, &resp); err != nil {
		return nil, fmt.Errorf("GetHistoricalFunding %q: %w", ticker, err)
	}
	return &resp, nil
}

// GetFundingPayments fetches GET /v4/fundingPayments?address=&subaccountNumber=
func (c *Client) GetFundingPayments(ctx context.Context, address string, subaccountNumber uint32) (*FundingPaymentsResponse, error) {
	q := url.Values{}
	q.Set("address", address)
	q.Set("subaccountNumber", fmt.Sprintf("%d", subaccountNumber))
	var resp FundingPaymentsResponse
	if err := c.get(ctx, "/fundingPayments", q, &resp); err != nil {
		return nil, fmt.Errorf("GetFundingPayments %s/%d: %w", address, subaccountNumber, err)
	}
	return &resp, nil
}
