package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"cosmossdk.io/log"
	"github.com/ethereum/go-ethereum/common"

	"github.com/dydxprotocol/v4-chain/protocol/app"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/auth"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/builder"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/chain"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/faucet"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/indexer"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/limits"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/logging"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/markets"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/policy"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/tools"
)

// dynamicTenantAdapter wraps auth.DynamicTenantStore to satisfy
// policy.DynamicSource — converts auth.TenantRecord (which auth knows
// about) into policy.TenantPolicy (which the policy engine wants). Kept
// in main to keep auth from importing policy (and vice versa).
type dynamicTenantAdapter struct{ store *auth.DynamicTenantStore }

func (a dynamicTenantAdapter) LookupTenantPolicy(tenantID string) (policy.TenantPolicy, bool) {
	rec, err := a.store.LookupByTenantID(tenantID)
	if err != nil {
		return policy.TenantPolicy{}, false
	}
	return policy.TenantPolicy{
		TenantID:           rec.TenantID,
		Owner:              rec.Owner,
		AllowedSubaccounts: rec.AllowedSubaccounts,
		KillSwitch:         rec.KillSwitch,
	}, true
}

// BuildServer wires the configuration into a ready-to-run Server: dials
// the chain gRPC, builds the Indexer client, sets up the markets cache,
// policy engine, audit log, idempotency, and tool handlers, and returns
// the Server. The caller invokes Server.Run(ctx) to start it.
func BuildServer(ctx context.Context, cfg *Config) (*Server, error) {
	logger := logging.NewLogger(log.NewLogger(os.Stderr))

	// gRPC + interface registry from the app's encoding config.
	grpcConn, err := chain.Dial(ctx, cfg.GrpcAddr)
	if err != nil {
		return nil, fmt.Errorf("dial chain gRPC: %w", err)
	}
	encCfg := app.GetEncodingConfig()

	chainDeps := tools.ChainDeps{
		Account:         chain.NewAccountClient(grpcConn, encCfg.InterfaceRegistry),
		Broadcast:       chain.NewBroadcastClient(grpcConn),
		ClobQuery:       chain.NewClobQueryClient(grpcConn),
		SubaccountQuery: chain.NewSubaccountQueryClient(grpcConn),
		PricesQuery:     chain.NewPricesQueryClient(grpcConn),
		BankQuery:       chain.NewBankQueryClient(grpcConn),
	}
	cometClient, err := chain.NewCometBftClient(cfg.CometRPCURL)
	if err != nil {
		grpcConn.Close()
		return nil, fmt.Errorf("cometbft client: %w", err)
	}
	chainDeps.CometBft = cometClient

	// EVM is optional: only wire the JSON-RPC client + assembler when an
	// evm_rpc_url is configured. Without it, EVM tools refuse at call time
	// (Deps.EVM.Assembler stays nil) and non-EVM deployments are unaffected.
	var evmDeps tools.EVMDeps
	if cfg.EVMRPCURL != "" {
		evmClient, err := chain.NewEVMClient(ctx, cfg.EVMRPCURL)
		if err != nil {
			grpcConn.Close()
			return nil, fmt.Errorf("evm client: %w", err)
		}
		chainDeps.EVM = evmClient
		evmDeps = tools.EVMDeps{
			Assembler: builder.NewEVMAssembler(evmClient),
		}
		// Swap tools are wired only when the router + WSVP addresses are also
		// configured (config validation guarantees both-or-neither + valid
		// hex). Without them Uniswap stays nil and the swap tools refuse.
		if cfg.EVMUniswapRouterAddr != "" {
			uni, err := builder.NewUniswapV2(
				common.HexToAddress(cfg.EVMUniswapRouterAddr),
				common.HexToAddress(cfg.EVMWSVPAddr),
			)
			if err != nil {
				grpcConn.Close()
				return nil, fmt.Errorf("uniswap binding: %w", err)
			}
			evmDeps.Uniswap = uni
		}
	}

	// Faucet is optional: only wire the HTTP client when a faucet_base_url is
	// configured. Without it, the faucet tools refuse at call time
	// (Deps.Faucet stays nil) and non-faucet deployments are unaffected.
	var faucetClient *faucet.Client
	if cfg.FaucetBaseURL != "" {
		faucetClient = faucet.NewClient(cfg.FaucetBaseURL, faucet.Options{})
	}

	// Indexer + markets cache.
	idx := indexer.NewClient(cfg.IndexerBaseURL, indexer.Options{})
	mkts := markets.NewCache(idx, chainDeps.ClobQuery, time.Duration(cfg.Cache.MarketsRefresh), logger)

	// v0.3 dropped the static [[tenants]] table — every tenant is
	// auto-issued at runtime via the auth_challenge → auth_verify flow,
	// so the policy engine starts empty and is populated dynamically.

	// Funds-tool safety rails: caps come straight from cfg.Limits; the
	// withdraw ledger is in-memory, keyed by tenant_id, and resets on
	// restart. Swapping in a durable backend is an implementation of
	// limits.WithdrawLedger — no handler changes required.
	limitsCfg := limits.Config{
		DepositMaxUSDC:       cfg.Limits.DepositMaxUSDC,
		WithdrawMaxUSDC:      cfg.Limits.WithdrawMaxUSDC,
		TransferMaxUSDC:      cfg.Limits.TransferMaxUSDC,
		DailyWithdrawCapUSDC: cfg.Limits.DailyWithdrawCapUSDC,
	}
	withdrawLedger := limits.NewMemoryLedger(limitsCfg.DailyWithdrawCapUSDC, nil)

	// v0.3 self-service auth state. Both stores are in-memory + TTL-
	// bounded; the durable backend lands alongside the durable withdraw
	// ledger. Auto-issued tenants inherit a fixed allowlist of subaccount
	// numbers (v0.3.0 ships 0..9; per-user negotiation deferred).
	nonceStore := auth.NewNonceStore(auth.DefaultChallengeTTL, nil)
	dynamicTenants := auth.NewDynamicTenantStore(auth.DynamicTenantStoreConfig{
		BearerTTL:                 auth.DefaultBearerTTL,
		DefaultAllowedSubaccounts: []uint32{0, 1, 2, 3, 4, 5, 6, 7, 8, 9},
	}, nil)
	ipLimit := auth.NewIPRateLimiter(auth.DefaultIPChallengeRate, auth.DefaultIPChallengeWindow, nil)
	sessionBearers := auth.NewSessionBearers(auth.DefaultBearerTTL, nil)

	policyEngine := policy.NewEngine(nil)
	policyEngine.SetDynamicSource(dynamicTenantAdapter{store: dynamicTenants})

	deps := tools.Deps{
		Chain:             chainDeps,
		Indexer:           idx,
		Markets:           mkts,
		Builder:           builder.NewAssembler(cfg.ChainID, cfg.Fee.Denom, cfg.Fee.Amount, cfg.Fee.GasLimit),
		Faucet:            faucetClient,
		EVM:               evmDeps,
		Policy:            policyEngine,
		Auditor:           policy.NewStdoutAuditor(),
		Idempotency:       policy.NewIdempotency(0),
		RateLimit:         policy.NewRateLimiter(0, 0),
		Limits:            limitsCfg,
		WithdrawLedger:    withdrawLedger,
		NonceStore:        nonceStore,
		DynamicTenants:    dynamicTenants,
		IPChallengeLimit:  ipLimit,
		SessionBearers:    sessionBearers,
		Logger:            logger,
		InterfaceRegistry: encCfg.InterfaceRegistry,
		BroadcastMode:     cfg.BroadcastMode,
	}
	handlers := tools.New(cfg.ChainID, deps)

	return NewServer(cfg, handlers, grpcConn, mkts, logger, dynamicTenants, sessionBearers), nil
}
