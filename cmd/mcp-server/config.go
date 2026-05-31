package main

import (
	"fmt"
	"time"

	"github.com/BurntSushi/toml"
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

	// BroadcastMode is informational for whoami. The server always
	// broadcasts the signed tx the client returns; "local" mode (where
	// the client broadcasts directly) is documented for a future version.
	BroadcastMode string `toml:"broadcast_mode"`

	Cache  CacheConfig  `toml:"cache"`
	Limits LimitsConfig `toml:"limits"`
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
	return nil
}
