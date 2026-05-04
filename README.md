<div align="center">

# Kanon

> Index `/root/docs` and mirror it to a local reading directory.

[![Go](https://img.shields.io/badge/Go-1.22+-00ADD8.svg)](https://go.dev/)
[![Platform](https://img.shields.io/badge/Platform-Linux%20server%20%2B%20macOS%20client-333333.svg)](#)
[![License](https://img.shields.io/badge/License-MIT-yellow.svg)](./LICENSE)

[Chinese](./README_zh.md) · [macOS foreground guide](./docs/macos-foreground-client-guide.md) · [Linux supervisord deploy guide](./docs/linux-supervisord-deploy-guide.md)

</div>

---

Kanon is a Go client/server system for `/root/docs`.

Current responsibilities:

- watch the Linux docs tree
- maintain a file snapshot and append-only change journal
- serve snapshot, changes, stream, and file transfer endpoints
- mirror changed files to a local macOS reading directory

Planned responsibility:

- build an index over `/root/docs`
- answer query requests with document locations and routing signals

Kanon does not define how `/root/docs` should be written or organized.

## How It Works

```mermaid
flowchart LR
    A[/root/docs on Linux] --> B[kanon-server]
    B --> C[Snapshot, change journal, file endpoints]
    D[kanon-client on macOS] -->|SSH tunnel for control plane| C
    D -->|rsync or HTTP for file transfer| A
    D --> E[Local reading mirror]
    F[Editor or reader] --> E
    G[Query clients] -->|future query API| B
```

## Components

| Component | Role |
| --- | --- |
| `kanon-server` | Watches `/root/docs`, stores a file snapshot and append-only event log, serves HTTP endpoints |
| `kanon-client` | Pulls incremental updates, maintains a local cursor, deletes removed files, fetches changed files |
| `rsync` | Preferred file transfer path for changed files |
| HTTP fallback | Used when `rsync` is unavailable or fails |
| SSH tunnel | Lets the client reach the server control plane when the remote HTTP port is not directly accessible |

## Quick Start

Build:

```bash
go build ./...
```

Start the server on the Linux host:

```bash
./bin/kanon-server \
  -addr :39090 \
  -root /root/docs \
  -state-dir "$HOME/.local/state/kanon/server" \
  -filter-config ./config/filter.json
```

Start the client on macOS in foreground stream mode:

```bash
./bin/kanon-client \
  -stream \
  -server http://127.0.0.1 \
  -tunnel-host server-host \
  -tunnel-remote-port 39090 \
  -local-root "$HOME/Documents/kanon" \
  -state-dir "$HOME/Library/Application Support/kanon" \
  -sync-mode auto \
  -rsync-source server-host:/root/docs/ \
  -rsync-bin /opt/homebrew/bin/rsync
```

Then open the local mirror with the reader or editor of choice:

```text
$HOME/Documents/kanon
```

## Features

- incremental journal with persistent cursor
- `inotify` plus periodic reconcile on the server
- one-shot sync and long-lived stream mode on the client
- `rsync --files-from` as the preferred data path
- HTTP archive fallback when `rsync` is unavailable or failing
- built-in SSH tunnel for the HTTP control plane
- configurable filter rules through `config/filter.json`

## Configuration

Server-side filter rules live in `config/filter.json`.

Default behavior:

- exclude `.git/`, `.obsidian/`, `.venv/`, `venv/`, `node_modules/`, `__pycache__/`, `.ruff_cache/`, `.mypy_cache/`, `.pytest_cache/`
- exclude `.DS_Store`
- exclude basename file patterns such as `*.log`, `*.tmp`, `*.swp`, `*.swo`
- include `.md`, `.png`, `.jpg`, `.jpeg`, `.gif`, `.webp`, `.svg`, `.pdf`, `.canvas`
- optionally exclude whole path subtrees or glob-style path patterns with `excluded_path_patterns`

Transfer modes:

| Mode | Behavior |
| --- | --- |
| `auto` | Try `rsync` first, then fall back to HTTP archive transfer |
| `archive` | Force HTTP archive transfer |
| `rsync` | Require `rsync` |
| `http` | Force one-file-at-a-time HTTP transfer |

Tunnel flags:

- `-tunnel-host`: SSH host that exposes the remote server port
- `-tunnel-remote-host`: remote target host seen from the SSH server; defaults to the host part of `-server`
- `-tunnel-remote-port`: remote target port; defaults to the port part of `-server`
- `-tunnel-local-port`: local forwarded port; `0` means auto-pick a free port above `30000`
- default server listen port: `39090`

## Repository Layout

- `cmd/kanon-server/`: Linux server entrypoint
- `cmd/kanon-client/`: macOS client entrypoint
- `internal/core/`: filter, journal store, reconcile, watcher
- `internal/protocol/`: shared wire types
- `config/`: default filter config
- `scripts/`: server/client run scripts
- `deploy/`: supervisor, `systemd`, and launchd examples
- `docs/`: operator notes and environment-specific guides

## Deployment Files

- Linux server: `deploy/supervisor/kanon-server.conf`
- Linux user service: `deploy/systemd/user/kanon-server.service`
- macOS client: `deploy/launchd/dev.kanon.client.plist`

For Linux hosts that run `kanon-server` under `supervisord`, see:

- `docs/linux-supervisord-deploy-guide.md`

For foreground macOS usage, see:

- `docs/macos-foreground-client-guide.md`
