#!/usr/bin/env bash
#
# scripts/mcp-server-deploy.sh — install the svpchain remote MCP server
# (cmd/mcp-server) onto a remote SSH host as a docker container.
#
# Flow:
#   1. On operator: build svpchain-mcp:<tag> via docker (linux/amd64).
#   2. On operator: docker save the image to a tar (cached by image id).
#   3. On operator → remote: rsync the tar + a rendered mcp.toml +
#      docker-compose.yml to ~/svpchain-mcp/ (the ssh user's home — no
#      sudo needed to write).
#   4. On remote: docker load the tar (skipped when image id matches).
#   5. On remote: docker compose up -d (from the shipped compose file).
#   6. On remote (via ssh): POST a JSON-RPC `initialize` to the listener
#      on 127.0.0.1 to smoke-test that it's up. Loopback so --host can be
#      a bare ~/.ssh/config alias.
#
# The remote host needs only docker + the compose v2 plugin (reachable by
# the ssh user without sudo, e.g. via the docker group) + sshd. No Go
# toolchain, no repo checkout, no GOPRIVATE credentials, no sudo. The
# container is stateless —
# v0.3 keeps the nonce / dynamic-tenants / session-bearers / withdraw
# ledger in memory, so a redeploy cleanly wipes auth state and rejoins
# the chain via the configured gRPC/RPC endpoints.
#
# Required flags (or env equivalents):
#   --host user@hostname           SSH target.            SVPCHAIN_DEPLOY_HOST
#
# Optional flags:
#   --chain-id <id>                Default svp-2517-1.    SVPCHAIN_CHAIN_ID
#   --grpc-addr <h:p>              Default 127.0.0.1:9090. SVPCHAIN_GRPC_ADDR
#   --comet-rpc <url>              Default http://127.0.0.1:26657. SVPCHAIN_COMET_RPC
#   --indexer <url>                Default http://127.0.0.1:3002.  SVPCHAIN_INDEXER
#   --listen-port <port>           Default 8765.          SVPCHAIN_LISTEN_PORT
#   --evm-rpc <url>                EVM JSON-RPC endpoint, enabling the EVM
#                                  tool family (broadcast_evm_tx, evm_tx_status).
#                                  Default http://127.0.0.1:8545 (the node's
#                                  local JSON-RPC port). Set "" to disable EVM
#                                  (omits evm_rpc_url). SVPCHAIN_EVM_RPC
#   --faucet-url <url>             Faucet backend base URL, enabling the faucet
#                                  tools (faucet_claim, list_faucet_tokens).
#                                  Default https://pre-faucet.svpchain.org. Set
#                                  "" to disable (omits faucet_base_url; the
#                                  faucet tools then refuse). SVPCHAIN_FAUCET_URL
#   --evm-uniswap-router <0xaddr>  UniswapV2 router address, enabling the swap
#                                  tools (quote_swap, build_token_approval,
#                                  build_swap). Requires --evm-rpc and
#                                  --evm-wsvp. Defaults to the known deployment
#                                  (0xFe7bf2DF…01e4); set "" to disable swaps.
#                                  SVPCHAIN_EVM_UNISWAP_ROUTER
#   --evm-wsvp <0xaddr>            Wrapped-native (WSVP) token address; must be
#                                  set together with --evm-uniswap-router.
#                                  Defaults to the known deployment
#                                  (0x771a0a63…6531). SVPCHAIN_EVM_WSVP
#   --install-dir <path>           Default ~/svpchain-mcp on remote (a
#                                  leading ~ expands to the ssh user's $HOME).
#   --image-tag <tag>              Default <git-short-sha>.
#   --platform <p>                 Default linux/amd64. Override for ARM.
#   --container-name <name>        Default svpchain-mcp.
#   --deposit-max-usdc <n>         Funds caps, copied into [limits].
#   --withdraw-max-usdc <n>
#   --transfer-max-usdc <n>
#   --daily-withdraw-cap-usdc <n>
#   --markets-refresh <dur>        Default 30s (e.g. "60s", "2m").
#   --skip-build                   Reuse an existing local image.
#   --print-config                 Render mcp.toml to stdout and exit.
#   --dry-run                      Print every command; touch nothing.
#   --uninstall                    Remove the container + image + dir
#                                  on the remote.
#   -h|--help                      This help.
#
# Examples:
#   # First deploy
#   ./scripts/mcp-server-deploy.sh \
#     --host www@svpdev1.example.com \
#     --chain-id localsvp-1 \
#     --grpc-addr 127.0.0.1:9090 \
#     --comet-rpc http://127.0.0.1:26657 \
#     --indexer http://127.0.0.1:3002
#
#   # See what would happen
#   ./scripts/mcp-server-deploy.sh --dry-run --host ... <rest>
#
#   # Tear down
#   ./scripts/mcp-server-deploy.sh --uninstall --host www@svpdev1.example.com
#
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
source "${SCRIPT_DIR}/lib/common.sh"

