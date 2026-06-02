# Deploying the svpchain remote MCP server

> Mirror of [`mcp-server-deploy.zh-CN.md`](mcp-server-deploy.zh-CN.md) — keep both
> updated when changing this file.

`scripts/mcp-server-deploy.sh` installs the remote MCP server
(`cmd/mcp-server`) onto a single remote SSH host as a docker container,
managed by docker compose. The script builds the image locally, ships
it + a rendered `mcp.toml` + a generated `docker-compose.yml` to the
remote, and runs the container under `restart: unless-stopped` with
`network_mode: host`.

The container is stateless: nonce / dynamic-tenants / session-bearers /
withdraw-ledger all live in memory. A redeploy cleanly wipes auth
state — clients holding bearers must re-run
`auth_challenge → sign_challenge → auth_verify` after each restart.

## Prerequisites

**On operator:**

- `docker` (with buildx) — build, save, and load the image.
- `ssh` + `rsync` — ship the tar + config to the remote.
- `jq` — used by the script for small JSON helpers.
- Network access to fetch `github.com/deltaping/*` (set `GOPRIVATE` /
  configure your git credentials before building).

**On remote:**

- `docker` reachable by the SSH user **without sudo** — i.e. the user
  is in the `docker` group. The script runs nothing under `sudo`; every
  remote action (mkdir, rsync, docker compose, docker run, rm) executes
  as the connecting user.
- `docker compose` v2 plugin available (`docker compose version`
  succeeds). The script generates a compose file and drives the
  container through `docker compose up -d` rather than bare
  `docker run`.
- A writable directory at `--install-dir` (default `~/svpchain-mcp`).
  The default puts everything under the ssh user's `$HOME` so no
  privileged paths are needed; override with `--install-dir <path>`
  if you'd rather use somewhere else (the ssh user must be able to
  `mkdir -p` it).
- The chain's gRPC, CometBFT RPC, and indexer endpoints reachable from
  the remote — the container runs with `network_mode: host`, so
  anything the host can reach the container can too.

## Quickstart

**On operator:**

```bash
./scripts/mcp-server-deploy.sh --host www@svpdev1.example.com
```

That's the whole command for the common case. Chain endpoints default
to `127.0.0.1:9090` / `http://127.0.0.1:26657` / `http://127.0.0.1:3002`
and the chain id defaults to `svp-2517-1`; pass `--chain-id` /
`--grpc-addr` / `--comet-rpc` / `--indexer` to override.

`--host` can be a bare `~/.ssh/config` alias — DNS and key resolution
happen on the operator. The smoke step (phase 6) runs `curl` on the
remote against `127.0.0.1`, so the operator never needs to reach the
listener directly.

What runs, in order:

1. **On operator (build):**
   `docker build --platform linux/amd64 -t svpchain-mcp:<git-sha> -f cmd/mcp-server/Dockerfile .`
2. **On operator (save):** `docker save` to `build/mcp-server.image.tar`
   (skipped on rerun when the image id hasn't changed).
3. **On operator → remote (ship):** `rsync` the tar, the rendered
   `mcp.toml`, and a generated `docker-compose.yml` to
   `~/svpchain-mcp/` on the remote.
4. **On remote (load):** `docker load` (skipped when the remote already
   has that image id). The script also tags the loaded image as
   `svpchain-mcp:latest` on the remote.
5. **On remote (run):** `docker compose -f ~/svpchain-mcp/docker-compose.yml up -d`.
   The compose file pins the just-loaded image and mounts the shipped
   `mcp.toml` read-only at `/etc/svpchain-mcp/mcp.toml`.
6. **On remote (verify):** via `ssh`, POST a JSON-RPC `initialize` at
   `http://127.0.0.1:<port>/` and assert HTTP 200/202.

`--platform linux/amd64` is pinned because the operator is usually Apple
Silicon (arm64) and remotes are Linux amd64. Override with `--platform`
only for ARM hosts.

## Useful knobs

- `--print-config` — render `mcp.toml` to stdout and exit.
- `--dry-run` — print every command that would run, change nothing.
- `--skip-build` — reuse the existing local `svpchain-mcp:<tag>` image
  (CI / iterative deploys).
- `--listen-port 8765` — change the host-bound port.
- `--install-dir ~/svpchain-mcp` — change where on the remote the tar +
  config + compose file land. A leading `~` is expanded to the ssh
  user's `$HOME` via one ssh round-trip; absolute paths are used as-is.
- `--image-tag <tag>` — override the default `<git-short-sha>` tag.
- `--deposit-max-usdc / --withdraw-max-usdc / --transfer-max-usdc /
  --daily-withdraw-cap-usdc` — emitted into the `[limits]` block. Omit
  to use server defaults.

## Redeploy after a rebuild

Just rerun the same command. `save_if_changed` and `load_if_missing`
short-circuit when the image id hasn't changed, so a redeploy that's
config-only does not transfer the ~30 MB image again. When the binary
itself changes, the script removes the old container (via
`docker compose down` plus a defensive `docker rm -f`), rsyncs the new
tar, loads, re-tags, and runs `docker compose up -d` — clients must
re-auth.

## Uninstall

**On operator:**

```bash
./scripts/mcp-server-deploy.sh --uninstall --host www@svpdev1.example.com
```

This runs `docker compose down`, removes any pre-compose container of
the same name, deletes every `svpchain-mcp:*` image on the remote, and
removes the install directory.

## Operational notes

- The server reads `mcp.toml` once at startup. Editing the config in
  place on the remote has no effect until you redeploy (or you `docker
  compose -f ~/svpchain-mcp/docker-compose.yml restart` from the
  remote shell).
- Logs land in the container's stderr; tail with
  `ssh <host> 'docker logs svpchain-mcp --tail=200 -f'` (no `sudo` —
  the ssh user is already in the docker group).
- The smoke test in phase 6 only confirms the container responds on
  loopback. **External reachability is not verified** — if your client
  needs to hit the listener from another machine, the operator is
  responsible for any firewall / security-group / reverse-proxy
  configuration.
- The script does **not** configure TLS or a reverse proxy. Front it
  with whatever (Caddy / nginx / Traefik) if you need TLS or hostname
  routing.
- The signer MCP server (`cmd/mcp-signer`) is unrelated to this script —
  it runs on the agent's machine, not the remote.
