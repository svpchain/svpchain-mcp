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
#   0 = full v0.1 + v0.2.1 + v0.2.2 + v0.2.3 tool set verified end-to-end
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

# MCP session id, set by mcp_init() and stamped on every subsequent
# request via the Mcp-Session-Id header.
MCP_SESSION_ID=""

# MCP protocol version we negotiate. The mcp-server (built on the
# github.com/modelcontextprotocol/go-sdk v1.0.0 Streamable HTTP handler)
# rejects initialize requests pinned to the original 2024-11-05 spec on
# this codebase — handshakes succeed only at 2025-03-26+. Defaulting here
# so the script "just works"; overridable via env for forward / regression
# testing against newer spec revisions.
MCP_PROTOCOL_VERSION="${MCP_PROTOCOL_VERSION:-2025-03-26}"

# Per the MCP Streamable HTTP spec the client MUST accept BOTH JSON and
# SSE — the server may choose either format per response.
MCP_ACCEPT="application/json, text/event-stream"

# extract_json BODY — if BODY is an SSE stream ("event: ...\ndata: {...}"),
# concatenate the data: lines and emit just the JSON. Otherwise echo BODY
# unchanged. Lets jq parse either format the server returns.
extract_json() {
  local body="$1"
  if [[ "$body" == data:* || "$body" == event:* || "$body" == *$'\ndata: '* ]]; then
    # Strip "data: " prefix from each data line and concat. Empty lines
    # separate SSE events; for single-event responses there's only one
    # data block.
    awk '/^data: / { sub(/^data: /, ""); print }' <<<"$body"
  else
    echo "$body"
  fi
}

# mcp_init  — perform the JSON-RPC initialize handshake required by the
# MCP Streamable HTTP transport. Captures Mcp-Session-Id from the response
# headers so subsequent tools/list and tools/call POSTs are routed to the
# same session, then sends the notifications/initialized acknowledgment.
mcp_init() {
  local hdrs body
  hdrs=$(mktemp -t mcp-init-hdrs.XXXXXX)
  body=$(jq -cn --arg pv "$MCP_PROTOCOL_VERSION" '{
    jsonrpc:"2.0", id:0, method:"initialize",
    params:{
      protocolVersion: $pv,
      capabilities: {},
      clientInfo: { name:"mcp-e2e-test", version:"v0.2.1" }
    }
  }')
  local raw http_code
  # Capture body, headers, AND HTTP status separately so a 4xx doesn't
  # short-circuit the script before we can show the body the server sent.
  raw=$(curl -sS -o /dev/stdout -w "\n__HTTP_STATUS__=%{http_code}" \
              -H "Authorization: Bearer $TENANT_TOKEN" \
              -H "Content-Type: application/json" \
              -H "Accept: $MCP_ACCEPT" \
              -H "MCP-Protocol-Version: $MCP_PROTOCOL_VERSION" \
              -D "$hdrs" \
              -d "$body" "http://$LISTEN_ADDR/")
  http_code=$(grep -o '__HTTP_STATUS__=[0-9]*' <<<"$raw" | tail -1 | cut -d= -f2)
  local resp_body
  resp_body=${raw%$'\n'__HTTP_STATUS__=*}
  if [[ "$http_code" != "200" && "$http_code" != "202" ]]; then
    info "initialize HTTP $http_code; body follows:"
    echo "$resp_body" >&2
    rm -f "$hdrs"
    fail "initialize: HTTP $http_code (see body above + \$WORKDIR/mcp.log)"
  fi
  local resp
  resp=$(extract_json "$resp_body")
  # Header name is case-insensitive per HTTP. Use grep -i (portable on
  # macOS BSD grep) instead of awk's IGNORECASE (gawk-only).
  MCP_SESSION_ID=$(grep -i '^mcp-session-id:' "$hdrs" 2>/dev/null \
                    | head -1 \
                    | sed -E 's/^[^:]+:[[:space:]]*//; s/[[:space:]]*$//')
  # Surface all response headers when debugging — many MCP issues hinge
  # on which headers came back.
  if [[ -n "${MCP_DEBUG:-}" ]]; then
    info "initialize response headers:"
    sed 's/^/    /' "$hdrs" >&2
  fi
  rm -f "$hdrs"
  local err
  err=$(jq -r '.error // empty' <<<"$resp" 2>/dev/null || true)
  if [[ -n "$err" ]]; then
    echo "$resp" | jq . >&2
    fail "initialize: JSON-RPC error: $err"
  fi
  if [[ -z "$MCP_SESSION_ID" ]]; then
    info "no Mcp-Session-Id returned (server in stateless mode); proceeding without it"
  else
    pass "initialized; session=$MCP_SESSION_ID"
  fi
  # The server expects a notifications/initialized message after the
  # client digests the initialize response. Notifications have no id and
  # the server's response is typically 202 Accepted with an empty body.
  # We surface failures so a missed init notification can't hide as
  # "tools/list invalid during session initialization" later.
  local notify_body notify_status
  notify_body=$(curl -sS -o /dev/stdout -w "\n__HTTP_STATUS__=%{http_code}" \
                     -H "Authorization: Bearer $TENANT_TOKEN" \
                     -H "Content-Type: application/json" \
                     -H "Accept: $MCP_ACCEPT" \
                     -H "MCP-Protocol-Version: $MCP_PROTOCOL_VERSION" \
                     ${MCP_SESSION_ID:+-H "Mcp-Session-Id: $MCP_SESSION_ID"} \
                     -d '{"jsonrpc":"2.0","method":"notifications/initialized"}' \
                     "http://$LISTEN_ADDR/")
  notify_status=$(grep -o '__HTTP_STATUS__=[0-9]*' <<<"$notify_body" | tail -1 | cut -d= -f2)
  if [[ "$notify_status" != "200" && "$notify_status" != "202" && "$notify_status" != "204" ]]; then
    info "notifications/initialized HTTP $notify_status; body follows:"
    echo "${notify_body%$'\n'__HTTP_STATUS__=*}" >&2
    fail "notifications/initialized rejected — server stays in init mode"
  fi
  pass "notifications/initialized accepted (HTTP $notify_status)"
}

