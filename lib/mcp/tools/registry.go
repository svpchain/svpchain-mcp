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

	// v0.2.1 market-data extensions.
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_candles",
		Description: "Fetch OHLC candles for a perpetual market at the requested resolution.",
	}, h.GetCandles)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_trades",
		Description: "Fetch recent trades on a perpetual market.",
	}, h.GetTrades)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_sparklines",
		Description: "Fetch sparkline price series for every perpetual market over a fixed time period.",
	}, h.GetSparklines)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_historical_funding",
		Description: "Fetch historical funding rate samples for a perpetual market.",
	}, h.GetHistoricalFunding)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_height",
		Description: "Fetch the latest block height indexed by Comlink (with the matching block time).",
	}, h.GetHeight)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_time",
		Description: "Fetch the indexer's wall-clock time — useful as a freshness sentinel.",
	}, h.GetTime)

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
		Name:        "get_balance",
		Description: "Fetch an owner's wallet (x/bank) balances across all denoms — the USDC the deposit/withdraw tools move into and out of subaccount collateral. Distinct from get_subaccount, which reads trading collateral.",
	}, h.GetBalance)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "whoami",
		Description: "Return the calling tenant's identity, allowed subaccounts, broadcast mode, and kill-switch state.",
	}, h.Whoami)

	// v0.2.1 owner-scoped read extensions.
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_orders",
		Description: "List orders for a (owner, subaccount) with optional status/side/type filters.",
	}, h.GetOrders)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_order",
		Description: "Fetch a single order by its on-chain order id.",
	}, h.GetOrder)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_fills",
		Description: "List fills for a (owner, subaccount), optionally filtered by market.",
	}, h.GetFills)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_transfers",
		Description: "List transfers (deposits, withdrawals, subaccount-to-subaccount) for a (owner, subaccount).",
	}, h.GetTransfers)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_pnl",
		Description: "Fetch the latest PnL snapshot for a (owner, subaccount).",
	}, h.GetPnl)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_historical_pnl",
		Description: "Fetch the PnL time-series for a (owner, subaccount).",
	}, h.GetHistoricalPnl)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_funding_payments",
		Description: "List historical funding payments for a (owner, subaccount).",
	}, h.GetFundingPayments)

	// C. Trading (build only — no signing).
	//
	// Every build_* tool returns a TxPayload. The canonical write flow is
	// build_* → sign_transaction (local signer MCP) → broadcast_signed_tx
	// (this server). Each description names the chain explicitly so an
	// LLM picking tools from a flat catalog has a clear next-step signal.
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "build_place_limit_order",
		Description: "Construct (but do not sign) a short-term limit order. Returns a TxPayload — pass to sign_transaction (local signer) then broadcast_signed_tx to land on chain.",
	}, h.BuildPlaceLimitOrder)

	// v0.2.2: market / conditional / cancel / batch_cancel.
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "build_place_market_order",
		Description: "Construct a short-term IOC \"market\" order — an IOC limit at a worst price the caller commits to (explicit worst_price, or derived from oracle_price + slippage_bps). Returns a TxPayload — pass to sign_transaction then broadcast_signed_tx.",
	}, h.BuildPlaceMarketOrder)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "build_place_conditional_order",
		Description: "Construct a stateful conditional order (STOP_LOSS or TAKE_PROFIT). Activates as a limit order when the oracle crosses trigger_price; expires at good_til_block_time. Returns a TxPayload — pass to sign_transaction then broadcast_signed_tx.",
	}, h.BuildPlaceConditionalOrder)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "build_cancel_order",
		Description: "Construct a cancel for a single open order. order_flags must be set explicitly (0=ShortTerm, 32=Conditional, 64=LongTerm). Returns a TxPayload — pass to sign_transaction then broadcast_signed_tx.",
	}, h.BuildCancelOrder)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "build_batch_cancel_orders",
		Description: "Construct a batch cancel of short-term orders (chain accepts MsgBatchCancel for short-term only). Accepts (clob_pair_id, client_ids) tuples. Returns a TxPayload — pass to sign_transaction then broadcast_signed_tx.",
	}, h.BuildBatchCancelOrders)

	// D. Funds movement (v0.2.3).
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "build_deposit_to_subaccount",
		Description: "Construct a deposit from the owner's bank account into one of their subaccounts (USDC only). Returns a TxPayload — pass to sign_transaction (local signer) then broadcast_signed_tx to land on chain. Per-tx cap enforced if configured.",
	}, h.BuildDepositToSubaccount)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "build_withdraw_from_subaccount",
		Description: "Construct a withdraw from the owner's subaccount back into their bank account (USDC only). Returns a TxPayload — pass to sign_transaction then broadcast_signed_tx. Per-tx cap + per-tenant daily cap enforced; broadcast_signed_tx re-checks the daily cap as a safety net.",
	}, h.BuildWithdrawFromSubaccount)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "build_transfer_between_subaccounts",
		Description: "Construct a USDC transfer between two subaccounts under the same owner. Returns a TxPayload — pass to sign_transaction then broadcast_signed_tx. v0.2.3 is same-owner only; cross-owner transfers are deferred until a future version with the right cap surface.",
	}, h.BuildTransferBetweenSubaccounts)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "build_bank_send",
		Description: "Construct a native bank send (cosmos x/bank MsgSend) of any denom from the owner's wallet to an arbitrary recipient address — e.g. send SVP (denom \"asvp\") or USDC (\"erc20/usdc\") to a third party. Amount is human units for known denoms (SVP, USDC) or base units otherwise. Returns a TxPayload — pass to sign_transaction (local signer) then broadcast_signed_tx to land on chain.",
	}, h.BuildBankSend)

	// E. Cross-cutting.
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "broadcast_signed_tx",
		Description: "Broadcast a tx signed locally by the MCP client — typically the SignedTx returned by sign_transaction on the local signer MCP server. Verifies the embedded signer address matches the tenant owner before broadcasting.",
	}, h.BroadcastSignedTx)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_tx_status",
		Description: "Poll CometBFT for the status of a previously broadcast tx by hash.",
	}, h.GetTxStatus)

	// F. Self-service auth (v0.3). The auth_challenge → sign_challenge
	// (local signer) → auth_verify flow mints a bearer token bound to
	// the signing key's owner address. These two tools deliberately
	// bypass tenant authorization — they ARE the mechanism.
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "auth_challenge",
		Description: "Issue a nonce + signed-challenge text for self-service auth. Pass the returned challenge to sign_challenge on the local signer, then pass nonce + signature to auth_verify. Rate-limited per IP.",
	}, h.AuthChallenge)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "auth_verify",
		Description: "Verify a signature over a previously-issued challenge nonce; if it recovers to the same address the nonce was issued for, mint a bearer token bound to that owner. Use the returned bearer_token in the Authorization header for every subsequent request until expires_at.",
	}, h.AuthVerify)

	// G. EVM. The EVM write flow mirrors the Cosmos one but over JSON-RPC:
	// build_<contract>_<action> → sign_evm_transaction (local signer) →
	// broadcast_evm_tx (this server) → evm_tx_status. broadcast_evm_tx and
	// evm_tx_status are contract-agnostic; per-contract build_* tools sit above.
	registerEVMTools(srv, h)
}

// registerEVMTools registers the EVM tool family. Adding a new EVM contract
// adds its build_* tool here next to build_faucet_claim — no other registry
// changes.
func registerEVMTools(srv *mcp.Server, h *Handlers) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "broadcast_evm_tx",
		Description: "Broadcast an EVM transaction signed locally by sign_evm_transaction on the svpchain signer MCP — the SignedEvmTx returned for an EVMTxPayload from an EVM build_* tool. Verifies the recovered sender matches the tenant owner before submitting via eth_sendRawTransaction.",
	}, h.BroadcastEVMTx)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "evm_tx_status",
		Description: "Poll the EVM JSON-RPC for the receipt of a previously broadcast EVM tx by hash. Returns status pending (not yet included), success, or failed.",
	}, h.EVMTxStatus)

	// Per-contract build_* tools.
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "build_faucet_claim",
		Description: "Construct (but do not sign) a faucet claim transaction. Returns an EVMTxPayload — pass to sign_evm_transaction (svpchain signer MCP) then broadcast_evm_tx to land on chain.",
	}, h.BuildFaucetClaim)
}
