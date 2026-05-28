# 面向 LLM Agent 的 MCP 服务设计文档（MCP server design doc）

> 状态：设计稿（design doc），供评审。**本文只做设计，不落地任何生产代码。**
> 目标读者：协议、indexer、交易与安全方向的工程评审者。

本文说明如何为 svpchain（一个 dYdX v4 风格的 Cosmos SDK 永续/DEX 链）新增一个 **MCP（Model Context Protocol）server**，让 LLM agent 能够完成完整交易闭环：读行情、查账户与持仓、下单/撤单、划转资金。文中给出工具目录（tool catalog）、后端映射、实现栈（Go on `protocol/`）、密钥托管安全模型与待评审的开放问题。所有结论均配 `file:line` 代码引用。

---

## 1. 背景与动机

`MCP（Model Context Protocol）` 是一种让 LLM agent 以标准化方式发现并调用外部能力的协议：server 向 client（如 Claude Desktop、各类 agent runtime）暴露三类原语——`tools`（可调用的动作）、`resources`（可读取的上下文）、`prompts`（预设提示）。agent 通过这些原语与外部系统交互，而无需把每个系统的私有 API 硬编码进模型。

目前仓库**完全没有**任何 MCP / LLM agent 相关代码（greenfield）。我们希望先用一份设计文档，把「agent 能做什么、数据从哪来、交易怎么签、密钥怎么托管、放在哪个技术栈」这几件事对齐清楚，再决定是否进入实现阶段，从而降低返工与安全风险。

预期产出：一个 MCP server，让一个 LLM agent 能够在 svpchain 上自主完成「看盘 → 判断 → 下单/撤单 → 管理仓位与资金」的完整流程，同时把「能动用资金」这一最高风险面收敛在明确的安全边界内。

---

## 2. 目标与非目标（Goals / Non-goals）

**In scope（四类能力）：**

- **行情（market data）**：perpetual markets、orderbook、candles、trades、sparklines、funding、oracle price。
- **账户与持仓（account & positions）**：subaccount 余额、perpetual/asset positions、open orders、fills、transfers、PnL、funding payments。
- **交易（trading）**：下单/撤单，覆盖 `limit` / `market` / `TWAP` / `scaled` / `conditional`（stop-loss / take-profit）订单类型，以及 `BatchCancel`。
- **资金划转（transfers & funds）**：subaccount 之间转账、bank↔subaccount 的 deposit / withdraw。

**Out of scope（本期不做，但点到为止）：**

- 治理（`x/gov`、`govplus`）、建市（`MsgCreateClobPair` 等带 `authority` signer 的 admin 消息）、validator 运维。
- EVM precompile 充值路径（MCP server 默认走标准 Cosmos `Msg` 的 `DepositToSubaccount`；EVM precompile 充值路径作为后续扩展提及但不设计）。
- 跨链 bridge 流程（`x/bridge`）。

---

## 3. 现状与可用接口（Current state & available interfaces）

关键结论先行：**Comlink（indexer 的 REST）是只读的，没有下单接口；任何写操作（下单/撤单/划转）都必须以签名 `Msg` 的形式提交到链上 gRPC。** 这直接印证了「双后端」的取数策略——读路径优先走 indexer（数据更丰富、含历史），写路径与实时状态走链。

| 后端 | 用途 | 入口 / 引用 |
| --- | --- | --- |
| Comlink REST `/v4/*` | 行情 + 账户历史数据（**只读**） | `indexer/services/comlink/src/controllers/api/index-v4.ts:34-60` 挂载各 controller |
| Socks WebSocket | 行情/账户实时推送 | `indexer/services/socks/src/types.ts:19-27` 的 `Channel` 枚举 |
| 链 gRPC `:9090` —— `Msg` | 下单/撤单/划转（**写，需签名**） | `proto/dydxprotocol/clob/tx.proto`、`proto/dydxprotocol/sending/tx.proto` |
| 链 gRPC `:9090` —— `Query` | 实时（未必已落 indexer）链上状态 | `proto/dydxprotocol/{subaccounts,clob,prices}/query.proto` |
| 链 gRPC `:9090` —— `Tx.BroadcastTx` | 广播已签名交易 | 标准 SDK `Tx.ServiceClient.BroadcastTx`（`BROADCAST_MODE_SYNC`） |
| CometBFT RPC `:26657` | 交易状态查询、区块订阅 | 标准 CometBFT JSON-RPC |
| 全节点流式 `/ws` | 低延迟 orderbook/subaccount 流（与 indexer 解耦的备选） | `protocol/streaming/ws/websocket_server.go`；对应 `clob.Query/StreamOrderbookUpdates`（`proto/dydxprotocol/clob/query.proto:90`） |

