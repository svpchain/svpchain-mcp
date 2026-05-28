package main

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"cosmossdk.io/log"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/grpc"

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
	grpcConn  *grpc.ClientConn
	logger    log.Logger

	tokenToTenant map[string]string
	tenantToOwner map[string]string
}

// NewServer constructs a Server. It registers the v0.1 tools onto a new
// mcp.Server, wraps the Streamable HTTP handler with bearer-token auth
// middleware, and binds the result to cfg.ListenAddr.
func NewServer(
	cfg *Config,
	handlers *tools.Handlers,
	grpcConn *grpc.ClientConn,
	mkts *markets.Cache,
	logger log.Logger,
	tokenToTenant, tenantToOwner map[string]string,
) *Server {
	mcpSrv := mcp.NewServer(&mcp.Implementation{
		Name:    "svpchain-mcp",
		Version: "v0.1.0",
	}, nil)
	tools.Register(mcpSrv, handlers)

	s := &Server{
		cfg:           cfg,
		mcpServer:     mcpSrv,
		markets:       mkts,
		grpcConn:      grpcConn,
		logger:        logger,
		tokenToTenant: tokenToTenant,
		tenantToOwner: tenantToOwner,
	}

	// Streamable HTTP handler. The per-request callback returns the same
	// MCP server for every request; tenant identity is carried through the
	// request context by the auth middleware.
	rawHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return mcpSrv
	}, nil)
	authed := bearerAuthMiddleware(s, rawHandler)
	s.transport = transport.NewServer(cfg.ListenAddr, authed, logger)

	return s
}

// LookupTenantByToken implements tenantLookup for the auth middleware.
func (s *Server) LookupTenantByToken(token string) (tools.TenantContext, bool) {
	tenantID, ok := s.tokenToTenant[token]
	if !ok {
		return tools.TenantContext{}, false
	}
	owner := s.tenantToOwner[tenantID]
	return tools.TenantContext{TenantID: tenantID, Owner: owner}, true
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
	return nil
}

// Close releases the chain gRPC connection. Safe to call multiple times.
func (s *Server) Close() {
	if s.grpcConn != nil {
		_ = s.grpcConn.Close()
		s.grpcConn = nil
	}
}
