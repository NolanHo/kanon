#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(cd -- "$SCRIPT_DIR/.." && pwd)"
BIN="$REPO_ROOT/bin/vault-bridge-server"

ADDR="${ADDR:-:39090}"
ROOT="${ROOT:-/srv/vault-bridge/source}"
STATE_DIR="${STATE_DIR:-$HOME/.local/state/vault-bridge/server}"
CONFIG_FILE="${CONFIG_FILE:-$REPO_ROOT/config/filter.json}"
RECONCILE_INTERVAL="${RECONCILE_INTERVAL:-10m}"
WATCH_DEBOUNCE="${WATCH_DEBOUNCE:-1s}"
BUILD_ON_RUN="${BUILD_ON_RUN:-1}"

mkdir -p "$REPO_ROOT/bin" "$STATE_DIR"

if [[ "$BUILD_ON_RUN" == "1" || ! -x "$BIN" ]]; then
  (cd "$REPO_ROOT" && go build -o "$BIN" ./cmd/vault-bridge-server)
fi

exec "$BIN" \
  -addr "$ADDR" \
  -root "$ROOT" \
  -state-dir "$STATE_DIR" \
  -filter-config "$CONFIG_FILE" \
  -reconcile-interval "$RECONCILE_INTERVAL" \
  -watch-debounce "$WATCH_DEBOUNCE"
