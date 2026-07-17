#!/usr/bin/env bash
# test_evm_faucet_e2e.sh — end-to-end smoke for the HTTP faucet MCP path.
#
# The faucet is a standalone HTTP service (faucet_base_url): its operator
# account signs and submits the on-chain claim, so the client never signs or
# broadcasts anything. This drives the full tool flow against a running remote
# MCP server (with faucet_base_url set):
#
#   auth_challenge → devsign --sign-challenge → auth_verify   (bearer)
#   list_faucet_tokens → faucet_claim
#
# It is the faucet analog of scripts/mcp-e2e-test.sh's CLOB flow; kept small
# and assumes the MCP server is already up and pointed at a reachable faucet.
#
# Required env:
#   MCP_URL         remote MCP HTTP endpoint   (e.g. http://127.0.0.1:8080/mcp)
#   OWNER_ADDR      svp1… owner the signer controls
#   DEVSIGN_KEY_HEX 32-byte hex key for OWNER_ADDR (used by devsign to sign the
#                                                   auth challenge)
#
# Optional env:
#   FAUCET_TOKEN    0x token address to claim. Default: the native token
#                   (0x0000…0000). Use list_faucet_tokens output to pick one.
#
# Deps: curl, jq on PATH.
#
# NOTE: the faucet enforces a per-address/token rate limit (1h on pre-faucet).
# A repeat run within the window surfaces a "rate limited" error from
# faucet_claim — that is the faucet working, not a test bug.
set -euo pipefail

: "${MCP_URL:?set MCP_URL}"
: "${OWNER_ADDR:?set OWNER_ADDR}"
: "${DEVSIGN_KEY_HEX:?set DEVSIGN_KEY_HEX}"

NATIVE_TOKEN="0x0000000000000000000000000000000000000000"
FAUCET_TOKEN="${FAUCET_TOKEN:-$NATIVE_TOKEN}"

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

step() { printf '\n=== %s ===\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
pass() { printf 'ok: %s\n' "$*"; }

SESSION_HEADER=""
TENANT_TOKEN=""

# mcp_call ID NAME ARGS_JSON — POST a tools/call and echo the raw response.
mcp_call() {
  local id="$1" name="$2" args="$3"
  curl -fsS -H "Authorization: Bearer ${TENANT_TOKEN}" \
       ${SESSION_HEADER:+-H "$SESSION_HEADER"} \
       -H "Content-Type: application/json" \
       -d "{\"jsonrpc\":\"2.0\",\"id\":$id,\"method\":\"tools/call\",\"params\":{\"name\":\"$name\",\"arguments\":$args}}" \
       "$MCP_URL"
}

step "Build devsign"
go build -o "$ROOT/build/devsign" "$ROOT/cmd/devsign" \
  || fail "build devsign"
pass "build/devsign"

# --- self-service auth: challenge → sign → verify ---------------------
step "auth_challenge"
AUTH_CH=$(mcp_call 900 "auth_challenge" "$(jq -nc --arg o "$OWNER_ADDR" '{owner:$o}')")
AUTH_NONCE=$(jq -r '.result.structuredContent.nonce' <<<"$AUTH_CH")
AUTH_TEXT=$(jq -r '.result.structuredContent.challenge' <<<"$AUTH_CH")
[[ -n "$AUTH_NONCE" && -n "$AUTH_TEXT" ]] || { echo "$AUTH_CH" | jq . >&2; fail "auth_challenge empty"; }

step "sign challenge (devsign)"
AUTH_SIG=$("$ROOT/build/devsign" --sign-challenge "$AUTH_TEXT")
[[ -n "$AUTH_SIG" ]] || fail "devsign --sign-challenge empty"

step "auth_verify"
AUTH_VF=$(mcp_call 901 "auth_verify" "$(jq -nc --arg n "$AUTH_NONCE" --arg s "$AUTH_SIG" '{nonce:$n,signature:$s}')")
TENANT_TOKEN=$(jq -r '.result.structuredContent.bearer_token' <<<"$AUTH_VF")
[[ -n "$TENANT_TOKEN" && "$TENANT_TOKEN" != "null" ]] || { echo "$AUTH_VF" | jq . >&2; fail "no bearer_token"; }
pass "bearer obtained"

# --- list_faucet_tokens ----------------------------------------------
step "list_faucet_tokens"
TOKENS=$(mcp_call 10 "list_faucet_tokens" '{}')
TOKEN_COUNT=$(jq '.result.structuredContent.tokens | length' <<<"$TOKENS")
[[ "$TOKEN_COUNT" =~ ^[0-9]+$ && "$TOKEN_COUNT" -gt 0 ]] || { echo "$TOKENS" | jq . >&2; fail "no enabled tokens"; }
pass "enabled tokens: $TOKEN_COUNT"

# --- faucet_claim -----------------------------------------------------
step "faucet_claim ($FAUCET_TOKEN)"
CLAIM=$(mcp_call 11 "faucet_claim" "$(jq -nc --arg t "$FAUCET_TOKEN" '{token:$t}')")
TX_HASH=$(jq -r '.result.structuredContent.tx_hash' <<<"$CLAIM")
AMOUNT=$(jq -r '.result.structuredContent.amount' <<<"$CLAIM")
if [[ -z "$TX_HASH" || "$TX_HASH" == "null" ]]; then
  # A rate-limit error here is expected on a repeat run within the window.
  echo "$CLAIM" | jq . >&2
  fail "faucet_claim returned no tx_hash (rate limited? see header)"
fi
pass "claim landed: tx_hash=$TX_HASH amount=$AMOUNT"

printf '\nALL GOOD — faucet dispensed via HTTP.\n'
