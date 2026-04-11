#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "$SCRIPT_DIR/.." && pwd)"
BIN="$REPO_ROOT/bin/vault-bridge-client"

SERVER="${SERVER:-http://127.0.0.1:9090}"
LOCAL_ROOT="${LOCAL_ROOT:-$HOME/Documents/vault-bridge}"
STATE_DIR="${STATE_DIR:-$HOME/Library/Application Support/vault-bridge}"
LOG_FILE="${LOG_FILE:-$STATE_DIR/client.log}"
STREAM="${STREAM:-1}"
STREAM_POLL_INTERVAL="${STREAM_POLL_INTERVAL:-1s}"
DEBOUNCE="${DEBOUNCE:-1s}"
RECONNECT="${RECONNECT:-5s}"
SYNC_MODE="${SYNC_MODE:-auto}"
RSYNC_SOURCE="${RSYNC_SOURCE:-server-host:/srv/vault-bridge/source/}"
RSYNC_BIN="${RSYNC_BIN:-/opt/homebrew/bin/rsync}"
RSYNC_SHELL="${RSYNC_SHELL:-ssh}"
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
  -sync-mode "$SYNC_MODE"
  -rsync-source "$RSYNC_SOURCE"
  -rsync-bin "$RSYNC_BIN"
  -rsync-shell "$RSYNC_SHELL"
  -batch-limit "$BATCH_LIMIT"
)

if [[ "$STREAM" == "1" ]]; then
  ARGS=(-stream "${ARGS[@]}")
fi

exec "$BIN" "${ARGS[@]}"