fail() { printf "  ${C_RED}✗${C_RESET} %s\n" "$*" >&2; exit 1; }

# ---- args ------------------------------------------------------------------

mode="install"        # install | uninstall | print-config

host=""
chain_id="${SVPCHAIN_CHAIN_ID:-svp-2517-1}"
grpc_addr="${SVPCHAIN_GRPC_ADDR:-127.0.0.1:9090}"
comet_rpc="${SVPCHAIN_COMET_RPC:-http://127.0.0.1:26657}"
indexer="${SVPCHAIN_INDEXER:-http://127.0.0.1:3002}"
listen_port="${SVPCHAIN_LISTEN_PORT:-8765}"
evm_rpc="${SVPCHAIN_EVM_RPC:-http://127.0.0.1:8545}"
faucet_url="${SVPCHAIN_FAUCET_URL:-https://pre-faucet.svpchain.org}"
evm_uniswap_router="${SVPCHAIN_EVM_UNISWAP_ROUTER:-0xFe7bf2DFd5CB268C6779f1F614638a436Cb701e4}"
evm_wsvp="${SVPCHAIN_EVM_WSVP:-0x771a0a63D8198b7dbea4a16910ff68AB38006531}"
install_dir="~/svpchain-mcp"
image_tag=""
platform="linux/amd64"
container_name="svpchain-mcp"
deposit_max=""
withdraw_max=""
transfer_max=""
daily_withdraw_cap=""
markets_refresh="30s"
skip_build="0"
dry_run="0"

while [[ $# -gt 0 ]]; do
  case "$1" in
    --host)                   host="$2";              shift 2 ;;
    --chain-id)               chain_id="$2";          shift 2 ;;
    --grpc-addr)              grpc_addr="$2";         shift 2 ;;
    --comet-rpc)              comet_rpc="$2";         shift 2 ;;
    --indexer)                indexer="$2";           shift 2 ;;
    --listen-port)            listen_port="$2";       shift 2 ;;
    --evm-rpc)                evm_rpc="$2";           shift 2 ;;
    --faucet-url)             faucet_url="$2";        shift 2 ;;
    --evm-uniswap-router)     evm_uniswap_router="$2"; shift 2 ;;
    --evm-wsvp)               evm_wsvp="$2";          shift 2 ;;
    --install-dir)            install_dir="$2";       shift 2 ;;
    --image-tag)              image_tag="$2";         shift 2 ;;
    --platform)               platform="$2";          shift 2 ;;
    --container-name)         container_name="$2";    shift 2 ;;
    --deposit-max-usdc)       deposit_max="$2";       shift 2 ;;
    --withdraw-max-usdc)      withdraw_max="$2";      shift 2 ;;
    --transfer-max-usdc)      transfer_max="$2";      shift 2 ;;
    --daily-withdraw-cap-usdc) daily_withdraw_cap="$2"; shift 2 ;;
    --markets-refresh)        markets_refresh="$2";   shift 2 ;;
    --skip-build)             skip_build="1";         shift ;;
    --print-config)           mode="print-config";    shift ;;
    --dry-run)                dry_run="1";            shift ;;
    --uninstall)              mode="uninstall";       shift ;;
    -h|--help)
      sed -n '2,/^set -euo/p' "${BASH_SOURCE[0]}" | sed -n '/^#/p' | sed 's/^# \{0,1\}//'
      exit 0 ;;
    *) fail "unknown flag: $1" ;;
  esac
