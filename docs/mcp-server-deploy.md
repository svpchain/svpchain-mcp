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

## Daily "transfer out" cap (`set_/get_transfer_out_cap`)

Caps how much of each token a tenant may move OUT of its wallet per UTC
day. Unlike the USDC withdraw caps above, it is keyed by end-user token
symbol and the total sums a symbol's outflow across **both** rails it can
leave through — `build_bank_send` (x/bank) and `broadcast_evm_tx` (ERC-20
`transfer`/`transferFrom`, and native-SVP value sends). So a `usdc` bank
send and a `usdc` ERC-20 transfer draw down one shared daily total. Swaps
are intentionally **not** counted.

There is **no operator config**: caps are set entirely by the
authenticated user at runtime via two MCP tools. Every symbol starts
**unlimited** until the user opts into a cap.

- `get_transfer_out_cap` — returns each symbol's effective cap, the amount
  moved out so far this UTC day, and the remaining headroom.
- `set_transfer_out_cap(symbol, amount)` — sets that user's own cap for a
  symbol in whole-token human units (`"500"`, `"1.5"`); `"0"` means
  unlimited. Known symbols are `svp`, `usdc`, `usdv`.

Because the cap is **fully agent-controlled with no operator ceiling**, it
bounds an *honest* agent's blast radius (a runaway loop, an over-eager
transfer) but is **not** a hard guard against a compromised or
prompt-injected agent, which could call `set_transfer_out_cap` to lift its
own limit before draining. If you need a boundary an agent cannot cross,
do not enable the transfer tools (`build_bank_send`, `broadcast_evm_tx`)
for that tenant. Caps and usage are keyed by owner wallet and reset at UTC
midnight.

The cap state's lifetime depends on `transfer_out_cap_path`. When unset, it
lives only in memory and is lost on restart (so a restart re-opens every
wallet's full daily allowance). When set, both the caps and today's usage tally
are persisted to that JSON file and reloaded on boot:

```toml
# When set, caps + today's usage survive a restart. A relative path resolves
# next to this config file (like evm_bridge_routes_path); an absolute path is
# used as-is. Created on first write; need not exist at startup.
transfer_out_cap_path = "transfer-out-caps.json"
```

The server rewrites the file (atomically) after every cap change and every
successful transfer, and reloads it on boot. A corrupt or hand-edited file
fails startup loudly rather than silently dropping every cap.

**Persistence is on by default in this deploy script.** `render_mcp_toml` emits
an **absolute** `transfer_out_cap_path = "/var/lib/svpchain-mcp/transfer-out-caps.json"`,
and `render_compose_yaml` bind-mounts the host dir `<install_dir>/data` read-write
at `/var/lib/svpchain-mcp` so the file survives container recreates and redeploys.
An absolute path (not config-dir-relative) is required here: the config dir
`/etc/svpchain-mcp` only holds the read-only `mcp.toml` mount, so a relative path
would write into the container's ephemeral layer and be lost on the next deploy.
The cap JSON is inspectable / backup-able on the host at `<install_dir>/data/transfer-out-caps.json`.

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