**链上身份与密钥（最关键的前提）**：本链是 EVM 化的 Cosmos 链，account bech32 前缀为 `svp`（`protocol/app/config/config.go:9`），coin type 走 cosmos/evm 的 `Bip44CoinType`（`protocol/app/config/config.go:31`），即密钥为 **`eth_secp256k1`**。这意味着任何能交易/动用资金的组件，都必须托管或代理一把 `eth_secp256k1` 私钥——这是整套系统的最高风险点（见第 9 章）。

**Socks WS 频道**（`indexer/services/socks/src/types.ts:19-27`）：`v4_orderbook`、`v4_subaccounts`（账户/订单/持仓/fills）、`v4_trades`、`v4_markets`、`v4_candles`、`v4_parent_subaccounts`、`v4_block_height`。

---

## 4. MCP 概念简介（MCP primer）

- **tools**：agent 可主动调用的「动作」，带 JSON Schema 入参与结构化出参。本设计的下单、撤单、划转、查询都建模为 tools。
- **resources**：可被 agent 读取的「上下文资源」，以 URI 寻址。本设计用 `market://{ticker}` 暴露市场元数据（见第 6/7 章），让单位换算所需的常量可被缓存与引用。
- **prompts**：预置的提示模板（本期可选，先不展开）。
- **传输（transport）**：
  - `stdio`：本地单用户场景（agent 与 MCP server 同机），无网络鉴权需求，最简单、风险最低。
  - `Streamable HTTP / SSE`：远程或多客户端共享场景，**必须**加鉴权与网络隔离（见第 9、10 章）。
- agent 通过 `tools/list`、`resources/list` 发现能力，再以 `tools/call` 调用。文档的工具目录（第 5 章）即对应 `tools/list` 的内容。

---

## 5. 能力 → 工具目录（Capability → tool catalog）

每个 MCP tool 都映射到一个具体后端端点 / RPC。约定：**读优先走 Comlink/Socks，写与实时状态走链 gRPC。**

### 5.A 行情（读 → Indexer，可选实时 → 链）

| MCP tool | 主要入参 | 后端映射 |
| --- | --- | --- |
| `list_markets` | （可选 `ticker`/`limit`） | `GET /v4/perpetualMarkets`（`perpetual-markets-controller.ts:46,48`） |
| `get_market` | `ticker` | `GET /v4/perpetualMarkets?ticker=` |
| `get_orderbook` | `ticker` | `GET /v4/orderbooks/perpetualMarket/:ticker`（`orderbook-controller.ts:27,29`） |
| `get_candles` | `ticker`,`resolution`,`limit`,`fromISO`,`toISO` | `GET /v4/candles/perpetualMarkets/:ticker`（`candles-controller.ts:26,28`） |
| `get_trades` | `ticker`,`limit` | `GET /v4/trades/perpetualMarket/:ticker`（`trades-controller.ts:43,45`） |
| `get_sparklines` | `timePeriod` | `GET /v4/sparklines`（`sparklines-controller.ts:32,34`） |
| `get_historical_funding` | `ticker` | `GET /v4/historicalFunding/:ticker`（`historical-funding-controller.ts:40,42`） |
| `get_oracle_price` | `marketId`（实时） | 链 `prices.Query/MarketPrice`（`prices/query.proto:15`）/ `AllMarketPrices`（`:20`） |
| `get_height` / `get_time` | — | `GET /v4/height`（`height-controller.ts:17,19`）、`GET /v4/time`（`time-controller.ts:16,18`） |
| `subscribe_market_data`（可选，仅 HTTP 传输） | `channel`,`ticker` | Socks `v4_orderbook`/`v4_trades`/`v4_candles`/`v4_markets`（`socks/.../types.ts:19-27`） |

**Resource** `market://{ticker}`：缓存 `clobPairId`、`atomicResolution`、`stepBaseQuantums`、`subticksPerTick` 等元数据（来源 `list_markets`），供第 7 章单位换算引用。

### 5.B 账户与持仓（读 → Indexer，实时 → 链）

