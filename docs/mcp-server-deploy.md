# Deploying the svpchain remote MCP server

> Mirror of [`mcp-server-deploy.zh-CN.md`](mcp-server-deploy.zh-CN.md) — keep both
> updated when changing this file.

`scripts/mcp-server-deploy.sh` installs the remote MCP server
(`cmd/mcp-server`) onto a single remote SSH host as a docker container.
The script builds the image locally, ships it + a rendered `mcp.toml`
to the remote, and runs it under `--restart unless-stopped`.

The container is stateless: nonce / dynamic-tenants / session-bearers /
withdraw-ledger all live in memory. A redeploy cleanly wipes auth
state — clients holding bearers must re-run
`auth_challenge → sign_challenge → auth_verify` after each restart.

## Prerequisites

**On operator:**

- `docker` (with buildx) — build, save, and load the image.
- `ssh` + `rsync` — ship the tar + config to the remote.
- `curl` + `jq` — smoke-test the listener after start.
- Network access to fetch `github.com/deltaping/*` (set `GOPRIVATE` /
  configure your git credentials before building).

**On remote:**

- `docker` reachable by the SSH user (either group membership or `sudo`
  permission for `docker`).
- `sudo` permission for `/opt/svpchain-mcp` (created by the script).
- The chain's gRPC, CometBFT RPC, and indexer endpoints reachable from
  the remote — the container runs with `--network host`, so anything
  the host can reach the container can too.

## Quickstart

**On operator:**

```bash
./scripts/mcp-server-deploy.sh \
  --host www@svpdev1.example.com \
  --chain-id localsvp-1 \
  --grpc-addr 127.0.0.1:9090 \
  --comet-rpc http://127.0.0.1:26657 \
  --indexer http://127.0.0.1:3002
```

What runs, in order:

1. **On operator (build):**
   `docker build --platform linux/amd64 -t svpchain-mcp:<git-sha> -f cmd/mcp-server/Dockerfile .`
2. **On operator (save):** `docker save` to `build/mcp-server.image.tar`
   (skipped on rerun when the image id hasn't changed).
3. **On operator → remote (ship):** `rsync` the tar and a rendered
   `mcp.toml` to `/opt/svpchain-mcp/`.
4. **On remote (load):** `docker load` (skipped when the remote already
   has that image id).
5. **On remote (run):**
   `docker run -d --name svpchain-mcp --restart unless-stopped --network host -v /opt/svpchain-mcp/mcp.toml:/etc/svpchain-mcp/mcp.toml:ro svpchain-mcp:<tag>`
6. **On operator (verify):** POST a JSON-RPC `initialize` to
   `http://<remote-host>:<port>/` and assert HTTP 200/202.

`--platform linux/amd64` is pinned because the operator is usually Apple
Silicon (arm64) and remotes are Linux amd64. Override with `--platform`
only for ARM hosts.

## Useful knobs

- `--print-config` — render `mcp.toml` to stdout and exit.
- `--dry-run` — print every command that would run, change nothing.
- `--skip-build` — reuse the existing local `svpchain-mcp:<tag>` image
  (CI / iterative deploys).
- `--listen-port 8765` — change the host-bound port.
- `--install-dir /opt/svpchain-mcp` — change where on the remote the
  tar + config land.
- `--image-tag <tag>` — override the default `<git-short-sha>` tag.
- `--deposit-max-usdc / --withdraw-max-usdc / --transfer-max-usdc /
  --daily-withdraw-cap-usdc` — emitted into the `[limits]` block. Omit
  to use server defaults.

## Redeploy after a rebuild

Just rerun the same command. `save_if_changed` and `load_if_missing`
short-circuit when the image id hasn't changed, so a redeploy that's
config-only does not transfer the ~30 MB image again. When the binary
itself changes, the script tears down the old container, rsyncs the
new tar, loads, and starts a fresh container — clients must re-auth.

## Uninstall

**On operator:**

```bash
./scripts/mcp-server-deploy.sh --uninstall --host www@svpdev1.example.com
```

This removes the container, every local `svpchain-mcp:*` image on the
remote, and `/opt/svpchain-mcp/`.

## Operational notes

- The server reads `mcp.toml` once at startup. Editing the config in
  place on the remote has no effect until you redeploy.
- Logs land in the container's stderr; tail with
  `ssh <host> 'sudo docker logs svpchain-mcp --tail=200 -f'`.
- The script does **not** configure TLS or a reverse proxy. Front it
  with whatever (Caddy / nginx / Traefik) if you need TLS or hostname
  routing.
- The script does **not** open a firewall port. If the host has one,
  allow inbound TCP on `--listen-port`.
- The signer MCP server (`cmd/mcp-signer`) is unrelated to this script —
  it runs on the agent's machine, not the remote.
