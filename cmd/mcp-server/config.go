package main

import (
	"fmt"
	"path/filepath"
	"time"

	"cosmossdk.io/math"
	"github.com/BurntSushi/toml"
	"github.com/ethereum/go-ethereum/common"
)

// Config is the full server configuration loaded from a TOML file.
//
// v0.3 dropped the static [[tenants]] table: tenant identity is now
// established at runtime via the auth_challenge → sign_challenge →
// auth_verify self-service flow. Operators no longer pre-provision per-
// user entries.
type Config struct {
	ChainID        string `toml:"chain_id"`
	GrpcAddr       string `toml:"grpc_addr"`
	CometRPCURL    string `toml:"comet_rpc_url"`
	IndexerBaseURL string `toml:"indexer_base_url"`
	ListenAddr     string `toml:"listen_addr"`

	// EVMRPCURL is the chain's EVM JSON-RPC endpoint (e.g.
	// "http://127.0.0.1:8545"). Optional: when empty the EVM tool family
	// (broadcast_evm_tx / evm_tx_status) refuses, and non-EVM deployments
	// keep booting unchanged.
	EVMRPCURL string `toml:"evm_rpc_url"`

	// EVMUniswapRouterAddr / EVMWSVPAddr bind the swap tools (quote_swap /
	// build_token_approval / build_swap) to a UniswapV2Router02 deployment and
	// its wrapped-native (WSVP) token. The router key names its protocol since
	// it is protocol-specific (a future V3 / aggregator router would be a
	// distinct key); WSVP stays generic because wrapped-native is shared infra.
	// Both are optional and must be set together; setting either also requires
	// evm_rpc_url. When unset the swap tools refuse, and the rest of the EVM
	// family is unaffected.
	EVMUniswapRouterAddr string `toml:"evm_uniswap_router_addr"`
	EVMWSVPAddr          string `toml:"evm_wsvp_addr"`

	// EVMOracleAddr binds get_oracle_price to an OffChainAggregator price-feed
	// deployment (a Chainlink AggregatorV3-style contract read via eth_call).
	// Independent of the swap addresses — it is a standalone read feed, not
	// both-or-neither with anything. Optional and must be a valid 0x address
	// when set; setting it also requires evm_rpc_url. When unset, get_oracle_price
	// refuses and the rest of the EVM family is unaffected.
	EVMOracleAddr string `toml:"evm_oracle_addr"`

	// EVMBridgeAddr / EVMBridgeRoutesPath / EVMBridgeSourceChainID bind
	// build_bridge_deposit to an SVPBridge deployment. EVMBridgeAddr is the
	// bridge contract on this chain (the deposit target); EVMBridgeRoutesPath is
	// a JSON file describing the (sourceToken, targetChainId -> targetToken)
	// routes the bridge honors; EVMBridgeSourceChainID is this deployment's EVM
	// chain id, used to scope route lookups to outbound-from-svpchain pairs (the
	// route file may list every direction). All three are optional but must be
	// set together; setting any also requires evm_rpc_url. When unset the bridge
	// tool refuses and the rest of the EVM family is unaffected.
	EVMBridgeAddr          string `toml:"evm_bridge_addr"`
	EVMBridgeRoutesPath    string `toml:"evm_bridge_routes_path"`
	EVMBridgeSourceChainID uint64 `toml:"evm_bridge_source_chain_id"`

	// EVMForeignChains declares the foreign EVM chains that can bridge INTO
	// svpchain, backing the inbound build_bridge_deposit_inbound tool. Each entry
	// is a [[evm_foreign_chain]] table giving that chain's EVM chain id, its own
	// JSON-RPC endpoint (the deposit is built/broadcast/tracked there, not on the
	// home RPC), and the SVPBridge contract deployed on it. The route whitelist is
	// shared with the outbound direction (evm_bridge_routes_path), so the home
	// bridge must also be configured. Optional; when empty the inbound tool refuses.
	EVMForeignChains []EVMForeignChain `toml:"evm_foreign_chain"`

	// FaucetBaseURL is the faucet backend's HTTP base URL (e.g.
	// "https://pre-faucet.svpchain.org"). Optional: when empty the faucet
	// tools (faucet_claim / list_faucet_tokens) refuse. The faucet runs its
	// own operator, so no EVM RPC or contract address is needed here.
	FaucetBaseURL string `toml:"faucet_base_url"`

	// BroadcastMode is informational for whoami. The server always
	// broadcasts the signed tx the client returns; "local" mode (where
	// the client broadcasts directly) is documented for a future version.
	BroadcastMode string `toml:"broadcast_mode"`

	Cache  CacheConfig  `toml:"cache"`
	Limits LimitsConfig `toml:"limits"`
	Fee    FeeConfig    `toml:"fee"`
}

