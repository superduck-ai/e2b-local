# e2b-local

[Chinese version](README.zh-CN.md)

`e2b-local` is a local E2B-compatible gateway written in Go. It accepts requests from E2B SDKs and runs sandboxes on local infrastructure:

- Docker containers through the Docker Engine API
- OrbStack Linux VMs through the OrbStack CLI

The HTTP layer follows the E2B OpenAPI schema where practical, while runtime-specific work lives behind Docker and OrbStack backend packages.

## Quick Start

Copy the default config:

```bash
cp config.example.yaml config.yaml
```

The default runtime is Docker. Start the gateway:

```bash
go run ./cmd/e2b-local --config config.yaml
```

Create a volume through the CLI:

```bash
go run ./cmd/e2b-local volume create --config config.yaml test-volume
```

The command reuses the configured runtime and returns the same JSON shape as `POST /volumes`, for example:

```json
{"volumeID":"test-volume","name":"test-volume","token":"compat-volume-token-test-volume"}
```

## Status

Implemented capabilities include:

- Gin-based HTTP server and middleware.
- Config-driven local runtime selection.
- Generated request/response DTOs, client types, and server interfaces from the E2B OpenAPI schema.
- E2B sandbox lifecycle APIs: create, list, get, kill, pause, resume, connect, and logs.
- Template, build, volume, snapshot, and metrics resource endpoints.
- Docker runtime for creating, pausing, resuming, deleting, restoring, logging, and collecting stats from real containers.
- OrbStack runtime for cloning/starting/stopping/deleting VMs, installing `envd` as a systemd service, managing volume mounts, and creating snapshots with `orb clone`.

## Repository Layout

- `cmd/e2b-local`: CLI entrypoint for serving the gateway and helper commands.
- `internal/gateway`: core gateway package with config, routes, store, callbacks, and runtime interfaces.
- `internal/backends/docker`: Docker runtime implementation.
- `internal/backends/orbstack`: OrbStack VM runtime implementation.
- `internal/e2bapi`: generated OpenAPI client/server/DTO code.
- `envd-bin`: checked-in Linux `envd` binaries used by Docker and OrbStack.
- `scripts`: local smoke-test and helper scripts.
- `tests/sdk_integration`: optional Go/JS SDK integration tests.

Backends register themselves through `RegisterSandboxRuntimeFactory`, so runtime logic stays outside the HTTP router.

## Requirements

- Go 1.24 or newer.
- Docker or OrbStack, depending on the selected runtime.
- A compatible Linux `envd` binary from `envd-bin`.

The repository tracks:

- `envd-bin/envd-linux-amd64`
- `envd-bin/envd-linux-arm64`

Docker inspects the selected image architecture and bind-mounts the matching `envd` binary into each sandbox container at `/usr/local/bin/envd`. OrbStack copies the configured binary into each sandbox VM and installs it as `/usr/local/bin/envd` before starting the systemd service.

## Configuration

See `config.example.yaml` for the full local config shape. Use `config.docker.yaml` for a Docker-focused example and `config.orb.yaml` for an OrbStack-focused example.

A compact Docker config:

```yaml
server:
  addr: "127.0.0.1:3000"

runtime:
  type: "docker"

docker:
  container_name_prefix: "e2b-envd-"
  health_timeout_seconds: 30
```

Important fields:

- `runtime.type` supports `docker` and `orbstack`.
- `docker.host` can be omitted. The gateway uses `DOCKER_HOST`, then the current user's OrbStack socket when present, then `unix:///var/run/docker.sock`.
- Docker templates are discovered from tagged local Docker images. The gateway never pulls images; pull, build, and tag them locally before creating sandboxes.
- `docker.platform` is optional. Empty means Docker chooses the image platform, then the gateway inspects the selected image.
- `docker.envd_binary` is optional. Empty means the gateway picks `envd-bin/envd-linux-amd64` or `envd-bin/envd-linux-arm64` from the selected image architecture. When set, it can be relative to the config file.
- `orbstack.envd_binary` can be relative to the config file. The gateway copies it into each VM before installing the service.
- `orbstack.volume_host_path` stores local volume directories on macOS and supports `~` and config-relative paths.

## Docker envd Helper

To run a standalone envd container for manual debugging:

```bash
scripts/start-docker-envd.sh
```

Defaults:

- image: `e2b-local/code-interpreter:latest`
- standalone container platform: `linux/amd64`
- envd binary: selected from `envd-bin` based on the helper platform
- container name: `e2b-envd`
- external URL: `http://127.0.0.1:49984`

Useful overrides:

