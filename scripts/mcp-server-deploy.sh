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
#   --evm-oracle <0xaddr>          OffChainAggregator price-feed address,
#                                  enabling get_oracle_price. Requires --evm-rpc
#                                  (standalone; independent of the swap addrs).
#                                  Defaults to the known deployment
#                                  (0xAE351F2d…0B56); set "" to disable.
#                                  SVPCHAIN_EVM_ORACLE
#   --evm-lendora-comptroller <0xaddr>
#                                  Lendora (Compound V2 fork) Comptroller
#                                  address, enabling the lendora_* money-market
#                                  tools. Requires --evm-rpc; markets + price
#                                  oracle are discovered on-chain from it.
#                                  Defaults to the svp_testnet deployment
#                                  (0x0FAdfaA9…E02D); set "" to disable.
#                                  SVPCHAIN_EVM_LENDORA_COMPTROLLER
#   --evm-bridge-addr <0xaddr>     SVPBridge contract address, enabling
#                                  build_bridge_deposit. Requires --evm-rpc.
#                                  Bridge is ON by default; addr defaults to the
#                                  known deployment (0x78Aca10a…9F4a).
#                                  SVPCHAIN_EVM_BRIDGE
#   --evm-bridge-routes <path>     Route-registry path WRITTEN INTO mcp.toml.
#                                  Default "routes.json" (relative → resolved by
#                                  the server against the mcp.toml directory, so
#                                  it points at routes.json beside the config in
#                                  every layout). Set "" to disable the bridge.
#                                  SVPCHAIN_EVM_BRIDGE_ROUTES
#   --evm-bridge-routes-src <path>  Optional LOCAL route-registry file that
#                                  OVERRIDES the generated one (install path
#                                  only). When unset, the script generates the
#                                  registry itself (render_routes_json) and ships
#                                  it to install_dir/routes.json. Set to a file
#                                  to ship your own instead; deploy fails fast if
#                                  that file is missing. SVPCHAIN_EVM_BRIDGE_ROUTES_SRC
#   --evm-bridge-source-chain-id <n>  This deployment's EVM chain id, used to
#                                  scope route lookups to outbound-from-svpchain
#                                  pairs. Default 2517.
#                                  SVPCHAIN_EVM_BRIDGE_SOURCE_CHAIN_ID
#   --evm-foreign-chains <list>    Inbound source chains ([[evm_foreign_chain]]),
#                                  enabling build_bridge_deposit_inbound. ";"-
#                                  separated "chainId,rpcUrl,bridgeAddr" triples.
#                                  Defaults wire arbitrum_sepolia + sepolia (the
#                                  sources the shipped routes advertise). Requires
#                                  the bridge to be on. Set "" to disable inbound.
#                                  SVPCHAIN_EVM_FOREIGN_CHAINS
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
#   --print-routes                 Render the bridge route registry (routes.json)
#                                  to stdout and exit. Pair with --print-config
#                                  for a local run: save both side by side.
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