done

: "${host:=${SVPCHAIN_DEPLOY_HOST:-}}"

# ---- shared helpers -------------------------------------------------------

# render_mcp_toml — emit the operator-side mcp.toml on stdout. listen_addr
# is always 0.0.0.0:<port> inside the container; --network host on the
# remote means that's also the host-bound port. Limits block emitted only
# if the operator supplied at least one cap (omitted → server defaults
# apply, see cmd/mcp-server/config.go::LimitsConfig). evm_rpc_url is emitted
# when --evm-rpc is non-empty (default on); faucet_base_url when --faucet-url
# is set.
render_mcp_toml() {
  cat <<EOF
# Auto-generated by scripts/mcp-server-deploy.sh — do not edit by hand.
# Mirror of the e2e config in scripts/mcp-e2e-test.sh.

chain_id         = "${chain_id}"
grpc_addr        = "${grpc_addr}"
comet_rpc_url    = "${comet_rpc}"
indexer_base_url = "${indexer}"
listen_addr      = "0.0.0.0:${listen_port}"
broadcast_mode   = "server"
EOF
  # evm_rpc_url, faucet_base_url, and the swap addresses are top-level keys, so
  # they must precede any [table] header. evm_uniswap_router_addr / evm_wsvp_addr
  # are both-or-neither and require evm_rpc_url (config.go::validateSwap).
  [[ -n "$evm_rpc" ]]            && echo "evm_rpc_url             = \"${evm_rpc}\""
  [[ -n "$faucet_url" ]]         && echo "faucet_base_url         = \"${faucet_url}\""
  [[ -n "$evm_uniswap_router" ]] && echo "evm_uniswap_router_addr = \"${evm_uniswap_router}\""
  [[ -n "$evm_wsvp" ]]           && echo "evm_wsvp_addr           = \"${evm_wsvp}\""
  cat <<EOF

[cache]
markets_refresh = "${markets_refresh}"
EOF
  if [[ -n "${deposit_max}${withdraw_max}${transfer_max}${daily_withdraw_cap}" ]]; then
    echo ""
    echo "[limits]"
    [[ -n "$deposit_max"        ]] && echo "deposit_max_usdc        = ${deposit_max}"
    [[ -n "$withdraw_max"       ]] && echo "withdraw_max_usdc       = ${withdraw_max}"
    [[ -n "$transfer_max"       ]] && echo "transfer_max_usdc       = ${transfer_max}"
    [[ -n "$daily_withdraw_cap" ]] && echo "daily_withdraw_cap_usdc = ${daily_withdraw_cap}"
  fi
}

# render_compose_yaml — emit the docker-compose.yml deployed alongside
# mcp.toml in install_dir. Pins the exact image just built/loaded; the
# mcp.toml volume uses an absolute path (install_dir is resolved before this
# runs) so `docker compose up -d` works regardless of project directory.
render_compose_yaml() {
  cat <<EOF
# Auto-generated by scripts/mcp-server-deploy.sh — do not edit by hand.
services:
  svpchain-mcp:
    image: ${image_ref}
    container_name: ${container_name}
    restart: unless-stopped
    # network_mode: host — listener binds to 0.0.0.0:${listen_port} (compose
    # \`ports:\` is ignored in host mode; the interface/port live in mcp.toml).
    network_mode: host
    volumes:
      - ${install_dir}/mcp.toml:/etc/svpchain-mcp/mcp.toml:ro
EOF
}

# require_install_args — fail with a clear message if a required flag
# is empty. Only --host has no default; the chain endpoints all default
# to the node's local ports (see the var block above).
require_install_args() {
  [[ -n "$host" ]] || fail "--host is required (or set SVPCHAIN_DEPLOY_HOST)"
}

