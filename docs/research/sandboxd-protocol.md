# sandboxd 私有协议完整记录（v0.38.0 / docker-next v0.28.0）

> 逆向日期：2026-08-11。来源：静态符号分析（debug/gosym，152,662 函数）+ 动态探测（HTTP over unix socket）+ 官方 CLI 请求序列捕获（daemon.log HTTP request 记录）+ 官方 sandbox 配置 inspect。
> 目标：e2b-local 对接 sandboxd / docker.sock 的完整协议契约。

---

## 1. 总览

sandboxd 暴露两个 Unix socket（同一 daemon，同一 containerd）：

| Socket | 路径 | 认证 | 协议 |
| --- | --- | --- | --- |
| **sandboxd.sock** | `~/.sbx/run/d/sandboxd.sock`（软链到 `~/Library/Application Support/com.docker.sandboxes/sandboxes/sandboxd/sandboxd.sock`） | **必须**（除 3 个只读 GET） | 私有 HTTP API（oapi-codegen 风格） |
| **docker.sock** | `~/.sbx/run/d/docker.sock` | **无** | 标准 Docker Engine API（docker-next 实现，仅部分端点） |

认证：`Authorization` 头携带 OAuth JWT（Docker 登录后 secretskit 存储）。认证中间件 `authmiddleware.VerifyAuthenticated`，豁免路径（`authSkipsForPath`）仅：
```
GET /daemon/health
GET /daemon/info
GET /daemon/settings（+ /daemon/settings/{key}）
```
其余全部 401（`no valid user session found, please sign in to Docker to proceed`）。

User-Agent：`sbx-cli/v0.38.0 (darwin/arm64)`。

---

## 2. sandboxd 私有 HTTP API（完整）

### 2.1 系统

| 方法 | 路径 | 认证 | 请求 | 响应 |
| --- | --- | --- | --- | --- |
| GET | `/daemon/health` | 无 | - | `{"api_version":"0.26.0","release":true,"revision":"c022b...","status":"healthy","version":"v0.38.0"}` |
| GET | `/daemon/info` | 无 | - | `{"api_socket":"...","docker_socket":"..."}` |
| GET | `/daemon/settings` | 无 | - | `{"settings":[{18 项设置}]}` |
| GET | `/daemon/settings/{key}` | 无 | - | 单项设置对象 |
| PUT | `/daemon/settings/{key}` | - | JSON | **405**（设计只读） |
| GET | `/daemon/diagnostics` | ✅ | - | **105KB 诊断**：info（Version=github.com/docker/docker-next v0.28.0、Host、State、Process、ContainerdConfig、Goroutines×90）+ socket_paths |
| GET | `/daemon/debug-state` | ✅ | - | 404（端点已移除/改名） |

18 项设置（GET /daemon/settings）：
```
clipboard.imagePaste / kit.allowLocalKits / kit.allowedSources
mcp.forceLocalGateway / no_proxy / no_proxy.daemon / no_proxy.sandbox
platform.allowExperimentalFeatures / platform.images.useDHI / proxy
proxy.daemon / proxy.integratedAuth / proxy.sandbox
ssh.autoCreate / ssh.defaultAgent / ssh.defaultTemplate / ssh.workspaceRoot
tls.allowNegativeSerial
```

### 2.2 Sandbox 生命周期

**请求序列**（官方 `sbx create` 捕获，daemon.log HTTP request）：
```
1. GET  /daemon/health                                   健康检查
2. GET  /policy/network/setup                            策略检查
3. GET  /sandbox/{name} → 404                            查重（存在则复用）
4. POST /docker/images/create?fromImage=...&tag=...      拉镜像（req=0，streaming）
5. GET  /docker/images/inspect?name=...                  镜像检查
6. POST /sandbox/{name}/mcp/gateway                      可选 MCP 网关（req=29B）
7. POST /sandbox                                         **创建**（req=196B）
8. GET  /sandbox/{name} → 200 (res=538B)                 验证
9. POST /sandbox/{name}/start                            **启动**
```

