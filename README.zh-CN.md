# e2b-local

[English README](README.md)

`e2b-local` 是一个用 Go 编写的本地 E2B 兼容 gateway。它接收 E2B SDK 的请求，并把 sandbox 跑在本地基础设施上：

- Docker Engine API 管理的容器
- OrbStack CLI 管理的 Linux VM
- macOS 上通过原生 XPC 服务管理的 Apple Container

HTTP 层尽量贴近 E2B OpenAPI schema；Docker、OrbStack 和 Apple Container 的具体行为放在独立 backend package 里。

## 快速开始

复制默认配置：

```bash
cp config.example.yaml config.yaml
```

默认 runtime 是 Docker。启动 gateway：

```bash
go run ./cmd/e2b-local --config config.yaml
```

通过 CLI 创建 volume：

```bash
go run ./cmd/e2b-local volume create --config config.yaml test-volume
```

该命令会复用当前配置的 runtime，返回和 `POST /volumes` 一致的 JSON，例如：

```json
{"volumeID":"test-volume","name":"test-volume","token":"compat-volume-token-test-volume"}
```

## 从 E2B SDK 调用

把 E2B SDK 指向本地 gateway，而不是托管版 E2B API：

```bash
export E2B_API_URL="http://127.0.0.1:3000"
export E2B_API_KEY="local"
unset E2B_SANDBOX_URL
```

`E2B_API_KEY` 只是为了兼容 SDK。local gateway 不需要真实的托管版 E2B key。

