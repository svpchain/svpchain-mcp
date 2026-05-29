package tools

import (
	"context"
	"fmt"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/builder"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/payload"
)

// v0.2.2 write-side handlers: market, conditional, cancel, batch_cancel.
// Pattern matches build_place_limit_order in trade.go.

// -- build_place_market_order ------------------------------------------

type BuildPlaceMarketOrderInput struct {
	SubaccountNumber uint32 `json:"subaccount_number"`
	Ticker           string `json:"ticker" jsonschema:"e.g. BTC-USD"`
	Side             string `json:"side" jsonschema:"BUY or SELL"`
	Size             string `json:"size" jsonschema:"human base size, e.g. 0.001"`

	// Worst-price strategy: either WorstPrice or (OraclePrice + SlippageBps).
	WorstPrice  string `json:"worst_price,omitempty" jsonschema:"explicit worst limit price (e.g. 65000 for BUY BTC)"`
	OraclePrice string `json:"oracle_price,omitempty" jsonschema:"current oracle price, used with slippage_bps"`
	SlippageBps uint32 `json:"slippage_bps,omitempty" jsonschema:"slippage in basis points (100 = 1%)"`

	ReduceOnly      bool   `json:"reduce_only,omitempty"`
	GoodTilBlock    uint32 `json:"good_til_block"`
	OrderClientID   uint32 `json:"order_client_id"`
	PayloadClientID string `json:"payload_client_id"`
}

type BuildPlaceMarketOrderOutput struct {
	Payload payload.TxPayload `json:"payload"`
}

func (h *Handlers) BuildPlaceMarketOrder(
	ctx context.Context, _ *mcp.CallToolRequest, in BuildPlaceMarketOrderInput,
) (*mcp.CallToolResult, BuildPlaceMarketOrderOutput, error) {
	tc, ok := TenantFrom(ctx)
	if !ok {
		return nil, BuildPlaceMarketOrderOutput{}, ErrNoTenant
	}
	if err := h.Deps.Policy.CheckTenant(tc.TenantID); err != nil {
		return nil, BuildPlaceMarketOrderOutput{}, err
	}
	tp, err := h.Deps.Policy.Tenant(tc.TenantID)
	if err != nil {
		return nil, BuildPlaceMarketOrderOutput{}, err
	}
	if err := h.Deps.Policy.CheckSubaccount(tc.TenantID, in.SubaccountNumber); err != nil {
		return nil, BuildPlaceMarketOrderOutput{}, err
	}
	if !h.Deps.RateLimit.Allow("build_place_market_order:" + tc.TenantID) {
		return nil, BuildPlaceMarketOrderOutput{}, userErrf("rate limit exceeded")
	}
	acc, err := h.Deps.Chain.Account.Account(ctx, tp.Owner)
	if err != nil {
		return nil, BuildPlaceMarketOrderOutput{}, fmt.Errorf("read account state: %w", err)
	}
	_, p, err := builder.BuildPlaceMarketOrder(
		builder.PlaceMarketOrderInput{
			Owner:           tp.Owner,
			SubaccountNum:   in.SubaccountNumber,
			Ticker:          in.Ticker,
			Side:            in.Side,
			HumanSize:       in.Size,
			WorstPrice:      in.WorstPrice,
			OraclePrice:     in.OraclePrice,
			SlippageBps:     in.SlippageBps,
			ReduceOnly:      in.ReduceOnly,
			GoodTilBlock:    in.GoodTilBlock,
			OrderClientID:   in.OrderClientID,
			PayloadClientID: in.PayloadClientID,
		},
		h.Deps.Markets,
		h.Deps.Builder,
		acc.AccountNumber,
		acc.Sequence,
	)
	if err != nil {
		return nil, BuildPlaceMarketOrderOutput{}, err
	}
	return nil, BuildPlaceMarketOrderOutput{Payload: *p}, nil
}

// -- build_place_conditional_order -------------------------------------

type BuildPlaceConditionalOrderInput struct {
	SubaccountNumber uint32 `json:"subaccount_number"`
	Ticker           string `json:"ticker"`
	Side             string `json:"side" jsonschema:"BUY or SELL"`
	Size             string `json:"size"`
	Price            string `json:"price" jsonschema:"limit price applied when the trigger fires"`

	ConditionType string `json:"condition_type" jsonschema:"STOP_LOSS or TAKE_PROFIT"`
	TriggerPrice  string `json:"trigger_price" jsonschema:"oracle price at which the order activates"`

	TimeInForce string `json:"time_in_force,omitempty" jsonschema:"empty | IOC | POST_ONLY"`
	ReduceOnly  bool   `json:"reduce_only,omitempty"`

	GoodTilBlockTime uint32 `json:"good_til_block_time" jsonschema:"unix seconds when the stateful order expires"`
	OrderClientID    uint32 `json:"order_client_id"`
	PayloadClientID  string `json:"payload_client_id"`
}