mode="install"        # install | uninstall | print-config | print-routes

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
evm_oracle="${SVPCHAIN_EVM_ORACLE:-0xAE351F2dF66DF1A7d2eB0D7574BcDb909E680B56}"
# Lendora (Compound V2 fork) money markets: the singleton Comptroller (Unitroller
# proxy) address enables the lendora_* tool family (config.go::validateLendora,
# requires evm_rpc_url). Markets + the price oracle are discovered on-chain from
# it, so only this address is needed. Defaults to the svp_testnet deployment
# (networks/svptestnet.json). Set --evm-lendora-comptroller "" to disable.
evm_lendora_comptroller="${SVPCHAIN_EVM_LENDORA_COMPTROLLER:-0x0FAdfaA907859DC4Cd5582dFd1CA4C761385E02D}"
# Bridge is enabled by default: addr + source-chain-id default to the known
# svp_chain deployment, and the route-registry path defaults to "routes.json"
# (a path relative to mcp.toml — the server resolves it against the config dir,
# see config.go::LoadConfig). The route registry itself is GENERATED by this
# script (render_routes_json) and shipped to install_dir/routes.json on deploy —
# no operator-supplied file needed. evm_bridge_routes_src optionally overrides
# that generated file with a local one. Set --evm-bridge-routes "" to disable
# the bridge entirely. See render_mcp_toml / render_routes_json.
evm_bridge_addr="${SVPCHAIN_EVM_BRIDGE:-0x78Aca10afd5b28E838ECf0De20c5621CE39D9F4a}"
evm_bridge_routes="${SVPCHAIN_EVM_BRIDGE_ROUTES:-routes.json}"
evm_bridge_routes_src="${SVPCHAIN_EVM_BRIDGE_ROUTES_SRC:-}"
evm_bridge_source_chain_id="${SVPCHAIN_EVM_BRIDGE_SOURCE_CHAIN_ID:-2517}"
# Inbound bridging ([[evm_foreign_chain]]): each foreign chain that can bridge
# INTO svp_chain needs its own SVPBridge address + JSON-RPC endpoint (the deposit
# is built / broadcast / tracked there, not on evm_rpc_url). Defaults wire the
# two testnet sources the shipped route registry already advertises inbound
# routes for (arbitrum_sepolia, sepolia), so build_bridge_deposit_inbound works
# out of the box. Format: ";"-separated "chainId,rpcUrl,bridgeAddr" triples. Only
# emitted when the bridge is enabled (config.go::validateForeignChains requires
# it). Set --evm-foreign-chains "" to disable inbound bridging. See
# emit_foreign_chains / render_mcp_toml.
evm_foreign_chains="${SVPCHAIN_EVM_FOREIGN_CHAINS:-421614,https://sepolia-rollup.arbitrum.io/rpc,0xB6c74A758E3fA7bf57c22037821f7cA974d0CdfD;11155111,https://ethereum-sepolia-rpc.publicnode.com,0xb9a9937006E886F0Ec145a19634426300dD20a64}"
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
# Populated by the install flow once install_dir / CWD are known: the basename
# mounted into the container, and (only when --evm-bridge-routes-src is given) an
# absolute local override file. Both empty when the bridge is off or
# evm_bridge_routes is an absolute (operator-managed) path; when basename is set
# but src_abs is empty, the registry is generated via render_routes_json.
bridge_routes_basename=""
bridge_routes_src_abs=""

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
    --evm-oracle)             evm_oracle="$2";        shift 2 ;;
    --evm-lendora-comptroller) evm_lendora_comptroller="$2"; shift 2 ;;
    --evm-bridge-addr)        evm_bridge_addr="$2";   shift 2 ;;
    --evm-bridge-routes)      evm_bridge_routes="$2"; shift 2 ;;
    --evm-bridge-routes-src)  evm_bridge_routes_src="$2"; shift 2 ;;
    --evm-bridge-source-chain-id) evm_bridge_source_chain_id="$2"; shift 2 ;;
    --evm-foreign-chains)     evm_foreign_chains="$2"; shift 2 ;;
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
    --print-routes)           mode="print-routes";    shift ;;
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

