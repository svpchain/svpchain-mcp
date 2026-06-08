package tools

import (
	"cosmossdk.io/log"
	codectypes "github.com/cosmos/cosmos-sdk/codec/types"

	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/auth"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/builder"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/chain"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/faucet"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/indexer"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/limits"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/markets"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/policy"
)

// ChainDeps groups the gRPC clients used by tool handlers.
type ChainDeps struct {
	Account         chain.AccountClient
	Broadcast       chain.BroadcastClient
	ClobQuery       chain.ClobQueryClient
	SubaccountQuery chain.SubaccountQueryClient
	PricesQuery     chain.PricesQueryClient
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
// new EVM contract adds its deployment-specific address here.
type EVMDeps struct {
	Assembler *builder.EVMAssembler
}

// Deps is the full dependency bundle every tool handler receives. v0.1
// keeps it flat; v0.2 may split into smaller per-capability bundles when
// the handler count grows.
type Deps struct {
	Chain   ChainDeps
	Indexer *indexer.Client
	Markets *markets.Cache
	Builder *builder.Assembler

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
