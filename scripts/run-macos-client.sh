#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "$SCRIPT_DIR/.." && pwd)"
BIN="$REPO_ROOT/bin/vault-bridge-client"

SERVER="${SERVER:-http://127.0.0.1}"
LOCAL_ROOT="${LOCAL_ROOT:-$HOME/Documents/vault-bridge}"
STATE_DIR="${STATE_DIR:-$HOME/Library/Application Support/vault-bridge}"
LOG_FILE="${LOG_FILE:-$STATE_DIR/client.log}"
STREAM="${STREAM:-1}"
STREAM_POLL_INTERVAL="${STREAM_POLL_INTERVAL:-0}"
DEBOUNCE="${DEBOUNCE:-200ms}"
RECONNECT="${RECONNECT:-5s}"
VERIFY_ON_START="${VERIFY_ON_START:-1}"
SYNC_MODE="${SYNC_MODE:-auto}"
RSYNC_SOURCE="${RSYNC_SOURCE:-server-host:/srv/vault-bridge/source/}"
RSYNC_BIN="${RSYNC_BIN:-/opt/homebrew/bin/rsync}"
RSYNC_SHELL="${RSYNC_SHELL:-ssh}"
TUNNEL_HOST="${TUNNEL_HOST:-}"
TUNNEL_REMOTE_HOST="${TUNNEL_REMOTE_HOST:-}"
TUNNEL_REMOTE_PORT="${TUNNEL_REMOTE_PORT:-39090}"
TUNNEL_LOCAL_PORT="${TUNNEL_LOCAL_PORT:-0}"
TUNNEL_BIN="${TUNNEL_BIN:-ssh}"
BATCH_LIMIT="${BATCH_LIMIT:-10000}"
BUILD_ON_RUN="${BUILD_ON_RUN:-1}"

mkdir -p "$REPO_ROOT/bin" "$STATE_DIR" "$LOCAL_ROOT"

if [[ "$BUILD_ON_RUN" == "1" || ! -x "$BIN" ]]; then
  (cd "$REPO_ROOT" && go build -o "$BIN" ./cmd/vault-bridge-client)
fi

ARGS=(
  -server "$SERVER"
  -local-root "$LOCAL_ROOT"
  -state-dir "$STATE_DIR"
  -log-file "$LOG_FILE"
  -stream-poll-interval "$STREAM_POLL_INTERVAL"
  -debounce "$DEBOUNCE"
  -reconnect "$RECONNECT"
  -verify-on-start="$VERIFY_ON_START"
  -sync-mode "$SYNC_MODE"
  -rsync-source "$RSYNC_SOURCE"
  -rsync-bin "$RSYNC_BIN"
  -rsync-shell "$RSYNC_SHELL"
  -batch-limit "$BATCH_LIMIT"
)

if [[ -n "$TUNNEL_HOST" ]]; then
  ARGS+=(
    -tunnel-host "$TUNNEL_HOST"
    -tunnel-remote-port "$TUNNEL_REMOTE_PORT"
    -tunnel-local-port "$TUNNEL_LOCAL_PORT"
    -tunnel-bin "$TUNNEL_BIN"
  )
  if [[ -n "$TUNNEL_REMOTE_HOST" ]]; then
    ARGS+=( -tunnel-remote-host "$TUNNEL_REMOTE_HOST" )
  fi
fi

if [[ "$STREAM" == "1" ]]; then
  ARGS=(-stream "${ARGS[@]}")
fi

exec "$BIN" "${ARGS[@]}"
