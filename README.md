# vault-bridge

Go client/server sync for mirroring a remote document tree into a local vault.

Chinese version: [README_zh.md](README_zh.md)

## Overview

`vault-bridge` targets the case where the source of truth stays on a Linux server and a macOS client needs a near-real-time local mirror.

Current project scope:

- server on Linux
- client on macOS
- one-way sync from server to client
- markdown and common vault assets
- operator-friendly deployment with plain binaries and shell wrappers

## Features

- incremental journal with persistent cursor
- `inotify` plus periodic reconcile on the server
- one-shot sync and long-lived stream mode on the client
- `rsync --files-from` as the preferred data path
- HTTP fallback when `rsync` is unavailable or failing
- configurable filter rules through `config/filter.json`

## Architecture

- server: watches the authoritative root, stores a snapshot plus append-only event log, and serves change and file endpoints over HTTP
- client: consumes the event stream, coalesces updates, deletes removed paths locally, and fetches changed files through `rsync` or HTTP
- transport split: HTTP for journal/control traffic, `rsync` or HTTP for file transfer

## Repository Layout

- `cmd/vault-bridge-server/`: Linux server entrypoint
- `cmd/vault-bridge-client/`: macOS client entrypoint
- `internal/bridge/`: filter, journal store, reconcile, watcher
- `internal/protocol/`: shared wire types
- `config/`: default filter config
- `scripts/`: runnable wrappers for server and client
- `deploy/`: example service definitions for supervisor, `systemd`, and launchd
- `docs/`: operator notes and environment-specific guides

## Quick Start

Build:

```bash
go build ./...
```

Run the server:

```bash
./bin/vault-bridge-server \
  -addr :9090 \
  -root /srv/vault-bridge/source \
  -state-dir "$HOME/.local/state/vault-bridge/server" \
  -filter-config ./config/filter.json
```

Run the macOS client in foreground stream mode:

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

## Configuration

Server-side filter rules live in `config/filter.json`.

Default behavior matches the previous Python implementation:

- exclude `.git/`
- exclude `.obsidian/`
- exclude `.DS_Store`
- include `.md`, `.png`, `.jpg`, `.jpeg`, `.gif`, `.webp`, `.svg`, `.pdf`, `.canvas`

Transfer modes:

- `auto`: try `rsync` first, then fall back to HTTP
- `rsync`: require `rsync`
- `http`: force HTTP file fetch

## Deployment

This repository includes example deployment files:

- Linux server: `deploy/supervisor/vault-bridge-server.conf`
- Linux user service: `deploy/systemd/user/vault-bridge-server.service`
- macOS client: `deploy/launchd/dev.vault-bridge.client.plist`

Foreground terminal usage on macOS is documented here:

- `docs/macos-foreground-client-guide.md`

## Status

Current implementation is the first production-oriented MVP. It covers the existing Linux-server to macOS-client workflow and replaces the earlier Python watcher/client pair with a Go codebase.
