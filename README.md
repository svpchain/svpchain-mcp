# svpchain-mcp

The svpchain **remote MCP server** — a stateless HTTP service that exposes
svpchain trading, transfer, bridging, faucet, and read tools over the
[Model Context Protocol](https://modelcontextprotocol.io). It **constructs**
(never signs) transactions and serves reads; the private key stays with the
client / local signer. See `cmd/mcp-server/doc.go` for the server design and
`docs/` for the agent-service design and deploy runbooks.

This repo was extracted from the svpchain monorepo (`protocol/cmd/mcp-server`
and `protocol/lib/mcp`) with full git history preserved.

## Relationship to the protocol module

The server is a **client** of the chain (gRPC / CometBFT-RPC / Indexer-REST /
EVM-JSON-RPC) — it never runs chain state. It does, however, reuse the svpchain
protocol module's generated proto types (`x/*/types`), address-prefix config
(`app/config`), and interface registry (`app/module`, `app/basic_manager`).

It therefore depends on `github.com/dydxprotocol/v4-chain/protocol` as a Go
module, consumed from a **sibling checkout** of the monorepo via a `replace`
directive in `go.mod`:

```
replace github.com/dydxprotocol/v4-chain/protocol => ../svpchain-main/protocol
```

Expected on-disk layout:

```
projects/svpchain/
├── svpchain-main/     # the monorepo (protocol module lives in ./protocol)
└── svpchain-mcp/      # this repo
```

Point the `replace` at wherever `protocol/` lives if your layout differs.

Because Go does not apply a dependency module's own `replace` directives, the
20 fork replaces from `protocol/go.mod` (cosmos-sdk, cometbft, cosmos/evm,
iavl, …) are **copied verbatim** into this repo's `go.mod`. Keep them in sync
with `protocol/go.mod`.

The interface registry is built by `lib/mcp/mcpcodec` (a drop-in replacement
for the chain's `app.GetEncodingConfig()`) so the server does not import the
heavy top-level `app` package.

## Layout

```
cmd/mcp-server/   the remote MCP HTTP server binary (+ Dockerfile)
cmd/devsign/      one-shot CLI that signs a TxPayload with lib/mcp/signer (dev/e2e)
lib/mcp/          server internals: tools, chain/indexer clients, builder,
                  auth, policy, signer, payload, units, …
lib/mcp/mcpcodec/ local interface-registry/codec (replaces app.GetEncodingConfig)
scripts/          mcp-server-deploy.sh (remote docker deploy), mcp-e2e-test.sh
docs/             agent-service design + deploy runbooks
```

## Build & test

```
make build   # -> build/mcp-server, build/devsign
make test
make vet
```

Runtime config is an operator-supplied `mcp.toml` (see `cmd/mcp-server/config.go`).
Run with `mcp-server --config /path/to/mcp.toml`.

## Docker & deploy

The image build context is this repo's root, which does not contain the
sibling protocol checkout the `replace` points at. `make docker` therefore
runs `go mod vendor` first so the build is self-contained (no sibling checkout
or GOPRIVATE credentials needed inside the image):

```
make docker                     # vendors, then builds svpchain-mcp:<version>
./scripts/mcp-server-deploy.sh --host user@remote   # ship + run on a remote host
```

See `docs/mcp-server-deploy.md` for the full deploy flow and flags.
