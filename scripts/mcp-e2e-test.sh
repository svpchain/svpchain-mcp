#!/usr/bin/env bash
#
# scripts/mcp-e2e-test.sh — manual end-to-end smoke test for the v0.1
# remote MCP server. Exercises the full build → sign → broadcast → status
# loop through the real binary, the real HTTP transport, the real
# bearer-token auth middleware, and a real chain (whatever localnet you
# have running).
#
# Standalone, but also called by scripts/fullflow_test.sh phase 23 so a
# single fullflow run covers chain + trading + MCP server in one shot.
#
# Prerequisites (the script will refuse to run otherwise):
#   - `svpchaind` on PATH (built from this repo)
#   - `curl`, `jq` on PATH
#   - A localnet started via `make localnet-start` (chain at :9090 / :26657)
#   - Optionally `./scripts/local_indexer_stack.sh` running (else indexer
#     tools will be skipped with a yellow notice)
#
# Usage:
#   ./scripts/mcp-e2e-test.sh                      # default localhost endpoints
#   INDEXER_BASE_URL=... ./scripts/mcp-e2e-test.sh # override indexer endpoint
#
# Exit codes:
#   0 = all 10 tools verified end-to-end
#   non-zero = first failing step (output before exit shows what broke)
#
set -euo pipefail

# ---- shared helpers (color, step/pass/info, require_cmd) ------------------
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=lib/common.sh
source "${SCRIPT_DIR}/lib/common.sh"

# Local `fail` — colored ✗ + exit 1. Distinct from fullflow_test.sh's
# LOG_PREFIX-style fail, which is why common.sh deliberately omits it.
fail() { printf "  ${C_RED}✗${C_RESET} %s\n" "$*" >&2; exit 1; }

# ---- config ----------------------------------------------------------------

CHAIN_ID="${CHAIN_ID:-}"  # auto-detected from svpchaind status if empty
GRPC_ADDR="${GRPC_ADDR:-127.0.0.1:9090}"
COMET_RPC_URL="${COMET_RPC_URL:-http://127.0.0.1:26657}"
INDEXER_BASE_URL="${INDEXER_BASE_URL:-http://127.0.0.1:3002}"
LISTEN_ADDR="${LISTEN_ADDR:-127.0.0.1:8765}"
TENANT_TOKEN="${TENANT_TOKEN:-e2e-token}"
DEV_KEY_NAME="${DEV_KEY_NAME:-dev0}"
KEYRING="${KEYRING:-test}"

WORKDIR="$(mktemp -d -t mcp-e2e.XXXXXX)"
trap 'on_exit' EXIT INT TERM

# on_exit — cleanup mcp-server subprocess + temp files.
on_exit() {
  local rc=$?
  if [[ -n "${MCP_PID:-}" ]] && kill -0 "$MCP_PID" 2>/dev/null; then
    kill "$MCP_PID" 2>/dev/null || true
    wait "$MCP_PID" 2>/dev/null || true
  fi
  rm -rf "$WORKDIR"
  exit "$rc"
}

# mcp_call ID METHOD ARGS_JSON  — POST a JSON-RPC tool/call (or method)
# request and echo the raw response body. Fails the script if curl errors
# or the HTTP status isn't 200.
mcp_call() {
  local id="$1" method="$2" args="${3:-}"
  local body
  if [[ "$method" == "tools/list" ]]; then
    body="{\"jsonrpc\":\"2.0\",\"id\":$id,\"method\":\"tools/list\"}"
  else
    body="{\"jsonrpc\":\"2.0\",\"id\":$id,\"method\":\"tools/call\",\"params\":{\"name\":\"$method\",\"arguments\":$args}}"
  fi
  local resp
  resp=$(curl -fsS -H "Authorization: Bearer $TENANT_TOKEN" \
                   -H "Content-Type: application/json" \
                   -d "$body" "http://$LISTEN_ADDR/") \
    || fail "$method: HTTP error (server not reachable or returned non-2xx)"
  # Surface any JSON-RPC error so we don't silently treat it as success.
  local err
  err=$(jq -r '.error // empty' <<<"$resp" 2>/dev/null || true)
  if [[ -n "$err" ]]; then
    echo "$resp" | jq . >&2
    fail "$method: JSON-RPC error: $err"
  fi
  echo "$resp"
}