// EVMForeignChain is one inbound source chain: its EVM chain id, its own
// JSON-RPC endpoint, and the SVPBridge address deployed on it. The inbound
// deposit is assembled (nonce/gas/fees), signed for ChainID, broadcast, and
// status-tracked against this chain's RPC — never the home evm_rpc_url.
type EVMForeignChain struct {
	ChainID    uint64 `toml:"chain_id"`
	RPCURL     string `toml:"rpc_url"`
	BridgeAddr string `toml:"bridge_addr"`
}

// FeeConfig sets the gas fee stamped onto non-CLOB txs (deposit / withdraw /
// transfer / conditional orders / long-term cancels / broadcast). Short-term
// CLOB orders are gas-free on svpchain and always ship with an empty fee
// regardless of this config — see builder.Assemble.
//
// The chain's required fee comes from the node's minimum-gas-prices (operator
// config, not queryable via gRPC), so it's configured here. Amount is the
// total fee at GasLimit, kept as a string to preserve the full uint range.
// Defaults (asvp, 25000000000000000, 1_000_000) satisfy a chain whose
// minimum-gas-prices is 25000000000asvp at a 1,000,000 gas limit.
type FeeConfig struct {
	Denom    string `toml:"denom"`
	Amount   string `toml:"amount"`
	GasLimit uint64 `toml:"gas_limit"`
}

// LimitsConfig caps the size of funds movements. All values are in human
// USDC. A zero value disables the corresponding check — useful for dev
// configs that don't care about the safety rail. DailyWithdrawCapUSDC is
// enforced per tenant_id; per-tenant overrides are deferred to a later
// version.
type LimitsConfig struct {
	DepositMaxUSDC       uint64 `toml:"deposit_max_usdc"`
	WithdrawMaxUSDC      uint64 `toml:"withdraw_max_usdc"`
	TransferMaxUSDC      uint64 `toml:"transfer_max_usdc"`
	DailyWithdrawCapUSDC uint64 `toml:"daily_withdraw_cap_usdc"`
}

type CacheConfig struct {
	// MarketsRefresh is parsed as a Go duration string ("60s", "2m"…).
	// Zero (or unset) means use the package default in markets.NewCache.
	MarketsRefresh Duration `toml:"markets_refresh"`
}

// Duration parses TOML strings like "60s" into a time.Duration via
// UnmarshalText (BurntSushi/toml calls UnmarshalText on basic types
// automatically).
type Duration time.Duration

func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	if err != nil {
		return fmt.Errorf("parse duration %q: %w", string(b), err)
	}
	*d = Duration(v)
	return nil
}

