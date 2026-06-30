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
		Description: "Fetch the latest price from the configured EVM oracle feed (an OffChainAggregator / Chainlink AggregatorV3-style contract) via read-only eth_call. Returns the decimal-adjusted price plus the feed's description, decimals, round id, and last-updated time. Requires evm_rpc_url + evm_oracle_addr; refuses otherwise.",
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
		Description: "Fetch an owner's wallet balances: every x/bank denom (the USDC the deposit/withdraw tools move into and out of subaccount collateral) plus known pure-ERC-20 tokens (e.g. USDV) read directly from their contracts. ERC-20 entries carry source=\"erc20\" and are NOT bank-transferable. Distinct from get_subaccount, which reads trading collateral.",
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

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "get_transfer_out_cap",
		Description: "Read the authenticated user's daily transfer-out caps (svp / usdc / usdv): each symbol's effective cap, amount already moved out this UTC day, and remaining headroom. The cap sums outflow across both rails (bank sends and EVM transfers). \"unlimited\" means no cap.",
	}, h.GetTransferOutCap)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "set_transfer_out_cap",
		Description: "Set the authenticated user's own daily transfer-out cap for a token symbol (svp / usdc / usdv), in human units (\"500\", \"1.5\"); \"0\" means unlimited. This is a per-user self-limit on funds leaving the wallet (bank sends + EVM transfers) per UTC day — there is no operator ceiling, so it bounds honest mistakes but is not a hard guard against a misused agent. Resets at restart / UTC midnight.",
	}, h.SetTransferOutCap)

	// E. Cross-cutting.
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "broadcast_signed_tx",
		Description: "Broadcast a tx signed locally by the MCP client — the SignedTx returned by sign_transaction on the local signer MCP server. Pass the signed_tx object through with every field copied VERBATIM from sign_transaction (tx_raw_bytes_b64, signature_b64, pub_key_b64) — do not modify, re-encode, or reformat any of them. Verifies the embedded signer address matches the tenant owner before broadcasting.",
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

	// G. Faucet (HTTP). The faucet backend runs its own operator that signs
	// and submits the on-chain claim, so these tools dispense funds in one
	// call — no client-side signing, EVM RPC, or contract address.
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "list_faucet_tokens",
		Description: "List the tokens the faucet will dispense, each with its per-claim amount (base units). Call before faucet_claim to discover the native token and any claimable ERC-20 addresses.",
	}, h.ListFaucetTokens)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "faucet_claim",
		Description: "Claim test funds from the faucet to your own wallet (the authenticated owner's EVM address). The faucet operator signs and submits the on-chain transfer; returns the resulting tx_hash and amount. Omit token (or pass 0x0) for the native token (SVP); pass an ERC-20 address from list_faucet_tokens otherwise. Rate-limited per address/token by the faucet.",
	}, h.FaucetClaim)

	// H. EVM. The EVM write flow mirrors the Cosmos one but over JSON-RPC:
	// build_<contract>_<action> → sign_evm_transaction (local signer) →
	// broadcast_evm_tx (this server) → evm_tx_status. broadcast_evm_tx and
	// evm_tx_status are contract-agnostic; per-contract build_* tools sit above.
	registerEVMTools(srv, h)
}

