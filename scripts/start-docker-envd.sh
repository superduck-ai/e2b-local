#!/usr/bin/env bash
set -euo pipefail

IMAGE="${E2B_DOCKER_IMAGE:-e2b-local/code-interpreter:latest}"
CONTAINER_NAME="${E2B_ENVD_CONTAINER:-e2b-envd}"
PLATFORM="${E2B_DOCKER_PLATFORM:-linux/amd64}"
HOST="${E2B_ENVD_HOST:-127.0.0.1}"
HOST_PORT="${E2B_ENVD_HOST_PORT:-49984}"
CONTAINER_PORT="${E2B_ENVD_CONTAINER_PORT:-49983}"
REPLACE="${E2B_ENVD_REPLACE:-1}"
PULL_POLICY="${E2B_DOCKER_PULL:-missing}"
HEALTH_TIMEOUT_SECONDS="${E2B_ENVD_HEALTH_TIMEOUT_SECONDS:-30}"
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

envd_arch="${E2B_ENVD_ARCH:-}"
if [[ -z "$envd_arch" ]]; then
  case "$PLATFORM" in
    *arm64* | *aarch64*) envd_arch="arm64" ;;
    *) envd_arch="amd64" ;;
  esac
fi

default_envd_bin="$ROOT_DIR/envd-bin/envd-linux-$envd_arch"
ENVD_BIN="${E2B_ENVD_BIN:-$default_envd_bin}"

resolve_path() {
  local value="$1"
  if [[ "$value" == /* ]]; then
    printf '%s\n' "$value"
    return
  fi
  if [[ -e "$ROOT_DIR/$value" ]]; then
    printf '%s/%s\n' "$ROOT_DIR" "$value"
    return
  fi
  printf '%s/%s\n' "$PWD" "$value"
}

ENVD_BIN="$(resolve_path "$ENVD_BIN")"

if ! command -v docker >/dev/null 2>&1; then
  echo "docker is required but was not found in PATH" >&2
  exit 1
fi

if [[ ! -f "$ENVD_BIN" ]]; then
  echo "envd binary not found: $ENVD_BIN" >&2
  exit 1
fi

if [[ ! -x "$ENVD_BIN" ]]; then
  echo "envd binary is not executable: $ENVD_BIN" >&2
  exit 1
fi

if [[ "$REPLACE" == "1" ]]; then
  docker rm -f "$CONTAINER_NAME" >/dev/null 2>&1 || true
elif docker inspect "$CONTAINER_NAME" >/dev/null 2>&1; then
  echo "container already exists: $CONTAINER_NAME" >&2
  echo "set E2B_ENVD_REPLACE=1 to recreate it" >&2
  exit 1
fi

docker_args=()
if [[ -n "${E2B_DOCKER_ARGS:-}" ]]; then
  read -r -a docker_args <<<"$E2B_DOCKER_ARGS"
fi

envd_args=(-isnotfc -port "$CONTAINER_PORT")
if [[ -n "${E2B_ENVD_ARGS:-}" ]]; then
  read -r -a envd_args <<<"$E2B_ENVD_ARGS"
fi

echo "starting envd container"
echo "  image:     $IMAGE"
echo "  container: $CONTAINER_NAME"
echo "  platform:  $PLATFORM"
echo "  envd:      $ENVD_BIN"
echo "  url:       http://$HOST:$HOST_PORT"

run_cmd=(
  docker run -d
  --name "$CONTAINER_NAME"
  --platform "$PLATFORM"
  --pull "$PULL_POLICY"
  --user root
  --init
  -p "$HOST:$HOST_PORT:$CONTAINER_PORT"
  -v "$ENVD_BIN:/usr/local/bin/envd:ro"
)

if [[ -n "${E2B_DOCKER_ARGS:-}" ]]; then
  run_cmd+=("${docker_args[@]}")
fi

run_cmd+=("$IMAGE" /usr/local/bin/envd "${envd_args[@]}")

"${run_cmd[@]}" >/dev/null

health_url="http://$HOST:$HOST_PORT/health"
deadline=$((SECONDS + HEALTH_TIMEOUT_SECONDS))
until curl -fsS "$health_url" >/dev/null 2>&1; do
  if (( SECONDS >= deadline )); then
    echo "envd did not become healthy within ${HEALTH_TIMEOUT_SECONDS}s" >&2
    echo "container logs:" >&2
    docker logs "$CONTAINER_NAME" >&2 || true
    exit 1
  fi
  sleep 1
done

echo "envd is healthy: $health_url"
echo
echo "Standalone envd URL:"
echo "  http://$HOST:$HOST_PORT"
echo
echo "Useful commands:"
echo "  docker logs -f $CONTAINER_NAME"
echo "  docker rm -f $CONTAINER_NAME"