# emit_foreign_chains — emit the [[evm_foreign_chain]] array-of-tables parsed
# from evm_foreign_chains (";"-separated "chainId,rpcUrl,bridgeAddr" triples).
# No-op when the list is empty (inbound disabled). A malformed triple (missing
# field) fails the deploy loudly rather than shipping a half-configured chain.
emit_foreign_chains() {
  [[ -z "$evm_foreign_chains" ]] && return 0
  local triple cid rpc addr
  local saved_ifs="$IFS"
  IFS=';'
  for triple in $evm_foreign_chains; do
    IFS="$saved_ifs"
    [[ -z "$triple" ]] && continue
    IFS=',' read -r cid rpc addr <<<"$triple"
    if [[ -z "$cid" || -z "$rpc" || -z "$addr" ]]; then
      fail "--evm-foreign-chains: malformed triple \"$triple\" (want chainId,rpcUrl,bridgeAddr)"
    fi
    printf '\n[[evm_foreign_chain]]\n'
    printf 'chain_id    = %s\n' "$cid"
    printf 'rpc_url     = "%s"\n' "$rpc"
    printf 'bridge_addr = "%s"\n' "$addr"
    IFS=';'
  done
  IFS="$saved_ifs"
}

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
  # evm_rpc_url, faucet_base_url, the swap addresses, and evm_oracle_addr are
  # top-level keys, so they must precede any [table] header. evm_uniswap_router_addr
  # / evm_wsvp_addr are both-or-neither (config.go::validateSwap); evm_oracle_addr
  # is standalone. All three require evm_rpc_url (config.go::validateOracle).
  [[ -n "$evm_rpc" ]]            && echo "evm_rpc_url             = \"${evm_rpc}\""
  [[ -n "$faucet_url" ]]         && echo "faucet_base_url         = \"${faucet_url}\""
  # Persist per-symbol daily transfer-out caps + usage across restarts. Absolute
  # (not config-dir-relative like the bridge routes) on purpose: the config dir
  # /etc/svpchain-mcp holds only the read-only mcp.toml bind-mount, so the cap
  # file must live on the separate writable data volume render_compose_yaml
  # mounts at /var/lib/svpchain-mcp — otherwise it lands in the container's
  # ephemeral layer and is lost on the next redeploy.
  echo "transfer_out_cap_path   = \"/var/lib/svpchain-mcp/transfer-out-caps.json\""
  [[ -n "$evm_uniswap_router" ]] && echo "evm_uniswap_router_addr = \"${evm_uniswap_router}\""
  [[ -n "$evm_wsvp" ]]           && echo "evm_wsvp_addr           = \"${evm_wsvp}\""
  [[ -n "$evm_oracle" ]]         && echo "evm_oracle_addr         = \"${evm_oracle}\""
  # Lendora Comptroller is standalone (config.go::validateLendora) and requires
  # evm_rpc_url. Non-empty by default, so the lendora_* tools are enabled out of
  # the box; --evm-lendora-comptroller "" disables them and the rest of the EVM
  # family is unaffected.
  [[ -n "$evm_lendora_comptroller" ]] && echo "evm_lendora_comptroller_addr = \"${evm_lendora_comptroller}\""
  # Bridge keys are all-or-nothing (config.go::validateBridge) and require
  # evm_rpc_url. All three default non-empty, so build_bridge_deposit is enabled
  # out of the box; --evm-bridge-routes "" (or clearing addr/source) disables it
  # and the rest of the EVM family is unaffected. evm_bridge_routes_path is left
  # relative ("routes.json") on purpose — the server resolves it against the
  # config dir, so it points at routes.json beside mcp.toml in every layout.
  # evm_bridge_source_chain_id is an integer (unquoted).
  if [[ -n "$evm_bridge_addr" && -n "$evm_bridge_routes" && -n "$evm_bridge_source_chain_id" ]]; then
    echo "evm_bridge_addr             = \"${evm_bridge_addr}\""
    echo "evm_bridge_routes_path      = \"${evm_bridge_routes}\""
    echo "evm_bridge_source_chain_id  = ${evm_bridge_source_chain_id}"
    # Inbound [[evm_foreign_chain]] array-of-tables, emitted after the top-level
    # bridge keys (TOML: bare keys must precede any table header) and only when
    # the bridge is on (validateForeignChains requires it).
    emit_foreign_chains
  elif [[ -n "$evm_bridge_routes" ]]; then
    echo "# WARNING: --evm-bridge-routes set but evm_bridge_addr / evm_bridge_source_chain_id are empty;" >&2
    echo "#          bridge omitted (config.go::validateBridge requires all three)." >&2
  fi
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