| MCP tool | 主要入参 | 后端映射 |
| --- | --- | --- |
| `get_address_summary` | `address` | `GET /v4/addresses/:address`（`addresses-controller.ts:86,88`） |
| `get_subaccount` | `address`,`subaccountNumber` | `GET /v4/addresses/:address/subaccountNumber/:n`（`addresses-controller.ts:165`） |
| `get_perpetual_positions` | `address`,`subaccountNumber`,filters | `GET /v4/perpetualPositions`（`perpetual-positions-controller.ts:64,66`） |
| `get_asset_positions` | `address`,`subaccountNumber` | `GET /v4/assetPositions`（`asset-positions-controller.ts:60,62`） |
| `get_orders` | `address`,`subaccountNumber`,status/side/type | `GET /v4/orders`（`orders-controller.ts:216,218`） |
| `get_order` | `orderId` | `GET /v4/orders/:orderId`（`orders-controller.ts:301`） |
| `get_fills` | `address`,`subaccountNumber`,`market` | `GET /v4/fills`（`fills-controller.ts:55,57`） |
| `get_transfers` | `address`,`subaccountNumber` | `GET /v4/transfers`（`transfers-controller.ts:63,65`） |
| `get_pnl` / `get_historical_pnl` | `address`,`subaccountNumber` | `GET /v4/pnl`（`pnl-controller.ts:45,47`）、`GET /v4/historical-pnl`（`historical-pnl-controller.ts:42,44`） |
| `get_funding_payments` | `address`,`subaccountNumber` | `GET /v4/fundingPayments`（`funding-payments-controller.ts:47,49`） |
| `get_live_subaccount` | `owner`,`number`（实时、含未落库状态） | 链 `subaccounts.Query/Subaccount`（`subaccounts/query.proto:15`） |
| `get_stateful_order` | `orderId`（实时） | 链 `clob.Query/StatefulOrder`（`clob/query.proto:61`） |
| `get_twap_order` / `get_scaled_order` | `orderId`（实时） | 链 `clob.Query/TwapOrderPlacement`（`clob/query.proto:66`）/ `ScaledOrderPlacement`（`:71`） |

### 5.C 交易（写 → 链 gRPC，需签名）

所有交易 tool 内部统一流程：`ticker → clobPairId` 解析 + 单位换算（见第 7 章）→ 构造 `MsgPlaceOrder`/`MsgCancelOrder`/`MsgBatchCancel` → 用标准 SDK 签名（`SIGN_MODE_DIRECT`，并按第 7 章规则管理 sequence）→ 经 `Tx.BroadcastTx` 广播。

| MCP tool | 后端映射（`clob.Msg`，`clob/tx.proto`） | 关键字段（`clob/order.proto`） |
| --- | --- | --- |
| `place_limit_order` | `PlaceOrder`（`:27`）/`MsgPlaceOrder`（`:81`） | `side`(`:136`),`quantums`(`:149`),`subticks`(`:154`),`time_in_force`(`:174`),`reduce_only`(`:203`),`good_til_*`(`:157`) |
| `place_market_order` | `PlaceOrder` | 以 `TIME_IN_FORCE_IOC` + 远端 `subticks` 实现激进 limit（见第 7 章） |
| `place_twap_order` | `PlaceOrder`（`twap_parameters`） | `TwapParameters`(`:350`)：`duration`[300,86400]、`interval`[30,3600]、`price_tolerance` ppm |
| `place_scaled_order` | `PlaceOrder`（`scaled_order_parameters`） | `ScaledOrderParameters`(`:304`)：`lowest/highest_price`、`num_orders`[2,50]、`profile`、`price_spacing`、`magnitude_ppm`、`time_in_force` |
| `place_conditional_order` | `PlaceOrder`（conditional） | `condition_type`(`:209`)、`conditional_order_trigger_subticks`(`:231`) |
| `cancel_order` | `CancelOrder`（`:29`）/`MsgCancelOrder`（`:87`） | `order_id` + `good_til_block`(short-term)/`good_til_block_time`(stateful) |
| `batch_cancel_orders` | `BatchCancel`（`:31`）/`MsgBatchCancel`（`:110`） | `subaccount_id` + `short_term_cancels` + `good_til_block` |

### 5.D 资金划转（写 → 链 gRPC，需签名）

