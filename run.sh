#!/usr/bin/env bash
# Build ai-squad and run it locally.
#
#   ./run.sh            open the desktop app (native window)
#   ./run.sh serve      run headless: scheduler + dashboard on --addr, no window
#   ./run.sh init       just build + initialize, don't launch anything
#
# Config/data live at ~/.ai-squad by default (override with AI_SQUAD_CONFIG
# or AI_SQUAD_DATA_DIR, same as the binary itself understands).

set -euo pipefail
cd "$(dirname "${BASH_SOURCE[0]}")"

BIN_DIR="bin"
BIN="$BIN_DIR/ai-squad"
MODE="${1:-desktop}"

mkdir -p "$BIN_DIR"
echo "==> building"
go build -o "$BIN" ./cmd/ai-squad

CONFIG_PATH="${AI_SQUAD_CONFIG:-${AI_SQUAD_DATA_DIR:+$AI_SQUAD_DATA_DIR/config.yaml}}"
CONFIG_PATH="${CONFIG_PATH:-$HOME/.ai-squad/config.yaml}"

if [ ! -f "$CONFIG_PATH" ]; then
  echo "==> no config at $CONFIG_PATH, running init"
  "$BIN" init
fi

case "$MODE" in
  desktop)
    echo "==> opening desktop app"
    exec "$BIN" desktop
    ;;
  serve)
    shift || true
    echo "==> starting headless server"
    exec "$BIN" serve "$@"
    ;;
  init)
    echo "==> done (init only)"
    ;;
  *)
    echo "usage: $0 [desktop|serve|init]" >&2
    exit 1
    ;;
esac