# resolve_remote_install_dir — expand a leading ~ in $install_dir to the
# remote $HOME (one ssh round-trip). docker bind-mounts require an absolute
# host path, and it keeps mkdir / rsync / rm / the -v mount all pointing at
# one canonical location. No-op for an absolute --install-dir, and in
# --dry-run (the value is only printed there, so leave the ~ visible).
resolve_remote_install_dir() {
  case "$install_dir" in
    "~"|"~/"*)
      [[ "$dry_run" == "1" ]] && return 0
      local home
      home="$(ssh -o BatchMode=yes "$host" 'printf %s "$HOME"')" \
        || fail "could not resolve remote \$HOME on $host"
      [[ -n "$home" ]] || fail "remote \$HOME is empty on $host"
      install_dir="${home}${install_dir#\~}"
      ;;
  esac
}

# run_or_print CMD…  — used by --dry-run to print but not execute. All
# side-effecting calls (ssh, rsync, docker on the operator side) go
# through this wrapper so dry-run prints a complete trace.
run_or_print() {
  if [[ "$dry_run" == "1" ]]; then
    printf "  [dry-run] %s\n" "$*"
  else
    eval "$@"
  fi
}

# remote_exec  — run a shell command on the SSH host. Pass through
# run_or_print so --dry-run shows the actual ssh invocation.
remote_exec() {
  run_or_print "ssh -o BatchMode=yes '$host' $(printf '%q ' "$@")"
}

# remote_image_id IMG  — get the image id (sha256:…) of IMG on the remote,
# or empty string if absent. Mirrors deploy-remote-testnet.sh's helper of
# the same name; simplified for a single host.
remote_image_id() {
  local img="$1"
  if [[ "$dry_run" == "1" ]]; then
    echo ""  # dry-run: pretend remote is empty so the script prints all phases
    return
  fi
  ssh -o BatchMode=yes "$host" "docker image inspect --format '{{.Id}}' $img 2>/dev/null || true"
}

# local_image_id IMG  — get the image id of IMG locally, or empty string.
local_image_id() {
  docker image inspect --format '{{.Id}}' "$1" 2>/dev/null || true
}

# save_if_changed IMG TAR  — docker save IMG to TAR, but skip the save
# (and bypass rsync's mtime rewrite) when TAR.id already matches the
# current image id. Lifted from deploy-remote-testnet.sh.
save_if_changed() {
  local img="$1" tar="$2" id
  if [[ "$dry_run" == "1" ]]; then
    # In dry-run the image likely doesn't exist (build was only printed).
    # Show the save command without consulting docker.
    info "[dry-run] would docker save $img → $(basename "$tar") (if image id changed)"
    run_or_print "docker save -o '$tar' '$img'"
    return 0
  fi
  id="$(local_image_id "$img")"
  [[ -n "$id" ]] || fail "image $img not found locally; build failed?"
  if [[ -f "$tar" && -f "${tar}.id" && "$(cat "${tar}.id")" == "$id" ]]; then
    info "$img unchanged — skipping save"
    return 0
  fi
  info "$img → $(basename "$tar")"
  run_or_print "docker save -o '$tar' '$img'"
  echo "$id" > "${tar}.id"
}

# load_if_missing IMG REMOTE_TAR EXPECTED_ID  — docker load on the remote
# only when the remote doesn't already have IMG at EXPECTED_ID. Caller is
# responsible for the prior rsync.
load_if_missing() {
  local img="$1" remote_tar="$2" expected_id="$3"
  local remote_id; remote_id="$(remote_image_id "$img")"
  if [[ "$remote_id" == "$expected_id" && -n "$expected_id" ]]; then
    info "$img already loaded on remote — skipping load"
    return 0
  fi
  remote_exec "docker load < $remote_tar"
}

# ---- mode: print-config ---------------------------------------------------

if [[ "$mode" == "print-config" ]]; then
  render_mcp_toml
  exit 0
