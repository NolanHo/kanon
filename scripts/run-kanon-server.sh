#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "$SCRIPT_DIR/.." && pwd)"
BIN="$REPO_ROOT/bin/kanon-server"

ADDR="${ADDR:-:39090}"
ROOT="${ROOT:-/root/docs}"
STATE_DIR="${STATE_DIR:-$HOME/.local/state/kanon/server}"
CONFIG_FILE="${CONFIG_FILE:-$REPO_ROOT/config/filter.json}"
RECONCILE_INTERVAL="${RECONCILE_INTERVAL:-30m}"
WATCH_DEBOUNCE="${WATCH_DEBOUNCE:-200ms}"
BUILD_ON_RUN="${BUILD_ON_RUN:-1}"

mkdir -p "$REPO_ROOT/bin" "$STATE_DIR"

if [[ "$BUILD_ON_RUN" == "1" || ! -x "$BIN" ]]; then
  (cd "$REPO_ROOT" && go build -o "$BIN" ./cmd/kanon-server)
fi

exec "$BIN" \
  -addr "$ADDR" \
  -root "$ROOT" \
  -state-dir "$STATE_DIR" \
  -filter-config "$CONFIG_FILE" \
  -reconcile-interval "$RECONCILE_INTERVAL" \
  -watch-debounce "$WATCH_DEBOUNCE"