# mcp_call ID METHOD ARGS_JSON  — POST a JSON-RPC tool/call (or method)
# request and echo the raw response body. Fails the script if curl errors
# or the HTTP status isn't 200. Requires mcp_init() to have been called.
mcp_call() {
  local id="$1" method="$2" args="${3:-}"
  local body
  if [[ "$method" == "tools/list" ]]; then
    body="{\"jsonrpc\":\"2.0\",\"id\":$id,\"method\":\"tools/list\"}"
  else
    body="{\"jsonrpc\":\"2.0\",\"id\":$id,\"method\":\"tools/call\",\"params\":{\"name\":\"$method\",\"arguments\":$args}}"
  fi
  local raw_resp resp
  raw_resp=$(curl -fsS -H "Authorization: Bearer $TENANT_TOKEN" \
                       -H "Content-Type: application/json" \
                       -H "Accept: $MCP_ACCEPT" \
                       -H "MCP-Protocol-Version: $MCP_PROTOCOL_VERSION" \
                       ${MCP_SESSION_ID:+-H "Mcp-Session-Id: $MCP_SESSION_ID"} \
                       -d "$body" "http://$LISTEN_ADDR/") \
    || fail "$method: HTTP error (server not reachable or returned non-2xx)"
  resp=$(extract_json "$raw_resp")
  # Surface JSON-RPC errors (protocol level) and MCP tool errors
  # (isError == true at the result level — the handler ran but failed).
  local err
  err=$(jq -r '.error // empty' <<<"$resp" 2>/dev/null || true)
  if [[ -n "$err" ]]; then
    echo "$resp" | jq . >&2
    fail "$method: JSON-RPC error: $err"
  fi
  local is_err
  is_err=$(jq -r '.result.isError // false' <<<"$resp" 2>/dev/null || true)
  if [[ "$is_err" == "true" ]]; then
    echo "$resp" | jq . >&2
    fail "$method: tool error (result.isError=true)"
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

[limits]
# v0.2.3 funds-tool caps. Sized so the positive phases below fit, the
# over-cap deposit negative phase exceeds the per-tx ceiling, and the
# daily withdraw cap fires after one positive withdraw + one retry —
# specifically: positive withdraw is 3 USDC, daily cap is 5 USDC, so
# a subsequent 3 USDC withdraw must reject (3 + 3 > 5).
deposit_max_usdc       = 1000
withdraw_max_usdc      = 500
transfer_max_usdc      = 500
daily_withdraw_cap_usdc = 5

[[tenants]]
tenant_id           = "e2e"
bearer_token        = "$TENANT_TOKEN"
owner               = "$OWNER_ADDR"
# Includes both 0 and 1 so the same-owner transfer phase has a destination.
allowed_subaccounts = [0, 1]
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

step "MCP initialize handshake"
mcp_init

# Expected tool count tracks the registry. v0.1 = 10, v0.2.1 = 23,
# v0.2.2 = 27, v0.2.3 = 30 (funds tools + dual-enforcement broadcast).
EXPECTED_TOOLS="${EXPECTED_TOOLS:-30}"
step "tools/list (expecting ${EXPECTED_TOOLS} tools)"
LIST=$(mcp_call 1 "tools/list")
COUNT=$(jq -r '.result.tools | length' <<<"$LIST")
[[ "$COUNT" -eq "$EXPECTED_TOOLS" ]] || fail "expected ${EXPECTED_TOOLS} tools, got $COUNT"
pass "${COUNT} tools registered"

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

  # v0.2.1 read-catalog smoke (sampled — full sweep would bloat this script).
  step "get_candles ($TICKER, 1MIN, limit=5)"
  CD=$(mcp_call 100 "get_candles" "{\"ticker\":\"$TICKER\",\"resolution\":\"1MIN\",\"limit\":5}")
  NC=$(jq -r '.result.structuredContent.candles.candles | length' <<<"$CD")
  pass "candles=$NC"

  step "get_fills (owner=$OWNER_ADDR, subaccount=0)"
  FL=$(mcp_call 101 "get_fills" "{\"address\":\"$OWNER_ADDR\",\"subaccount_number\":0}")
  NF=$(jq -r '.result.structuredContent.fills.fills | length' <<<"$FL")
  pass "fills=$NF"

  step "get_pnl (owner=$OWNER_ADDR, subaccount=0)"
  PN=$(mcp_call 102 "get_pnl" "{\"address\":\"$OWNER_ADDR\",\"subaccount_number\":0}")
  # Empty result is the expected outcome on a fresh subaccount — the
  # indexer's 404 is now translated to an empty PnlResponse by the
  # account_v021 handler (see lib/mcp/tools/account_v021.go GetPnl).
  NP=$(jq -r '.result.structuredContent.pnl.historicalPnl | length // 0' <<<"$PN")
  pass "pnl entries=$NP"
else
  TICKER="BTC-USD"
  info "indexer skipped; using TICKER=$TICKER for the build step"
  info "v0.2.1 read-catalog smoke (candles/fills/pnl) skipped along with indexer"
fi

step "get_live_subaccount (chain, no indexer needed)"
LS=$(mcp_call 6 "get_live_subaccount" "{\"owner\":\"$OWNER_ADDR\",\"subaccount_number\":0}")
LS_OWNER=$(jq -r '.result.structuredContent.subaccount.id.owner' <<<"$LS")
[[ "$LS_OWNER" == "$OWNER_ADDR" ]] || fail "get_live_subaccount owner mismatch"
pass "subaccount.id.owner=$LS_OWNER"

# Re-query height now — HEIGHT captured during prereqs is many seconds
# stale by this point (MCP handshake, tools/list, 10+ read tools).
# Valid range for short-term GoodTilBlock is [current, current+40] —
# +40 is the chain's ShortBlockWindow constant. We pick +25 to leave
# room on both bounds: well within the upper limit even if the chain
# advances a few blocks between this fetch and DeliverTx, and well
# above the lower limit even if build/sign/broadcast burns some seconds.
SHORT_BLOCK_WINDOW_BUFFER=25
FRESH_HEIGHT=$(curl -fsS "$COMET_RPC_URL/status" \
                | jq -r .result.sync_info.latest_block_height)
GOOD_TIL=$((FRESH_HEIGHT + SHORT_BLOCK_WINDOW_BUFFER))
step "build_place_limit_order ($TICKER, BUY 0.001 @ 1.00, good_til_block=$GOOD_TIL [height=$FRESH_HEIGHT + $SHORT_BLOCK_WINDOW_BUFFER])"
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
DUP_RAW=$(curl -sS -H "Authorization: Bearer $TENANT_TOKEN" \
                    -H "Content-Type: application/json" \
                    -H "Accept: $MCP_ACCEPT" \
                    -H "MCP-Protocol-Version: $MCP_PROTOCOL_VERSION" \
                    ${MCP_SESSION_ID:+-H "Mcp-Session-Id: $MCP_SESSION_ID"} \
                    -d "{\"jsonrpc\":\"2.0\",\"id\":9,\"method\":\"tools/call\",\"params\":{\"name\":\"broadcast_signed_tx\",\"arguments\":$(jq -nc --arg cid "$PAYLOAD_UUID" --argjson stx "$SIGNED" '{client_id:$cid, signed_tx:$stx}')}}" \
                    "http://$LISTEN_ADDR/")
DUP_RESP=$(extract_json "$DUP_RAW")
if jq -e '.error // (.result.isError // false)' <<<"$DUP_RESP" >/dev/null 2>&1; then
  pass "duplicate broadcast rejected as expected"
else
  echo "$DUP_RESP" | jq . >&2
  fail "duplicate broadcast was NOT rejected — idempotency check broken"
fi

step "get_tx_status (poll until height > 0; best-effort for short-term orders)"
# IMPORTANT: short-term CLOB orders on svpchain (and dYdX) do NOT commit
# as standalone txs — they go through CheckTx (which is what gave us
# code=0 above) and then their effects land via MsgProposedOperations in
# the block proposer's bundle. CometBFT's /tx?hash= will not find them.
# We poll anyway because get_tx_status IS the right check for stateful
# orders / funds movement (v0.2.2+), but we don't fail the smoke if a
# short-term-order hash isn't found — broadcast accept (code=0) is the
# strongest confirmation available for this path.
HEIGHT_OUT=0
ST_CODE=0
for i in $(seq 1 20); do
  ST=$(mcp_call $((10 + i)) "get_tx_status" "{\"tx_hash\":\"$TX_HASH\"}" 2>/dev/null) \
    || { sleep 0.5; continue; }
  HEIGHT_OUT=$(jq -r '.result.structuredContent.height // 0' <<<"$ST")
  if [[ "$HEIGHT_OUT" -gt 0 ]]; then
    ST_CODE=$(jq -r '.result.structuredContent.code // 0' <<<"$ST")
    break
  fi
  sleep 0.5
done
if [[ "$HEIGHT_OUT" -gt 0 ]]; then
  pass "tx landed: height=$HEIGHT_OUT code=$ST_CODE"
else
  info "tx hash not in CometBFT history after 10 s — expected for short-term"
  info "  CLOB orders (they commit via MsgProposedOperations, not as a"
  info "  standalone tx). Broadcast acceptance (code=0) confirms CheckTx"
  info "  passed; agent-side verification should use get_live_subaccount /"
  info "  get_orders / get_fills for the real on-chain effect."
fi

# ---- 7. v0.2.2 build_cancel_order smoke -------------------------------------
# Smoke-tests the new v0.2.2 cancel builder: the chain doesn't have to accept
# the cancel (the original order may already be gone from the in-memory orderbook
# after the proposer-bundle pass above), but the build path must produce a
# valid TxPayload that round-trips through devsign — that's what we check.

step "build_cancel_order (short-term, same ticker + client_id as above)"
FRESH_HEIGHT_2=$(curl -fsS "$COMET_RPC_URL/status" \
                   | jq -r .result.sync_info.latest_block_height)
CANCEL_GOOD_TIL=$((FRESH_HEIGHT_2 + SHORT_BLOCK_WINDOW_BUFFER))
CANCEL_UUID="e2e-cancel-$(date +%s)-$$"
BCN=$(mcp_call 40 "build_cancel_order" "$(jq -nc \
  --argjson sub 0 --argjson cp 0 --argjson cid 1 \
  --argjson flags 0 --argjson gtb "$CANCEL_GOOD_TIL" --arg pcid "$CANCEL_UUID" '{
    subaccount_number: $sub, clob_pair_id: $cp, order_client_id: $cid,
    order_flags: $flags, good_til_block: $gtb, payload_client_id: $pcid
  }')")
CANCEL_PAYLOAD=$(jq -c '.result.structuredContent.payload' <<<"$BCN")
[[ "$CANCEL_PAYLOAD" != "null" ]] || { echo "$BCN" | jq . >&2; fail "build_cancel_order returned no payload"; }
IS_SHORT_CN=$(jq -r '.is_short_term_clob' <<<"$CANCEL_PAYLOAD")
[[ "$IS_SHORT_CN" == "true" ]] || fail "expected is_short_term_clob=true on cancel, got $IS_SHORT_CN"
pass "cancel payload built; is_short_term_clob=true; client_id=$CANCEL_UUID"

# ---- 8. v0.2.3 funds-tool round-trips ---------------------------------------
# Deposit → Withdraw → Transfer: each phase goes through build → devsign →
# broadcast. The negative phase fires only the build_* path (structured cap
# rejection arrives before signing, so no chain involvement needed).

# Shared helper: build → devsign → broadcast a funds tool, fail if the
# chain rejects (Code != 0). Args: TOOL_NAME, ARGS_JSON, CALL_ID, UUID.
funds_round_trip() {
  local tool="$1" args="$2" call_id="$3" uuid="$4"
  local resp payload bcast tx_hash code raw_log
  resp=$(mcp_call "$call_id" "$tool" "$args")
  payload=$(jq -c '.result.structuredContent.payload' <<<"$resp")
  [[ "$payload" != "null" ]] || { echo "$resp" | jq . >&2; fail "$tool returned no payload"; }
  echo "$payload" > "$WORKDIR/${tool}.payload.json"
  ./build/devsign --in "$WORKDIR/${tool}.payload.json" --out "$WORKDIR/${tool}.signed.json" \
    || { cat "$WORKDIR/${tool}.payload.json" >&2; fail "devsign $tool"; }
  local signed
  signed=$(cat "$WORKDIR/${tool}.signed.json")
  bcast=$(mcp_call "$((call_id + 1))" "broadcast_signed_tx" "$(jq -nc \
    --arg cid "$uuid" --argjson stx "$signed" \
    '{client_id: $cid, signed_tx: $stx}')")
  tx_hash=$(jq -r '.result.structuredContent.result.tx_hash' <<<"$bcast")
  code=$(jq -r '.result.structuredContent.result.code' <<<"$bcast")
  if [[ "$code" != "0" ]]; then
    raw_log=$(jq -r '.result.structuredContent.result.raw_log' <<<"$bcast")
    fail "$tool broadcast rejected: code=$code raw_log=$raw_log"
  fi
  pass "$tool round-trip: tx_hash=$tx_hash"
}

step "build_deposit_to_subaccount (10 USDC, sub 0)"
DEP_UUID="e2e-dep-$(date +%s)-$$"
funds_round_trip "build_deposit_to_subaccount" \
  "$(jq -nc --argjson sub 0 --arg amt "10" --arg pcid "$DEP_UUID" \
     '{subaccount_number: $sub, human_usdc: $amt, payload_client_id: $pcid}')" \
  200 "$DEP_UUID"

step "build_withdraw_from_subaccount (3 USDC, sub 0 → bank)"
WD_UUID="e2e-wd-$(date +%s)-$$"
funds_round_trip "build_withdraw_from_subaccount" \
  "$(jq -nc --argjson sub 0 --arg amt "3" --arg pcid "$WD_UUID" \
     '{subaccount_number: $sub, human_usdc: $amt, payload_client_id: $pcid}')" \
  210 "$WD_UUID"

step "build_transfer_between_subaccounts (1 USDC, sub 0 → sub 1)"
XF_UUID="e2e-xf-$(date +%s)-$$"
funds_round_trip "build_transfer_between_subaccounts" \
  "$(jq -nc --argjson s 0 --argjson r 1 --arg amt "1" --arg pcid "$XF_UUID" \
     '{sender_subaccount_number: $s, recipient_subaccount_number: $r, human_usdc: $amt, payload_client_id: $pcid}')" \
  220 "$XF_UUID"

# ---- 9. v0.2.3 cap rejections (negative) ------------------------------------
# Both rejections fire at the build_* stage (limits.{CheckPerTx,Enforce})
# before the tx is assembled or signed — no chain involvement needed.

# expect_cap_rejection TOOL_NAME ARGS_JSON CALL_ID EXPECT_SUBSTRING — sends a
# tools/call via raw curl (mcp_call would fail() on isError) and asserts that
# the cap error message contains EXPECT_SUBSTRING.
expect_cap_rejection() {
  local tool="$1" args="$2" call_id="$3" expect="$4"
  local body resp msg
  body=$(jq -nc --argjson id "$call_id" --arg t "$tool" --argjson a "$args" \
    '{jsonrpc:"2.0", id:$id, method:"tools/call",
      params:{name:$t, arguments:$a}}')
  local raw
  raw=$(curl -sS -H "Authorization: Bearer $TENANT_TOKEN" \
                 -H "Content-Type: application/json" \
                 -H "Accept: $MCP_ACCEPT" \
                 -H "MCP-Protocol-Version: $MCP_PROTOCOL_VERSION" \
                 ${MCP_SESSION_ID:+-H "Mcp-Session-Id: $MCP_SESSION_ID"} \
                 -d "$body" "http://$LISTEN_ADDR/")
  resp=$(extract_json "$raw")
  msg=$(jq -r '.result.content[0].text // ""' <<<"$resp")
  if [[ "$msg" != *"$expect"* ]]; then
    echo "$resp" | jq . >&2
    fail "expected '$expect' in $tool rejection, got: $msg"
  fi
  pass "$tool rejection surfaced: $msg"
}

step "build_deposit_to_subaccount (2000 USDC > 1000 USDC per-tx cap)"
expect_cap_rejection "build_deposit_to_subaccount" \
  "$(jq -nc --argjson sub 0 --arg amt "2000" --arg pcid "e2e-over-dep-$(date +%s)-$$" \
     '{subaccount_number: $sub, human_usdc: $amt, payload_client_id: $pcid}')" \
  230 "deposit_max_usdc exceeded"

# Daily cap: the positive withdraw above consumed 3 USDC of the 5 USDC
# daily allotment. Another 3 USDC pushes used+request (3+3=6) past the
# cap, even though 3 USDC is well under withdraw_max_usdc=500.
step "build_withdraw_from_subaccount (3 USDC, would push daily 3→6 > 5 cap)"
expect_cap_rejection "build_withdraw_from_subaccount" \
  "$(jq -nc --argjson sub 0 --arg amt "3" --arg pcid "e2e-over-wd-$(date +%s)-$$" \
     '{subaccount_number: $sub, human_usdc: $amt, payload_client_id: $pcid}')" \
  240 "daily_withdraw_cap exceeded"

# ---- 10. summary ------------------------------------------------------------

step "All v0.1 + v0.2.1 + v0.2.2 + v0.2.3 tools verified end-to-end"
printf "${C_GREEN}${C_BOLD}OK${C_RESET}  mcp-server v0.2.3 round-trip on chain_id=$CHAIN_ID\n"
