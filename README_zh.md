# vault-bridge

`vault-bridge` 是一个用 Go 实现的 C/S 文档同步工具，用来把远端文档树镜像到本地 vault。

英文版: [README.md](README.md)

## 项目概览

这个项目面向一种很具体的场景：权威内容留在 Linux server 上，本地 macOS client 需要一个接近实时的镜像副本。

当前项目范围：

- server 运行在 Linux
- client 运行在 macOS
- 同步方向为 server 到 client 的单向同步
- 内容类型是 markdown 和常见 vault 附件
- 部署方式优先简单可控，使用普通二进制和 shell wrapper

## 功能

- 增量事件日志和持久化 cursor
- server 端使用 `inotify` 加周期性全量 reconcile
- client 支持 one-shot 和长连接 stream 模式
- 文件传输优先使用 `rsync --files-from`
- `rsync` 不可用或失败时回退到 HTTP
- 通过 `config/filter.json` 配置过滤规则

## 架构

- server: 监控权威根目录，维护快照和追加式事件日志，并通过 HTTP 提供变更接口和文件接口
- client: 消费事件流，合并一批变更，删除本地失效路径，并通过 `rsync` 或 HTTP 拉取变更文件
- 传输分层: journal/control plane 走 HTTP，文件数据面走 `rsync` 或 HTTP

## 仓库结构

- `cmd/vault-bridge-server/`: Linux server 入口
- `cmd/vault-bridge-client/`: macOS client 入口
- `internal/bridge/`: filter、journal store、reconcile、watcher
- `internal/protocol/`: 共享协议结构
- `config/`: 默认过滤配置
- `scripts/`: server/client 运行脚本
- `deploy/`: supervisor、`systemd`、launchd 示例
- `docs/`: 运维说明和环境相关指南

## 快速开始

构建：

```bash
go build ./...
```

启动 server：

```bash
./bin/vault-bridge-server \
  -addr :9090 \
  -root /srv/vault-bridge/source \
  -state-dir "$HOME/.local/state/vault-bridge/server" \
  -filter-config ./config/filter.json
```

以前台 stream 模式启动 macOS client：

```bash
./bin/vault-bridge-client \
  -stream \
  -server http://server-host:9090 \
  -local-root "$HOME/Documents/vault-bridge" \
  -state-dir "$HOME/Library/Application Support/vault-bridge" \
  -sync-mode auto \
  -rsync-source server-host:/srv/vault-bridge/source/ \
  -rsync-bin /opt/homebrew/bin/rsync
```

## 配置

server 端过滤规则放在 `config/filter.json`。

默认行为和之前的 Python 实现一致：

- 排除 `.git/`
- 排除 `.obsidian/`
- 排除 `.DS_Store`
- 只包含 `.md`、`.png`、`.jpg`、`.jpeg`、`.gif`、`.webp`、`.svg`、`.pdf`、`.canvas`

传输模式：

- `auto`: 先尝试 `rsync`，失败时回退到 HTTP
- `rsync`: 强制要求 `rsync`
- `http`: 强制使用 HTTP 文件拉取

## 部署

仓库内已经带了部署示例：

- Linux server: `deploy/supervisor/vault-bridge-server.conf`
- Linux user service: `deploy/systemd/user/vault-bridge-server.service`
- macOS client: `deploy/launchd/dev.vault-bridge.client.plist`

macOS 前台终端运行指南在这里：

- `docs/macos-foreground-client-guide.md`

