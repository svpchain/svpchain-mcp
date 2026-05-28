package builder

import (
	"fmt"

	sdk "github.com/cosmos/cosmos-sdk/types"

	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/markets"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/payload"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/units"
	clobtypes "github.com/dydxprotocol/v4-chain/protocol/x/clob/types"
	satypes "github.com/dydxprotocol/v4-chain/protocol/x/subaccounts/types"
)

// PlaceLimitOrderInput is the v0.1 input shape for build_place_limit_order.
// Only short-term orders are supported in v0.1; LongTerm / Conditional /
// TWAP / Scaled land in v0.2 / v0.3.
type PlaceLimitOrderInput struct {
	Owner         string
	SubaccountNum uint32

	Ticker      string
	Side        string // "BUY" | "SELL"
	HumanSize   string // e.g. "0.05"
	HumanPrice  string // e.g. "65000.00"
	TimeInForce string // "" (UNSPECIFIED → GTT), "IOC", or "POST_ONLY"
	ReduceOnly  bool

	// GoodTilBlock is the block height after which this short-term order
	// expires. Must be the current height + a few blocks; the agent reads
	// height from get_height and adds a small buffer.
	GoodTilBlock uint32

	// OrderClientID is the per-order uint32 client id that goes on-chain
	// (Order.ClientId). The agent picks this and is responsible for
	// avoiding collisions with currently open orders.
	OrderClientID uint32

	// PayloadClientID is the broadcast-idempotency uuid for the TxPayload
	// envelope. Distinct from OrderClientID.
	PayloadClientID string
}

// BuildPlaceLimitOrder turns a PlaceLimitOrderInput into a
// MsgPlaceOrder and an envelope TxPayload. It performs:
//
//   - ticker → MarketMeta resolution (markets.Cache)
//   - size → quantums and price → subticks conversion (units)
//   - Order construction with ShortTerm flags + GoodTilBlock
//   - MsgPlaceOrder.ValidateBasic so we surface chain rejects early
//   - TxPayload assembly via Assembler
//
// Caller is responsible for: (1) running policy checks BEFORE this, and
// (2) reading {AccountNumber, Sequence} via chain.AccountClient.Account.
func BuildPlaceLimitOrder(
	in PlaceLimitOrderInput,
	cache *markets.Cache,
	asm *Assembler,
	accountNumber, sequence uint64,
) (*clobtypes.MsgPlaceOrder, *payload.TxPayload, error) {
	meta, ok := cache.ResolveTicker(in.Ticker)
	if !ok {
		return nil, nil, fmt.Errorf("unknown ticker %q (markets cache not populated?)", in.Ticker)
	}

	quantums, err := units.SizeToQuantums(in.HumanSize, meta)
	if err != nil {
		return nil, nil, fmt.Errorf("size: %w", err)
	}
	subticks, err := units.PriceToSubticks(in.HumanPrice, meta)
	if err != nil {
		return nil, nil, fmt.Errorf("price: %w", err)
	}

	side, err := parseSide(in.Side)
	if err != nil {
		return nil, nil, err
	}
	tif, err := parseTimeInForce(in.TimeInForce)
	if err != nil {
		return nil, nil, err
	}
	if in.GoodTilBlock == 0 {
		return nil, nil, fmt.Errorf("good_til_block is required for short-term orders")
	}

	order := clobtypes.Order{
		OrderId: clobtypes.OrderId{
			SubaccountId: satypes.SubaccountId{Owner: in.Owner, Number: in.SubaccountNum},
			ClientId:     in.OrderClientID,
			OrderFlags:   clobtypes.OrderIdFlags_ShortTerm,
			ClobPairId:   meta.ClobPairID,
		},
		Side:         side,
		Quantums:     quantums,
		Subticks:     subticks,
		TimeInForce:  tif,
		ReduceOnly:   in.ReduceOnly,
		GoodTilOneof: &clobtypes.Order_GoodTilBlock{GoodTilBlock: in.GoodTilBlock},
	}
	msg := clobtypes.NewMsgPlaceOrder(order)
	if err := msg.ValidateBasic(); err != nil {
		return nil, nil, fmt.Errorf("MsgPlaceOrder.ValidateBasic: %w", err)
	}

	summary := payload.Summary{
		ToolName:      "build_place_limit_order",
		MsgTypeURL:    "/dydxprotocol.clob.MsgPlaceOrder",
		Subaccount:    payload.SubaccountRef{Owner: in.Owner, Number: in.SubaccountNum},
		Ticker:        in.Ticker,
		Side:          in.Side,
		SizeHuman:     in.HumanSize,
		PriceHuman:    in.HumanPrice,
		GoodTil:       fmt.Sprintf("block:%d", in.GoodTilBlock),
		ReduceOnly:    in.ReduceOnly,
		OrderClientID: in.OrderClientID,
	}
	txPayload, err := asm.Assemble(Args{
		Msgs:          []sdk.Msg{msg},
		SignerAddress: in.Owner,
		AccountNumber: accountNumber,
		Sequence:      sequence,
		ClientID:      in.PayloadClientID,
		Summary:       summary,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("assemble: %w", err)
	}
	return msg, txPayload, nil
}

func parseSide(s string) (clobtypes.Order_Side, error) {
	switch s {
	case "BUY":
		return clobtypes.Order_SIDE_BUY, nil
	case "SELL":
		return clobtypes.Order_SIDE_SELL, nil
	default:
		return clobtypes.Order_SIDE_UNSPECIFIED, fmt.Errorf("invalid side %q (want BUY or SELL)", s)
	}
}

func parseTimeInForce(s string) (clobtypes.Order_TimeInForce, error) {
	switch s {
	case "", "UNSPECIFIED":
		return clobtypes.Order_TIME_IN_FORCE_UNSPECIFIED, nil
	case "IOC":
		return clobtypes.Order_TIME_IN_FORCE_IOC, nil
	case "POST_ONLY":
		return clobtypes.Order_TIME_IN_FORCE_POST_ONLY, nil
	default:
		return clobtypes.Order_TIME_IN_FORCE_UNSPECIFIED, fmt.Errorf("invalid time_in_force %q", s)
	}
}
