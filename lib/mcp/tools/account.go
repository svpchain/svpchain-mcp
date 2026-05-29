package tools

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	satypes "github.com/dydxprotocol/v4-chain/protocol/x/subaccounts/types"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/indexer"
)

// -- get_subaccount (indexer) -------------------------------------------

type GetSubaccountInput struct {
	Address          string `json:"address" jsonschema:"svp1... bech32 owner address"`
	SubaccountNumber uint32 `json:"subaccount_number"`
}
type GetSubaccountOutput struct {
	Subaccount indexer.Subaccount `json:"subaccount"`
}

func (h *Handlers) GetSubaccount(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	in GetSubaccountInput,
) (*mcp.CallToolResult, GetSubaccountOutput, error) {
	tc, ok := TenantFrom(ctx)
	if !ok {
		return nil, GetSubaccountOutput{}, ErrNoTenant
	}
	if err := h.Deps.Policy.CheckOwner(tc.TenantID, in.Address); err != nil {
		return nil, GetSubaccountOutput{}, err
	}
	if err := h.Deps.Policy.CheckSubaccount(tc.TenantID, in.SubaccountNumber); err != nil {
		return nil, GetSubaccountOutput{}, err
	}
	if !h.Deps.RateLimit.Allow("get_subaccount:" + tc.TenantID) {
		return nil, GetSubaccountOutput{}, userErrf("rate limit exceeded")
	}
	sa, err := h.Deps.Indexer.GetSubaccount(ctx, in.Address, in.SubaccountNumber)
	if err != nil {
		return nil, GetSubaccountOutput{}, err
	}
	return nil, GetSubaccountOutput{Subaccount: *sa}, nil
}

// -- get_live_subaccount (chain) ----------------------------------------

type GetLiveSubaccountInput struct {
	Owner            string `json:"owner" jsonschema:"svp1... bech32 owner address"`
	SubaccountNumber uint32 `json:"subaccount_number"`
}

// LiveSubaccountDTO is the JSON-friendly projection of satypes.Subaccount.
// The raw proto carries dtypes.SerializableInt for Quantums / FundingIndex
// / QuoteBalance — those fields marshal to JSON strings but the MCP-SDK
// schema reflector sees them as Go structs and demands JSON objects,
// which fails output-validation at runtime against any populated
// subaccount. Mapping to string fields here lets the auto-generated
// schema match the actual JSON shape.
type LiveSubaccountDTO struct {
	Id                 LiveSubaccountIDDTO        `json:"id"`
	AssetPositions     []LiveAssetPositionDTO     `json:"asset_positions"`
	PerpetualPositions []LivePerpetualPositionDTO `json:"perpetual_positions"`
	MarginEnabled      bool                       `json:"margin_enabled"`
}

type LiveSubaccountIDDTO struct {
	Owner  string `json:"owner"`
	Number uint32 `json:"number"`
}

type LiveAssetPositionDTO struct {
	AssetId  uint32 `json:"asset_id"`
	Quantums string `json:"quantums"` // dec string from SerializableInt.String
	Index    uint64 `json:"index"`
}

type LivePerpetualPositionDTO struct {
	PerpetualId  uint32 `json:"perpetual_id"`
	Quantums     string `json:"quantums"`
	FundingIndex string `json:"funding_index"`
	QuoteBalance string `json:"quote_balance"`
}

type GetLiveSubaccountOutput struct {
	Subaccount LiveSubaccountDTO `json:"subaccount"`
}

// liveSubaccountFromChain projects the raw proto into the JSON-friendly
// DTO. Arrays are initialised non-nil so clients that don't tolerate
// `null` get `[]` instead.
func liveSubaccountFromChain(sub satypes.Subaccount) LiveSubaccountDTO {
	out := LiveSubaccountDTO{
		AssetPositions:     []LiveAssetPositionDTO{},
		PerpetualPositions: []LivePerpetualPositionDTO{},
		MarginEnabled:      sub.MarginEnabled,
	}
	if sub.Id != nil {
		out.Id = LiveSubaccountIDDTO{Owner: sub.Id.Owner, Number: sub.Id.Number}
	}
	for _, ap := range sub.AssetPositions {
		if ap == nil {
			continue
		}
		out.AssetPositions = append(out.AssetPositions, LiveAssetPositionDTO{
			AssetId:  ap.AssetId,
			Quantums: ap.Quantums.String(),
			Index:    ap.Index,
		})
	}
	for _, pp := range sub.PerpetualPositions {
		if pp == nil {
			continue
		}
		out.PerpetualPositions = append(out.PerpetualPositions, LivePerpetualPositionDTO{
			PerpetualId:  pp.PerpetualId,
			Quantums:     pp.Quantums.String(),
			FundingIndex: pp.FundingIndex.String(),
			QuoteBalance: pp.QuoteBalance.String(),
		})
	}
	return out
}

func (h *Handlers) GetLiveSubaccount(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	in GetLiveSubaccountInput,
) (*mcp.CallToolResult, GetLiveSubaccountOutput, error) {
	tc, ok := TenantFrom(ctx)
	if !ok {
		return nil, GetLiveSubaccountOutput{}, ErrNoTenant
	}
	if err := h.Deps.Policy.CheckOwner(tc.TenantID, in.Owner); err != nil {
		return nil, GetLiveSubaccountOutput{}, err
	}
	if err := h.Deps.Policy.CheckSubaccount(tc.TenantID, in.SubaccountNumber); err != nil {
		return nil, GetLiveSubaccountOutput{}, err
	}
	if !h.Deps.RateLimit.Allow("get_live_subaccount:" + tc.TenantID) {
		return nil, GetLiveSubaccountOutput{}, userErrf("rate limit exceeded")
	}
	sub, err := h.Deps.Chain.SubaccountQuery.Subaccount(ctx, in.Owner, in.SubaccountNumber)
	if err != nil {
		return nil, GetLiveSubaccountOutput{}, err
	}
	return nil, GetLiveSubaccountOutput{Subaccount: liveSubaccountFromChain(sub)}, nil
}

// -- whoami -------------------------------------------------------------

type WhoamiInput struct{}
type WhoamiOutput struct {
	ChainID            string   `json:"chain_id"`
	TenantID           string   `json:"tenant_id"`
	Owner              string   `json:"owner"`
	AllowedSubaccounts []uint32 `json:"allowed_subaccounts"`
	BroadcastMode      string   `json:"broadcast_mode"`
	KillSwitch         bool     `json:"kill_switch"`
}

func (h *Handlers) Whoami(
	ctx context.Context,
	_ *mcp.CallToolRequest,
	_ WhoamiInput,
) (*mcp.CallToolResult, WhoamiOutput, error) {
	tc, ok := TenantFrom(ctx)
	if !ok {
		return nil, WhoamiOutput{}, ErrNoTenant
	}
	tp, err := h.Deps.Policy.Tenant(tc.TenantID)
	if err != nil {
		return nil, WhoamiOutput{}, err
	}
	subs := make([]uint32, 0, len(tp.AllowedSubaccounts))
	for s := range tp.AllowedSubaccounts {
		subs = append(subs, s)
	}
	return nil, WhoamiOutput{
		ChainID:            h.ChainID,
		TenantID:           tp.TenantID,
		Owner:              tp.Owner,
		AllowedSubaccounts: subs,
		BroadcastMode:      h.Deps.BroadcastMode,
		KillSwitch:         tp.KillSwitch,
	}, nil
}