| 方法 | 路径 | 认证 | 请求 | 响应 |
| --- | --- | --- | --- | --- |
| GET | `/sandbox` | ✅ | - | `{"sandboxes":[{Sandbox 对象}]}`（注：裸 GET 返回数组 `[]`） |
| POST | `/sandbox` | ✅ | CreateSandboxRequest | 201 `{"agent","name","status","workspace"}`（**创建即 running**） |
| GET | `/sandbox/{name}` | ✅ | - | Sandbox 对象（见下） |
| POST | `/sandbox/{name}/start` | ✅ | - | 200 Sandbox 对象 |
| POST | `/sandbox/{name}/stop` | ✅ | - | 200 |
| POST | `/sandbox/{name}/exec` | ✅ | `{"cmd":["..."]}` | 400 校验：`cmd is required and must contain at least one element` |
| POST | `/sandbox/{name}/save` | ✅ | `{"tag":"..."}` | 400 校验：`tag is required` |
| POST | `/sandbox/{name}/ssh` | ✅ | Connection: Upgrade | 400 校验：`missing "Connection: Upgrade" header` |
| GET | `/sandbox/{name}/files?path=...` | ✅ | query `path` | 文件读取 |
| POST | `/sandbox/{name}/mcp/gateway` | ✅ | 29B JSON | 200（MCP 网关） |
| DELETE | `/sandbox/{name}` | ✅ | - | 删除 |

**Sandbox 对象**（GET /sandbox/{name} 完整结构）：
```json
{
  "agent": "shell",
  "created_at": "2026-08-11T18:07:09+08:00",
  "id": "ddc12427-947e-417d-bb0c-118aab2e04bf",
  "labels": {
    "com.docker.sandbox.agent": "shell",
    "com.docker.sandbox.kits": "[]",
    "com.docker.sandbox.name": "e2b-protocol-test",
    "com.docker.sandbox.usesWorktree": "false",
    "com.docker.sandbox.workingDirectory": "/tmp",
    "com.docker.sdk": "true",
    "com.docker.sdk.client": "0.1.0-alpha013",
    "com.docker.sdk.container": "0.1.0-alpha016",
    "com.docker.sdk.lang": "go",
    "docker/sandbox": "true"
  },
  "name": "e2b-protocol-test",
  "status": "running",
  "workspace": "/tmp"
}
```

**CreateSandboxRequest 字段**（错误反推 + CLI 请求体，2026-08-11 补测）：
- `agent`（必填，`"agent is required"`）
- `workspace`（必填，`"workspace is required"`）
- `image`（可选，默认 agent 模板）
- `name`（可选，默认 `<agent>-<workspace-basename>`）
- `memory`（可选，**字符串**格式，`expected=string got=number`，如 `"2GB"`）
- `cpus`（可选，数字，接受）
- `workspaces`（可选，复数数组，接受）
- `kits`（可选，**JSON 字符串**，`expected=string got=object`）
- `env`（可选，对象，接受）
- 其他校验（`validateCreateRequestScope`/`validateMemoryCPUs`/`validateAdditionalWorkspaces`/`validateSecretsScope`）

### 2.3 策略 / 网络

| 方法 | 路径 | 认证 | 请求 | 响应 |
| --- | --- | --- | --- | --- |
| GET | `/policy/network/setup` | ✅ | - | 状态（CLI 用） |
| GET | `/policy/network/rules?type=all` | ✅ | query | `{"rules":[{Rule 对象}]}` |
| GET | `/policy/network/profiles` | ✅ | - | `{"profiles":[]}` |
| GET | `/policy/network/log` | ✅ | - | `{"blocked_hosts":[],"allowed_hosts":[]}` |
| POST | `/policy/network/rules` | ✅ | Rule JSON | 增删规则（CLI `sbx policy allow/deny`） |
| POST | `/policy/refresh` | ✅ | - | 刷新策略 |

**Rule 对象**：
```json
{
  "applies_to": "all",
  "decision": "allow",
  "editable": true,
  "id": "default-allow-all",
  "layer": "local",
  "name": "default-allow-all",
  "origin": "local",
  "policy_id": "local-policy",
  "resource_type": "network",
  "resources": ["**"],
  "scope": "global",
  "status": "active"
}
```

