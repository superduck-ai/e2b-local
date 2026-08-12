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
SERVER_PORT="${E2B_LOCAL_PORT:-}"

if ! command -v go >/dev/null 2>&1; then
  echo "go is required but was not found in PATH" >&2
  exit 1
fi

if ! command -v lsof >/dev/null 2>&1; then
  echo "lsof is required but was not found in PATH" >&2
  exit 1
fi

if [[ ! -f "$CONFIG_PATH" ]]; then
  echo "config file not found: $CONFIG_PATH" >&2
  exit 1
fi

read_server_addr() {
  awk '
    /^[[:space:]]*#/ {
      next
    }
    /^[[:space:]]*server:[[:space:]]*(#.*)?$/ {
      in_server = 1
      next
    }
    in_server && /^[^[:space:]]/ {
      exit
    }
    in_server && /^[[:space:]]+addr:[[:space:]]*/ {
      value = $0
      sub(/^[[:space:]]+addr:[[:space:]]*/, "", value)
      sub(/[[:space:]]+#.*$/, "", value)
      gsub(/^[[:space:]]+|[[:space:]]+$/, "", value)

      quote = substr(value, 1, 1)
      if ((quote == "\"" || quote == "\047") && substr(value, length(value), 1) == quote) {
        value = substr(value, 2, length(value) - 2)
      }

      print value
      exit
    }
  ' "$CONFIG_PATH"
}

resolve_target_path() {
  local path="$1"
  local directory

  directory="$(cd "$(dirname "$path")" && pwd -P)"
  printf '%s/%s\n' "$directory" "$(basename "$path")"
}

process_executable_path() {
  local pid="$1"
  local path=""

  if [[ -L "/proc/$pid/exe" ]]; then
    path="$(readlink "/proc/$pid/exe" 2>/dev/null || true)"
  fi
  if [[ -z "$path" ]]; then
    path="$(lsof -a -p "$pid" -d txt -Fn 2>/dev/null | awk '/^n/ { sub(/^n/, ""); print; exit }')"
  fi

  # Linux appends " (deleted)" after the binary is replaced while it is running.
  path="${path% (deleted)}"
  printf '%s\n' "$path"
}

listening_pids() {
  lsof -nP -iTCP:"$SERVER_PORT" -sTCP:LISTEN -t 2>/dev/null | sort -u || true
}

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

if [[ -z "$SERVER_PORT" ]]; then
  server_addr="$(read_server_addr)"
  if [[ -z "$server_addr" || "$server_addr" != *:* ]]; then
    echo "server.addr with a TCP port was not found in config: $CONFIG_PATH" >&2
    exit 1
  fi
  SERVER_PORT="${server_addr##*:}"
fi
if [[ ! "$SERVER_PORT" =~ ^[0-9]+$ ]] ||
  (( 10#$SERVER_PORT < 1 || 10#$SERVER_PORT > 65535 )); then
  echo "invalid gateway port: $SERVER_PORT" >&2
  exit 1
fi

EXPECTED_BIN_PATH="$(resolve_target_path "$BIN_PATH")"
BIN_PATH="$EXPECTED_BIN_PATH"
BUILD_PATH="${BIN_PATH}.new"

echo "building gateway binary"
(
  cd "$ROOT_DIR"
  go build -o "$BUILD_PATH" ./cmd/e2b-local
)

old_pids="$(listening_pids)"
matching_old_pids=""
if [[ -n "$old_pids" ]]; then
  # A matching port alone is not sufficient because unrelated applications can
  # listen on the same port through another local address. Select only processes
  # whose executable path also matches the managed gateway binary.
  for old_pid in $old_pids; do
    old_executable="$(process_executable_path "$old_pid")"
    if [[ "$old_executable" == "$EXPECTED_BIN_PATH" ]]; then
      matching_old_pids+="${matching_old_pids:+ }$old_pid"
    else
      echo "ignoring unrelated listener pid=$old_pid port=$SERVER_PORT executable=${old_executable:-unknown}"
    fi
  done
fi

if [[ -n "$matching_old_pids" ]]; then
  for old_pid in $matching_old_pids; do
    echo "stopping existing gateway pid=$old_pid port=$SERVER_PORT executable=$EXPECTED_BIN_PATH"
    stop_process_tree "$old_pid"
  done
else
  echo "no existing gateway from $EXPECTED_BIN_PATH is listening on port $SERVER_PORT"
fi

rm -f "$PID_FILE"
mv "$BUILD_PATH" "$BIN_PATH"
chmod +x "$BIN_PATH"

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
started=false
while (( SECONDS < deadline )); do
  if ! kill -0 "$new_pid" >/dev/null 2>&1; then
    rm -f "$PID_FILE"
    echo "gateway failed to start; recent log output:" >&2
    tail -n 40 "$LOG_FILE" >&2 || true
    exit 1
  fi

  for listening_pid in $(listening_pids); do
    if [[ "$listening_pid" == "$new_pid" ]] &&
      [[ "$(process_executable_path "$listening_pid")" == "$EXPECTED_BIN_PATH" ]]; then
      started=true
      break
    fi
  done
  if [[ "$started" == true ]]; then
    break
  fi
  sleep 1
done

if [[ "$started" != true ]]; then
  echo "gateway did not listen on port $SERVER_PORT from $EXPECTED_BIN_PATH within ${START_WAIT_SECONDS}s" >&2
  stop_process_tree "$new_pid"
  rm -f "$PID_FILE"
  echo "recent log output:" >&2
  tail -n 40 "$LOG_FILE" >&2 || true
  exit 1
fi

echo "gateway restarted"
echo "  pid:    $new_pid"
echo "  port:   $SERVER_PORT"
echo "  binary: $EXPECTED_BIN_PATH"
echo "  config: $CONFIG_PATH"
echo "  log:    $LOG_FILE"