| MCP tool | 后端映射（`sending.Msg`，`sending/tx.proto`） |
| --- | --- |
| `deposit_to_subaccount` | `DepositToSubaccount`（`:14`）—— bank → subaccount |
| `withdraw_from_subaccount` | `WithdrawFromSubaccount`（`:18`）—— subaccount → bank |
| `transfer_between_subaccounts` | `CreateTransfer`（`:11`）/`MsgCreateTransfer`（`:31`）—— subaccount ↔ subaccount |

### 5.E 横切工具（cross-cutting）

| MCP tool | 说明 |
| --- | --- |
| `simulate` | 构造并本地校验交易（单位、字段、护栏），**不广播**。资金类 tool 默认先走此路径。 |
| `get_tx_status` | 通过 CometBFT RPC `:26657` 按 tx hash 查询落块状态。 |
| `whoami` | 仅返回当前签名地址与被允许操作的 subaccount 白名单（**绝不返回私钥**）。 |

---

## 6. 架构与数据流（Architecture & data flow）

```
+- 用户本地机器 (local) ------------------+        +- 运营方网络 (remote) ------------+
|                                          |        |                                   |
|   MCP client (Cursor / Claude Desktop)   |  HTTP  |   Remote MCP server               |
|                  |                       |  /SSE  |   - tool dispatch                 |
|                  | stdio                 | <----> |   - market metadata 缓存          |
|                  v                       |        |   - 单位换算 / 校验 / 护栏        |
|   Local signer MCP server                |        |   - 交易构造 + (可选) 广播        |
|   - sign_transaction                     |        |   - server 不持密钥               |
|   - approval / policy                    |        +-------+-----------+---------------+
|   - 持本地密钥 / 硬件钱包                |             读 |           | 写 + 实时
+------------------------------------------+               v           v
                                                   +-----------+ +-----------------+
   ★ 密钥永不离开本地机器                          | Indexer   | | 链 svpchaind     |
                                                   | Comlink   | | gRPC :9090       |
                                                   | Socks WS  | | CometBFT :26657  |
                                                   +-----------+ +-----------------+
```

- **部署拓扑与信任边界**：本设计默认拓扑——**远程 MCP server** 跑在运营方网络（承担工具调度、行情读、单位换算、护栏、交易构造与可选广播），**MCP client + local signer MCP server** 跑在用户本地机器（signer 经 stdio 暴露 `sign_transaction`，持有本地密钥或对接硬件钱包）。两者经 Streamable HTTP/SSE 通信（详见 §9.1(c)、§10）。
- **读路径**：行情与账户历史 → Comlink REST（必要时 Socks WS 实时推送）。这是数据最丰富的来源。
- **写路径 + 实时状态**：下单/撤单/划转的 `Msg` 与「未必已落 indexer」的实时链上状态 → 链 gRPC；广播走 `Tx.BroadcastTx`；落块确认走 CometBFT RPC。
- **市场元数据缓存**：远程 server 启动时（并定期刷新）从 `list_markets` 拉取 `clobPairId ↔ ticker`、`atomicResolution`、`stepBaseQuantums`、`subticksPerTick`，作为单位换算（第 7 章）的单一数据源，避免每次下单都实时查询。
- **签名位置**：签名**严格只发生在用户本地机器上的 local signer**——远程 server 永不接触私钥；私钥也不经过 agent、不进入 tool 出入参（第 9 章）。

---

## 7. 单位换算与下单语义（正确性关键章）

这是 agent 最容易出错、也最该由 MCP server 兜底的地方。MCP server 必须对 agent 暴露「人类直觉单位」（价格、数量），在内部转成链上单位。

