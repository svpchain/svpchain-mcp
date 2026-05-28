# devsign — dev-only local signer stand-in

`devsign` is a **dev-only** helper that signs a `TxPayload` produced by the
remote `mcp-server` and emits a `SignedTx` ready to hand to
`broadcast_signed_tx`. It plays the role the future local-signer MCP binary
(`protocol/cmd/mcp-signer/`) will play, so we can exercise the
non-custodial `build → sign → broadcast → query` loop end-to-end before
that binary lands.

> Not part of any release. If you find this in your `$PATH`, you've made a
> mistake.

## When you'd use it

Local manual smoke-tests of `mcp-server` against `make localnet-start`.
Anywhere a real local signer would otherwise be required.

## Build

```sh
GOPRIVATE='github.com/deltaping/*' go build -o build/devsign ./scripts/devsign
```

## Run

```sh
./build/devsign --key-hex 0x<32-byte hex priv> < /tmp/payload.json > /tmp/signed.json
```

Or with explicit paths:

```sh
./build/devsign --key-hex 0x... --in /tmp/payload.json --out /tmp/signed.json
```

The private key can also be supplied via `DEVSIGN_KEY_HEX`.

## Full v0.1 e2e walkthrough (against `make localnet-start`)

This is the canonical demo for the v0.1 remote MCP server. About 2 minutes
end-to-end after the localnet has booted.

### 1. Boot the localnet + indexer

```sh
make localnet-start
./scripts/local_indexer_stack.sh   # if you want the Indexer endpoints to work
```

The localnet provisions dev keys `dev0` … `dev3` (mnemonics in
`scripts/local_node_evm.sh:266-273`) plus `user2` (mnemonic
`abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon abandon about`).
All use the chain's default `eth_secp256k1` algo and the `svp` bech32
prefix.

### 2. Pick a key and grab its address + hex priv

For a quick demo, use one of the dev mnemonics. The bech32 address can be
shown with:

```sh
svpchaind keys show dev0 --keyring-backend test --address
# → svp1...
```

To get the **raw hex private key** (devsign needs this; do NOT do this in
prod):

```sh
svpchaind keys export dev0 --keyring-backend test --unarmored-hex --unsafe
# → <64 hex chars>
```

Stash both:

```sh
ALICE_ADDR="$(svpchaind keys show dev0 --keyring-backend test --address)"
ALICE_KEY_HEX="$(echo y | svpchaind keys export dev0 --keyring-backend test \
                   --unarmored-hex --unsafe 2>/dev/null)"
export DEVSIGN_KEY_HEX="$ALICE_KEY_HEX"
```

### 3. Write the mcp-server config

```sh
cat > /tmp/mcp.toml <<EOF
chain_id         = "localsvp-1"
grpc_addr        = "127.0.0.1:9090"
comet_rpc_url    = "http://127.0.0.1:26657"
indexer_base_url = "http://127.0.0.1:3002"
listen_addr      = "127.0.0.1:8765"
broadcast_mode   = "server"

[auth]
mode = "bearer"

[cache]
markets_refresh = "30s"

[[tenants]]
tenant_id           = "alice"
bearer_token        = "dev-token-alice"
owner               = "$ALICE_ADDR"
allowed_subaccounts = [0]
kill_switch         = false
EOF
```

(Adjust `chain_id` if your localnet uses a different one — check with
`svpchaind status | jq .node_info.network`.)

### 4. Start the mcp-server

```sh
./build/mcp-server --config /tmp/mcp.toml &
```

### 5. Exercise the 10 v0.1 tools

