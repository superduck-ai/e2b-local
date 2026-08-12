#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
IMAGE="${E2B_SBX_IMAGE:-e2b-local/sbx-envd:dev}"
SBX_DOCKER_HOST="${E2B_SBX_DOCKER_HOST:-unix://${HOME}/.sbx/run/d/docker.sock}"
started_at=${SECONDS}

case "$(uname -m)" in
  arm64|aarch64) default_arch="arm64" ;;
  x86_64|amd64) default_arch="amd64" ;;
  *)
    echo "unsupported host architecture: $(uname -m)" >&2
    exit 1
    ;;
esac
PLATFORM="${E2B_SBX_PLATFORM:-linux/${default_arch}}"
ENVD_BINARY="${ROOT_DIR}/envd-bin/envd-linux-${default_arch}"

if [[ ! -x "${ENVD_BINARY}" ]]; then
	echo "missing executable envd artifact for ${default_arch}: ${ENVD_BINARY}" >&2
	exit 1
fi

echo "Building reusable SBX base ${IMAGE} from the current e2b-local source for ${PLATFORM}"
docker build --pull --no-cache --platform "${PLATFORM}" \
	--tag "${IMAGE}" \
	--file "${ROOT_DIR}/internal/backends/sbx/image/Dockerfile" \
	"${ROOT_DIR}"

echo "Importing ${IMAGE} into sbx Docker at ${SBX_DOCKER_HOST}"
docker save "${IMAGE}" | docker --host "${SBX_DOCKER_HOST}" image load

echo "Built and imported ${IMAGE} in $((SECONDS - started_at))s"