- **数量 → quantums**：`Order.quantums`（`clob/order.proto:149`）以 base quantums 计，且必须是 `ClobPair.StepBaseQuantums` 的整数倍。
- **价格 → subticks**：`Order.subticks`（`:154`）必须是 `ClobPair.SubticksPerTick` 的整数倍。换算依赖 `atomicResolution` 等市场元数据（来自第 6 章缓存）。
- **market order**：本链无独立「市价单」类型；约定以 `TIME_IN_FORCE_IOC`（`:174` 的 `TIME_IN_FORCE_IOC`）+ 远离盘口的激进 `subticks` 实现，由 `place_market_order` 按 `slippageBps`/`worstPrice` 计算并兜底滑点。
- **short-term vs stateful**：`good_til_oneof`（`:157`）二选一——`good_til_block`（short-term，按区块高度过期）或 `good_til_block_time`（stateful/conditional，按时间过期）。`order_flags`（`:29-36`）取值：`ShortTerm=0`、`Conditional=32`、`LongTerm=64`、`Twap=128`、`ScaledOrder=512`（`*Suborder`/`*Child` 为内部使用）。
- **sequence（最易出错）**：短期 CLOB 交易不消耗 account sequence——ante handler 对短期 CLOB 单跳过 `incrementSequence`（`app/ante.go:331-342`，由 `IsShortTermClobMsgTx` 判定），因此连续的短期单复用同一 `seq`；stateful 交易则用单调递增的 `seq`。MCP server 的 signer 必须区分这两类并正确管理 nonce。
- **签名与手续费**：单签名者、标准 SDK `SIGN_MODE_DIRECT`；CLOB 交易在本链免手续费（`Fee.Amount` 置空），`GasLimit` 取一个高于真实消耗的固定值即可。
- **TWAP / Scaled 约束**：`TwapParameters`（`:350`）`duration`∈[300,86400]s、`interval`∈[30,3600]s 且需整除 duration；`ScaledOrderParameters`（`:304`）`num_orders`∈[2,50]、`magnitude_ppm` 按 `profile` 有不同合法区间。这些校验应在 `simulate`/下单前由 MCP server 强制执行，给 agent 明确报错。

---

## 8. 实现栈：`protocol/` 内 Go 组件

远程 MCP server 采用 **Go**，作为 `protocol/` 仓库内独立的 `cmd/` 二进制（**不**作 in-process daemon——尽管本设计下远程 server 已不持私钥，但与 validator 进程分离仍有助于权限边界与部署独立性）。本地 signer 同样用 Go（见 §10「本地 signer 实现栈」）。理由：

- **签名 / 密钥**：与链自身一致的 `eth_secp256k1` 路径，最不易出错；可复用 `cmd/svpchaind` 的 keyring 接线。
- **高层交易客户端**：仓库**无 in-tree 高层 client**（`CompositeClient` 在外部仓库 `dydxprotocol/v4-clients`，见根 `README.md`）；Go 路径无需引外部 client —— 直接复用链自身的 `x/clob/types` 等 message 类型构造交易，并用标准 SDK 的签名/广播栈。
- **单位/语义正确性**：可直接 import 链自身 `x/clob/types`、`x/subaccounts/types` 作单一真相源，单位换算与 sequence 规则不必跨语言重写。
- **与 local signer 同语言**：两者共享 `protocol/lib/mcp/`，杜绝 proto / 单位换算 / sequence 语义跨语言漂移。
- **部署**：作 `protocol/` 仓库内独立 `cmd/` 二进制；版本随 `svpchaind` 同步发布。
- **MCP Go SDK**：用官方 `github.com/modelcontextprotocol/go-sdk`（生态较 TS 年轻，但 stdio / Streamable HTTP 接口已足够）。

**为什么不用 TypeScript（不采用的原因）**：若必须把 MetaMask / Keplr / WalletConnect 等**浏览器扩展钱包**作为主要密钥来源（dYdX 前端风格），可考虑改用 TS + `@dydxprotocol/v4-client-js`；但本设计偏自主 agent，硬件钱包 + 本地 key 已覆盖现实需求，且 TS 路径须承担与本 fork proto / `svp` / `eth_secp256k1` 的版本漂移成本，故继续 Go。

---

## 9. 密钥管理与安全（最高风险章）

该组件**能下单、能动用资金**，必须按「热钱包签名服务（hot-wallet signing service）」对待。设计要点：