// LoadConfig reads and validates a TOML config file.
func LoadConfig(path string) (*Config, error) {
	var c Config
	if _, err := toml.DecodeFile(path, &c); err != nil {
		return nil, fmt.Errorf("decode TOML %s: %w", path, err)
	}
	if c.BroadcastMode == "" {
		c.BroadcastMode = "server"
	}
	c.Fee.applyDefaults()
	// A relative evm_bridge_routes_path is resolved against the config file's
	// own directory, not the server's working directory — so the common
	// "routes.json next to mcp.toml" layout works regardless of where
	// mcp-server is launched from (the local checkout, a systemd unit, or the
	// container, where both files are mounted into /etc/svpchain-mcp).
	if c.EVMBridgeRoutesPath != "" && !filepath.IsAbs(c.EVMBridgeRoutesPath) {
		c.EVMBridgeRoutesPath = filepath.Join(filepath.Dir(path), c.EVMBridgeRoutesPath)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return &c, nil
}

// Validate enforces the required network-level fields. v0.3 removed the
// static [[tenants]] table — tenant identity is established at runtime
// via self-service auth, so the config only needs to describe how to
// reach the chain + the safety caps.
func (c *Config) Validate() error {
	if c.ChainID == "" {
		return fmt.Errorf("chain_id is required")
	}
	if c.GrpcAddr == "" {
		return fmt.Errorf("grpc_addr is required")
	}
	if c.CometRPCURL == "" {
		return fmt.Errorf("comet_rpc_url is required")
	}
	if c.IndexerBaseURL == "" {
		return fmt.Errorf("indexer_base_url is required")
	}
	if c.ListenAddr == "" {
		return fmt.Errorf("listen_addr is required")
	}
	if err := c.Fee.validate(); err != nil {
		return err
	}
	if err := c.validateSwap(); err != nil {
		return err
	}
	if err := c.validateOracle(); err != nil {
		return err
	}
	if err := c.validateBridge(); err != nil {
		return err
	}
	if err := c.validateForeignChains(); err != nil {
		return err
	}
	return nil
}

// validateBridge enforces the bridge invariants: the contract address, routes
// path, and source chain id are all-or-nothing; the address must be valid hex
// when set; and the family requires an EVM RPC endpoint (deposits go out over
// eth_sendRawTransaction, and the allowance check reads via eth_call).
func (c *Config) validateBridge() error {
	set := 0
	if c.EVMBridgeAddr != "" {
		set++
	}
	if c.EVMBridgeRoutesPath != "" {
		set++
	}
	if c.EVMBridgeSourceChainID != 0 {
		set++
	}
	if set == 0 {
		return nil
	}
	if set != 3 {
		return fmt.Errorf("evm_bridge_addr, evm_bridge_routes_path and evm_bridge_source_chain_id must be set together")
	}
	if !common.IsHexAddress(c.EVMBridgeAddr) {
		return fmt.Errorf("evm_bridge_addr %q is not a valid 0x address", c.EVMBridgeAddr)
	}
	if c.EVMRPCURL == "" {
		return fmt.Errorf("evm_rpc_url is required when the bridge is configured")
	}
	return nil
}

// validateForeignChains enforces the inbound-bridge invariants: each
// [[evm_foreign_chain]] needs a non-zero chain id, a non-empty RPC url, and a
// valid 0x bridge address; chain ids must be unique and must not collide with
// the home chain (evm_bridge_source_chain_id). Because inbound shares the home
// route file, configuring any foreign chain requires the home bridge to be set
// (which also guarantees evm_rpc_url). Per-chain inbound-route existence is
// checked at wire time against the loaded registry, not here.
func (c *Config) validateForeignChains() error {
	if len(c.EVMForeignChains) == 0 {
		return nil
	}
	if c.EVMBridgeAddr == "" {
		return fmt.Errorf("evm_foreign_chain requires the bridge to be configured (evm_bridge_addr, evm_bridge_routes_path, evm_bridge_source_chain_id)")
	}
	seen := map[uint64]bool{}
	for i, fc := range c.EVMForeignChains {
		if fc.ChainID == 0 {
			return fmt.Errorf("evm_foreign_chain[%d] chain_id is required", i)
		}
		if fc.ChainID == c.EVMBridgeSourceChainID {
			return fmt.Errorf("evm_foreign_chain[%d] chain_id %d is the home chain (evm_bridge_source_chain_id)", i, fc.ChainID)
		}
		if seen[fc.ChainID] {
			return fmt.Errorf("evm_foreign_chain[%d] chain_id %d is declared more than once", i, fc.ChainID)
		}
		seen[fc.ChainID] = true
		if fc.RPCURL == "" {
			return fmt.Errorf("evm_foreign_chain[%d] (chain_id %d) rpc_url is required", i, fc.ChainID)
		}
		if !common.IsHexAddress(fc.BridgeAddr) {
			return fmt.Errorf("evm_foreign_chain[%d] (chain_id %d) bridge_addr %q is not a valid 0x address", i, fc.ChainID, fc.BridgeAddr)
		}
	}
	return nil
}

// validateOracle enforces the oracle-feed invariants: when set it must be a
// valid 0x address and requires an EVM RPC endpoint (get_oracle_price reads it
// via eth_call). Independent of the swap addresses.
func (c *Config) validateOracle() error {
	if c.EVMOracleAddr == "" {
		return nil
	}
	if !common.IsHexAddress(c.EVMOracleAddr) {
		return fmt.Errorf("evm_oracle_addr %q is not a valid 0x address", c.EVMOracleAddr)
	}
	if c.EVMRPCURL == "" {
		return fmt.Errorf("evm_rpc_url is required when evm_oracle_addr is set")
	}
	return nil
}

// validateSwap enforces the swap-address invariants: router + WSVP are
// both-or-neither, must be valid 0x addresses when set, and require an EVM RPC
// endpoint (the swap tools need eth_call + eth_sendRawTransaction).
func (c *Config) validateSwap() error {
	if c.EVMUniswapRouterAddr == "" && c.EVMWSVPAddr == "" {
		return nil
	}
	if c.EVMUniswapRouterAddr == "" || c.EVMWSVPAddr == "" {
		return fmt.Errorf("evm_uniswap_router_addr and evm_wsvp_addr must be set together")
	}
	if !common.IsHexAddress(c.EVMUniswapRouterAddr) {
		return fmt.Errorf("evm_uniswap_router_addr %q is not a valid 0x address", c.EVMUniswapRouterAddr)
	}
	if !common.IsHexAddress(c.EVMWSVPAddr) {
		return fmt.Errorf("evm_wsvp_addr %q is not a valid 0x address", c.EVMWSVPAddr)
	}
	if c.EVMRPCURL == "" {
		return fmt.Errorf("evm_rpc_url is required when evm_uniswap_router_addr / evm_wsvp_addr are set")
	}
	return nil
}

// DefaultFeeDenom / DefaultFeeAmount / DefaultFeeGasLimit are the fee applied
// to non-CLOB txs when the [fee] section is absent. They match a chain whose
// minimum-gas-prices is 25000000000asvp evaluated at a 1,000,000 gas limit
// (≈0.025 SVP total).
const (
	DefaultFeeDenom    = "asvp"
	DefaultFeeAmount   = "25000000000000000"
	DefaultFeeGasLimit = uint64(1_000_000)
)

// applyDefaults fills unset fields so an operator can omit the whole [fee]
// section and still produce broadcastable non-CLOB txs.
func (f *FeeConfig) applyDefaults() {
	if f.Denom == "" {
		f.Denom = DefaultFeeDenom
	}
	if f.Amount == "" {
		f.Amount = DefaultFeeAmount
	}
	if f.GasLimit == 0 {
		f.GasLimit = DefaultFeeGasLimit
	}
}

// validate runs after applyDefaults: denom must be non-empty and amount must
// parse as a non-negative integer (a zero amount is allowed for a hypothetical
// fee-free chain).
func (f *FeeConfig) validate() error {
	if f.Denom == "" {
		return fmt.Errorf("fee.denom is required")
	}
	amt, ok := math.NewIntFromString(f.Amount)
	if !ok {
		return fmt.Errorf("fee.amount %q is not a valid integer", f.Amount)
	}
	if amt.IsNegative() {
		return fmt.Errorf("fee.amount %q must be non-negative", f.Amount)
	}
	return nil
}