type BuildPlaceConditionalOrderOutput struct {
	Payload payload.TxPayload `json:"payload"`
}

func (h *Handlers) BuildPlaceConditionalOrder(
	ctx context.Context, _ *mcp.CallToolRequest, in BuildPlaceConditionalOrderInput,
) (*mcp.CallToolResult, BuildPlaceConditionalOrderOutput, error) {
	tc, ok := TenantFrom(ctx)
	if !ok {
		return nil, BuildPlaceConditionalOrderOutput{}, ErrNoTenant
	}
	if err := h.Deps.Policy.CheckTenant(tc.TenantID); err != nil {
		return nil, BuildPlaceConditionalOrderOutput{}, err
	}
	tp, err := h.Deps.Policy.Tenant(tc.TenantID)
	if err != nil {
		return nil, BuildPlaceConditionalOrderOutput{}, err
	}
	if err := h.Deps.Policy.CheckSubaccount(tc.TenantID, in.SubaccountNumber); err != nil {
		return nil, BuildPlaceConditionalOrderOutput{}, err
	}
	if !h.Deps.RateLimit.Allow("build_place_conditional_order:" + tc.TenantID) {
		return nil, BuildPlaceConditionalOrderOutput{}, userErrf("rate limit exceeded")
	}
	acc, err := h.Deps.Chain.Account.Account(ctx, tp.Owner)
	if err != nil {
		return nil, BuildPlaceConditionalOrderOutput{}, fmt.Errorf("read account state: %w", err)
	}
	_, p, err := builder.BuildPlaceConditionalOrder(
		builder.PlaceConditionalOrderInput{
			Owner:            tp.Owner,
			SubaccountNum:    in.SubaccountNumber,
			Ticker:           in.Ticker,
			Side:             in.Side,
			HumanSize:        in.Size,
			HumanPrice:       in.Price,
			ConditionType:    in.ConditionType,
			TriggerPrice:     in.TriggerPrice,
			TimeInForce:      in.TimeInForce,
			ReduceOnly:       in.ReduceOnly,
			GoodTilBlockTime: in.GoodTilBlockTime,
			OrderClientID:    in.OrderClientID,
			PayloadClientID:  in.PayloadClientID,
		},
		h.Deps.Markets,
		h.Deps.Builder,
		acc.AccountNumber,
		acc.Sequence,
	)
	if err != nil {
		return nil, BuildPlaceConditionalOrderOutput{}, err
	}
	return nil, BuildPlaceConditionalOrderOutput{Payload: *p}, nil
}

// -- build_cancel_order ------------------------------------------------

type BuildCancelOrderInput struct {
	SubaccountNumber uint32 `json:"subaccount_number"`

	ClobPairID    uint32 `json:"clob_pair_id"`
	OrderClientID uint32 `json:"order_client_id"`
	OrderFlags    uint32 `json:"order_flags" jsonschema:"0=ShortTerm, 32=Conditional, 64=LongTerm — required, no inference"`

	GoodTilBlock     uint32 `json:"good_til_block,omitempty" jsonschema:"required when order_flags=0"`
	GoodTilBlockTime uint32 `json:"good_til_block_time,omitempty" jsonschema:"required when order_flags=32 or 64"`

	PayloadClientID string `json:"payload_client_id"`
}

type BuildCancelOrderOutput struct {
	Payload payload.TxPayload `json:"payload"`
}