```sh
H='Authorization: Bearer dev-token-alice'
C='Content-Type: application/json'
URL=http://127.0.0.1:8765

# tools/list
curl -sH "$H" -H "$C" -d '{"jsonrpc":"2.0","id":1,"method":"tools/list"}' $URL | jq .

# get_market
curl -sH "$H" -H "$C" -d '{"jsonrpc":"2.0","id":2,"method":"tools/call",
  "params":{"name":"get_market","arguments":{"ticker":"BTC-USD"}}}' $URL | jq .

# get_orderbook (requires the indexer)
curl -sH "$H" -H "$C" -d '{"jsonrpc":"2.0","id":3,"method":"tools/call",
  "params":{"name":"get_orderbook","arguments":{"ticker":"BTC-USD"}}}' $URL | jq .

# whoami
curl -sH "$H" -H "$C" -d '{"jsonrpc":"2.0","id":4,"method":"tools/call",
  "params":{"name":"whoami","arguments":{}}}' $URL | jq .

# build a short-term limit order
HEIGHT=$(curl -s http://127.0.0.1:26657/status | jq -r .result.sync_info.latest_block_height)
GOOD_TIL=$((HEIGHT + 20))

curl -sH "$H" -H "$C" -d "$(cat <<EOF
{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{
  "name":"build_place_limit_order",
  "arguments":{
    "subaccount_number": 0,
    "ticker": "BTC-USD",
    "side": "BUY",
    "size": "0.001",
    "price": "1.00",
    "good_til_block": $GOOD_TIL,
    "order_client_id": 1,
    "payload_client_id": "demo-$(date +%s)"
  }
}}
EOF
)" $URL | jq '.result.structuredContent.payload' > /tmp/payload.json

# sign with devsign
./build/devsign --in /tmp/payload.json --out /tmp/signed.json

# broadcast
CLIENT_ID=$(jq -r .client_id /tmp/payload.json)
curl -sH "$H" -H "$C" -d "$(cat <<EOF
{"jsonrpc":"2.0","id":6,"method":"tools/call","params":{
  "name":"broadcast_signed_tx",
  "arguments":{
    "client_id": "$CLIENT_ID",
    "signed_tx": $(cat /tmp/signed.json)
  }
}}
EOF
)" $URL | jq .

# pull the tx hash out of the previous response, then:
TX_HASH=...  # paste the hex returned above
curl -sH "$H" -H "$C" -d "{\"jsonrpc\":\"2.0\",\"id\":7,\"method\":\"tools/call\",
  \"params\":{\"name\":\"get_tx_status\",\"arguments\":{\"tx_hash\":\"$TX_HASH\"}}}" $URL | jq .

# confirm via live subaccount
curl -sH "$H" -H "$C" -d "{\"jsonrpc\":\"2.0\",\"id\":8,\"method\":\"tools/call\",
  \"params\":{\"name\":\"get_live_subaccount\",\"arguments\":{
    \"owner\":\"$ALICE_ADDR\",\"subaccount_number\":0}}}" $URL | jq .
```

A successful run shows `code: 0` from `broadcast_signed_tx`, a non-zero
`height` from `get_tx_status`, and an order in `get_live_subaccount` (or
an immediately-filled order, depending on the book).

## Known sharp edges

- **Order rejection on price/size alignment.** `build_place_limit_order`
  rejects sizes that aren't multiples of `StepBaseQuantums` or prices that
  aren't multiples of `SubticksPerTick`. For BTC-USD on localnet these are
  small (e.g. step `1000000` quantums); try `size: "0.001"`.
- **`good_til_block` too low** → CheckTx rejects the order. Always set it
  to "current height + a healthy buffer" (20 blocks ≈ ~30 seconds).
- **`payload_client_id` must be unique.** Re-using it triggers the
  idempotency check in `broadcast_signed_tx` (which is the point).
- **The localnet's `chain_id`** may be `svpchain-test` rather than
  `localsvp-1`. Confirm with `svpchaind status | jq .node_info.network`
  and update `/tmp/mcp.toml`.
- **Indexer not running** → `get_market` / `get_orderbook` /
  `get_subaccount` will fail. The chain-side reads (`get_live_subaccount`,
  `get_oracle_price`) work without the indexer.
