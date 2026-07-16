package tools

import (
	"cosmossdk.io/log"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"

	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/auth"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/bridge"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/builder"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/chain"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/faucet"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/indexer"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/lendora"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/limits"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/markets"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/policy"
)

// ChainDeps groups the gRPC clients used by tool handlers.
type ChainDeps struct {
	Account         chain.AccountClient
	Broadcast       chain.BroadcastClient
	ClobQuery       chain.ClobQueryClient
	PerpetualsQuery chain.PerpetualsQueryClient
	SubaccountQuery chain.SubaccountQueryClient
	BankQuery       chain.BankQueryClient
	CometBft        chain.CometBftClient

	// EVM is the EVM JSON-RPC client backing the EVM tool family
	// (broadcast_evm_tx, evm_tx_status, and future per-contract build_*
	// tools). Nil when the server is configured without evm_rpc_url; EVM
	// tools refuse in that case.
	EVM chain.EVMClient
}

// EVMDeps groups the EVM build-path dependencies: the contract-agnostic
// assembler that fills nonce/gas/fees for per-contract build_* tools. Adding a
// new EVM contract adds its deployment-specific binding here.
type EVMDeps struct {
	Assembler *builder.EVMAssembler

	// Uniswap binds the swap tools (quote_swap / build_token_approval /
	// build_swap) to a UniswapV2Router02 + WSVP deployment. Nil unless both
	// evm_uniswap_router_addr and evm_wsvp_addr are configured (which also requires
	// evm_rpc_url); the swap tools check it and refuse otherwise.
	Uniswap *builder.UniswapV2

	// Oracle binds get_oracle_price to an OffChainAggregator price feed
	// deployment. Nil unless evm_oracle_addr is configured (which also requires
	// evm_rpc_url); get_oracle_price checks it and refuses otherwise.
	Oracle *builder.OracleFeed

	// Lendora binds the lendora_* money-market tools to a Lendora (Compound V2
	// fork) Comptroller deployment. Markets are resolved per-call (via
	// LendoraMarkets); only the singleton Comptroller is bound here. Nil unless
	// evm_lendora_comptroller_addr is configured (which also requires
	// evm_rpc_url); the lendora tools check it (requireLendora) and refuse otherwise.
	Lendora *builder.Lendora

	// Bridge + BridgeRoutes back build_bridge_deposit: the SVPBridge contract
	// binding and the (sourceToken, targetChainId -> targetToken) route registry.
	// BridgeSourceChainID is the EVM chain id of this deployment, used to scope
	// route lookups to outbound-from-svpchain pairs. All three are set together
	// (or all nil) — guaranteed by config validation, which also requires
	// evm_rpc_url. build_bridge_deposit checks them and refuses otherwise.
	Bridge              *builder.Bridge
	BridgeRoutes        *bridge.Registry
	BridgeSourceChainID uint64

	// ForeignChains backs the inbound build_bridge_deposit_inbound tool: the
	// foreign EVM chains that bridge INTO svpchain, keyed by their EVM chain id.
	// Each bundle has its own dialed client, assembler, and SVPBridge binding, so
	// the inbound deposit is built/broadcast/tracked on that chain rather than the
	// home RPC. The route whitelist is shared (BridgeRoutes). Empty/nil unless
	// [[evm_foreign_chain]] entries are configured; the inbound tool refuses then.
	ForeignChains map[uint64]*ForeignChain

	// HomeChainID is this deployment's EVM chain id (the home/svpchain RPC,
	// Chain.EVM). Used to route broadcast/status to the home client and to scope
	// inbound route lookups to (<foreign> -> home). Zero when the EVM family is
	// unconfigured.
	HomeChainID uint64
}

// ForeignChain is the per-foreign-chain bundle for inbound bridging: the EVM
// JSON-RPC client, the assembler that fills nonce/gas/fees against it, and the
// SVPBridge binding deployed on that chain.
type ForeignChain struct {
	Client    chain.EVMClient
	Assembler *builder.EVMAssembler
	Bridge    *builder.Bridge
}