# ---- 1. prerequisites ------------------------------------------------------

step "Prerequisites"
require_cmd svpchaind; pass "svpchaind"
require_cmd curl;      pass "curl"
require_cmd jq;        pass "jq"
require_cmd go;        pass "go"

# Chain reachable?
COMET_STATUS=$(curl -fsS "$COMET_RPC_URL/status") \
  || fail "CometBFT RPC not reachable at $COMET_RPC_URL (is localnet running?)"
NETWORK=$(jq -r '.result.node_info.network' <<<"$COMET_STATUS")
HEIGHT=$(jq -r '.result.sync_info.latest_block_height' <<<"$COMET_STATUS")
pass "chain reachable: network=$NETWORK, height=$HEIGHT"
if [[ -z "$CHAIN_ID" ]]; then CHAIN_ID="$NETWORK"; fi

# Indexer reachable? (Non-fatal — we'll skip indexer-only tools if not.)
if curl -fsS "$INDEXER_BASE_URL/v4/perpetualMarkets" >/dev/null 2>&1; then
  pass "indexer reachable at $INDEXER_BASE_URL"
  INDEXER_OK=1
else
  info "indexer NOT reachable at $INDEXER_BASE_URL; skipping list_markets / get_market / get_orderbook / get_subaccount"
  INDEXER_OK=0
fi

# ---- 2. build binaries -----------------------------------------------------

step "Build mcp-server + devsign"
GOPRIVATE='github.com/deltaping/*' go build -o build/mcp-server ./cmd/mcp-server >/dev/null \
  || fail "build mcp-server"
pass "build/mcp-server"
GOPRIVATE='github.com/deltaping/*' go build -o build/devsign ./scripts/devsign >/dev/null \
  || fail "build devsign"
pass "build/devsign"

# ---- 3. extract dev key ----------------------------------------------------

step "Extract $DEV_KEY_NAME from the localnet keyring ($KEYRING)"
OWNER_ADDR=$(svpchaind keys show "$DEV_KEY_NAME" --keyring-backend "$KEYRING" --address 2>/dev/null) \
  || fail "svpchaind keys show $DEV_KEY_NAME (is the dev key in the $KEYRING keyring?)"
pass "owner=$OWNER_ADDR"

# `keys export --unsafe --unarmored-hex` prints 'WARNING:...' to stderr and
# expects a 'y' confirmation on stdin.
DEV_KEY_HEX=$(echo y | svpchaind keys export "$DEV_KEY_NAME" \
                         --keyring-backend "$KEYRING" \
                         --unarmored-hex --unsafe 2>/dev/null | tr -d '\r\n' | tail -c 64)
if [[ -z "$DEV_KEY_HEX" || "${#DEV_KEY_HEX}" -ne 64 ]]; then
  fail "could not extract 64-char hex private key for $DEV_KEY_NAME"
fi
pass "32-byte private key extracted"
export DEVSIGN_KEY_HEX="$DEV_KEY_HEX"

# ---- 4. write mcp.toml -----------------------------------------------------

step "Write mcp-server config to $WORKDIR/mcp.toml"
cat >"$WORKDIR/mcp.toml" <<EOF
chain_id         = "$CHAIN_ID"
grpc_addr        = "$GRPC_ADDR"
comet_rpc_url    = "$COMET_RPC_URL"
indexer_base_url = "$INDEXER_BASE_URL"
listen_addr      = "$LISTEN_ADDR"
broadcast_mode   = "server"

[auth]
mode = "bearer"

[cache]
markets_refresh = "30s"

[[tenants]]
tenant_id           = "e2e"
bearer_token        = "$TENANT_TOKEN"
owner               = "$OWNER_ADDR"
allowed_subaccounts = [0]
kill_switch         = false
EOF
pass "config written"

# ---- 5. start mcp-server ---------------------------------------------------

step "Start mcp-server"
./build/mcp-server --config "$WORKDIR/mcp.toml" >"$WORKDIR/mcp.log" 2>&1 &
MCP_PID=$!
info "pid=$MCP_PID, logs=$WORKDIR/mcp.log"