```bash
E2B_ENVD_HOST_PORT=49985 \
E2B_ENVD_CONTAINER=e2b-envd-2 \
scripts/start-docker-envd.sh
```

## Docker Runtime

Use Docker runtime when you want the gateway to create one container per sandbox:

```yaml
runtime:
  type: "docker"
```

In Docker runtime:

- `POST /sandboxes` creates a container.
- `DELETE /sandboxes/{sandboxID}` removes the container.
- `pause` and `connect` map to Docker pause/unpause.
- envd listens on container port `49983`; Docker publishes a separate localhost host port for each sandbox automatically.
- Templates are resolved from local tagged Docker images, or from a full image reference passed as `templateID`. The image must already exist locally.
- The gateway stores non-sensitive runtime metadata in `e2b.gateway.*` container labels and restores running/paused sandboxes after process restart.
- The selected envd binary is mounted at `/usr/local/bin/envd`.
- Requested E2B volumes use Docker native named volumes.
- Sandbox responses return the direct runtime `envdURL` assigned by Docker.

Example sandbox request:

```json
{
  "templateID": "code-interpreter",
  "volumeMounts": [
    {
      "name": "my-data",
      "path": "/mnt/data"
    }
  ]
}
```

## OrbStack Runtime

Use OrbStack runtime when each sandbox should run inside a full Linux VM:

```yaml
runtime:
  type: "orbstack"

orbstack:
  orb_binary: "/usr/local/bin/orb"
  machine_name_prefix: "e2b-sandbox-"
  envd_binary: "envd-bin/envd-linux-arm64"
  envd_port: 49983
  volume_host_path: "~/.e2b-local/volumes"
```

In OrbStack runtime:

- Existing OrbStack machines whose names do not start with `machine_name_prefix` are exposed as templates.
- Sandbox creation clones the selected template machine.
- The gateway copies `envd_binary` into the VM and installs `/usr/local/bin/envd`.
- envd runs as a systemd service inside the VM.
- Sandbox envd URLs prefer the VM IP and fixed `envd_port`.
- `orbstack.isolated: true` prevents sandbox VMs from seeing the full macOS filesystem.
- Volumes are exposed through OrbStack selective mounts and symlinked to the requested paths inside the VM.
- Snapshots are created with `orb clone`.

## SDK Usage

For JS SDK smoke tests:

```bash
export E2B_API_URL="http://127.0.0.1:3000"
export E2B_API_KEY="local"
node scripts/js-sdk-smoke.mjs
```

If you want to use a local JS SDK build:

```bash
E2B_API_URL="http://127.0.0.1:3000" \
E2B_API_KEY="local" \
E2B_JS_SDK_IMPORT="/absolute/path/to/js-sdk/dist/index.mjs" \
node scripts/js-sdk-smoke.mjs
```

Minimal SDK acceptance scenario:

```ts
import { Sandbox } from 'e2b'

const sandbox = await Sandbox.create()

const result = await sandbox.commands.run('echo "hello"')
console.log(result.stdout)

await sandbox.kill()
```

Expected behavior:

- `Sandbox.create()` returns a sandbox.
- `sandbox.sandboxId` is generated by the gateway.
- `sandbox.commands.run(...)` succeeds.
- `result.exitCode === 0`.
- `result.stdout` contains `hello`.
- `sandbox.kill()` succeeds.

## Tests

Run regular tests:

```bash
go test ./...
```

Optional JS SDK integration test:

```bash
go test -tags=js_sdk_integration -run TestJSSDKGatewaySmoke -count=1 -v
```

Optional Go SDK integration tests:

```bash
go test -tags=go_sdk_integration -run 'TestGoSDKGatewayMVP|TestGoSDKGatewayFilesystemDirectEnvd|TestGoSDKGatewayVolumeLifecycle' -count=1 -v
```

The SDK integration tests read `config.yaml` through `LoadConfig("config.yaml")` and skip when Docker, envd, Node, or SDK dependencies are unavailable.

## OpenAPI Regeneration

Generated code lives in `internal/e2bapi/api.gen.go`, and the checked-in schema lives in `internal/e2bapi/openapi.json`.

Regenerate it with:

```bash
go generate ./internal/e2bapi
```

After schema changes, update explicit handlers in `internal/gateway/gateway_api.go` or the corresponding `GatewayCallbacks`.

## Current Limitations

The following areas still need more work or validation:

- multi-tenant isolation
- database-backed persistence and HA
- file synchronization
- more Docker and OrbStack lifecycle edge-case coverage
- more end-to-end WebSocket and SSE envd compatibility coverage