fi

# ---- mode: uninstall ------------------------------------------------------

if [[ "$mode" == "uninstall" ]]; then
  [[ -n "$host" ]] || fail "--host is required (or set SVPCHAIN_DEPLOY_HOST)"
  step "svpchain-mcp uninstall on $host"
  # install_dir defaults to ~/svpchain-mcp; expand a leading ~ so rm hits
  # the right absolute path (the remote shell would expand it anyway, but
  # be explicit and consistent with the install path).
  resolve_remote_install_dir
  # Prefer `compose down` (removes the container + any project network); fall
  # back to a direct rm for containers left by older bare docker-run deploys.
  remote_exec "docker compose -f $install_dir/docker-compose.yml down 2>/dev/null || true"
  remote_exec "docker rm -f $container_name 2>/dev/null || true"
  # Image tag may not be known at uninstall time — best-effort wipe of
  # any svpchain-mcp:* tags rather than requiring --image-tag.
  remote_exec "sh -c 'docker images --format \"{{.Repository}}:{{.Tag}}\" svpchain-mcp 2>/dev/null | xargs -r docker rmi 2>/dev/null || true'"
  remote_exec "rm -rf $install_dir"
  step "Done"
  exit 0
fi

# ---- mode: install --------------------------------------------------------

require_install_args
require_cmd docker
require_cmd rsync
require_cmd ssh
require_cmd jq

PROTOCOL_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
cd "$PROTOCOL_DIR"

# Tag defaulting — git short SHA when on a real repo, "dev" otherwise.
if [[ -z "$image_tag" ]]; then
  if image_tag="$(git rev-parse --short HEAD 2>/dev/null)"; then :
  else image_tag="dev"; fi
fi
image_ref="svpchain-mcp:${image_tag}"
image_tar="${PROTOCOL_DIR}/build/mcp-server.image.tar"
mkdir -p "${PROTOCOL_DIR}/build"

step "Preflight (operator + remote)"
info "host=$host image=$image_ref platform=$platform"
info "install_dir=$install_dir container=$container_name"
info "listen_port=$listen_port"
# Remote docker reachable? Must work without sudo — the whole deploy runs
# as the ssh user (docker group membership, not sudo).
if [[ "$dry_run" != "1" ]]; then
  ssh -o BatchMode=yes "$host" "docker version --format '{{.Server.Version}}'" \
    >/dev/null 2>&1 \
    || fail "remote docker not reachable at $host without sudo (ssh keys ok? docker installed? ssh user in the docker group?)"
  ssh -o BatchMode=yes "$host" "docker compose version" >/dev/null 2>&1 \
    || fail "remote 'docker compose' (v2 plugin) not available at $host"
  pass "remote docker + compose reachable"
else
  info "[dry-run] skipping ssh-to-docker reachability check"
fi

# Expand a leading ~ in install_dir to the remote $HOME now (needs the ssh
# connection just verified above) so phases 3–5 use one absolute path.
resolve_remote_install_dir
info "install_dir=$install_dir"

# Phase 1: build (On operator)
step "On operator: docker build --platform $platform"
if [[ "$skip_build" == "1" ]]; then
  info "--skip-build: reusing existing local image $image_ref"
  [[ -n "$(local_image_id "$image_ref")" ]] || fail "image $image_ref not found locally; drop --skip-build"
else
  build_cmd="docker build --platform $platform"
  build_cmd+=" --build-arg VERSION=$image_tag"
  build_cmd+=" --build-arg COMMIT=$(git rev-parse HEAD 2>/dev/null || echo unknown)"
  build_cmd+=" -t $image_ref"
  build_cmd+=" -t svpchain-mcp:latest"
  build_cmd+=" -f cmd/mcp-server/Dockerfile ."
  run_or_print "$build_cmd"
fi

# Phase 2: save (On operator)
step "On operator: docker save (cached by image id)"
save_if_changed "$image_ref" "$image_tar"
expected_id="$(cat "${image_tar}.id" 2>/dev/null || echo "")"