1. **托管模型（按环境区分；列为开放问题）** —— 本设计默认拓扑（见 §6、§10）：远程 MCP server 跑在运营方网络，MCP client + local signer MCP server 跑在用户本地。在此拓扑下，(a)/(b) 实际等同于**运营方代用户托管热钱包**（custodial），用户须信任运营方持/管签名密钥；(c) 是唯一让用户密钥不离本机的方案，本文以 (c) 为默认。
   - **(a) 本地持私钥（KMS 包裹）本地签名** —— 最简单；blast radius（受害面，被攻破后的可造成损失范围）最大；在远程拓扑下属于 custodial 模型（运营方代管签名密钥）。
   - **(b) 外部 signer / KMS / HSM 代理签名，本进程不持原始密钥** —— 密钥不可导出；在远程拓扑下仍属 custodial 模型（运营方代管签名密钥）；基础设施成本更高。
   - **(c) 非托管 / Remote Transaction Crafting Pattern（推荐）** —— 远程 server 永不接触私钥，只构造未签名交易（可选广播），签名在用户本地完成。流程：
     1. **请求**：本地 AI client（Cursor / Claude Desktop 等）发起交易意图。
     2. **构造**：远程 server 解析 `ticker→clobPairId`、做单位换算、组装 `MsgPlaceOrder`/`MsgCancelOrder`、查 `account_number`/`sequence`，产出未签名 payload。
     3. **返回 payload**：远程 server 把未签名 payload 回传本地 client。
     4. **本地签名**：本地 client 把 payload 交给本地钱包 / 硬件钱包，或本地 signer MCP server（走 `stdio`、暴露 `sign_transaction`；实现栈见 §10）签名。
     5. **广播**：本地直接广播，**或** 通过 `broadcast_signed_tx` 交回远程 server 广播（见下文「变体」）。
     - **本链适配**：CLOB 交易为 Cosmos `Msg`、须 `eth_secp256k1` + `svp` + `SIGN_MODE_DIRECT`（非 EVM），纯 EVM 钱包（MetaMask / `eth_sign`）不能签 CLOB 单；sequence 规则见 §7。
     - **变体：server 端广播 vs 本地广播** —— **本地广播** 最大非托管纯度但远程 server 不在广播路径，§9.3/§9.7/§9.10 护栏只能作建议性。**server 端广播（推荐为生产默认）**：密钥仍只在本地，server 只看到已签名字节（签名绑定字节，可广播或拒绝但不能篡改）；护栏、`client_id` 幂等、AIMD 背压、审计日志可被强制；代价是 server 成为可用性 SPOF，能审查不能伪造。**混合**：默认 server 端广播，超时/被拒回退本地广播。
     - **提供本地签名器**：运营方可向 client 侧提供本地签名工具（即上述 local signer MCP server；实现栈见 §10）。要点：用户自带密钥/硬件钱包（运营方只给代码、不托管，同 MetaMask 模型）；信任模型由「运营方结构上无法触密钥」弱化为「运营方客户端代码不外泄密钥」，故应**开源/可审计**、优先支持硬件钱包；本地 signer 是 §9.3 审批 / 护栏的天然落点。**反面（不推荐）**：把签名钱包放进运营方自管的 client runtime 自动签名——「server 不持密钥」字面成立，但热私钥只是挪到运营方另一进程，仍在同一信任域。
   - **建议**：testnet 默认 (a)；远程/共享部署推荐 (c) Remote Transaction Crafting Pattern + server 端广播；若 mainnet 必须服务端自动签名且资金量大，用 (b)。
2. **单签名者 + subaccount 白名单**：MCP server 只绑定一个 `owner` 地址与一组允许的 `subaccountNumber`（身份模型见 `subaccounts/subaccount.proto:11`），对任何引用其他 owner 的调用直接拒绝。
3. **策略护栏（policy guardrails）**：单笔/单位时间名义上限、最大仓位、提现目的地白名单、日提现上限；并做硬分级——`place/cancel` 可自动放行，而 `withdraw_from_subaccount` / `transfer_between_subaccounts` 需更高信任的显式确认（MCP 确认流或单独审批开关）。**资金划转是最高风险操作。**
4. **资金类工具默认 dry-run**：`withdraw`/`transfer`/`deposit` 默认走 `simulate`，须显式 `confirm: true` 才广播。
5. **密钥零暴露**：私钥绝不出现在 tool 出入参或日志；`whoami` 只返回地址。日志复用结构化 logger，且**不**记录 tx 字节与签名。
6. **传输鉴权**：`stdio`（本地单用户）无需网络鉴权；`HTTP/SSE` 必须加鉴权（bearer token / mTLS）、绑定回环（loopback）或私网、**绝不公网暴露**——被攻破的 endpoint 等于资金可被直接盗取。
7. **限流与幂等**：以 `client_id`（`clob/order.proto:24`）做去重幂等；广播侧用 inflight 窗口背压（类似 TCP 拥塞窗口的 AIMD 思路：健康涨窗、拥塞减半），避免压垮 mempool/自我 DoS；并尊重 Comlink 自身的 rate limiter。
8. **交易正确性即安全**：强制第 7 章的 short-term/stateful sequence 规则与下单前单位校验——签错 nonce 会导致静默拒绝或重放混乱。
9. **进程隔离**：local signer 是唯一持密钥的进程，跑在用户本地、独立二进制；远程 server 虽不持密钥也以独立 `cmd/` 二进制部署，避免与 validator 共进程。
10. **kill switch 与审计**：全局「禁止交易」开关；对每一笔签名/广播的交易做仅追加（append-only）审计日志（hash、msg 类型、subaccount、名义额），便于事后复盘。

