package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"cosmossdk.io/log"

	"github.com/dydxprotocol/v4-chain/protocol/app"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/builder"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/chain"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/indexer"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/limits"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/logging"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/markets"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/policy"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/tools"
)

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
	}
	cometClient, err := chain.NewCometBftClient(cfg.CometRPCURL)
	if err != nil {
		grpcConn.Close()
		return nil, fmt.Errorf("cometbft client: %w", err)
	}
	chainDeps.CometBft = cometClient

	// Indexer + markets cache.
	idx := indexer.NewClient(cfg.IndexerBaseURL, indexer.Options{})
	mkts := markets.NewCache(idx, chainDeps.ClobQuery, time.Duration(cfg.Cache.MarketsRefresh), logger)

	// Policy: build per-tenant table and the bearer-token lookup tables
	// the auth middleware uses.
	tenants := make([]policy.TenantPolicy, 0, len(cfg.Tenants))
	tokenToTenant := make(map[string]string, len(cfg.Tenants))
	tenantToOwner := make(map[string]string, len(cfg.Tenants))
	for _, t := range cfg.Tenants {
		allow := make(map[uint32]struct{}, len(t.AllowedSubaccounts))
		for _, s := range t.AllowedSubaccounts {
			allow[s] = struct{}{}
		}
		tenants = append(tenants, policy.TenantPolicy{
			TenantID:           t.TenantID,
			Owner:              t.Owner,
			AllowedSubaccounts: allow,
			KillSwitch:         t.KillSwitch,
		})
		tokenToTenant[t.BearerToken] = t.TenantID
		tenantToOwner[t.TenantID] = t.Owner
	}

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

	deps := tools.Deps{
		Chain:             chainDeps,
		Indexer:           idx,
		Markets:           mkts,
		Builder:           builder.NewAssembler(cfg.ChainID),
		Policy:            policy.NewEngine(tenants),
		Auditor:           policy.NewStdoutAuditor(),
		Idempotency:       policy.NewIdempotency(0),
		RateLimit:         policy.NewRateLimiter(0, 0),
		Limits:            limitsCfg,
		WithdrawLedger:    withdrawLedger,
		Logger:            logger,
		InterfaceRegistry: encCfg.InterfaceRegistry,
		BroadcastMode:     cfg.BroadcastMode,
	}
	handlers := tools.New(cfg.ChainID, deps)

	return NewServer(cfg, handlers, grpcConn, mkts, logger, tokenToTenant, tenantToOwner), nil
}