// EVMClientFor returns the EVM JSON-RPC client for chainID: the home client
// (Chain.EVM, scoped by HomeChainID) or a configured foreign chain's client.
// Used to route broadcast_evm_tx / evm_tx_status to the chain a tx belongs to.
func (d *Deps) EVMClientFor(chainID uint64) (chain.EVMClient, bool) {
	if chainID == d.EVM.HomeChainID && d.Chain.EVM != nil {
		return d.Chain.EVM, true
	}
	if fc, ok := d.EVM.ForeignChains[chainID]; ok {
		return fc.Client, true
	}
	return nil, false
}

// Deps is the full dependency bundle every tool handler receives. v0.1
// keeps it flat; v0.2 may split into smaller per-capability bundles when
// the handler count grows.
type Deps struct {
	Chain   ChainDeps
	Indexer *indexer.Client
	Markets *markets.Cache
	Builder *builder.Assembler

	// LendoraMarkets resolves a Lendora asset (underlying symbol or 0x address)
	// to its on-chain market metadata for the lendora_* tools. Nil unless
	// evm_lendora_comptroller_addr is configured; the tools check it (requireLendora)
	// and refuse otherwise.
	LendoraMarkets *lendora.Cache

	// Faucet is the HTTP client for the faucet backend (faucet_base_url).
	// Nil when the server runs without faucet_base_url; the faucet tools
	// check Faucet != nil and refuse otherwise.
	Faucet *faucet.Client

	// EVM holds the EVM build dependencies (assembler + per-contract
	// addresses). Zero-valued when the server runs without evm_rpc_url; EVM
	// tools check EVM.Assembler != nil and refuse otherwise.
	EVM EVMDeps

	Policy      *policy.Engine
	Auditor     *policy.Auditor
	Idempotency *policy.Idempotency
	RateLimit   *policy.RateLimiter

	// Limits + WithdrawLedger drive the v0.2.3 funds-tool safety rails.
	// Limits is a pure config; WithdrawLedger holds per-tenant daily spend
	// state (MemoryLedger by default, swappable for a durable backend
	// without touching handler code).
	Limits         limits.Config
	WithdrawLedger limits.WithdrawLedger

	// TransferOut holds each owner wallet's per-symbol daily "transfer out" caps
	// and usage (svp / usdc / usdv), keyed by owner address so all of a wallet's
	// concurrent agents / re-auths share one cap and daily total. A symbol's
	// outflow accumulates across both rails it can leave through — x/bank sends
	// (build_bank_send) and EVM transfers (broadcast_evm_tx) — and is enforced
	// at broadcast. Caps are set at runtime via set_transfer_out_cap; there is
	// no operator config. In-memory; resets on restart / UTC midnight.
	TransferOut *limits.MemoryTransferOutStore

	// Self-service auth backend (v0.3). NonceStore + DynamicTenants are
	// populated by auth_challenge / auth_verify; IPChallengeLimit caps
	// auth_challenge per-IP since the tool runs before any tenant
	// context is established.
	NonceStore       *auth.NonceStore
	DynamicTenants   *auth.DynamicTenantStore
	IPChallengeLimit *auth.IPRateLimiter
	SessionBearers   *auth.SessionBearers

	Logger log.Logger

	// InterfaceRegistry is used by broadcast_signed_tx to decode the
	// signer pubkey (eth_secp256k1) carried inside the TxRaw's AuthInfo,
	// and to verify the resulting bech32 address matches the tenant's
	// configured owner.
	InterfaceRegistry codectypes.InterfaceRegistry

	// BroadcastMode reports which broadcast variant is configured (for
	// whoami). v0.1 always "server" — server broadcasts the signed tx
	// the client returns.
	BroadcastMode string
}
