package main

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"cosmossdk.io/log"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc"

	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/auth"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/lendora"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/markets"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/tools"
	"github.com/dydxprotocol/v4-chain/protocol/lib/mcp/transport"
)

// Server owns the runtime: the MCP server instance, the HTTP transport,
// the markets cache, and the underlying chain gRPC connection. Constructed
// by wire.BuildServer.
type Server struct {
	cfg       *Config
	mcpServer *mcp.Server
	transport *transport.Server
	markets   *markets.Cache
	// lendoraMarkets is the EVM-sourced Lendora market cache. Nil when Lendora is
	// not configured; Run skips its refresher then.
	lendoraMarkets *lendora.Cache
	grpcConn       *grpc.ClientConn
	logger         log.Logger

	// dynamicTenants is the only tenant source in v0.3 — populated on the
	// fly by the auth_challenge → auth_verify flow. Bearers are looked
	// up directly against this store; LookupTenantByToken has no other
	// path.
	dynamicTenants *auth.DynamicTenantStore
	sessionBearers *auth.SessionBearers
}

// NewServer constructs a Server. Registers all tools onto a new
// mcp.Server, wraps the Streamable HTTP handler with the auth
// middleware, and binds the result to cfg.ListenAddr.
func NewServer(
	cfg *Config,
	handlers *tools.Handlers,
	grpcConn *grpc.ClientConn,
	mkts *markets.Cache,
	lendoraMkts *lendora.Cache,
	logger log.Logger,
	dynamicTenants *auth.DynamicTenantStore,
	sessionBearers *auth.SessionBearers,
) *Server {
	mcpSrv := mcp.NewServer(&mcp.Implementation{
		Name:    "svpchain-mcp",
		Version: "v0.1.0",
	}, nil)
	tools.Register(mcpSrv, handlers)

	s := &Server{
		cfg:            cfg,
		mcpServer:      mcpSrv,
		markets:        mkts,
		lendoraMarkets: lendoraMkts,
		grpcConn:       grpcConn,
		logger:         logger,
		dynamicTenants: dynamicTenants,
		sessionBearers: sessionBearers,
	}

	// Auth runs at the MCP layer, not the HTTP layer: the go-sdk's
	// StreamableHTTP transport captures req.Context() only at the
	// initialize call, so per-request ctx mutations from an HTTP
	// middleware never reach the handler. The receiving middleware
	// here reads each call's headers from req.Extra.Header instead.
	// See mcp_auth.go for the full rationale.
	mcpSrv.AddReceivingMiddleware(authReceivingMiddleware(s, sessionBearers))

	// Streamable HTTP handler. The HTTP-layer wrapper now does one
	// thing only — stamp the client IP into a synthetic header so the
	// MCP receiving middleware can read it (r.RemoteAddr doesn't reach
	// req.Extra otherwise). See auth.go.
	rawHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return mcpSrv
	}, nil)
	s.transport = transport.NewServer(cfg.ListenAddr, ipMiddleware(rawHandler), logger)

	return s
}

// LookupTenantByToken implements tenantLookup for the auth middleware.
// v0.3 only knows about dynamic tenants — bearers are minted by
// auth_verify and live in the dynamic store. An unknown bearer simply
// fails to resolve; downstream handlers reject the request because
// TenantFrom returns ok=false.
func (s *Server) LookupTenantByToken(token string) (tools.TenantContext, bool) {
	rec, err := s.dynamicTenants.LookupByBearer(token)
	if err != nil {
		return tools.TenantContext{}, false
	}
	return tools.TenantContext{TenantID: rec.TenantID, Owner: rec.Owner}, true
}

// Run starts the markets cache refresher and the HTTP server, then blocks
// until ctx is cancelled (SIGINT/SIGTERM). On exit it gracefully shuts down
// the HTTP server with a 10s deadline and drains the cache goroutine.
func (s *Server) Run(ctx context.Context) error {
	// Markets cache: required to be populated before any build_* tool can
	// run. Failure here is fatal; the periodic refresher logs further
	// errors but does not stop the server.
	cacheDone := make(chan error, 1)
	go func() {
		cacheDone <- s.markets.Run(ctx)
	}()

	// Lendora market cache: runs under the same lifecycle when Lendora is
	// configured. Its initial refresh failing is fatal (the lendora_* tools
	// cannot resolve assets without it); periodic errors are logged.
	lendoraDone := make(chan error, 1)
	if s.lendoraMarkets != nil {
		go func() {
			lendoraDone <- s.lendoraMarkets.Run(ctx)
		}()
	} else {
		lendoraDone <- nil
	}

	// HTTP server.
	httpDone := make(chan error, 1)
	go func() {
		httpDone <- s.transport.Start()
	}()

	select {
	case <-ctx.Done():
		s.logger.Info("shutting down (signal received)")
	case err := <-httpDone:
		if err != nil {
			return fmt.Errorf("http server: %w", err)
		}
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.transport.Shutdown(shutdownCtx); err != nil {
		s.logger.Error("http shutdown error", "error", err)
	}
	// markets.Run honors ctx.Done; wait for it to wind down.
	if err := <-cacheDone; err != nil {
		s.logger.Error("markets cache exited with error", "error", err)
	}
	if err := <-lendoraDone; err != nil {
		s.logger.Error("lendora market cache exited with error", "error", err)
	}
	return nil
}

// Close releases the chain gRPC connection. Safe to call multiple times.
func (s *Server) Close() {
	if s.grpcConn != nil {
		_ = s.grpcConn.Close()
		s.grpcConn = nil
	}
}