### 2.4 MCP

| 方法 | 路径 | 认证 | 响应 |
| --- | --- | --- | --- |
| GET | `/mcp/gateway-mode` | ✅ | `{"decision":"local","gateway_url":"none","reason":"not entitled to the SaaS gateway → local"}` |
| POST | `/mcp/gateway/servers` | ✅ | 管理 MCP 服务器 |
| GET | `/mcp/registration/check` | ✅ | 405（POST 用） |

### 2.5 事件流

| 方法 | 路径 | 认证 | 响应 |
| --- | --- | --- | --- |
| GET | `/events` | ✅ | **NDJSON**（`application/x-ndjson`），事件：`{"action":"complete","id":"<uuid>","timestamp":"...","type":"sync"}` |

### 2.6 其他

| 方法 | 路径 | 认证 | 说明 |
| --- | --- | --- | --- |
| POST | `/oauth/reload` | ✅ | 404（端点已移除；符号表有 `ReloadOAuthService`） |
| GET | `/docker/images` | ✅ | 镜像列表（私有 Engine 代理） |
| POST | `/docker/images/create?fromImage=...&tag=...` | ✅ | 拉镜像（streaming） |
| GET | `/docker/images/inspect?name=...` | ✅ | 镜像检查 |

---

## 3. docker.sock（Docker Engine API，免认证）—— 实测可用端点

### 3.1 可用（标准 Docker API）

| 方法 | 路径 | 结果 |
| --- | --- | --- |
| GET | `/_ping` | 200 `OK` |
| GET | `/version` | 200 `{"Platform":{"Name":"docker-next"},"Components":[{"Name":"nerdbox","Version":"0.0.1"}]}` |
| GET | `/info` | 200（Driver=erofs, Runtime=nerdbox） |
| GET | `/containers/json` | 200 `[]` |
| GET | `/images/json` | 200（镜像列表） |
| POST | `/images/create?fromImage=...` | 200 streaming（pull） |
| POST | `/containers/create` | **201**（返回 Id + Warnings） |
| POST | `/containers/{id}/start` | 204 |
| POST | `/containers/{id}/stop` | 204 |
| DELETE | `/containers/{id}?force=1` | 204 |
| GET | `/containers/{id}/json` | 200（完整 inspect） |
| POST | `/containers/{id}/exec` | 201（exec Id） |
| POST | `/exec/{id}/start` | 200 + **hijack raw-stream**（PTY） |
| POST | `/exec/{id}/resize?h=..&w=..` | 200（query 参数，运行中才可） |
| GET | `/exec/{id}/json` | 200（Running/ExitCode） |
| POST | `/containers/{id}/attach` | **101 UPGRADED**（multiplexed-stream） |
| POST | `/containers/{id}/wait` | 200 |
| GET | `/networks` | 200（Driver=gvisor） |
| GET | `/volumes` | 200 `{"Volumes":[],"Warnings":[]}` |
| POST | `/commit?container=...&repo=...&tag=...` | 409（运行中）→ stop 后可 commit |
| POST | `/images/load` | 200（需真实 OCI tar） |

### 3.2 不可用（501 / 404）

| 方法 | 路径 | 结果 |
| --- | --- | --- |
| GET | `/containers/{id}/logs` | **501 not implemented** |
| GET | `/containers/{id}/stats` | **501 not implemented** |
| GET | `/containers/{id}/top` | **501 not implemented** |
| GET | `/containers/{id}/changes` | **501 not implemented** |
| GET | `/containers/{id}/export` | **501 not implemented** |
| POST | `/containers/{id}/restart` | **501 not implemented** |
| POST | `/build` | 404 |
| GET | `/_version` | 404 |

### 3.3 容器配置语义（docker-next 特有）