# render_routes_json — emit the SVPBridge route registry (the file mcp.toml's
# evm_bridge_routes_path points at). This is the canonical (sourceToken,
# targetChainId → targetToken) whitelist published by the bridge backend for the
# svp_chain ↔ sepolia / arbitrum_sepolia testnet deployment, baked in here the
# same way the default contract addresses are. srcToken/targetToken of the zero
# address denote the native coin on that chain; decimals is the source asset's
# decimals. build_bridge_deposit only uses the svp_chain-origin rows, but the
# full bidirectional set is emitted so the file is a faithful copy of the
# registry. Pin a new deployment by overriding with --evm-bridge-routes-src.
# Heredoc delimiter is quoted ('ROUTES') so nothing is shell-expanded.
render_routes_json() {
  cat <<'ROUTES'
[
  {"srcChain":"arbitrum_sepolia","srcChainId":421614,"targetChain":"svp_chain","targetChainId":2517,"srcToken":"0x0000000000000000000000000000000000000000","targetToken":"0x1c12dbda863900c680a3836c53d408feaf63f0ba","symbol":"WETH","decimals":18},
  {"srcChain":"arbitrum_sepolia","srcChainId":421614,"targetChain":"svp_chain","targetChainId":2517,"srcToken":"0x7a8EcFa70374c1B8702CB98aaf23dE19675981d6","targetToken":"0x0000000000000000000000000000000000000000","symbol":"SVP","decimals":18},
  {"srcChain":"arbitrum_sepolia","srcChainId":421614,"targetChain":"svp_chain","targetChainId":2517,"srcToken":"0xc2bda8290a2e01984da81acf7e2d6ec9b14d7b10","targetToken":"0x8787384b8640f6e9c30e94585d3d62b03f80a5df","symbol":"WBNB","decimals":18},
  {"srcChain":"arbitrum_sepolia","srcChainId":421614,"targetChain":"svp_chain","targetChainId":2517,"srcToken":"0xd10d01ebf3cb825da77a025b1d861e7ae5370c20","targetToken":"0x6c22ceb0852bd7781b57574aaa5de0f22cd44162","symbol":"WBTC","decimals":8},
  {"srcChain":"arbitrum_sepolia","srcChainId":421614,"targetChain":"svp_chain","targetChainId":2517,"srcToken":"0xf93b6aae0ffa91c5f8795cda651b376fceb692e3","targetToken":"0x732f6ea7afd5edc02e7ba052075dd0780e285489","symbol":"USDC","decimals":6},
  {"srcChain":"arbitrum_sepolia","srcChainId":421614,"targetChain":"svp_chain","targetChainId":2517,"srcToken":"0xfa9857651febd22c0a76c958adb25b4af0370688","targetToken":"0x013a61e622e6abfcab64f52d274c3fc0aa37f951","symbol":"USDV","decimals":6},
  {"srcChain":"sepolia","srcChainId":11155111,"targetChain":"svp_chain","targetChainId":2517,"srcToken":"0x0000000000000000000000000000000000000000","targetToken":"0x1c12dbda863900c680a3836c53d408feaf63f0ba","symbol":"WETH","decimals":18},
  {"srcChain":"sepolia","srcChainId":11155111,"targetChain":"svp_chain","targetChainId":2517,"srcToken":"0x16B065D7519D5C1c53eff6ed5AE732E90d602A00","targetToken":"0x0000000000000000000000000000000000000000","symbol":"SVP","decimals":18},
  {"srcChain":"sepolia","srcChainId":11155111,"targetChain":"svp_chain","targetChainId":2517,"srcToken":"0x7af80a20da5a4000175eb8babcab73da6ed01f9d","targetToken":"0x732f6ea7afd5edc02e7ba052075dd0780e285489","symbol":"USDC","decimals":6},
  {"srcChain":"sepolia","srcChainId":11155111,"targetChain":"svp_chain","targetChainId":2517,"srcToken":"0x93e719f5458d112804122952033103f2eb349eac","targetToken":"0x013a61e622e6abfcab64f52d274c3fc0aa37f951","symbol":"USDV","decimals":6},
  {"srcChain":"sepolia","srcChainId":11155111,"targetChain":"svp_chain","targetChainId":2517,"srcToken":"0x9d45d6a420fbaf77a46a4822ef967d62a69dc7f8","targetToken":"0x6c22ceb0852bd7781b57574aaa5de0f22cd44162","symbol":"WBTC","decimals":8},
  {"srcChain":"sepolia","srcChainId":11155111,"targetChain":"svp_chain","targetChainId":2517,"srcToken":"0xf174007a92ae5cdfecfa85c94c5105e4851734d6","targetToken":"0x8787384b8640f6e9c30e94585d3d62b03f80a5df","symbol":"WBNB","decimals":18},
  {"srcChain":"svp_chain","srcChainId":2517,"targetChain":"arbitrum_sepolia","targetChainId":421614,"srcToken":"0x0000000000000000000000000000000000000000","targetToken":"0x7a8EcFa70374c1B8702CB98aaf23dE19675981d6","symbol":"SVP","decimals":18},
  {"srcChain":"svp_chain","srcChainId":2517,"targetChain":"arbitrum_sepolia","targetChainId":421614,"srcToken":"0x013a61e622e6abfcab64f52d274c3fc0aa37f951","targetToken":"0xfa9857651febd22c0a76c958adb25b4af0370688","symbol":"USDV","decimals":6},
  {"srcChain":"svp_chain","srcChainId":2517,"targetChain":"arbitrum_sepolia","targetChainId":421614,"srcToken":"0x1c12dbda863900c680a3836c53d408feaf63f0ba","targetToken":"0x0000000000000000000000000000000000000000","symbol":"WETH","decimals":18},
  {"srcChain":"svp_chain","srcChainId":2517,"targetChain":"arbitrum_sepolia","targetChainId":421614,"srcToken":"0x6c22ceb0852bd7781b57574aaa5de0f22cd44162","targetToken":"0xd10d01ebf3cb825da77a025b1d861e7ae5370c20","symbol":"WBTC","decimals":8},
  {"srcChain":"svp_chain","srcChainId":2517,"targetChain":"arbitrum_sepolia","targetChainId":421614,"srcToken":"0x732f6ea7afd5edc02e7ba052075dd0780e285489","targetToken":"0xf93b6aae0ffa91c5f8795cda651b376fceb692e3","symbol":"USDC","decimals":6},
  {"srcChain":"svp_chain","srcChainId":2517,"targetChain":"arbitrum_sepolia","targetChainId":421614,"srcToken":"0x8787384b8640f6e9c30e94585d3d62b03f80a5df","targetToken":"0xc2bda8290a2e01984da81acf7e2d6ec9b14d7b10","symbol":"WBNB","decimals":18},
  {"srcChain":"svp_chain","srcChainId":2517,"targetChain":"sepolia","targetChainId":11155111,"srcToken":"0x0000000000000000000000000000000000000000","targetToken":"0x16B065D7519D5C1c53eff6ed5AE732E90d602A00","symbol":"SVP","decimals":18},
  {"srcChain":"svp_chain","srcChainId":2517,"targetChain":"sepolia","targetChainId":11155111,"srcToken":"0x013a61e622e6abfcab64f52d274c3fc0aa37f951","targetToken":"0x93e719f5458d112804122952033103f2eb349eac","symbol":"USDV","decimals":6},
  {"srcChain":"svp_chain","srcChainId":2517,"targetChain":"sepolia","targetChainId":11155111,"srcToken":"0x1c12dbda863900c680a3836c53d408feaf63f0ba","targetToken":"0x0000000000000000000000000000000000000000","symbol":"WETH","decimals":18},
  {"srcChain":"svp_chain","srcChainId":2517,"targetChain":"sepolia","targetChainId":11155111,"srcToken":"0x6c22ceb0852bd7781b57574aaa5de0f22cd44162","targetToken":"0x9d45d6a420fbaf77a46a4822ef967d62a69dc7f8","symbol":"WBTC","decimals":8},
  {"srcChain":"svp_chain","srcChainId":2517,"targetChain":"sepolia","targetChainId":11155111,"srcToken":"0x732f6ea7afd5edc02e7ba052075dd0780e285489","targetToken":"0x7af80a20da5a4000175eb8babcab73da6ed01f9d","symbol":"USDC","decimals":6},
  {"srcChain":"svp_chain","srcChainId":2517,"targetChain":"sepolia","targetChainId":11155111,"srcToken":"0x8787384b8640f6e9c30e94585d3d62b03f80a5df","targetToken":"0xf174007a92ae5cdfecfa85c94c5105e4851734d6","symbol":"WBNB","decimals":18}
]
ROUTES
}

