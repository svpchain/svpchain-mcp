// Package transport hosts the Streamable HTTP transport for the remote MCP
// server.
//
// It wraps mcp.NewStreamableHTTPHandler from the official Go SDK into a
// stdlib *http.Server (so we can layer arbitrary middleware — bearer-token
// auth, structured logging, metrics — between the network and the MCP
// handler) and provides graceful-shutdown semantics matching
// protocol/streaming/ws/websocket_server.go (5s shutdown deadline).
package transport
