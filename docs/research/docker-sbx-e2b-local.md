# Docker sbx 接入 e2b-local 调研

调研日期：2026-08-11。仅使用 Docker 官方文档、Docker 官方 GitHub 仓库和本地源码；未假定未公开实现细节。

## 结论

**当前不建议为 e2b-local 接入 Docker Sandboxes。** 每个 sbx sandbox 确实是一台本地 microVM，拥有独立 Linux kernel、文件系统、网络和 Docker Engine；但 Docker 把官方 `sandboxd` lifecycle 绑定到 Docker OAuth，且没有公开稳定的 daemon/VMM API。e2b-local 不应把 Docker 账号、entitlement 或组织治理变成运行前提。 [架构](https://docs.docker.com/ai/sandboxes/architecture/) [隔离模型](https://docs.docker.com/ai/sandboxes/security/isolation/) [发布仓库](https://github.com/docker/sbx-releases)

2026-08-11 的本机二进制分析发现了一个更低层、但**不受支持**的备选：sbx 的 `sandboxd` 启动了私有 Docker-compatible Engine，默认 runtime 是 `nerdbox`。因此 e2b-local 有机会复用现有 Docker backend、仅把 Docker socket 指向 sbx 的 Engine，从而让每个 E2B sandbox 是 microVM，而不需要嵌套 Docker 或重写为 CLI adapter。这个入口必须先作为一次性 spike 验证；它依赖未公开 socket、内部 `docker-next` 和私有 runtime，不能作为生产契约。

在用户已登录的环境中，`envd` 可作为 sbx 的**自定义 sandbox kit + 自定义 OCI template**入口进程，启动后由 `sbx ports` 将 `49983` 转发到 loopback。这样 E2B SDK 仍可直连 `envdURL`，并不需要让 envd 去控制 VM 内的 Docker daemon。Kits 和相关 CLI 参数仍标为 experimental，因此它最多是可选 adapter 的验证路线，不能承诺完整 backend。 [自定义 kit](https://docs.docker.com/ai/sandboxes/customize/kits/) [kit 规范](https://docs.docker.com/ai/sandboxes/customize/kit-reference/)

## 需求定位

这项需求的目标是让 e2b-local 可选择 Docker Sandboxes 作为 sandbox 运行环境，使创建出的 E2B sandbox 具备 Docker Sandboxes 提供的 microVM 隔离，而不改变调用方使用 E2B sandbox API 的方式。

- 它是可选 runtime，不替换现有 Docker、OrbStack 或 Apple Container 路径。
- 调用方预期继续获得可创建、暂停、恢复和删除的 sandbox，以及可访问的运行环境服务端点。
- Docker Sandboxes 的模板、资源与网络能力应在确认可映射后体现为一致的运行效果；不应承诺当前未验证的 volume、snapshot、metrics 或网络策略语义。
- private Engine、CLI adapter、kit、镜像构建和更底层虚拟化调用都是待验证的实现选项，不属于本需求的既定方案。

## 已证实的 sbx 形态

- 一个 sandbox 是独立 microVM；VM 内的 Docker Engine、镜像缓存、容器和 package state 独立且可跨 stop/restart 保存。`-docker` template 会在 VM 内运行 Docker Engine 和 privileged agent container；选择非 `-docker` base 可避免无意义的内层 Docker。 [架构](https://docs.docker.com/ai/sandboxes/architecture/) [模板](https://docs.docker.com/ai/sandboxes/customize/templates/)
- 当前可依赖的公开控制面是 CLI：`sbx create/run/ls/stop/rm/exec/cp/ports/template`。`ls --json` 和 `ports --json` 有文档化 JSON 输出；`exec` 明确匹配 `docker exec` 的 env/user/workdir/detach 语义。 [CLI create](https://docs.docker.com/reference/cli/sbx/create/) [CLI exec](https://docs.docker.com/reference/cli/sbx/exec/) [CLI ports](https://docs.docker.com/reference/cli/sbx/ports/) [使用说明](https://docs.docker.com/ai/sandboxes/usage/)
- workspace 是以原绝对路径 passthrough 进入 VM，附加 workspace 只能追加路径并可标记 `:ro`。文档没有公开任意 host-source -> arbitrary-container-target 的动态 bind-mount API。 [架构](https://docs.docker.com/ai/sandboxes/architecture/#workspace-mounting) [使用说明](https://docs.docker.com/ai/sandboxes/usage/#multiple-workspaces)
- 出网由宿主 HTTP/HTTPS proxy 执行域名/IP/端口 policy；默认拒绝、原始 TCP/UDP/ICMP 和私网/loopback 被阻断。这不是通用 iptables API，也不等价于 E2B 的任意 allow/deny CIDR 策略。 [隔离模型](https://docs.docker.com/ai/sandboxes/security/isolation/#network-isolation) [网络 policy](https://docs.docker.com/ai/sandboxes/governance/access-controls/network/)
- template 可从 registry 拉取或通过 `sbx template load` 导入 `docker image save` 生成的 tar；`sbx template save` 可把 sandbox 保存成 template。sbx 不共享宿主 Docker image store。 [模板](https://docs.docker.com/ai/sandboxes/customize/templates/)

## 与现有 runtime 的差异

现有 Docker backend 在 `CreateSandbox` 中通过 Docker Engine API 创建容器、把宿主 `envd-bin` bind-mount 到 `/usr/local/bin/envd`、覆盖 entrypoint、inspect 动态端口，并直接返回 `envdURL`。它还依赖 Docker `exec`、volume bind mounts、container commit 和 stats。 [runtime.go](/Users/yueqi/Coding/Agent/e2b-local/internal/backends/docker/runtime.go:220) [README](../../README.md)

sbx 的公开接口可对应部分生命周期，但不能直接替换 Docker client：

| e2b-local 能力 | 已证实的 sbx 映射 | 状态 |
| --- | --- | --- |
| create/list/delete | `create`, `ls --json`, `rm --force` | 可做 CLI wrapper |
| pause/resume | `stop`; `exec` 会启动 stopped sandbox | resume 的 agent/entrypoint 行为需冒烟验证 |
| envd endpoint | `ports --publish 49983` + `ports --json` | 可做，需验证 health/PTY streaming |
| 运行管理命令 | `exec`, `cp` | 仅辅助控制面；SDK 数据面仍直连 envd |
| CPU/memory | `create --cpus/--memory`，kit `resources` | 映射可做 |
| snapshot/template | `template save/load` | 语义、耗时、恢复和 metadata 尚未验证 |
| E2B managed volume | 无公开动态 mount 对应物 | **阻塞完整兼容**；`cp` 不能替代共享、持久、路径可控的 volume |
| logs/metrics/restart recovery | `ls --json` 有状态；无公开等价 Docker stats/logs API | 未知 |
| 任意 E2B template image | sbx template 必须满足其 sandbox image/kit 约束 | 需重新构建并验证，不能直接复用所有现有 E2B image |

另一个必须验证的兼容点：sbx custom image 要提供 UID 1000 的非 root `agent` 用户、passwordless sudo，并保留 proxy 环境；现有 Docker backend 以 root 运行 envd。应先确认 `sudo envd` 是否能保持 envd 的 command/filesystem/PTY 语义，以及 FUSE 是否仍需要额外设置。 [kit base-image 要求](https://docs.docker.com/ai/sandboxes/customize/kit-reference/#sandbox-block) [现有 root 配置](/Users/yueqi/Coding/Agent/e2b-local/internal/backends/docker/runtime.go:251)

## 2026-08-11 本机二进制与守护进程分析

范围：只读取安装包元数据、CLI help、已运行守护进程的日志/socket 元数据和 Docker `info`；没有经下列私有接口创建、修改或删除 sandbox/container。`sbx --help` 显示版本为 `v0.38.0 c022b14634c4bea846ca12870d1d5e97d5868b54`，Homebrew cask 的 SHA-256 是 `6fc2306598b8185228d920c1fd0fc09695d8022ad785a5b6655752f1145e7d3c`，因此本报告原先“尚未获得 CLI”的表述已失效。

### 已证实的本地分层

安装包并不是单个 CLI。`brew cat docker/tap/sbx` 显示它来自 Docker 的签名 DMG；其中包含：

- `bin/sbx`：Go binary，build info 的主模块为 `github.com/docker/sandboxes v0.38.0+dirty`；它链接 `containerd`、Docker `moby` client/API、`github.com/mdlayher/vsock` 和 Docker 的 `sailor` Go binding。
- `libexec/nerdbox-kernel-arm64`：`file` 识别为 Linux ARM64 boot kernel；`libexec/nerdbox-rootfs-arm64.erofs` 是 EROFS rootfs；`libexec/containerd-shim-nerdbox-v1` 的 build info 依赖 `github.com/containerd/nerdbox v0.2.1`、`docker-next` 和 `sailor`。
- `libexec/lib/libsailor.dylib`：由 Docker 签名，直接链接 macOS `Hypervisor.framework`。其导出 API 包括 VM CPU/memory/cmdline、virtio block/fs/net、vsock/hvsock、端口转发和 egress authorizer；二进制内含 Rust 源路径 `crates/hypervisor/src/macos/*`、`crates/net-stack/src/egress.rs` 与多个 virtio device crate。

这不是 Firecracker。对 macOS 版本，以上是**直接证据**表明 sbx 使用 Docker 的 Sailor VMM 和 Apple Hypervisor Framework；Firecracker 的实现或 API 没有出现在该运行链中。Linux sbx 的实现不应从此推断。

`github.com/containerd/nerdbox` 在 GitHub 可读取；`github.com/docker/sandboxes`、`github.com/docker/docker-next`、`github.com/docker/sailor` 和 `github.com/docker/go-sdk-sandboxes` 的公开 Git remote 均不可读取。故 Docker 发布的 CLI/文档是公开契约，Sailor、sandboxd 和 Docker Engine glue 的源码与 ABI 不是。

### 宿主控制面：证据与边界

已运行的 `sandboxd` 把状态放在 `~/Library/Application Support/com.docker.sandboxes/sandboxes/sandboxd`，并创建短路径 `~/.sbx/run/d` 指向该目录。日志和 socket metadata 给出下面的接口：

| 路径 | 本机证据 | 用途判断 | 支持级别 |
| --- | --- | --- | --- |
| `.../sandboxd/sandboxd.sock` | daemon log: `starting api server`；mode `0600` | sbx CLI 对守护进程的 HTTP over Unix socket；日志显示 `GET /daemon/health`、`GET /sandbox` | **私有**；官方只把 CLI/SSH proxy 当接口 |
| `~/.sbx/run/d/docker.sock` | daemon log: `starting docker server`；mode `0660`；`docker --host unix://... info` 返回 `Server: docker-next`, `Default Runtime: nerdbox`, EROFS storage，且当时无 image/container | Docker-compatible Engine，创建 OCI container 后由默认 `nerdbox` runtime 拉起 microVM | **私有但最接近现有 e2b-local backend 的入口** |
| `~/.sbx/run/d/containerd/containerd.sock.ttrpc` | daemon log: `serving...`; mode `0660` | containerd internal TTRPC | **不要调用**；需要 Docker 私有 plugin/protobuf 语义 |
| `libsailor.dylib` FFI | `nm -gU` 导出 `_sailor_config_*`、`_sailor_vm_*` 等函数 | VMM library | **不要调用**；须自行构造 block/fs/net/vsock、guest init 与 containerd shim protocol |

官方文档只承诺 CLI 的 create/exec/ports/lifecycle、kit 和 SSH proxy，不承诺任一 socket 或 VMM handle。[架构](https://docs.docker.com/ai/sandboxes/architecture/) [CLI 使用](https://docs.docker.com/ai/sandboxes/usage/) [SSH 集成](https://docs.docker.com/ai/sandboxes/integrations/)

当前 CLI lifecycle 请求被 sandboxd 拒绝为 `401`，原因是本机未登录 Docker；该授权限制必须在任何创建实验前显式解决。不能把 socket 可见性误读为无认证、稳定或可分发的 API。

### 对 e2b-local 的含义

**优先级 1：私有 Engine spike。** e2b-local 已经以 Docker Engine API 完成 image/container/create/start/exec/port/bind mount/stats。若将其测试实例指向 `unix://$HOME/.sbx/run/d/docker.sock`，且创建出来的 OCI container 确实使用 `nerdbox`，则它天然得到一个 microVM；envd 仍是 PID 1/entrypoint，不需要 kit、CLI subprocess 或 VM 内 Docker。这是“从底层借 sbx”的最小实验，而不是重做 VMM。

必须逐项实测后才可保留：

1. sbx Engine 是否接受 e2b-local 的 create spec，尤其是 host `envd-bin` bind mount、entrypoint、环境变量、`SYS_ADMIN` 与 `/dev/fuse`。
2. inspect/port/exec/attach 的 Docker API 是否足以保留 envd health、PTY 和 reconnect；持久 volume、commit/snapshot、stats 与 recovery 是否仍可用。
3. sbx 的 domain/proxy egress policy 是否会改变现有 `SandboxNetworkConfig` 的 CIDR 语义。官方说明它是宿主强制的 HTTP(S) domain policy，不支持承诺任意 TCP/UDP/CIDR 对等映射。[隔离模型](https://docs.docker.com/ai/sandboxes/security/isolation/) [kit network policy](https://docs.docker.com/ai/sandboxes/customize/kits/)

**静态分析阶段的停止线：** 未取得对私有 Engine 的显式授权前，不向私有 socket 发写请求。若 Docker API 兼容性在关键能力上失败，回退到上文的 kit + `sbx ports` 方案；若追求 VMM-level control，则应 fork/合作于公开的 `containerd/nerdbox`，而不是从闭源 `libsailor` FFI 重新拼 containerd、guest init 和 policy stack。

复现本节只读取证所用命令：

```sh
brew info docker/tap/sbx
sbx version
sbx --help; sbx daemon --help
go version -m /opt/homebrew/Caskroom/sbx/0.38.0/bin/sbx
go version -m /opt/homebrew/Caskroom/sbx/0.38.0/libexec/containerd-shim-nerdbox-v1
otool -L /opt/homebrew/Caskroom/sbx/0.38.0/libexec/lib/libsailor.dylib
nm -gU /opt/homebrew/Caskroom/sbx/0.38.0/libexec/lib/libsailor.dylib
tail -n 160 "$HOME/Library/Application Support/com.docker.sandboxes/sandboxes/sandboxd/daemon.log"
docker --host "unix://$HOME/.sbx/run/d/docker.sock" info
```

## 2026-08-11 私有 Engine 动态 spike

在用户明确授权后，使用 `docker --host unix://$HOME/.sbx/run/d/docker.sock` 做了两个可清理的短生命周期实验容器，随后删除容器和临时构建目录。保留 `alpine:3.22` 作为后续实验的 sbx 私有 image-store 依赖。

已证实：`docker pull alpine:3.22` 成功；标准 Docker `ContainerCreate`/`ContainerStart` 分配了独立 IPv4/IPv6 地址和 loopback 动态端口。daemon log 记录了 `io.containerd.nerdbox.v1`、`VM connection established`、`VM started`、Sailor 的 virtio block/net/vsock，以及 guest 的 Linux kernel boot。因此 "Docker API container" 确实会获得 microVM，而非宿主 namespace 容器。

但第一轮兼容性结果是否定的：

- `docker run alpine sh -c ...` 与显式 `--entrypoint /bin/sh` 都启动了 VM，却在约 8ms 后以 exit code 127 结束；inspect 中 `Path` 为空，只保留了 `Args`。现有 e2b-local 依赖的 `Config.Entrypoint`/`Cmd` 语义不可假设可用。
- `GET /containers/{id}/stats` 和 `docker export` 返回 `501 method not implemented`；`docker logs` 也无实现。
- Docker CLI 的 BuildKit bootstrap 被拒绝，错误为不支持 `HostConfig.RestartPolicy` 与 `HostConfig.Init`。因此现有 template build 路径也不能直接复用。

结论：私有 Engine 是一个真实的 microVM 承载层，但截至 v0.38.0 只适合作为**协议适配 research spike**，不是把 `docker.host` 改成该 socket 就能工作的 backend。若未来重新立项并在已登录环境对照官方 sbx，才应先用 `sbx create` 生成正常 sandbox，记录其 Docker inspect/config 与网络/端口状态；不要猜测 containerd TTRPC 或 Sailor FFI 的写协议。

## 可选验证：仅限已登录环境的 smoke spike

1. 构建公开的 `e2b-local/sbx-envd:dev`：基于 `docker/sandbox-templates:shell`，预烘焙匹配架构的 `envd`，保留 sbx 要求的 `agent` 用户与 proxy 环境；用 `sudo` 启动 envd。不要选择 `shell-docker`，除非 E2B template 明确需要给用户 Docker 能力。
2. 写一个 schema v2 `kind: sandbox` kit，指定该 image、envd entrypoint、`ports: [{container: 49983}]`，并按 E2B 请求生成最小 `environment.variables` 和 `permissions.network`。`setup.files`/`setup.startup` 是官方支持的文件和启动注入点。 [kit 规范](https://docs.docker.com/ai/sandboxes/customize/kit-reference/) [自定义 agent 教程](https://docs.docker.com/ai/sandboxes/customize/build-an-agent/)
3. 用 `sbx create --name e2b-spike --kit ./kit e2b-envd <workspace>` 创建，`sbx ports e2b-spike --publish 49983` 后从 `sbx ports --json` 取 loopback 端口。验证 `/health`，然后用真实 E2B SDK 覆盖 command、filesystem、PTY、pause/resume、cleanup。
4. 仅在该 smoke 成功后，实现小型 `sbx` subprocess adapter：所有 stdout 都优先走文档化 `--json`；create/exec 的非 JSON 输出和错误码需要记录为兼容契约。不要调用未公开 socket，也不要把 VM 内私有 Docker daemon 暴露给宿主。

**停止条件：** 若 envd 不能以 sbx kit entrypoint 稳定服务，或 E2B volume/metrics/snapshot 没有可接受的公开映射，则 sbx 只能作为实验性 envd runtime，不能宣称 E2B-compatible backend。无论 smoke 结果如何，当前项目也不应以它替换默认 runtime；只有 Docker 提供无登录的稳定本地 API 时才重新评估。

## 未知与本次边界

- Docker 未公开 sbx 的稳定 VMM contract、daemon socket、VM lifecycle protocol 或本地 Go/HTTP SDK；组织治理 API 只面向 governance policy，不是 sandbox lifecycle API。macOS release 的 `libsailor.dylib` 是本机可观察到的实现证据，不构成公开 API。 [governance API](https://docs.docker.com/ai/sandboxes/governance/reference/api/)
- 官方公开源码中，`docker/sbx-releases` 是二进制发行仓库；未找到 sbx runtime 源码。 [sbx-releases](https://github.com/docker/sbx-releases)

## 无需 Docker 登录的公开资源

这些资源足以继续研究 microVM runtime 的实现和契约，但调用 Docker 官方 `sandboxd` lifecycle 仍必须先完成 `sbx login`；Docker 的入门文档把登录列为运行 sandbox 的必需步骤。[登录要求](https://docs.docker.com/ai/sandboxes/get-started/#install-and-sign-in)

| 资源 | 无需登录可做什么 | 不能替代什么 |
| --- | --- | --- |
| [Docker sbx Releases v0.38.0](https://github.com/docker/sbx-releases/releases/tag/v0.38.0) | 下载 macOS/Linux/Windows 完整发行包及 SBOM、provenance，离线对照 `sbx`、`sandboxd`、`docker-next`、`nerdbox` shim 的二进制和版本组成。 | 发行包不是 sandboxd 协议或 Sailor API 的公开契约；不要由此推断私有 socket 的写请求。 |
| [公开模板说明](https://docs.docker.com/ai/sandboxes/customize/templates/) | 得到公开基镜像名 `docker/sandbox-templates:<variant>`、`sbx template load` 的 OCI tar 导入流程和非 `-docker` 模板语义；可先用本地 tar 排除 registry 拉取变量。 | 导入 template 不免除 sandbox lifecycle 的 Docker 登录，也不能证明匿名拉取在当前网络可用。 |
| [docker/sbx-kits-contrib](https://github.com/docker/sbx-kits-contrib) | 读取并复用公开 kit 样本，例如 [code-server](https://github.com/docker/sbx-kits-contrib/blob/main/code-server/spec.yaml) 的端口发布和 [codex-app-server](https://github.com/docker/sbx-kits-contrib/blob/main/codex-app-server/spec.yaml) 的 guest 服务启动，作为 envd kit 的结构参照。 | kit 是实验性且仍由官方 sbx lifecycle 执行；它不暴露 sandboxd 的本地 API。 |
| [containerd/nerdbox](https://github.com/containerd/nerdbox)（Apache-2.0） | 从公开源码完整构建独立 `containerd + nerdbox` 实验环境；其 [macOS 配置](https://github.com/containerd/nerdbox/blob/main/examples/macos/config.toml)、[资源/网络 annotations](https://github.com/containerd/nerdbox/blob/main/docs/vm-configuration.md)、[virtiofs bind mount](https://github.com/containerd/nerdbox/blob/main/docs/bind-mounts.md)、[vsock streaming](https://github.com/containerd/nerdbox/blob/main/docs/vsock-streaming.md) 和 [UDS forwarding](https://github.com/containerd/nerdbox/blob/main/docs/socket-forwarding.md) 已公开。它足以验证 envd 的 PID 1、端口、PTY/stdio、workspace mount 和 host/guest socket 边界。 | 这是 containerd 的独立实验性 runtime，不是 Docker sbx 的 `sandboxd` 实现；不会产出官方 sbx HTTP/Unix-socket 协议，也不保证行为、镜像格式或网络 policy 与 sbx 相同。 |

**项目建议：** 不应为 e2b-local 启动独立 Nerdbox backend。虽然它不依赖 OAuth，但会把项目从本地 runtime adapter 变成自维护的 microVM runtime 平台。将 sbx release 仅作为只读版本/二进制差分样本；以后在已登录环境中，可做上节 smoke 对照。只有项目明确要承担 runtime 的长期构建、发布、漏洞修复和跨平台支持时，才重新考虑独立 Nerdbox spike。

## `sbx login` 的远端边界

**sandbox 算力不在 Docker 云端。** Docker 的架构文档明确每个 sandbox 在本机拥有独立 microVM、Docker daemon、文件系统和网络；本机私有 Engine 的动态 spike 也已观察到 Linux guest boot 与 Sailor VM 启动。登录是本地 `sandboxd` 对 Docker 身份和服务接入的强制门槛，不是把 `create` 请求提交给远端 VM 调度器。[架构](https://docs.docker.com/ai/sandboxes/architecture/) [FAQ](https://docs.docker.com/ai/sandboxes/faq/)

已公开确认的远端交互分为：

- **Docker OAuth 身份和 Docker 基础设施认证。** `sbx login` 是 OAuth device flow；官方说明该身份用于将 sandbox 关联到用户、拉取镜像并访问 Docker 服务。文档称账户邮箱只用于认证，不用于营销。[入门](https://docs.docker.com/ai/sandboxes/get-started/#install-and-sign-in) [FAQ](https://docs.docker.com/ai/sandboxes/faq/#why-do-i-need-to-sign-in)
- **可选组织治理。** 付费组织功能可下发 network/filesystem/MCP policy、登录强制和 audit log。本机 daemon 在未登录时的日志已反复记录 `governor: background policy fetch` 因缺少账户 token 失败，因此账户会被用于该后台策略同步；不能据此推断所有本地 lifecycle 都必须由远端执行。[组织治理](https://docs.docker.com/ai/sandboxes/governance/) [FAQ](https://docs.docker.com/ai/sandboxes/faq/#can-i-enforce-sandbox-policies-across-my-organization)
- **诊断与遥测。** Docker 文档列出 command、结果和耗时；已登录时含 Docker username，并说明不读取 prompt 或代码。当前本机 daemon 也可见 `marlin/persist` event batch upload。`SBX_NO_TELEMETRY=1` 是公开的 telemetry opt-out，但这不会解除登录要求。[FAQ](https://docs.docker.com/ai/sandboxes/faq/#does-the-cli-collect-telemetry)

本机可替代的部分是 OCI template/image、microVM runtime、host proxy 和本地 policy：template 可以由本地 OCI tar 导入，Nerdbox 可以从公开源码构建，`sbx settings` 也公开了 local MCP gateway 与本地 policy 选项。不能用公开资源替代的是 Docker OAuth 身份、Docker Hub/服务认证，或 Docker 组织治理/audit 的 SaaS 语义。Docker 没有公开 sandboxd 与远端之间的完整请求模型、token claims 或 entitlement contract；在有登录的环境抓包/日志前，这些保持未知。

### CLI 的只读实现结论

`go version -m /opt/homebrew/Caskroom/sbx/0.38.0/bin/sbx` 显示该发行版主模块为 `github.com/docker/sandboxes`，并以 `-tags=cloud` 构建；依赖中包含私有的 `github.com/docker/sbx-cloud`、`github.com/docker/go-sdk-sandboxes/sandboxes`、`github.com/docker/governor-lib` 和 Docker auth client。已公开的 `sbx settings ls --json` 只有本地 kit、proxy、MCP gateway、template 和 telemetry 等开关，没有 offline、skip-auth 或 local-only lifecycle 模式。

这与本机行为一致：daemon health 与 settings 可匿名读取，但 `/docker/images` 在缺少账户 profile 时返回 `401`，后台 governor policy 同步也因没有 token 失败。它说明当前发行版将 OAuth 检查放在官方 daemon 的资源/lifecycle API 边界。由于 `sandboxd`、`sbx-cloud` 和相关 SDK 没有公开源码，反编译、修改检查或伪造认证响应不会建立稳定接口。可行的选项仍只有已认证的官方 sbx adapter，或不依赖 sbx daemon 的独立公开 runtime。
