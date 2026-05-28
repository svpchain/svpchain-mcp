package transport

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"cosmossdk.io/log"
)

// DefaultShutdownTimeout matches the pattern in
// streaming/ws/websocket_server.go.
const DefaultShutdownTimeout = 5 * time.Second

// Server wraps an http.Server preconfigured with the MCP Streamable HTTP
// handler (passed in by the caller, already wrapped with any required
// middleware — auth, logging, etc.). Exposes a Start/Shutdown lifecycle
// matching the protocol's other long-running HTTP services.
type Server struct {
	inner  *http.Server
	logger log.Logger
}

// NewServer constructs a Server that listens on addr and dispatches every
// incoming request to handler.
//
// The caller is responsible for wrapping the bare MCP handler
// (mcp.NewStreamableHTTPHandler(...)) with bearer-token / mTLS
// middleware before passing it here.
func NewServer(addr string, handler http.Handler, logger log.Logger) *Server {
	return &Server{
		inner: &http.Server{
			Addr:              addr,
			Handler:           handler,
			ReadHeaderTimeout: 5 * time.Second,
		},
		logger: logger,
	}
}

// Start blocks on ListenAndServe until Shutdown is called or the server
// errors. http.ErrServerClosed is treated as the normal shutdown path and
// returns nil.
func (s *Server) Start() error {
	s.logger.Info("mcp http server listening", "addr", s.inner.Addr)
	if err := s.inner.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("mcp http server: %w", err)
	}
	return nil
}

// Shutdown gracefully drains in-flight requests for up to
// DefaultShutdownTimeout, then forces close. Safe to call from a separate
// goroutine while Start is blocked.
func (s *Server) Shutdown(ctx context.Context) error {
	shutdownCtx, cancel := context.WithTimeout(ctx, DefaultShutdownTimeout)
	defer cancel()
	return s.inner.Shutdown(shutdownCtx)
}