超时默认值与 E2B 的分层保持一致：原始
[`POST /sandboxes` API](https://e2b.dev/docs/api-reference/sandboxes/create-sandbox)
在省略 `timeout` 时默认 15 秒，而 E2B SDK 会在客户端应用 5 分钟默认值，并显式发送
`timeout: 300`。网关对缺少 `EndAt` 的记录统一使用 REST 默认值；正常 SDK 调用仍保持
SDK 的 5 分钟行为。

Template ID 来自本地 runtime：

- Docker runtime 会把本机已有 tag 的 Docker images 暴露为 template。例如 `e2b-local/code-interpreter:latest` 会暴露为 `code-interpreter`。
- OrbStack runtime 会把已有 OrbStack machine，或者配置里的 template ID，暴露为 template。
- Apple Container runtime 会把配置里的 template ID 映射到本机已经 pull 的 OCI image。
- 可以通过 SDK 的 `ListTemplates`，或者 `GET /templates`，查看当前机器上可用的准确 ID。

JavaScript / TypeScript 调用方：

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

Go 调用方可以使用 [superduck-ai/e2b-go-sdk](https://github.com/superduck-ai/e2b-go-sdk)：

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

调用方需要知道的 runtime 差异：

- Docker volume 是 `docker.volume_host_path` 下由 e2b-local 管理的本地目录，创建 sandbox 时会按请求路径 bind mount 进去。
- OrbStack volume 是 `orbstack.volume_host_path` 下的本地目录，会按需 mount 到 sandbox VM。
- Apple Container volume 使用 Apple Container 原生 named volume，并在创建 sandbox 时按请求挂载。
- SDK 在创建 sandbox 后会收到该 sandbox 的直连 `envdURL`，所以 commands、filesystem、PTY 和 streaming 调用会直接访问 sandbox runtime。

## 当前状态

已经实现的能力包括：

- 基于 Gin 的 HTTP server 和 middleware。
- 通过配置选择本地 runtime。
- 根据 E2B OpenAPI schema 生成 request/response DTO、client type 和 server interface。
- E2B sandbox 生命周期 API：create、list、get、kill、pause、resume、connect 和 logs。
- Template、build、volume、snapshot 和 metrics resource endpoints。
- Docker runtime：创建、暂停、恢复、删除、重启恢复、读取日志和采集容器 stats。
- OrbStack runtime：通过 OrbStack socket clone/start/stop/delete VM，把 `envd` 安装为 systemd service，管理 volume mount，并且无需 fork OrbStack CLI 创建 snapshot。
- Apple Container runtime：通过 `container-apiserver` XPC 创建、暂停、恢复、删除、重启恢复 sandbox，并管理 volume mount；sandbox 生命周期不 shell out 到 `container` CLI。

## 目录结构

- `cmd/e2b-local`：CLI 入口，用于启动 gateway 和执行辅助命令。
- `internal/gateway`：核心 gateway package，包括配置、路由、store、callback 和 runtime interface。
- `internal/backends/docker`：Docker runtime 实现。
- `internal/backends/orbstack`：OrbStack VM runtime 实现。
- `internal/backends/applecontainer`：Apple Container XPC runtime 实现。
- `internal/e2bapi`：生成的 OpenAPI client/server/DTO 代码。
- `envd-bin`：随仓库管理的 Linux `envd` 二进制，供 Docker、OrbStack 和 Apple Container 使用。
- `scripts`：本地 smoke test 和辅助脚本。
- `tests/sdk_integration`：可选的 Go/JS SDK 集成测试。

backend 通过 `RegisterSandboxRuntimeFactory` 注册，所以 runtime 逻辑不会混进 HTTP router。

## 依赖

- Go 1.25 或更新版本。
- Docker、OrbStack 或 Apple Container，取决于选择的 runtime。
- `envd-bin` 中对应架构的 Linux `envd` 二进制。

仓库当前管理：

- `envd-bin/envd-linux-amd64`
- `envd-bin/envd-linux-arm64`

Docker 会 inspect 选中镜像的架构，并把匹配的 `envd` 二进制 bind-mount 到每个 sandbox 容器的 `/usr/local/bin/envd`。OrbStack 会把配置的二进制复制到每个 sandbox VM，并在启动 systemd service 前安装为 `/usr/local/bin/envd`。Apple Container 会通过 XPC `copyIn` 把配置的二进制复制进每个 VM-backed container；如果选中 template 设置了 `prebaked_envd_path` 则跳过复制，然后作为 container process 启动 envd。

## 配置

完整本地配置见 `config.example.yaml`。Docker 专用样例见 `config.docker.yaml`，OrbStack 专用样例见 `config.orb.yaml`，Apple Container 专用样例见 `config.applecontainer.yaml`。

一个精简 Docker 配置：

```yaml
server:
  addr: "0.0.0.0:3000"

traffic:
  # 留空时 e2b-local 会在启动时自动探测出口网卡 IP。
  # advertised_host: "192.168.1.10"
  # 可选：强制使用宿主机网卡，比如 macOS 上被 VPN/TUN 改写默认路由时使用 en0。
  # interface: "en0"
  advertised_probe_addr: "8.8.8.8:80"

runtime:
  type: "docker"

docker:
  container_name_prefix: "e2b-envd-"
  published_ports: [5000]
  published_host_ip: "0.0.0.0"
  health_timeout_seconds: 30
  # 默认开启：向 sandbox 暴露 /dev/fuse 并授予 SYS_ADMIN。
  enable_fuse: true
```

重要字段：

- `runtime.type` 支持 `docker`、`orbstack` 和 `applecontainer`。
- `docker.host` 可以省略。gateway 会依次使用 `DOCKER_HOST`、当前用户的 OrbStack socket，以及 `unix:///var/run/docker.sock`。
- Docker templates 来自本机已有 tag 的 Docker images。gateway 不会自动 pull 镜像；请先在本机 pull、build 并打好 tag 再创建 sandbox。
- `traffic.advertised_host` 是端口查询接口返回给用户的 IP 或 host。留空时 gateway 会在启动时自动探测。`traffic.interface` 可以强制使用 `en0` 这类宿主机网卡；否则 macOS 回退到 UDP 探测，Linux 会先尝试 netlink 路由表，再回退到 UDP 探测。
- `docker.platform` 是可选 override。留空时 Docker 自己选择镜像平台，gateway 再 inspect 选中的镜像。
- `docker.envd_binary` 是可选 override。留空时 gateway 会按选中镜像架构自动选择 `envd-bin/envd-linux-amd64` 或 `envd-bin/envd-linux-arm64`；显式设置时支持相对配置文件路径。
- `docker.volume_host_path` 存放 Docker runtime 管理的本地 volume 目录，并支持 `~` 和相对配置文件路径。
- `docker.published_ports` 会把 `5000` 这类业务端口发布到每个 sandbox 的动态宿主机端口；`docker.published_host_ip` 默认是 `0.0.0.0`，便于内网访问。
- `docker.enable_fuse` 默认开启，sandbox 会获得 `/dev/fuse` 设备和 `SYS_ADMIN` capability，可在容器内挂载 FUSE 文件系统；设置为 `false` 可关闭该行为。该 capability 权限较高，只应在可信 sandbox 中使用。
- `orbstack.envd_binary` 可以写相对路径，gateway 会先解析再复制进 VM。
- `orbstack.volume_host_path` 存放 macOS 本地 volume 目录，并支持 `~` 和相对配置文件路径。
- `applecontainer.envd_binary` 可以写相对路径。除非选中 template 设置了 `prebaked_envd_path`，gateway 会先解析再复制进 Apple Container sandbox。
- `applecontainer.templates` 把本地 template ID 映射到 Apple Container image reference。gateway 不会自动 pull 镜像，请先用 `container image pull` 拉到本机。

### 对外 Host 探测

Docker 业务端口返回的 URL 使用 `traffic` 配置选出的 host。启动时解析顺序是：

1. 如果设置了 `traffic.advertised_host`，直接使用它。
2. 如果设置了 `traffic.interface`，必须能在宿主机上找到这个网卡，并且网卡有可用 IPv4；失败时启动失败，不会静默回退。
3. Linux 上会用 netlink 根据 `traffic.advertised_probe_addr` 查路由表，并使用路由选出的 source/interface IP。
4. 最后回退到 UDP probing，也就是用 `traffic.advertised_probe_addr` 做 UDP dial 来让内核选择本地地址。

macOS 上如果开启了 Surge、VPN 或 TUN，访问公网地址的路由可能会落到 `utun*`。内网访问场景建议设置 `traffic.interface: "en0"`，或者直接设置 `traffic.advertised_host` 为明确的局域网 IP。

### Docker 业务端口

`docker.published_ports` 会把每个 sandbox 里的同一个容器端口发布到不同的 Docker 动态宿主机端口。例如两个 sandbox 都监听容器内 `5000`，但对外可以分别是 `192.168.1.10:38123` 和 `192.168.1.10:39201`。

查询 published port：

```bash
curl http://127.0.0.1:3000/sandboxes/<sandboxID>/ports/5000
```

示例响应：

```json
{
  "containerPort": 5000,
  "host": "192.168.1.10",
  "hostPort": 38123,
  "url": "http://192.168.1.10:38123",
  "wsUrl": "ws://192.168.1.10:38123",
  "protocol": "tcp"
}
```

未发布的端口会返回 `404`，错误信息会提示配置 `docker.published_ports` 或 Dockerfile `EXPOSE`。

## Docker envd 调试脚本

如需单独启动一个 envd 容器做手动调试：

```bash
scripts/start-docker-envd.sh
```

默认值：

- 镜像：`e2b-local/code-interpreter:latest`
- standalone 调试容器 platform：`linux/amd64`
- envd 二进制：根据 helper platform 从 `envd-bin` 里选择
- 容器名：`e2b-envd`
- 外部 URL：`http://127.0.0.1:49984`

常用覆盖：

```bash
E2B_ENVD_HOST_PORT=49985 \
E2B_ENVD_CONTAINER=e2b-envd-2 \
scripts/start-docker-envd.sh
```

## Docker Runtime

当你希望 gateway 为每个 sandbox 创建一个容器时，使用 Docker runtime：

```yaml
runtime:
  type: "docker"
```

Docker runtime 下：

- `POST /sandboxes` 会创建容器。
- `DELETE /sandboxes/{sandboxID}` 会删除容器。
- `pause` 和 `connect` 对应 Docker pause/unpause。
- 使用 `autoPause: true` 创建的 sandbox 在 timeout 到期后会暂停而不是删除；后续显式 `connect` 会恢复 sandbox 并开始新的 timeout 窗口。timeout 策略会在 gateway 重启后保留；普通 SDK 请求或流量触发的自动恢复暂不支持。
- envd 在容器内固定监听 `49983`；Docker 会自动为每个 sandbox 分配独立的 localhost host port。
- Template 会从本机已有 tag 的 Docker images 解析，也可以在 `templateID` 中直接传完整 image reference；对应镜像必须已经存在本机。
- gateway 会把非敏感 runtime metadata 写入 `e2b.local.*` container label，进程重启后可恢复 running/paused sandbox。
- 自动选择或显式配置的 envd binary 会挂载为容器内 `/usr/local/bin/envd`。
- 配置 `docker.enable_fuse: true` 时，gateway 会向 sandbox 注入 `/dev/fuse` 和 `SYS_ADMIN`；镜像仍需自行安装 `fuse3`、`libfuse2` 或具体 FUSE 程序。
- 请求里的 E2B volume 是 `docker.volume_host_path` 下的本地目录，并会 bind mount 到 sandbox 容器里。
- Sandbox response 会返回 Docker runtime 分配的直连 `envdURL`。
- 通过 `docker.published_ports` 或 Dockerfile `EXPOSE` 声明的业务端口，可以用 `GET /sandboxes/{sandboxID}/ports/{port}` 查询；响应里包含 `url` 和 `wsUrl`，例如 `http://192.168.1.10:38123`。

示例 sandbox request：

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

当每个 sandbox 需要运行在完整 Linux VM 内时，使用 OrbStack runtime：

```yaml
runtime:
  type: "orbstack"

orbstack:
  machine_name_prefix: "e2b-sandbox-"
  envd_binary: "envd-bin/envd-linux-arm64"
  envd_port: 49983
  volume_host_path: "~/.e2b-local/volumes"
```

OrbStack runtime 下：

- 名称不以 `machine_name_prefix` 开头的现有 OrbStack machine 会作为 template 暴露。
- 创建 sandbox 时会 clone 选中的 template machine。
- gateway 会把 `envd_binary` 复制进 VM，并安装为 `/usr/local/bin/envd`。
- envd 会作为 VM 内的 systemd service 运行。
- sandbox envd URL 优先使用 VM IP 和固定 `envd_port`。
- `orbstack.isolated: true` 会阻止 sandbox VM 看到完整 macOS 文件系统。
- Volume 会通过 OrbStack selective mount 暴露，并在 VM 内 symlink 到请求路径。
- Snapshot 通过 OrbStack socket RPC clone VM 创建。

## Apple Container Runtime

在 Apple Silicon macOS 上，如果希望每个 sandbox 运行在 Apple Container 的 VM-backed container 中，可以使用 Apple Container runtime：

```yaml
runtime:
  type: "applecontainer"

applecontainer:
  container_name_prefix: "e2b-sandbox-"
  envd_binary: "envd-bin/envd-linux-arm64"
  envd_port: 49983
  templates:
    debian-bookworm-slim:
      image: "docker.io/library/debian:bookworm-slim"
      # 如果镜像内已经预烘焙 envd，可以设置这个路径。
      # prebaked_envd_path: "/usr/local/bin/envd"
```

系统前置条件：

```bash
export CGO_ENABLED=1
brew install container
brew services start container
container system status
container system kernel set --recommended
container image pull --platform linux/arm64 docker.io/library/debian:bookworm-slim
```

注意：

- Apple Container backend 需要 macOS 并启用 cgo，因为原生 XPC bridge 通过 cgo 编译。
- Apple Container 需要显示 `status running`；backend 会直接通过 XPC 访问 `com.apple.container.apiserver` 和 `com.apple.container.core.container-core-images`。
- 必须配置默认 kernel。如果 `container run` 报 `default kernel not configured`，执行 `container system kernel set --recommended`。
- Template image 必须先通过 Apple Container 拉到本机。只测试生命周期时可以用 Alpine 这类小镜像；E2B SDK command execution 需要镜像内有 `/bin/bash`，`debian:bookworm-slim` 已验证可用。
- 除非选中 template 设置了 `prebaked_envd_path`，backend 会从 `applecontainer.envd_binary` 复制 envd。
- envd 使用显式 localhost published port，因为 Apple Container 不支持 `hostPort: 0` 自动分配；如果 Apple Container 返回端口冲突，backend 会换端口重试。
- `pause` 映射为 Apple Container stop；`resume` 会 bootstrap 既有 container，并复用已持久化的 published port。
- Volume 使用 Apple Container named volume，并在创建 sandbox 时按请求挂载。

能力矩阵：

| 能力 | Apple Container backend |
| ---- | ----------------------- |
| Create/pause/resume/delete/restore | 支持 |
| Commands、filesystem、PTY、Git | 通过直连 `envdURL` 支持 |
| Volume create/list/get/delete | 通过 Apple Container named volume 支持 |
| Volume mounts | 创建 sandbox 时支持 |
| Snapshots | 不支持 |
| Runtime network updates | 不支持 |

## 测试

运行常规测试：

```bash
go test ./...
```

可选 JS SDK 集成测试：

```bash
go test -tags=js_sdk_integration -run TestJSSDKGatewaySmoke -count=1 -v
```

可选 Go SDK 集成测试：

```bash
go test -tags=go_sdk_integration -run 'TestGoSDKGatewayMVP|TestGoSDKGatewayFilesystemDirectEnvd|TestGoSDKGatewayVolumeLifecycle' -count=1 -v
```

可选 Apple Container 集成测试：

```bash
go test -tags=integration ./internal/backends/applecontainer/... -count=1 -v
go test -tags=go_sdk_integration ./tests/sdk_integration -run TestGoSDKGatewayAppleContainerDirectEnvd -count=1 -v
```

大部分 SDK 集成测试会通过 `LoadConfig("config.yaml")` 读取配置；当 Docker、envd、Node 或 SDK 依赖不可用时会自动跳过。`TestGoSDKGatewayAppleContainerDirectEnvd` 会构造自己的 Apple Container 配置，并在 `container-apiserver`、配置的 envd binary 或 template image 不可用时跳过。
