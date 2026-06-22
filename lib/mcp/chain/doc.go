// Package chain wraps the svpchain gRPC and CometBFT RPC clients used by
// the remote MCP server.
//
// Each service gets its own file and a small interface (so mockery can
// generate test doubles): AccountClient (auth.Query), BroadcastClient
// (sdktx.Service, BROADCAST_MODE_SYNC), ClobQueryClient, SubaccountQueryClient,
// BankQueryClient, EVMClient (EVM JSON-RPC), and CometBftClient (tx-status via
// cometbft/rpc/client/http).
//
// gRPC dialing reuses daemons/types.GrpcClientImpl.NewTcpConnection so the
// dial pattern matches existing daemons exactly.
package chain