---

## 10. 传输与部署（Transport & deployment）

- **两种 server，两种传输**（与 §6 拓扑对应）：
  - **远程 MCP server**（运营方网络，承担工具调度、构造、广播）走 **Streamable HTTP/SSE**，必须满足第 9.6 鉴权（bearer token / mTLS）、绑定私网或加 WAF、**绝不公网裸露**。
  - **本地 signer MCP server**（用户机器，与 MCP client 同机）走 **`stdio`**，无网络面，密钥不出本机。
- **部署位置**：
  - 远程 server → `protocol/cmd/mcp-server/`，独立 `cmd/` 二进制 + 独立容器/镜像，随 `svpchaind` 同步发布。
  - 本地 signer → 单独发布的小工具/包（参考 `mcp-wallet-signer`），用户自行安装；实现栈见下。
- **本地 signer 实现栈（推荐 Go）**：与远程 server 同语言，规避 proto / 单位换算 / sequence 语义的跨语言漂移；作为「持密钥」的工具，单二进制 + 最小依赖也更友好审计与分发。
  - **共享代码**：在 `protocol/lib/mcp/` 放公共代码（proto helpers、复用 `app/ante.go:331-342` 的 `IsShortTermClobMsgTx` 判定、单位换算、`MsgPlaceOrder`/`MsgCancelOrder` 构造、审计日志格式），remote 与 signer 都 import，作单一真相源。
  - **二进制布局**：`protocol/cmd/mcp-server/`（远程）+ `protocol/cmd/mcp-signer/`（本地），共享 `lib/mcp/`；signer 单独构建并发布跨平台静态二进制（`mcp-signer-{darwin-arm64,darwin-amd64,linux-amd64,windows-amd64}`），用户本地安装。
  - **MCP SDK**：用官方 `github.com/modelcontextprotocol/go-sdk`，以 `stdio` server 启动；暴露 `sign_transaction(payload)`、`whoami()` 等工具，并在 `sign_transaction` 内嵌 §9.3 审批 / 护栏层（额度、白名单、可选人工确认）——远程 server 退出广播路径后，这一层只能落在本地（见 §9.1 (c)「变体」chokepoint 段）。
  - **密钥来源（按优先级）**：① Ledger / 硬件钱包（cosmos-sdk keyring 内置支持）；② 本地 keyring 文件 + passphrase（cosmos-sdk `file` / `os` / `pass` backend）；③ 派生 trading key（dYdX 风格：EVM 钱包签一次确定性消息派生 Cosmos 交易密钥，加密落本地，后续自动签名时无需再唤起 EVM 钱包——对自主 agent 流最友好）。
  - **可直接复用**：cosmos-sdk `ethsecp256k1.PrivKey` 签名路径、keyring 后端、`SIGN_MODE_DIRECT` 编码——与 `cmd/svpchaind` 同栈，直接 import。
  - **何时改用 TypeScript**：若必须把 MetaMask / Keplr / WalletConnect 等**浏览器扩展钱包**作为主要密钥来源（dYdX 前端风格），改用 TS + `@dydxprotocol/v4-client-js`——Go 进程触达浏览器扩展须额外架 localhost HTTP 桥，不优雅。本设计偏自主 agent，硬件钱包 + 本地 key 已覆盖现实需求。
- **配置与密钥**：远程 server **不持私钥**，只配链 endpoint（gRPC/RPC）、Comlink/Socks base URL、chainId、护栏阈值；本地 signer 配本地钱包接入方式与签名审批策略。
- **可观测性**：复用 protocol 既有 metrics 与结构化日志；统一暴露每个 tool 的调用量/时延/错误率、广播 accept/reject、护栏触发次数。

---

## 11. 可靠性与限流（Reliability & rate limits）

- **广播模式**：默认 `BROADCAST_MODE_SYNC`，读 `TxResponse.Code`（0 = 进 mempool）；落块确认通过 `get_tx_status` 轮询 CometBFT RPC。
- **背压**：用 inflight 窗口的 AIMD 思路（健康涨窗、拥塞减半）控制并发广播，避免自我 DoS。
- **幂等**：同一意图复用同一 `client_id`，避免重复下单。
- **过期/重组**：short-term 单按 `good_til_block` 过期、stateful 单按 `good_til_block_time` 过期；MCP server 应在结果中明示订单生命周期，必要时引导 agent 重下。
- **尊重上游限流**：Comlink controller 自带 rate limiter，读路径需退避重试。