# Phase 3: ship config + compose + tar (On operator → remote)
step "On operator → remote: rsync config + image tar to $install_dir"
# Render mcp.toml + docker-compose.yml to tempfiles so we can rsync them like
# any other file.
toml_tmp="$(mktemp -t svpchain-mcp.toml.XXXXXX)"
compose_tmp="$(mktemp -t svpchain-mcp.compose.XXXXXX)"
trap 'rm -f "$toml_tmp" "$compose_tmp"' EXIT
render_mcp_toml > "$toml_tmp"
render_compose_yaml > "$compose_tmp"
# Create install_dir as the ssh user, so it — and everything we write into
# it — is owned by that user: mkdir, the rsyncs, and docker all run without
# sudo. install_dir need not be under $HOME, but it must be somewhere the
# ssh user can create (the default ~/svpchain-mcp is; if you override
# --install-dir to e.g. /opt, that user needs write access to the parent).
remote_exec "mkdir -p $install_dir"
run_or_print "rsync -avz '$toml_tmp' '$host:$install_dir/mcp.toml'"
run_or_print "rsync -avz '$compose_tmp' '$host:$install_dir/docker-compose.yml'"
run_or_print "rsync -avz '$image_tar' '$host:$install_dir/mcp-server.image.tar'"

# Phase 4: load (On remote)
step "On remote: docker load (skipped if image already loaded)"
load_if_missing "$image_ref" "$install_dir/mcp-server.image.tar" "$expected_id"
# Also expose the image as svpchain-mcp:latest on the remote. The build tags
# it locally, but `docker save $image_ref` only ships the sha tag, so add the
# latest alias here (idempotent — re-tagging the same id is a no-op).
remote_exec "docker tag $image_ref svpchain-mcp:latest"

# Phase 5: run (On remote)
step "On remote: docker compose up -d"
# Clear any pre-compose container of this name (older deploys used a bare
# `docker run`); compose would otherwise hit a name conflict. Harmless once
# compose owns the container — up -d just recreates it.
remote_exec "docker rm -f $container_name 2>/dev/null || true"
remote_exec "docker compose -f $install_dir/docker-compose.yml up -d"

# Phase 6: verify (On operator)
step "On remote: smoke test (POST initialize over loopback via ssh)"
if [[ "$dry_run" == "1" ]]; then
  info "[dry-run] would ssh $host curl -> http://127.0.0.1:$listen_port/"
else
  # Run curl on the remote against 127.0.0.1 so $host can be a bare
  # ~/.ssh/config alias — ssh resolves it; no operator-side DNS needed. This
  # confirms the listener is up but does NOT exercise the external firewall
  # path (that's now loopback-only). curl prints the body then a final line
  # with the HTTP code, which we split apart below.
  init_body='{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-03-26","capabilities":{},"clientInfo":{"name":"mcp-server-deploy","version":"v0.3.0"}}}'
  remote_curl="curl -sS -w '\n%{http_code}' --max-time 10 \
                    -H 'Content-Type: application/json' \
                    -H 'Accept: application/json, text/event-stream' \
                    -H 'MCP-Protocol-Version: 2025-03-26' \
                    -d '$init_body' \
                    'http://127.0.0.1:${listen_port}/'"
  resp=$(ssh -o BatchMode=yes "$host" "$remote_curl" 2>/dev/null || printf '\n000')
  http_code="${resp##*$'\n'}"
  body="${resp%$'\n'*}"
  if [[ "$http_code" == "200" || "$http_code" == "202" ]]; then
    pass "MCP initialize returned HTTP $http_code"
  else
    info "smoke test failed: HTTP $http_code (body below). Check container logs"
    info "  with: ssh $host 'docker logs $container_name --tail=80'."
    info "  Common causes: container failed to start; gRPC/RPC endpoints in"
    info "  mcp.toml not reachable from the container."
    printf '%s\n' "$body" >&2 || true
    fail "smoke test did not handshake"
  fi
fi

step "Done — svpchain-mcp $image_tag running on $host"