- `Path` 为空，`Cmd`/`Args` 语义是 **exec 风格**（`{"Cmd":["/bin/sh","-c","sleep 300"]}` → inspect 显示 `Path:"" Args:["/bin/sh","-c","sleep 300"]`）
- entrypoint 可用：`"Entrypoint":["/bin/sh"],"Cmd":["-c","..."]` → 主进程正常
- **`Cmd` 为单命令时 VM 8ms 后 exit 127**（`/bin/sh -c` 直接跑会崩，`sleep 300` 这类长命令正常）
- `NetworkMode` 默认容器 ID（每容器独立网络命名空间）
- `DriverOpts: {"nicless": "true"}`（gVisor 网络）

---

## 4. VM（microVM）能力

### 4.1 运行时

- **Runtime**：`io.containerd.nerdbox.v1`（containerd shim）
- **VMM**：Docker Sailor（Apple Hypervisor Framework，macOS）
- **内核**：Linux 7.0.12 aarch64（nerdbox kernel）
- **根文件系统**：overlay（`lowerdir=/run/bundles/<cid>/mounts/1`，upper 可写层持久化）
- **磁盘**：EROFS（`GET /info` Driver=erofs）
- **内存**：4GB 默认（HostConfig.Memory），**CPU**：4 核

### 4.2 生命周期

```
create → start（VM 拉起，新 shim）→ exec → stop（VM 销毁，shim 退出 + state 清理）
       → start（VM 重建，新 shim，数据保留）→ rm
```
- 磁盘持久化：overlay upper 层在 VM 重建后保留（实测 `/tmp` 文件跨 restart 存在）

### 4.3 PTY（exec hijack 流）

```
POST /containers/{id}/exec {"Cmd":["/bin/sh"],"AttachStdin":true,"AttachStdout":true,"Tty":true}
→ 201 {"Id":"..."}
POST /exec/{id}/start {"Detach":false,"Tty":true}
→ 200 + hijack（HTTP 头后接原始字节流）
```
- 双向流：stdin → VM，VM stdout/stderr ←（Tty 时无 stream 头）
- **TTY resize**：`POST /exec/{id}/resize?h=40&w=120` → 200（query 参数；未运行时 409）
- **detach 长运行**：`{"Detach":true}` → exec 后台存活（Running:true）
- **重连**：再次 `POST /exec/{id}/start` → 200 + raw-stream（`application/vnd.docker.raw-stream`）
- 多轮交互实测：shell 提示符、命令回显、`whoami`→root、`stty size`→`40 120` 全部正常

### 4.4 容器 attach（另一通道）

```
POST /containers/{id}/attach?stream=1&stdin=1&stdout=1&stderr=1
→ 101 UPGRADED + application/vnd.docker.multiplexed-stream
```

### 4.5 VM 环境

- `apk` 包管理可用（alpine 模板）
- 完整 /dev（tty/pts/random）
- 默认干净 env（HOME=/root, PATH 标准）
- PID 1 = 容器主进程（entrypoint）

---

## 5. 网络模型（gVisor）

### 5.1 拓扑

```
VM eth0 (172.17.0.1/31, IPv6 fd47:...::1/127)  ←veth→  gVisor netstack → 宿主
```

### 5.2 边界（实测）

| 方向 | 结果 |
| --- | --- |
| VM → 宿主（0.0.0.0 监听） | ✅ 反向连接 |
| VM → 宿主（127.0.0.1 监听） | ❌ |
| VM → 外网 HTTP | ✅ 稳定 |
| VM → 外网 HTTPS | ⚠️ 间歇（MITM 证书） |
| VM → 其他 VM | ❌ 隔离 |
| 宿主 → VM | ❌ 无入站 |
| VM 内 DNS | ✅（虚拟 DNS 172.17.0.0） |

### 5.3 官方 sandbox 的网络注入（e2b-local 可复制）

```
HTTPS_PROXY=http://gateway.docker.internal:3128
http_proxy / https_proxy / HTTP_PROXY 同
NO_PROXY=localhost,127.0.0.1,::1,gateway.docker.internal
SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt
PROXY_CA_CERT_B64=<CA 证书 base64>
MCP_GATEWAY_URL=http://mcp-gateway.docker.internal/mcp
```

### 5.4 端口暴露（反向隧道，免认证）