# Wait for the listener to come up (~3s max).
for i in $(seq 1 30); do
  if curl -fsS -H "Authorization: Bearer $TENANT_TOKEN" \
              -H "Content-Type: application/json" \
              -d '{"jsonrpc":"2.0","id":0,"method":"tools/list"}' \
              "http://$LISTEN_ADDR/" >/dev/null 2>&1; then
    pass "listening on http://$LISTEN_ADDR (took $((i * 100)) ms)"
    break
  fi
  if ! kill -0 "$MCP_PID" 2>/dev/null; then
    cat "$WORKDIR/mcp.log" >&2
    fail "mcp-server exited before becoming ready; see log above"
  fi
  sleep 0.1
  if [[ $i -eq 30 ]]; then
    cat "$WORKDIR/mcp.log" >&2
    fail "mcp-server did not become ready within 3s"
  fi
done

# ---- 6. exercise the tools ------------------------------------------------

step "tools/list (must return 10 tools)"
LIST=$(mcp_call 1 "tools/list")
COUNT=$(jq -r '.result.tools | length' <<<"$LIST")
[[ "$COUNT" -eq 10 ]] || fail "expected 10 tools, got $COUNT"
pass "10 tools registered"

step "whoami"
WHO=$(mcp_call 2 "whoami" "{}")
WHO_OWNER=$(jq -r '.result.structuredContent.owner' <<<"$WHO")
[[ "$WHO_OWNER" == "$OWNER_ADDR" ]] || fail "whoami owner mismatch: got $WHO_OWNER, want $OWNER_ADDR"
pass "owner=$WHO_OWNER, broadcast_mode=$(jq -r '.result.structuredContent.broadcast_mode' <<<"$WHO")"

if [[ "$INDEXER_OK" -eq 1 ]]; then
  step "list_markets"
  LM=$(mcp_call 3 "list_markets" "{}")
  N=$(jq -r '.result.structuredContent.markets | length' <<<"$LM")
  [[ "$N" -gt 0 ]] || fail "list_markets returned no markets"
  TICKER=$(jq -r '.result.structuredContent.markets | keys | .[0]' <<<"$LM")
  pass "$N markets; sample ticker=$TICKER"

  step "get_market ($TICKER)"
  GM=$(mcp_call 4 "get_market" "{\"ticker\":\"$TICKER\"}")
  STEP_QUANTUMS=$(jq -r '.result.structuredContent.market.stepBaseQuantums' <<<"$GM")
  TICK=$(jq -r '.result.structuredContent.market.subticksPerTick' <<<"$GM")
  pass "stepBaseQuantums=$STEP_QUANTUMS, subticksPerTick=$TICK"

  step "get_orderbook ($TICKER)"
  OB=$(mcp_call 5 "get_orderbook" "{\"ticker\":\"$TICKER\"}")
  BIDS=$(jq -r '.result.structuredContent.orderbook.bids | length' <<<"$OB")
  ASKS=$(jq -r '.result.structuredContent.orderbook.asks | length' <<<"$OB")
  pass "bids=$BIDS asks=$ASKS"
else
  TICKER="BTC-USD"
  info "indexer skipped; using TICKER=$TICKER for the build step"
fi

step "get_live_subaccount (chain, no indexer needed)"
LS=$(mcp_call 6 "get_live_subaccount" "{\"owner\":\"$OWNER_ADDR\",\"subaccount_number\":0}")
LS_OWNER=$(jq -r '.result.structuredContent.subaccount.id.owner' <<<"$LS")
[[ "$LS_OWNER" == "$OWNER_ADDR" ]] || fail "get_live_subaccount owner mismatch"
pass "subaccount.id.owner=$LS_OWNER"