// registerEVMTools registers the contract-agnostic EVM engine tools. Adding a
// new EVM contract adds its build_* tool here alongside them — no other
// registry changes.
func registerEVMTools(srv *mcp.Server, h *Handlers) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "broadcast_evm_tx",
		Description: "Broadcast an EVM transaction signed locally by sign_evm_transaction on the svpchain signer MCP — the SignedEvmTx returned for an EVMTxPayload from an EVM build_* tool. Verifies the recovered sender matches the tenant owner before submitting via eth_sendRawTransaction.",
	}, h.BroadcastEVMTx)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "evm_tx_status",
		Description: "Poll the EVM JSON-RPC for the receipt of a previously broadcast EVM tx by hash. Returns status pending (not yet included), success, or failed.",
	}, h.EVMTxStatus)

	// UniswapV2 swaps (the first per-contract EVM build_* family). Tokens are
	// 0x ERC-20 addresses, or empty/"native"/"svp" for the native SVP coin;
	// amounts are human units; output goes to the caller's own address.
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "quote_swap",
		Description: "Quote a UniswapV2 swap: the output token_in -> token_out would yield at current reserves (before slippage), for amount_in human units. Read-only (eth_call) — no signing. Use to preview/size a swap; build_swap re-quotes at build time.",
	}, h.QuoteSwap)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "build_token_approval",
		Description: "Construct an ERC-20 approve so the swap router can spend your input token — the prerequisite for any token-input swap (native-SVP-input swaps skip this). Pass unlimited=true to approve once for all future swaps of this token, or amount for an exact human amount. Returns an EVMTxPayload — pass to sign_evm_transaction (local signer) then broadcast_evm_tx.",
	}, h.BuildTokenApproval)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "build_swap",
		Description: "Construct an exact-input UniswapV2 swap of amount_in (human units) token_in -> token_out, output to your own address. Re-quotes and applies slippage_bps (default 50 = 0.5%) to set the on-chain minimum, and for token-input swaps checks the router allowance first (returns an \"approve first\" error pointing at build_token_approval if missing). Use empty/\"native\"/\"svp\" for the native SVP side. Returns an EVMTxPayload — pass to sign_evm_transaction then broadcast_evm_tx.",
	}, h.BuildSwap)

	// SVPBridge cross-chain deposit — bridge tokens OFF svpchain to another
	// network. Routing (which destination token a source token maps to) comes
	// from the operator's route registry; native SVP rides as the tx value,
	// ERC-20s go through deposit() and need a prior approval to the bridge.
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "build_bridge_deposit",
		Description: "Construct an SVPBridge deposit that bridges a token from svpchain to another network (e.g. Sepolia, Arbitrum Sepolia). dest_chain is a chain name (\"sepolia\", \"arbitrum_sepolia\") or numeric EVM chain id; token is a known symbol (\"USDC\", \"WETH\"), a 0x source-token address, or empty/\"native\"/\"svp\" for native SVP; amount is human units; recipient defaults to your own address on the destination chain. The (token, dest_chain) pair is validated against the configured route whitelist. For ERC-20 tokens the bridge must be approved first; if the allowance is short this returns successfully (not an error) with an approval_required object and no payload, naming build_erc20_approve with spender=the bridge — approve, then retry. Native SVP needs no approval. Returns an EVMTxPayload — pass to sign_evm_transaction (local signer) then broadcast_evm_tx. Requires evm_rpc_url + evm_bridge_addr + evm_bridge_routes_path; refuses otherwise.",
	}, h.BuildBridgeDeposit)

	// SVPBridge inbound deposit — bridge tokens INTO svpchain FROM a foreign
	// network. The deposit is built/broadcast/tracked on the foreign chain (its
	// own RPC, chain id, gas), so it needs a configured [[evm_foreign_chain]];
	// the route whitelist is shared with the outbound direction.
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "build_bridge_deposit_inbound",
		Description: "Construct an SVPBridge deposit on a foreign network that bridges a token INTO svpchain (the inbound counterpart of build_bridge_deposit). source_chain is the foreign chain name (\"sepolia\", \"arbitrum_sepolia\") or numeric EVM chain id; token is a known symbol (\"USDC\", \"WETH\", \"SVP\"), a 0x source-token address on the foreign chain, or empty/\"native\" for the foreign native coin; amount is human units; recipient defaults to your own address on svpchain. The (token, source_chain) pair is validated against the configured route whitelist. For ERC-20 tokens the foreign bridge must be approved first on the foreign chain; if the allowance is short this returns successfully (not an error) with an approval_required object and no payload, naming build_erc20_approve with spender=the foreign bridge and chain_id=the foreign chain — approve, then retry. The native coin needs no approval. The returned EVMTxPayload is stamped with the FOREIGN chain id — sign with sign_evm_transaction then broadcast_evm_tx (which routes to the foreign chain), and track with evm_tx_status passing the returned source_chain_id. Requires evm_bridge_* plus at least one [[evm_foreign_chain]]; refuses otherwise.",
	}, h.BuildBridgeDepositInbound)

	// Generic ERC-20 / ERC-721 build_* family — transfer / approve on any token
	// contract. Like the swap tools they return an EVMTxPayload (sign with
	// sign_evm_transaction, then broadcast_evm_tx). ERC-20 amounts are human
	// units (converted via the token's on-chain decimals); ERC-721 token ids are
	// bare integers. The daily transfer-out cap is enforced at broadcast_evm_tx.
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "build_erc20_transfer",
		Description: "Construct an ERC-20 transfer(to, amount) tx to send tokens from your wallet to a recipient. token is the 0x token contract; amount is human units (e.g. \"1.5\"), converted via the token's on-chain decimals. Returns an EVMTxPayload — pass to sign_evm_transaction (local signer) then broadcast_evm_tx.",
	}, h.BuildERC20Transfer)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "build_erc20_approve",
		Description: "Construct an ERC-20 approve(spender, amount) tx authorizing a spender to pull your tokens. amount is human units; pass unlimited=true for the max (2^256-1). Returns an EVMTxPayload — pass to sign_evm_transaction then broadcast_evm_tx.",
	}, h.BuildERC20Approve)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "build_erc20_transfer_from",
		Description: "Construct an ERC-20 transferFrom(from, to, amount) tx — pull tokens from an owner that previously approved you. amount is human units. Returns an EVMTxPayload — pass to sign_evm_transaction then broadcast_evm_tx.",
	}, h.BuildERC20TransferFrom)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "build_erc721_transfer_from",
		Description: "Construct an ERC-721 transferFrom(from, to, tokenId) tx to move one NFT. Prefer build_erc721_safe_transfer_from when the recipient is a contract. Returns an EVMTxPayload — pass to sign_evm_transaction then broadcast_evm_tx.",
	}, h.BuildERC721TransferFrom)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "build_erc721_safe_transfer_from",
		Description: "Construct the 3-arg ERC-721 safeTransferFrom(from, to, tokenId) tx to move one NFT (verifies a contract recipient can receive ERC-721). Returns an EVMTxPayload — pass to sign_evm_transaction then broadcast_evm_tx.",
	}, h.BuildERC721SafeTransferFrom)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "build_erc721_approve",
		Description: "Construct an ERC-721 approve(spender, tokenId) tx granting control of ONE NFT to spender (pass the zero address to clear). Returns an EVMTxPayload — pass to sign_evm_transaction then broadcast_evm_tx.",
	}, h.BuildERC721Approve)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "build_erc721_set_approval_for_all",
		Description: "Construct an ERC-721 setApprovalForAll(operator, approved) tx to grant (approved=true) or revoke (approved=false) operator control of your ENTIRE NFT collection in this contract. Returns an EVMTxPayload — pass to sign_evm_transaction then broadcast_evm_tx.",
	}, h.BuildERC721SetApprovalForAll)
}
