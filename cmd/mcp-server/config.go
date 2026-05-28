package main

import (
	"fmt"
	"time"

	"github.com/BurntSushi/toml"
)

// Config is the full server configuration loaded from a TOML file.
// v0.1 keeps it deliberately small; v0.2 adds per-tool caps,
// withdraw allowlists, mTLS paths, etc.
type Config struct {
	ChainID        string `toml:"chain_id"`
	GrpcAddr       string `toml:"grpc_addr"`
	CometRPCURL    string `toml:"comet_rpc_url"`
	IndexerBaseURL string `toml:"indexer_base_url"`
	ListenAddr     string `toml:"listen_addr"`

	// BroadcastMode is informational for whoami in v0.1. The server always
	// broadcasts the signed tx the client returns; "local" mode (where the
	// client broadcasts directly) is documented for v0.3+ in the design.
	BroadcastMode string `toml:"broadcast_mode"`

	Auth    AuthConfig     `toml:"auth"`
	Cache   CacheConfig    `toml:"cache"`
	Tenants []TenantConfig `toml:"tenants"`
}

type AuthConfig struct {
	Mode string `toml:"mode"` // "bearer" in v0.1
}

type CacheConfig struct {
	// MarketsRefresh is parsed as a Go duration string ("60s", "2m"…).
	// Zero (or unset) means use the package default in markets.NewCache.
	MarketsRefresh Duration `toml:"markets_refresh"`
}

// TenantConfig binds a bearer token to an owner + subaccount allowlist.
type TenantConfig struct {
	TenantID           string   `toml:"tenant_id"`
	BearerToken        string   `toml:"bearer_token"`
	Owner              string   `toml:"owner"`
	AllowedSubaccounts []uint32 `toml:"allowed_subaccounts"`
	KillSwitch         bool     `toml:"kill_switch"`
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

// Validate enforces v0.1's required fields and uniqueness invariants
// (bearer tokens and tenant ids must each be unique).
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
	if c.Auth.Mode != "bearer" {
		return fmt.Errorf("auth.mode must be \"bearer\" in v0.1 (got %q)", c.Auth.Mode)
	}
	if len(c.Tenants) == 0 {
		return fmt.Errorf("at least one [[tenants]] entry is required")
	}
	seenID := map[string]struct{}{}
	seenToken := map[string]struct{}{}
	for i, t := range c.Tenants {
		if t.TenantID == "" {
			return fmt.Errorf("tenants[%d].tenant_id is required", i)
		}
		if _, dup := seenID[t.TenantID]; dup {
			return fmt.Errorf("duplicate tenant_id %q", t.TenantID)
		}
		seenID[t.TenantID] = struct{}{}
		if t.BearerToken == "" {
			return fmt.Errorf("tenants[%d].bearer_token is required", i)
		}
		if _, dup := seenToken[t.BearerToken]; dup {
			return fmt.Errorf("duplicate bearer_token (collision in tenants[%d])", i)
		}
		seenToken[t.BearerToken] = struct{}{}
		if t.Owner == "" {
			return fmt.Errorf("tenants[%d].owner is required", i)
		}
		if len(t.AllowedSubaccounts) == 0 {
			return fmt.Errorf("tenants[%d].allowed_subaccounts must be non-empty", i)
		}
	}
	return nil
}
