package tools

import (
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Handlers bundles all MCP tool handlers. ChainID is read at boot from
// config and stamped onto every TxPayload + whoami response. Deps carries
// the rest.
type Handlers struct {
	ChainID string
	Deps    Deps
}

// New constructs a Handlers from the chain id and dep bundle.
func New(chainID string, deps Deps) *Handlers {
	return &Handlers{ChainID: chainID, Deps: deps}
}

// Register hooks every v0.1 tool onto srv with reflection-derived schemas
// (from `jsonschema:` struct tags on input/output types). v0.2 and v0.3
// add more tools to this list; the rest of the package remains untouched.
func Register(srv *mcp.Server, h *Handlers) {
	// A. Market data.
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_markets",
		Description: "List every perpetual market on svpchain.",
	}, h.ListMarkets)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_market",
		Description: "Fetch a single perpetual market by ticker.",
	}, h.GetMarket)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_orderbook",
		Description: "Fetch the L2 orderbook for a perpetual market.",
	}, h.GetOrderbook)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_oracle_price",
		Description: "Fetch the on-chain oracle price for a market by its prices-module id.",
	}, h.GetOraclePrice)

	// B. Account / positions.
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_subaccount",
		Description: "Fetch a subaccount snapshot from the indexer (committed state).",
	}, h.GetSubaccount)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_live_subaccount",
		Description: "Fetch a subaccount from chain gRPC (uncommitted, freshest).",
	}, h.GetLiveSubaccount)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "whoami",
		Description: "Return the calling tenant's identity, allowed subaccounts, broadcast mode, and kill-switch state.",
	}, h.Whoami)

	// C. Trading (build only — no signing).
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "build_place_limit_order",
		Description: "Construct (but do not sign) a short-term limit order. Returns a TxPayload to sign locally and pass to broadcast_signed_tx.",
	}, h.BuildPlaceLimitOrder)

	// E. Cross-cutting.
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "broadcast_signed_tx",
		Description: "Broadcast a tx signed locally by the MCP client. Server verifies signer address matches tenant owner before broadcasting.",
	}, h.BroadcastSignedTx)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_tx_status",
		Description: "Poll CometBFT for the status of a previously broadcast tx by hash.",
	}, h.GetTxStatus)
}