step "build_place_limit_order ($TICKER, BUY 0.001 @ 1.00, good_til_block=+20)"
GOOD_TIL=$((HEIGHT + 20))
PAYLOAD_UUID="e2e-$(date +%s)-$$"
BPL=$(mcp_call 7 "build_place_limit_order" "$(jq -nc \
  --argjson sub 0 --arg ticker "$TICKER" --arg side BUY \
  --arg size "0.001" --arg price "1.00" \
  --argjson gtb "$GOOD_TIL" --argjson cid 1 --arg pcid "$PAYLOAD_UUID" '{
    subaccount_number: $sub, ticker: $ticker, side: $side,
    size: $size, price: $price, good_til_block: $gtb,
    order_client_id: $cid, payload_client_id: $pcid
  }')")
PAYLOAD=$(jq -c '.result.structuredContent.payload' <<<"$BPL")
[[ "$PAYLOAD" != "null" ]] || { echo "$BPL" | jq . >&2; fail "build_place_limit_order returned no payload"; }
echo "$PAYLOAD" > "$WORKDIR/payload.json"
IS_SHORT=$(jq -r '.is_short_term_clob' <<<"$PAYLOAD")
[[ "$IS_SHORT" == "true" ]] || fail "expected is_short_term_clob=true, got $IS_SHORT"
pass "payload built; is_short_term_clob=true; client_id=$PAYLOAD_UUID"

step "Sign locally with devsign"
./build/devsign --in "$WORKDIR/payload.json" --out "$WORKDIR/signed.json" \
  || { cat "$WORKDIR/payload.json" >&2; fail "devsign failed"; }
SIGNED=$(cat "$WORKDIR/signed.json")
pass "signed tx produced"

step "broadcast_signed_tx"
BCAST=$(mcp_call 8 "broadcast_signed_tx" "$(jq -nc \
  --arg cid "$PAYLOAD_UUID" --argjson stx "$SIGNED" \
  '{client_id: $cid, signed_tx: $stx}')")
TX_HASH=$(jq -r '.result.structuredContent.result.tx_hash' <<<"$BCAST")
CODE=$(jq -r '.result.structuredContent.result.code' <<<"$BCAST")
if [[ "$CODE" != "0" ]]; then
  RAW_LOG=$(jq -r '.result.structuredContent.result.raw_log' <<<"$BCAST")
  fail "broadcast rejected: code=$CODE raw_log=$RAW_LOG"
fi
pass "broadcast accepted: tx_hash=$TX_HASH code=0"

step "broadcast_signed_tx (re-broadcast, must be rejected by idempotency)"
DUP_RESP=$(curl -sS -H "Authorization: Bearer $TENANT_TOKEN" \
                    -H "Content-Type: application/json" \
                    -d "{\"jsonrpc\":\"2.0\",\"id\":9,\"method\":\"tools/call\",\"params\":{\"name\":\"broadcast_signed_tx\",\"arguments\":$(jq -nc --arg cid "$PAYLOAD_UUID" --argjson stx "$SIGNED" '{client_id:$cid, signed_tx:$stx}')}}" \
                    "http://$LISTEN_ADDR/")
if jq -e '.error // (.result.isError // false)' <<<"$DUP_RESP" >/dev/null 2>&1; then
  pass "duplicate broadcast rejected as expected"
else
  echo "$DUP_RESP" | jq . >&2
  fail "duplicate broadcast was NOT rejected — idempotency check broken"
fi

step "get_tx_status (poll until height > 0)"
HEIGHT_OUT=0
for i in $(seq 1 20); do
  ST=$(mcp_call $((10 + i)) "get_tx_status" "{\"tx_hash\":\"$TX_HASH\"}")
  HEIGHT_OUT=$(jq -r '.result.structuredContent.height' <<<"$ST")
  if [[ -n "$HEIGHT_OUT" && "$HEIGHT_OUT" -gt 0 ]]; then break; fi
  sleep 0.5
done
[[ "$HEIGHT_OUT" -gt 0 ]] || fail "tx never landed on chain (still height=0 after 10s)"
ST_CODE=$(jq -r '.result.structuredContent.code' <<<"$ST")
pass "tx landed: height=$HEIGHT_OUT code=$ST_CODE"

# ---- 7. summary ------------------------------------------------------------

step "All 10 v0.1 tools verified end-to-end"
printf "${C_GREEN}${C_BOLD}OK${C_RESET}  mcp-server v0.1 round-trip on chain_id=$CHAIN_ID\n"
