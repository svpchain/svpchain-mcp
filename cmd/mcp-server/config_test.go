package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

const validConfigTOML = `
chain_id         = "localsvp-1"
grpc_addr        = "127.0.0.1:9090"
comet_rpc_url    = "http://127.0.0.1:26657"
indexer_base_url = "http://127.0.0.1:3002"
listen_addr      = "127.0.0.1:8765"
broadcast_mode   = "server"

[cache]
markets_refresh = "30s"

[limits]
deposit_max_usdc       = 1000
withdraw_max_usdc      = 500
transfer_max_usdc      = 500
daily_withdraw_cap_usdc = 100
`

func writeTempConfig(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.toml")
	require.NoError(t, os.WriteFile(p, []byte(body), 0o600))
	return p
}

func TestLoadConfig_HappyPath(t *testing.T) {
	cfg, err := LoadConfig(writeTempConfig(t, validConfigTOML))
	require.NoError(t, err)

	require.Equal(t, "localsvp-1", cfg.ChainID)
	require.Equal(t, "127.0.0.1:9090", cfg.GrpcAddr)
	require.Equal(t, "127.0.0.1:8765", cfg.ListenAddr)
	require.Equal(t, "server", cfg.BroadcastMode)
	require.Equal(t, Duration(30*time.Second), cfg.Cache.MarketsRefresh)
	require.EqualValues(t, 1000, cfg.Limits.DepositMaxUSDC)
}

func TestLoadConfig_DefaultsBroadcastMode(t *testing.T) {
	body := stripLine(validConfigTOML, "broadcast_mode")
	cfg, err := LoadConfig(writeTempConfig(t, body))
	require.NoError(t, err)
	require.Equal(t, "server", cfg.BroadcastMode, "should default to server-broadcast")
}

func TestLoadConfig_FeeDefaults(t *testing.T) {
	// validConfigTOML has no [fee] section, so the defaults must fill in.
	cfg, err := LoadConfig(writeTempConfig(t, validConfigTOML))
	require.NoError(t, err)
	require.Equal(t, DefaultFeeDenom, cfg.Fee.Denom)
	require.Equal(t, DefaultFeeAmount, cfg.Fee.Amount)
	require.Equal(t, DefaultFeeGasLimit, cfg.Fee.GasLimit)
}

func TestLoadConfig_FeeOverride(t *testing.T) {
	body := validConfigTOML + `
[fee]
denom     = "erc20/usdc"
amount    = "25000"
gas_limit = 2000000
`
	cfg, err := LoadConfig(writeTempConfig(t, body))
	require.NoError(t, err)
	require.Equal(t, "erc20/usdc", cfg.Fee.Denom)
	require.Equal(t, "25000", cfg.Fee.Amount)
	require.EqualValues(t, 2000000, cfg.Fee.GasLimit)
}

func TestLoadConfig_FeeRejectsBadAmount(t *testing.T) {
	body := validConfigTOML + `
[fee]
amount = "not-a-number"
`
	_, err := LoadConfig(writeTempConfig(t, body))
	require.Error(t, err)
	require.Contains(t, err.Error(), "fee.amount")
}

func TestLoadConfig_Rejects(t *testing.T) {
	cases := []struct {
		name        string
		body        string
		expectError string
	}{
		{
			name:        "missing chain_id",
			body:        stripLine(validConfigTOML, "chain_id"),
			expectError: "chain_id is required",
		},
		{
			name:        "missing grpc_addr",
			body:        stripLine(validConfigTOML, "grpc_addr"),
			expectError: "grpc_addr is required",
		},
		{
			name:        "missing listen_addr",
			body:        stripLine(validConfigTOML, "listen_addr"),
			expectError: "listen_addr is required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadConfig(writeTempConfig(t, tc.body))
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.expectError)
		})
	}
}

func TestLoadConfig_SwapAddresses(t *testing.T) {
	router := "0x1111111111111111111111111111111111111111"
	wsvp := "0x2222222222222222222222222222222222222222"

	// These are top-level keys, so they must precede the [cache]/[limits]
	// table headers in validConfigTOML — keys after a table header belong to
	// that table.
	withTop := func(extra string) string { return extra + validConfigTOML }

	t.Run("both set with evm_rpc_url", func(t *testing.T) {
		body := withTop("evm_rpc_url = \"http://127.0.0.1:8545\"\n" +
			"evm_uniswap_router_addr = \"" + router + "\"\nevm_wsvp_addr = \"" + wsvp + "\"\n")
		cfg, err := LoadConfig(writeTempConfig(t, body))
		require.NoError(t, err)
		require.Equal(t, router, cfg.EVMUniswapRouterAddr)
		require.Equal(t, wsvp, cfg.EVMWSVPAddr)
	})

	t.Run("neither set is fine", func(t *testing.T) {
		_, err := LoadConfig(writeTempConfig(t, validConfigTOML))
		require.NoError(t, err)
	})

	cases := []struct {
		name        string
		extra       string
		expectError string
	}{
		{
			"router without wsvp",
			"evm_rpc_url = \"http://127.0.0.1:8545\"\nevm_uniswap_router_addr = \"" + router + "\"\n",
			"must be set together",
		},
		{
			"wsvp without router",
			"evm_rpc_url = \"http://127.0.0.1:8545\"\nevm_wsvp_addr = \"" + wsvp + "\"\n",
			"must be set together",
		},
		{
			"invalid router address",
			"evm_rpc_url = \"http://127.0.0.1:8545\"\nevm_uniswap_router_addr = \"0xnope\"\nevm_wsvp_addr = \"" + wsvp + "\"\n",
			"not a valid 0x address",
		},
		{
			"set without evm_rpc_url",
			"evm_uniswap_router_addr = \"" + router + "\"\nevm_wsvp_addr = \"" + wsvp + "\"\n",
			"evm_rpc_url is required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := LoadConfig(writeTempConfig(t, withTop(tc.extra)))
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.expectError)
		})
	}
}

// stripLine removes any line in body that begins (after leading whitespace)
// with prefix — used by the rejection tests to strip a required field.
func stripLine(body, prefix string) string {
	var b strings.Builder
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimLeft(line, " \t"), prefix) {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	return b.String()
}
