# 远程部署 svpchain MCP server

> 与 [`mcp-server-deploy.md`](mcp-server-deploy.md) 互为镜像 — 修改本文件时务必
> 同步另一个。

`scripts/mcp-server-deploy.sh` 把 remote MCP server（`cmd/mcp-server`）作为
docker container 部署到单台远程 SSH 主机上，使用 docker compose 管理。
脚本在本机 build image，push 镜像 + 渲染好的 `mcp.toml` + 自动生成的
`docker-compose.yml` 到远端，然后用 `restart: unless-stopped` +
`network_mode: host` 启动 container。

Container 本身**不持有任何状态**：v0.3 把 nonce / dynamic-tenants /
session-bearers / withdraw-ledger 全部放在内存中。Redeploy 会清空 auth
state — 持有 bearer 的客户端需要重新走
`auth_challenge → sign_challenge → auth_verify` 流程。

## 前置要求

**On operator（本机）：**

- `docker`（带 buildx） — build、save、load image。
- `ssh` + `rsync` — 把 tar 和 config push 到远端。
- `jq` — 脚本里用来做小型 JSON 处理。
- 能拉取 `github.com/deltaping/*` 的网络访问（build 前请配好 `GOPRIVATE`
  和 git 凭据）。

**On remote（远端主机）：**

- SSH 用户可以**不走 sudo** 直接使用 `docker` —— 也就是用户在 `docker`
  组里。脚本所有远端动作（mkdir、rsync、docker compose、docker run、rm）
  都以 ssh 用户身份执行，没有 `sudo`。
- `docker compose` v2 plugin 已装好（`docker compose version` 能跑通）。
  脚本会生成一个 compose 文件并通过 `docker compose up -d` 启动 container，
  不再用裸的 `docker run`。
- `--install-dir`（默认 `~/svpchain-mcp`）所在目录对 ssh 用户可写。默认
  位置在 `$HOME` 下，不需要任何特权路径；如果想换地方，传
  `--install-dir <path>` 即可（前提是 ssh 用户能 `mkdir -p` 那条路径）。
- chain 的 gRPC、CometBFT RPC、indexer 端点从远端可达 — container 用
  `network_mode: host`，所以"host 能到的它就能到"。

## 快速开始

**On operator：**

```bash
./scripts/mcp-server-deploy.sh --host www@svpdev1.example.com
```

绝大多数场景下这一条命令就够了。chain 端点默认是
`127.0.0.1:9090` / `http://127.0.0.1:26657` / `http://127.0.0.1:3002`、
chain id 默认 `svp-2517-1`；需要覆盖请加
`--chain-id` / `--grpc-addr` / `--comet-rpc` / `--indexer`。

`--host` 可以直接是 `~/.ssh/config` 里的 alias —— DNS 和 key 解析都由
operator 这一端 ssh 自己处理。Phase 6 的 smoke test 是在远端跑 `curl`
打 `127.0.0.1`，operator 不需要直接能访问 listener。

执行顺序：

1. **On operator（build）：**
   `docker build --platform linux/amd64 -t svpchain-mcp:<git-sha> -f cmd/mcp-server/Dockerfile .`
2. **On operator（save）：** `docker save` 到
   `build/mcp-server.image.tar`（image id 未变时跳过）。
3. **On operator → remote（ship）：** 用 `rsync` 把 tar、渲染好的
   `mcp.toml`、生成的 `docker-compose.yml` 推到远端的
   `~/svpchain-mcp/`。
4. **On remote（load）：** `docker load`（远端已有同 id 镜像时跳过）。
   脚本随后会在远端把 image 也打上 `svpchain-mcp:latest` tag。
5. **On remote（run）：**
   `docker compose -f ~/svpchain-mcp/docker-compose.yml up -d`。
   Compose 文件里 pin 住刚 load 的 image，并把 `mcp.toml` 只读挂到
   `/etc/svpchain-mcp/mcp.toml`。
6. **On remote（verify）：** 通过 ssh 在远端向
   `http://127.0.0.1:<port>/` POST 一个 JSON-RPC `initialize`，
   断言返回 HTTP 200/202。

`--platform linux/amd64` 是默认值，因为 operator 通常是 Apple
Silicon（arm64）而远端是 Linux amd64。仅在远端是 ARM 时用
`--platform` 覆盖。

## 常用参数

- `--print-config` — 把 `mcp.toml` 打印到 stdout 后退出。
- `--dry-run` — 打印将要执行的全部命令，不真正执行。
- `--skip-build` — 复用本机已有的 `svpchain-mcp:<tag>` image
  （CI / 反复部署场景）。
- `--listen-port 8765` — 修改 host 上对外的端口。
- `--install-dir ~/svpchain-mcp` — 修改远端的安装目录。前缀 `~` 会通过
  一次 ssh 调用展开成远端 ssh 用户的 `$HOME`；绝对路径直接使用。
- `--image-tag <tag>` — 覆盖默认的 `<git-short-sha>` tag。
- `--deposit-max-usdc / --withdraw-max-usdc / --transfer-max-usdc /
  --daily-withdraw-cap-usdc` — 写入 `[limits]` 块；省略则使用 server
  端默认值。

## 重新部署

重复执行同一条命令即可。`save_if_changed` 和 `load_if_missing` 在
image id 未变时会自动跳过，所以只改 config 的 redeploy 不会再传一遍
约 30 MB 的镜像。如果换了 binary，脚本会先 `docker compose down`
（外加一道防御性的 `docker rm -f`）拆掉旧 container，rsync 新 tar，
load、重新打 tag，再 `docker compose up -d` —— 客户端需要重新走 auth
流程。

## 卸载

**On operator：**

```bash
./scripts/mcp-server-deploy.sh --uninstall --host www@svpdev1.example.com
```

会执行 `docker compose down`，移除任何残留的同名 container，删除远端所有
`svpchain-mcp:*` image 以及安装目录。

## 运维要点

- Server 在启动时读一次 `mcp.toml`。直接改远端的 config 不会生效；要么
  redeploy，要么在远端登录后 `docker compose -f ~/svpchain-mcp/docker-compose.yml restart`。
- 日志写到 container stderr，用
  `ssh <host> 'docker logs svpchain-mcp --tail=200 -f'`
  跟踪（**不需要 sudo**，ssh 用户已经在 docker 组里）。
- Phase 6 的 smoke test 只确认 container 在 loopback 上能响应，
  **不验证外部可达性**。如果客户端需要从其他机器访问 listener，防火墙
  / security group / reverse proxy 都由 operator 自己负责。
- 脚本**不**配置 TLS 或 reverse proxy。需要 TLS / hostname 路由请自行
  在前面架 Caddy / nginx / Traefik。
- signer MCP server（`cmd/mcp-signer`）与本脚本无关 — signer 跑在
  agent 所在的本机上，不在远端。
