# 远程部署 svpchain MCP server

> 与 [`mcp-server-deploy.md`](mcp-server-deploy.md) 互为镜像 — 修改本文件时务必
> 同步另一个。

`scripts/mcp-server-deploy.sh` 把 remote MCP server（`cmd/mcp-server`）作为
docker container 部署到单台远程 SSH 主机上。脚本在本机 build image，
push 镜像 + 渲染好的 `mcp.toml` 到远端，然后以 `--restart unless-stopped`
启动 container。

Container 本身**不持有任何状态**：v0.3 把 nonce / dynamic-tenants /
session-bearers / withdraw-ledger 全部放在内存中。Redeploy 会清空 auth
state — 持有 bearer 的客户端需要重新走
`auth_challenge → sign_challenge → auth_verify` 流程。

## 前置要求

**On operator（本机）：**

- `docker`（带 buildx） — build、save、load image。
- `ssh` + `rsync` — 把 tar 和 config push 到远端。
- `curl` + `jq` — 启动后 smoke-test listener。
- 能拉取 `github.com/deltaping/*` 的网络访问（build 前请配好 `GOPRIVATE`
  和 git 凭据）。

**On remote（远端主机）：**

- SSH 用户可以使用 `docker`（在 docker 组里，或 `sudo docker` 可用）。
- 对 `/opt/svpchain-mcp` 有 `sudo` 权限（目录由脚本自动创建）。
- chain 的 gRPC、CometBFT RPC、indexer 端点从远端可达 — container 用
  `--network host`，所以"host 能到的它就能到"。

## 快速开始

**On operator：**

```bash
./scripts/mcp-server-deploy.sh \
  --host www@svpdev1.example.com \
  --chain-id localsvp-1 \
  --grpc-addr 127.0.0.1:9090 \
  --comet-rpc http://127.0.0.1:26657 \
  --indexer http://127.0.0.1:3002
```

执行顺序：

1. **On operator（build）：**
   `docker build --platform linux/amd64 -t svpchain-mcp:<git-sha> -f cmd/mcp-server/Dockerfile .`
2. **On operator（save）：** `docker save` 到
   `build/mcp-server.image.tar`（image id 未变时跳过）。
3. **On operator → remote（ship）：** 用 `rsync` 把 tar 和渲染好的
   `mcp.toml` 推到 `/opt/svpchain-mcp/`。
4. **On remote（load）：** `docker load`（远端已有同 id 镜像时跳过）。
5. **On remote（run）：**
   `docker run -d --name svpchain-mcp --restart unless-stopped --network host -v /opt/svpchain-mcp/mcp.toml:/etc/svpchain-mcp/mcp.toml:ro svpchain-mcp:<tag>`
6. **On operator（verify）：** 向
   `http://<remote-host>:<port>/` POST 一个 JSON-RPC `initialize`，
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
- `--install-dir /opt/svpchain-mcp` — 修改远端的安装目录。
- `--image-tag <tag>` — 覆盖默认的 `<git-short-sha>` tag。
- `--deposit-max-usdc / --withdraw-max-usdc / --transfer-max-usdc /
  --daily-withdraw-cap-usdc` — 写入 `[limits]` 块；省略则使用 server
  端默认值。

## 重新部署

重复执行同一条命令即可。`save_if_changed` 和 `load_if_missing` 在
image id 未变时会自动跳过，所以只改 config 的 redeploy 不会再传一遍
约 30 MB 的镜像。如果换了 binary，脚本会拆掉旧 container、rsync 新
tar、load、起新 container — 客户端需要重新走 auth 流程。

## 卸载

**On operator：**

```bash
./scripts/mcp-server-deploy.sh --uninstall --host www@svpdev1.example.com
```

会移除 container、远端所有 `svpchain-mcp:*` image 以及
`/opt/svpchain-mcp/` 目录。

## 运维要点

- Server 在启动时读一次 `mcp.toml`。直接改远端的 config 不会生效，
  必须 redeploy。
- 日志写到 container stderr，用
  `ssh <host> 'sudo docker logs svpchain-mcp --tail=200 -f'` 跟踪。
- 脚本**不**配置 TLS 或 reverse proxy。需要 TLS / hostname 路由请自行
  在前面架 Caddy / nginx / Traefik。
- 脚本**不**改防火墙。host 有防火墙时请放行 `--listen-port` 对应的
  TCP 端口。
- signer MCP server（`cmd/mcp-signer`）与本脚本无关 — signer 跑在
  agent 所在的本机上，不在远端。
