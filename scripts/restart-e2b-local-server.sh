#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CONFIG_PATH="${E2B_LOCAL_CONFIG:-$ROOT_DIR/config.yaml}"
PID_FILE="${E2B_LOCAL_PID_FILE:-$ROOT_DIR/e2b-local.pid}"
LOG_FILE="${E2B_LOCAL_LOG_FILE:-$ROOT_DIR/e2b-local.log}"
BIN_PATH="${E2B_LOCAL_BIN_PATH:-$ROOT_DIR/e2b-local}"
BUILD_PATH="${BIN_PATH}.new"
STOP_TIMEOUT_SECONDS="${E2B_LOCAL_STOP_TIMEOUT_SECONDS:-10}"
START_WAIT_SECONDS="${E2B_LOCAL_START_WAIT_SECONDS:-3}"

if ! command -v go >/dev/null 2>&1; then
  echo "go is required but was not found in PATH" >&2
  exit 1
fi

if [[ ! -f "$CONFIG_PATH" ]]; then
  echo "config file not found: $CONFIG_PATH" >&2
  exit 1
fi

cleanup_build() {
  rm -f "$BUILD_PATH"
}

stop_process_tree() {
  local pid="$1"
  local deadline=$((SECONDS + STOP_TIMEOUT_SECONDS))
  local children

  if ! kill -0 "$pid" >/dev/null 2>&1; then
    return 0
  fi

  children="$(pgrep -P "$pid" || true)"

  kill "$pid" >/dev/null 2>&1 || true
  if [[ -n "$children" ]]; then
    kill $children >/dev/null 2>&1 || true
  fi

  while kill -0 "$pid" >/dev/null 2>&1; do
    if (( SECONDS >= deadline )); then
      echo "gateway did not stop within ${STOP_TIMEOUT_SECONDS}s, forcing kill" >&2
      kill -9 "$pid" >/dev/null 2>&1 || true
      if [[ -n "$children" ]]; then
        kill -9 $children >/dev/null 2>&1 || true
      fi
      break
    fi
    sleep 1
  done
}

trap cleanup_build EXIT

echo "building gateway binary"
(
  cd "$ROOT_DIR"
  go build -o "$BUILD_PATH" ./cmd/e2b-local
)

mv "$BUILD_PATH" "$BIN_PATH"
chmod +x "$BIN_PATH"

if [[ -f "$PID_FILE" ]]; then
  old_pid="$(tr -d '[:space:]' < "$PID_FILE")"
  if [[ -n "$old_pid" ]] && kill -0 "$old_pid" >/dev/null 2>&1; then
    echo "stopping existing gateway pid=$old_pid"
    stop_process_tree "$old_pid"
  else
    echo "removing stale pid file"
  fi
  rm -f "$PID_FILE"
fi

mkdir -p "$(dirname "$LOG_FILE")"
touch "$LOG_FILE"

echo "starting gateway"
(
  cd "$ROOT_DIR"
  nohup "$BIN_PATH" serve --config "$CONFIG_PATH" >>"$LOG_FILE" 2>&1 &
  echo $! >"$PID_FILE"
)

new_pid="$(tr -d '[:space:]' < "$PID_FILE")"
deadline=$((SECONDS + START_WAIT_SECONDS))
while (( SECONDS < deadline )); do
  if ! kill -0 "$new_pid" >/dev/null 2>&1; then
    rm -f "$PID_FILE"
    echo "gateway failed to start; recent log output:" >&2
    tail -n 40 "$LOG_FILE" >&2 || true
    exit 1
  fi
  sleep 1
done

echo "gateway restarted"
echo "  pid:    $new_pid"
echo "  config: $CONFIG_PATH"
echo "  log:    $LOG_FILE"