func (h *Handlers) BuildCancelOrder(
	ctx context.Context, _ *mcp.CallToolRequest, in BuildCancelOrderInput,
) (*mcp.CallToolResult, BuildCancelOrderOutput, error) {
	tc, ok := TenantFrom(ctx)
	if !ok {
		return nil, BuildCancelOrderOutput{}, ErrNoTenant
	}
	if err := h.Deps.Policy.CheckTenant(tc.TenantID); err != nil {
		return nil, BuildCancelOrderOutput{}, err
	}
	tp, err := h.Deps.Policy.Tenant(tc.TenantID)
	if err != nil {
		return nil, BuildCancelOrderOutput{}, err
	}
	if err := h.Deps.Policy.CheckSubaccount(tc.TenantID, in.SubaccountNumber); err != nil {
		return nil, BuildCancelOrderOutput{}, err
	}
	if !h.Deps.RateLimit.Allow("build_cancel_order:" + tc.TenantID) {
		return nil, BuildCancelOrderOutput{}, userErrf("rate limit exceeded")
	}
	acc, err := h.Deps.Chain.Account.Account(ctx, tp.Owner)
	if err != nil {
		return nil, BuildCancelOrderOutput{}, fmt.Errorf("read account state: %w", err)
	}
	_, p, err := builder.BuildCancelOrder(
		builder.CancelOrderInput{
			Owner:            tp.Owner,
			SubaccountNum:    in.SubaccountNumber,
			ClobPairID:       in.ClobPairID,
			OrderClientID:    in.OrderClientID,
			OrderFlags:       in.OrderFlags,
			GoodTilBlock:     in.GoodTilBlock,
			GoodTilBlockTime: in.GoodTilBlockTime,
			PayloadClientID:  in.PayloadClientID,
		},
		h.Deps.Builder,
		acc.AccountNumber,
		acc.Sequence,
	)
	if err != nil {
		return nil, BuildCancelOrderOutput{}, err
	}
	return nil, BuildCancelOrderOutput{Payload: *p}, nil
}

// -- build_batch_cancel_orders -----------------------------------------

type BuildBatchCancelOrdersInputBatch struct {
	ClobPairID uint32   `json:"clob_pair_id"`
	ClientIDs  []uint32 `json:"client_ids"`
}

type BuildBatchCancelOrdersInput struct {
	SubaccountNumber uint32                             `json:"subaccount_number"`
	Batches          []BuildBatchCancelOrdersInputBatch `json:"batches"`
	GoodTilBlock     uint32                             `json:"good_til_block" jsonschema:"short-term only — batch cancel is not supported for stateful orders"`
	PayloadClientID  string                             `json:"payload_client_id"`
}

type BuildBatchCancelOrdersOutput struct {
	Payload payload.TxPayload `json:"payload"`
}

func (h *Handlers) BuildBatchCancelOrders(
	ctx context.Context, _ *mcp.CallToolRequest, in BuildBatchCancelOrdersInput,
) (*mcp.CallToolResult, BuildBatchCancelOrdersOutput, error) {
	tc, ok := TenantFrom(ctx)
	if !ok {
		return nil, BuildBatchCancelOrdersOutput{}, ErrNoTenant
	}
	if err := h.Deps.Policy.CheckTenant(tc.TenantID); err != nil {
		return nil, BuildBatchCancelOrdersOutput{}, err
	}
	tp, err := h.Deps.Policy.Tenant(tc.TenantID)
	if err != nil {
		return nil, BuildBatchCancelOrdersOutput{}, err
	}
	if err := h.Deps.Policy.CheckSubaccount(tc.TenantID, in.SubaccountNumber); err != nil {
		return nil, BuildBatchCancelOrdersOutput{}, err
	}
	if !h.Deps.RateLimit.Allow("build_batch_cancel_orders:" + tc.TenantID) {
		return nil, BuildBatchCancelOrdersOutput{}, userErrf("rate limit exceeded")
	}
	acc, err := h.Deps.Chain.Account.Account(ctx, tp.Owner)
	if err != nil {
		return nil, BuildBatchCancelOrdersOutput{}, fmt.Errorf("read account state: %w", err)
	}
	batches := make([]builder.OrderBatchInput, 0, len(in.Batches))
	for _, b := range in.Batches {
		batches = append(batches, builder.OrderBatchInput{
			ClobPairID: b.ClobPairID,
			ClientIDs:  b.ClientIDs,
		})
	}
	_, p, err := builder.BuildBatchCancelOrders(
		builder.BatchCancelOrdersInput{
			Owner:           tp.Owner,
			SubaccountNum:   in.SubaccountNumber,
			Batches:         batches,
			GoodTilBlock:    in.GoodTilBlock,
			PayloadClientID: in.PayloadClientID,
		},
		h.Deps.Builder,
		acc.AccountNumber,
		acc.Sequence,
	)
	if err != nil {
		return nil, BuildBatchCancelOrdersOutput{}, err
	}
	return nil, BuildBatchCancelOrdersOutput{Payload: *p}, nil
}
