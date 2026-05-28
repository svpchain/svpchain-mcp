package indexer

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
)

// Subaccount mirrors Comlink's SubaccountResponseObject. Positions maps
// are kept as json.RawMessage in v0.1 — MCP tools pass them through to
// the agent as-is; v0.2 may type them when the policy engine needs to
// inspect positions for risk caps.
type Subaccount struct {
	Address                    string          `json:"address"`
	SubaccountNumber           int32           `json:"subaccountNumber"`
	Equity                     string          `json:"equity"`
	FreeCollateral             string          `json:"freeCollateral"`
	OpenPerpetualPositions     json.RawMessage `json:"openPerpetualPositions"`
	AssetPositions             json.RawMessage `json:"assetPositions"`
	MarginEnabled              bool            `json:"marginEnabled"`
	UpdatedAtHeight            string          `json:"updatedAtHeight"`
	LatestProcessedBlockHeight string          `json:"latestProcessedBlockHeight"`
}

// GetSubaccount fetches GET /v4/addresses/:address/subaccountNumber/:n.
// The endpoint returns SubaccountResponseObject directly (not wrapped).
func (c *Client) GetSubaccount(ctx context.Context, address string, number uint32) (*Subaccount, error) {
	path := "/addresses/" + address + "/subaccountNumber/" + strconv.FormatUint(uint64(number), 10)
	var sa Subaccount
	if err := c.get(ctx, path, nil, &sa); err != nil {
		return nil, fmt.Errorf("GetSubaccount %s/%d: %w", address, number, err)
	}
	return &sa, nil
}