# render_compose_yaml — emit the docker-compose.yml deployed alongside
# mcp.toml in install_dir. Pins the exact image just built/loaded; the
# mcp.toml volume uses an absolute path (install_dir is resolved before this
# runs) so `docker compose up -d` works regardless of project directory. When
# the bridge is enabled with a relative routes path, the route registry is
# mounted next to mcp.toml at the same container dir (/etc/svpchain-mcp) so the
# server's config-dir-relative resolution finds it (see bridge_routes_basename).
# The ${install_dir}/data host dir is mounted read-write at /var/lib/svpchain-mcp
# so transfer_out_cap_path (set absolute by render_mcp_toml) persists on the host
# across container recreates / redeploys, not in the ephemeral container layer.
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
      - ${install_dir}/data:/var/lib/svpchain-mcp
EOF
  [[ -n "$bridge_routes_basename" ]] && \
    echo "      - ${install_dir}/${bridge_routes_basename}:/etc/svpchain-mcp/${bridge_routes_basename}:ro"
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

# ---- mode: print-routes ---------------------------------------------------

if [[ "$mode" == "print-routes" ]]; then
  render_routes_json
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

# Bridge route shipping. The bridge is on whenever its three keys are non-empty
# (the default). With a RELATIVE routes path we ship a route registry to
# install_dir and mount it next to mcp.toml so the server's config-dir-relative
# resolution finds it; an ABSOLUTE evm_bridge_routes is operator-managed and not
# auto-shipped. The registry is GENERATED (render_routes_json) unless
# --evm-bridge-routes-src points at a local override file, which we resolve
# against the operator's CWD *before* the cd into the protocol dir below and
# require to exist (fail fast rather than ship a broken registry).
if [[ -n "$evm_bridge_addr" && -n "$evm_bridge_routes" && -n "$evm_bridge_source_chain_id" ]]; then
  case "$evm_bridge_routes" in
    /*)
      info "bridge: evm_bridge_routes is absolute ($evm_bridge_routes) — not auto-shipping; ensure that path exists on $host."
      ;;
    *)
      bridge_routes_basename="$(basename "$evm_bridge_routes")"
      if [[ -n "$evm_bridge_routes_src" ]]; then
        if [[ "$evm_bridge_routes_src" = /* ]]; then
          bridge_routes_src_abs="$evm_bridge_routes_src"
        else
          bridge_routes_src_abs="$(pwd)/$evm_bridge_routes_src"
        fi
        [[ -f "$bridge_routes_src_abs" ]] || fail "--evm-bridge-routes-src '$bridge_routes_src_abs' was not found"
      fi
      ;;
  esac
fi

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
routes_tmp=""   # set below only when the registry is generated (not overridden)
trap 'rm -f "$toml_tmp" "$compose_tmp" "$routes_tmp"' EXIT
render_mcp_toml > "$toml_tmp"
render_compose_yaml > "$compose_tmp"
# Resolve the bridge route registry to ship (when the bridge is on with a
# relative path, i.e. bridge_routes_basename is set): an operator override file
# if given, else the registry generated by render_routes_json.
bridge_routes_ship=""
if [[ -n "$bridge_routes_basename" ]]; then
  if [[ -n "$bridge_routes_src_abs" ]]; then
    bridge_routes_ship="$bridge_routes_src_abs"
  else
    routes_tmp="$(mktemp -t svpchain-mcp.routes.XXXXXX)"
    render_routes_json > "$routes_tmp"
    bridge_routes_ship="$routes_tmp"
  fi
fi
# Create install_dir as the ssh user, so it — and everything we write into
# it — is owned by that user: mkdir, the rsyncs, and docker all run without
# sudo. install_dir need not be under $HOME, but it must be somewhere the
# ssh user can create (the default ~/svpchain-mcp is; if you override
# --install-dir to e.g. /opt, that user needs write access to the parent).
# Pre-create the data dir (mounted rw at /var/lib/svpchain-mcp) as the ssh user
# so it's owned by them; left to Docker's bind-mount auto-create it would be
# root-owned and the non-root-safe write path could break on stricter images.
remote_exec "mkdir -p $install_dir $install_dir/data"
run_or_print "rsync -avz '$toml_tmp' '$host:$install_dir/mcp.toml'"
# Ship the bridge route registry next to mcp.toml (generated or overridden).
[[ -n "$bridge_routes_ship" ]] && \
  run_or_print "rsync -avz '$bridge_routes_ship' '$host:$install_dir/$bridge_routes_basename'"
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