---

## 12. 测试与上线（Testing & rollout）

- **先 testnet**：所有写路径先在本地 localnet / testnet 验证通过（`make localnet-start`）。
- **dry-run / simulate**：交易与资金 tool 默认 `simulate`，验证单位换算、字段合法性、护栏后再开放真实广播。
- **paper-trading 开关**：提供「只读 + 模拟」模式，让 agent 全流程演练而不真实下单。
- **kill switch**：保留全局禁交易开关，异常时一键停用。
- **回归**：针对第 7 章单位换算与 sequence 规则编写用例（这是最易回归的点）。

---

## 13. 待评审决策的开放问题（Open questions for reviewers）

1. **托管模型**：本地 KMS 包裹密钥 (a) / 外部 signer·HSM (b) / 非托管外部签名 (c，Remote Transaction Crafting Pattern；见 §9.1)？若走 (c)：签名钱包跑在哪（用户设备 vs 运营方 client）、用什么本地签名方（支持 eth_secp256k1 的 Cosmos 钱包 / `mcp-wallet-signer` / 硬件钱包）？由 server 还是 client 广播（护栏强制力取舍）？如何把密钥材料挡在 LLM 上下文之外？异步签名下如何协调 account sequence？是否按 testnet vs mainnet 分别取值？
2. **资金类工具门控**：`withdraw`/`transfer` 是否一律要求 human-in-the-loop 确认，还是仅按额度护栏放行？
3. **远程 server 鉴权**：HTTP/SSE 传输（已固定为远程 server 的方式，见 §10）下的鉴权机制——bearer token、mTLS、API key + IP allowlist，还是更强方案？是否对 testnet/mainnet 区分？
4. **实时读取策略**：何时走链 gRPC（新鲜、未落库）vs Comlink（已落库、更丰富）？是否默认 Comlink + `live:true` 显式切链？
5. **行情流式**：是否把 Socks WS（`v4_*` 频道）暴露为 MCP 订阅，还是 v1 只做 REST 轮询？
6. **多租户**：仅单签名者，还是支持多租户/每会话密钥？（多租户显著放大托管风险。）
7. **v1 订单类型范围**：先只上 `limit`/`market`，还是 `TWAP`/`scaled`/`conditional` 一次到位？

---

## 14. 附录（Appendix）

### 14.1 关键文件索引

- short-term 订单的 sequence 处理：`protocol/app/ante.go:331-342`（短期 CLOB 单跳过 `incrementSequence`）；签名/广播用标准 Cosmos SDK 的 `SIGN_MODE_DIRECT` 与 `Tx.BroadcastTx`。
- 交易 Msg 与订单类型：`proto/dydxprotocol/clob/tx.proto`、`proto/dydxprotocol/clob/order.proto`。
- 划转 Msg：`proto/dydxprotocol/sending/tx.proto`。
- 实时 Query：`proto/dydxprotocol/{subaccounts,clob,prices}/query.proto`。
- subaccount 身份：`proto/dydxprotocol/subaccounts/subaccount.proto`。
- 只读 REST 端点清单：`indexer/services/comlink/src/controllers/api/index-v4.ts` 及其 `v4/*-controller.ts`。
- 实时 WS 频道：`indexer/services/socks/src/types.ts`。
- bech32 前缀与密钥：`protocol/app/config/config.go`。
- 高层 client 在外部仓库的说明：根 `README.md`（`dydxprotocol/v4-clients`）。
- 中文文档风格模板：`protocol/docs/liquidation-service.md`。

### 14.2 术语表（glossary）

- **subaccount**：用户的保证金账户单元，由 `{owner, number}` 唯一标识（`subaccounts/subaccount.proto:11`），含 `AssetPositions`/`PerpetualPositions`。
- **quantums / subticks**：链上的数量/价格内部单位；下单前须由市场元数据换算（第 7 章）。
- **short-term order / stateful order**：分别按区块高度（`good_til_block`）与时间（`good_til_block_time`）过期；sequence 管理规则不同（第 7 章）。
- **Scaled order**：价格网格梯度订单（price-grid ladder），在 `[lowest_price, highest_price]` 区间展开为多个子限价单（`clob/order.proto:304`）。
