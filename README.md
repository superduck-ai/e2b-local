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

## Architecture

```mermaid
flowchart LR
  SDK["E2B SDK callers"] -->|"E2B API requests<br/>E2B_API_URL"| Gateway["e2b-local gateway<br/>cmd/e2b-local"]

  subgraph ControlPlane["Control plane"]
    Gateway --> HTTP["Gin HTTP server<br/>internal/gateway"]
    HTTP --> OpenAPI["Generated OpenAPI handlers<br/>internal/e2bapi"]
    OpenAPI --> Callbacks["Gateway callbacks<br/>sandbox, template, volume, metrics"]
    Callbacks --> Store["In-memory stores<br/>sandbox + management state"]
    Callbacks --> Registry["Runtime registry<br/>RegisterSandboxRuntimeFactory"]
  end

  subgraph RuntimeBackends["Runtime backends"]
    Registry --> Docker["Docker runtime<br/>internal/backends/docker"]
    Registry --> OrbStack["OrbStack runtime<br/>internal/backends/orbstack"]
  end

  subgraph DockerRuntime["Docker"]
    Docker --> Containers["Sandbox containers<br/>from local images"]
    Docker --> DockerVolumes["Docker named volumes"]
  end

  subgraph OrbRuntime["OrbStack"]
    OrbStack --> VMs["Cloned sandbox VMs"]
    OrbStack --> HostVolumes["Host volume directories<br/>orbstack.volume_host_path"]
  end

  EnvdBin["envd-bin<br/>linux amd64 / arm64"] --> Docker
  EnvdBin --> OrbStack
  Containers --> ContainerEnvd["envd inside container"]
  VMs --> VMEnvd["envd systemd service"]
  ContainerEnvd -. "direct envdURL" .-> SDK
  VMEnvd -. "direct envdURL" .-> SDK
```

The gateway handles E2B-compatible control-plane APIs such as sandbox lifecycle, templates, volumes, snapshots, metrics, and logs. After a sandbox is created, SDK calls for commands, filesystem, PTY, and streaming use the sandbox-specific `envdURL` returned by the runtime.

## Use From E2B SDKs

Point the E2B SDK at the local gateway instead of the hosted E2B API:

```bash
export E2B_API_URL="http://127.0.0.1:3000"
export E2B_API_KEY="local"
unset E2B_SANDBOX_URL
```

`E2B_API_KEY` is kept for SDK compatibility. The local gateway does not require a real hosted E2B key.

Template IDs are local runtime IDs:

- Docker runtime exposes tagged local Docker images as templates. For example, `e2b-local/code-interpreter:latest` is available as `code-interpreter`.
- OrbStack runtime exposes existing OrbStack machines, or configured template IDs, as templates.
- Call `ListTemplates` from the SDK, or `GET /templates`, to see the exact IDs available on the current machine.

JavaScript or TypeScript callers:

```ts
import { Sandbox, Volume } from 'e2b'

const template = 'code-interpreter'
const sandbox = await Sandbox.create(template)

try {
  const result = await sandbox.commands.run('echo "hello from e2b-local"')
  console.log(result.stdout)
} finally {
  await sandbox.kill()
}

const volume = await Volume.create('my-data')
const withVolume = await Sandbox.create(template, {
  volumeMounts: {
    '/mnt/data': volume,
  },
})
await withVolume.kill()
```

Go callers can use [superduck-ai/e2b-go-sdk](https://github.com/superduck-ai/e2b-go-sdk):

```go
package main

import (
	"context"
	"fmt"

	e2b "github.com/superduck-ai/e2b-go-sdk"
)

func main() {
	ctx := context.Background()
	template := "code-interpreter"

	sandbox, err := e2b.Create(ctx, template, nil)
	if err != nil {
		panic(err)
	}
	defer sandbox.Kill(ctx, nil)

	result, err := sandbox.Commands.Run(ctx, `echo "hello from e2b-local"`, nil)
	if err != nil {
		panic(err)
	}
	fmt.Println(result.(*e2b.CommandResult).Stdout)

	volume, err := e2b.CreateVolume(ctx, "my-data", nil)
	if err != nil {
		panic(err)
	}
	defer e2b.DestroyVolume(ctx, volume.VolumeID, nil)

	withVolume, err := e2b.Create(ctx, template, &e2b.SandboxOpts{
		VolumeMounts: map[string]any{
			"/mnt/data": volume,
		},
	})
	if err != nil {
		panic(err)
	}
	defer withVolume.Kill(ctx, nil)
}
```

Runtime notes for callers:

- Docker volumes are Docker native named volumes. The returned `volumeID` is the Docker volume name.
- OrbStack volumes are directories under `orbstack.volume_host_path` and are mounted into sandbox VMs on demand.
- The SDK receives a direct `envdURL` for each sandbox, so commands, filesystem, PTY, and streaming calls talk directly to the sandbox runtime after creation.

## Status

Implemented capabilities include:

- Gin-based HTTP server and middleware.
- Config-driven local runtime selection.
- Generated request/response DTOs, client types, and server interfaces from the E2B OpenAPI schema.
- E2B sandbox lifecycle APIs: create, list, get, kill, pause, resume, connect, and logs.
- Template, build, volume, snapshot, and metrics resource endpoints.
- Docker runtime for creating, pausing, resuming, deleting, restoring, logging, and collecting stats from real containers.
- OrbStack runtime for cloning/starting/stopping/deleting VMs through OrbStack sockets, installing `envd` as a systemd service, managing volume mounts, and creating snapshots without shelling out to the OrbStack CLI.

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
- The gateway stores non-sensitive runtime metadata in `e2b.local.*` container labels and restores running/paused sandboxes after process restart.
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
- Snapshots are created by cloning the VM through OrbStack's socket RPC.

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
