# Dockerfile for the svpchain remote MCP server (cmd/mcp-server).
#
# This is a stateless build — the resulting image contains only the
# mcp-server binary plus ca-certificates. Runtime config lives in
# mcp.toml, which the deployer mounts read-only at /etc/svpchain-mcp/mcp.toml
# (see scripts/mcp-server-deploy.sh).
#
# Built independently of the main protocol/Dockerfile (which ships
# svpchaind, not mcp-server) so deploy churn on either binary doesn't
# bust the other's layer cache.
#
# Build from the repo root. The image builds from a VENDORED dependency tree
# (`./vendor`) so the build context is self-contained: this repo depends on the
# protocol module via a go.mod `replace` to a sibling checkout that is NOT
# inside the Docker context, so a non-vendored build could not resolve it.
# Vendoring also removes the need for GOPRIVATE credentials in the builder.
#
# Vendor first, then build:
#   go mod vendor
#   docker build --platform linux/amd64 \
#     -t svpchain-mcp:<tag> \
#     -f cmd/mcp-server/Dockerfile .
# Or just `make docker`, which runs `go mod vendor` for you.
#
# --platform linux/amd64 is required when building on Apple Silicon for a
# linux/amd64 remote; see scripts/mcp-server-deploy.sh for the deploy flow.

# NB: digest pin matches the top-level protocol/Dockerfile. Get the
# current digest via `docker buildx imagetools inspect golang:1.23.8-alpine`.
ARG GOLANG_1_23_ALPINE_DIGEST="b7486658b87d34ecf95125e5b97e8dfe86c21f712aa36fc0c702e5dc41dc63e1"

# --------------------------------------------------------
# Builder
# --------------------------------------------------------

FROM golang@sha256:${GOLANG_1_23_ALPINE_DIGEST} AS builder

RUN set -eux; apk add --no-cache ca-certificates build-base; apk add git linux-headers bash binutils-gold

WORKDIR /svpchain

# The build is fully vendored (see header): all dependencies — including the
# protocol module resolved through go.mod's replace — are copied in via
# `./vendor`, so there is no `go mod download` step and no GOPRIVATE / network
# fetch. `go build -mod=vendor` (below) uses the vendored tree exclusively.
COPY . .

# VERSION/COMMIT are declared HERE, not at the top of the stage, for
# the same cache reason as protocol/Dockerfile: BuildKit folds in-scope
# ARG values into the cache key of every subsequent RUN, so declaring
# early would bust the apk-add and go-mod-download layers on every
# commit.
ARG VERSION
ARG COMMIT

# Build only the mcp-server binary. Same -tags / -ldflags pattern as
# the top-level Dockerfile (static linking via musl) so the resulting
# binary runs on the alpine runner without dynamic deps. The version
# strings target the cosmos-sdk version package the binary already
# imports transitively.
RUN --mount=type=cache,target=/root/.cache/go-build \
    --mount=type=cache,target=/root/go/pkg/mod \
    go build \
      -mod=vendor \
      -tags "netgo,muslc" \
      -ldflags "-X github.com/cosmos/cosmos-sdk/version.Name=svpchain \
              -X github.com/cosmos/cosmos-sdk/version.AppName=svpchain-mcp \
              -X github.com/cosmos/cosmos-sdk/version.Version=$VERSION \
              -X github.com/cosmos/cosmos-sdk/version.Commit=$COMMIT \
              -X github.com/cosmos/cosmos-sdk/version.BuildTags='netgo,muslc' \
              -w -s -linkmode=external -extldflags '-Wl,-z,muldefs -static'" \
      -trimpath \
      -o /svpchain/build/mcp-server \
      ./cmd/mcp-server

# --------------------------------------------------------
# Runner
# --------------------------------------------------------

FROM alpine:3.21

RUN apk add --no-cache bash ca-certificates

COPY --from=builder /svpchain/build/mcp-server /bin/mcp-server

# v0.3 keeps nonce / dynamic-tenants / session-bearers / withdraw-ledger
# in memory — no persistent volume required. The only filesystem read
# the runtime needs is the operator-supplied mcp.toml mounted by the
# deployer at the path below.

# Default MCP listen port. Override via mcp.toml listen_addr (see
# cmd/mcp-server/config.go). The container relies on --network host
# in the deploy script, so this EXPOSE is informational only.
EXPOSE 8765

ENTRYPOINT ["/bin/mcp-server"]
CMD ["--config", "/etc/svpchain-mcp/mcp.toml"]