```
VM 内: nc 172.26.176.70 <relay_port>   ← VM 主动反向连接（需保持活跃，循环重连）
宿主: relay 监听 0.0.0.0:<port>（必须 0.0.0.0，127.0.0.1 不可达）
   → 双向桥接 VM 连接与用户连接
```
- 实测：VM 数据 `REVERSE-TUNNEL-WORKS` 完整送达宿主
- `sbx ports`（官方通道）在 nerdbox 上 packet 转发不工作 → 反向隧道是替代方案

---

## 6. 观测（metrics.sock，免认证）

每 VM 一个 unix socket：`.../containerd/state/io.containerd.runtime.v2.task/docker/<cid>/vm/metrics.sock`

```
GET /metrics → Prometheus 文本（51 个 metric 家族）
```
- `sailor_guest_cpu_*_jiffies_total`（user/system/idle/iowait/steal）
- `sailor_guest_mem_*_pages`（anon/free/total/available）
- `sailor_guest_net_*`（rx/tx bytes/packets）
- `sailor_virtio_*`（interrupts/poll/kicks）、`sailor_fuse_*`、`sailor_balloon_*`、`sailor_vcpu_exits_*`

另有 `console.sock`（virtio-console，静默，协议未知）。

---

## 7. 认证

### 7.1 机制

- OAuth device flow（`sbx login`）→ Docker 签发 JWT → secretskit 落盘
- `authmiddleware.VerifyAuthenticated` 验证（cachedVerifier + GetDefaultProfileAccessToken）
- **策略**：governance（PrincipalAuthorizer.Authorize / MountPolicyEngine / EvaluateNetworkConnect）

### 7.2 登录（MITM 环境）

百度 XAgent MITM 证书无 SAN → Go TLS 验证失败。修复：`SSL_CERT_FILE` 信任锚注入（详见 sbx-binary-analysis.md §登录修复）。

### 7.3 免认证边界（最终确认）

| 通道 | 免认证能力 |
| --- | --- |
| docker.sock | VM 生命周期 + PTY + attach + 镜像 + 网络 inspect |
| metrics.sock | VM 指标 |
| sandboxd.sock | 仅 3 个只读 GET |
| 反向隧道 | 端口暴露 |

---

## 8. 官方 sandbox 完整配置（可复制模板）

docker.sock inspect 官方 sandbox（`com.docker.sandbox.*` labels + env）：

```json
{
  "Image": "docker/sandbox-templates:shell-docker",
  "Entrypoint": null,
  "Cmd": ["sh", "-c", "trap 'kill -TERM -- -1; wait' TERM; sleep infinity & wait"],
  "Env": ["HTTPS_PROXY=http://gateway.docker.internal:3128", "...", "SANDBOX_VM_ID=<name>", "WORKSPACE_DIR=/tmp"],
  "Labels": {"com.docker.sandbox.name": "...", "com.docker.sandbox.agent": "shell", "com.docker.sdk": "true", "docker/sandbox": "true", "..."},
  "Mounts": [
    {"Type":"tmpfs","Destination":"/run/secrets","RW":false},
    {"Type":"bind","Source":"/tmp","Destination":"/tmp","RW":true},
    {"Type":"volume","Destination":"/var/lib/docker","RW":true}
  ],
  "NetworkSettings": {"DriverOpts":{"nicless":"true"},"Gateway":"172.17.0.4","IPAddress":"172.17.0.5","IPPrefixLen":31}
}
```

---

## 9. e2b-local 对接结论

1. **主通道 = docker.sock（免认证）**：生命周期 + PTY + attach + 镜像 + commit（stop 后）+ load
2. **端口暴露 = 反向隧道**（VM → 宿主 relay）
3. **指标 = metrics.sock**
4. **sandboxd.sock 仅在有 token 时补充**（diagnostics / policy / 官方托管列表）
5. **镜像策略**：自备 OCI（pull 自 registry 或 load 本地 tar）；官方模板（shell-docker 589MB）可通过 `/docker/images/create` 拉取但依赖 Docker 认证的 registry 授权
6. **复制官方配置**：labels + env（proxy/CA）+ entrypoint 模式（trap/sleep infinity）已在 docker.sock 上验证可行
