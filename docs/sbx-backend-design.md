# e2b-local × Docker Sandboxes (sbx) Backend 设计

> 状态：设计阶段（部分方案待定，评估中）。基于三轮逆向调研（`docs/research/sandboxd-protocol.md`、`docs/research/sbx-binary-analysis.md`）+ 完整能力实测 + 性能基准。
> 目标：为 e2b-local 新增 `sbx` runtime backend，利用 Docker Sandboxes 的 microVM（nerdbox + Sailor）能力，优先引导用户登录以获得 100% 能力，未登录时降级至免认证通道（90% 能力）。
> **注意：§5.3（反向隧道）与 §5.6（logs 采集）为待定方案，仍在评估中。所有实现一律使用 Go（不混入 python）。**

---

## 1. 设计决策摘要

| # | 决策 | 理由 |
| --- | --- | --- |
| D1 | **默认要求用户登录**（使用 sbx backend 的前置条件，提示用户执行 `sbx login`） | 登录后 100% 能力（pause/resume 原生、CPU 限制生效、diagnostics、官方模板）；登录是产品默认路径 |
| D2 | 双模式：登录（主路径，100%）/ 免登录（降级选项，~90%） | 未登录时降级可用（pause→stop/start 模拟），但**默认引导登录**，对 SDK 调用方透明 |
| D3 | 主通道 sandboxd.sock（登录，认证 API） | 登录后的原生能力（生命周期/exec/files/SSH）；docker.sock 仅作补充（PTY hijack 流，实测更可靠） |
| D4 | docker.sock 作辅助通道（免认证） | PTY hijack（sandboxd TTY 挂起）、metrics.sock、免登录降级 |
| D5 | 端口暴露用反向隧道（VM→宿主 relay）【待定】 | `sbx ports` 在 nerdbox 上 packet 转发不工作（实测）；方案评估中 |
| D6 | Pause/Resume 降级映射为 Stop/Start（仅免登录模式） | 登录模式用 sandboxd 原生 stop/start；docker.sock 的 pause 是 501 |
| D7 | 镜像策略：自备 OCI 镜像优先 | 匿名 pull 公开镜像已实测可用；官方模板登录后可用 |
| D8 | 指标用 metrics.sock（Prometheus） | `docker stats` 是 501，metrics.sock 免认证 51 项 |

---

## 2. 能力矩阵（实测，2026-08-11）

> **默认要求登录**：sbx backend 的前置条件是用户已完成 Docker 登录（`sbx login`）。登录模式是主路径（100% 能力）；免登录是降级选项（~90%），仅在用户明确选择"跳过登录"时启用。

### 2.0 登录要求（默认路径）

| e2b-local 能力 | 实现 | 实测证据 |
| --- | --- | --- |
| 创建 sandbox | docker.sock `POST /containers/create` | 201，89ms |
| 启动/停止 | `POST /containers/{id}/start\|stop` | 204，VM 拉起/销毁 |
| 删除 | `DELETE /containers/{id}?force=1` | 204 |
| **Pause/Resume** | **映射 Stop/Start**（D6） | pause 501；stop/start 状态保持 |
| exec 命令 | exec create + start（hijack） | 完整输出 |
| **PTY 交互终端** | exec hijack（Tty=True）+ resize | 交互 shell + `stty size` 生效 |
| 长运行 + 断线重连 | detach exec + 重新 attach | Running 保持 + 重连成功 |
| 文件读写 | exec + `GetFile/PutFile` 等价 | 实测 |
| 镜像 pull（匿名） | `POST /images/create` | ubuntu:24.04 拉取成功 |
| 快照（snapshot） | stop 后 `POST /commit` | 4.13MB 镜像，save/load 正常 |
| Volume 持久化 | `POST /volumes/create` + mount | 跨容器共享实测 |
| 内存限制 | HostConfig.Memory | 512m 生效 |
| 服务暴露（端口） | 反向隧道（D5） | VM→宿主 relay 双向通 |
| VM 指标 | metrics.sock（51 项） | Prometheus 格式 |
| **CPU 限制** | ⚠️ **不生效**（NanoCpus=0，无 cgroup） | 实测 |
| 官方模板/kit | ❌ 需登录 | 401（自备镜像替代） |

### 2.2 登录模式（完整，100%）

| 额外能力 | sandboxd API | 实测 |
| --- | --- | --- |
| **原生 Pause/Resume** | `POST /sandbox/{name}/stop\|start` | 200 |
| **CPU 限制** | CreateSandboxRequest `cpus` | 完整校验（validateMemoryCPUs） |
| diagnostics（105KB） | `GET /daemon/diagnostics` | 200 |
| 策略管理 | `GET/POST /policy/network/*` | 200 |
| 事件流 | `GET /events`（NDJSON） | 200 |
| MCP 网关 | `GET/POST /mcp/*` | 200 |
| 官方模板/kit | `/docker/images/create`（sandboxd 代理） | 200 |
| sandbox 托管 | `GET/POST /sandbox` | 200 |

### 2.3 登录要求（D1 实现，默认路径）

**sbx backend 启动/首次使用时**：
1. 检测登录状态：`GET /daemon/health` → 200 且 `GET /sandbox` 非 401 → 已登录，直接完整模式
2. **未登录 → 明确要求用户登录**（UI/CLI 消息，默认阻塞）：
   - 提示：`Please sign in to Docker to use the sbx runtime (full capabilities)`
   - 给出登录命令：**`sbx login`**
   - 说明：登录后获得 100% 能力（pause/resume 原生、CPU 限制、官方模板、diagnostics）
   - 附注（MITM 环境）：如遇证书错误，用 `SSL_CERT_FILE=$HOME/.sbx/duguanjia-live.pem sbx login`
   - 仅当用户明确选择"跳过登录继续"时，才进入免登录降级模式（90% 能力）
3. 登录状态缓存（检测 token 有效后标记，避免每次探测）；token 过期时重新引导

---

## 3. 架构（对应 diagrams/e2b-sbx-architecture.html）

```
┌─ e2b-local ──────────────────────────────┐
│ API Server → Runtime Manager (SbxBackend) │
│            → PTY Bridge（exec hijack）     │
│            → Tunnel Relay（反向隧道）       │
└───────────────────────────────────────────┘
        │ 主通道（登录）           │ 辅助
        ▼                        ▼
   sandboxd.sock            docker.sock
        │ OAuth JWT              │ nerdbox shim
        ▼                        ▼
   MicroVM (nerdbox+Sailor)   PTY hijack / metrics.sock
   ├─ envd :49983 ← 反向隧道 ← Tunnel Relay
   ├─ metrics.sock（51 项指标）
   └─ console.sock（virtio-console）
```

---

## 4. 性能基准（实测对比，本机 arm64）

### 4.1 启动时间（关键优势）

| 阶段 | sbx microVM | OrbStack VM | 优势 |
| --- | --- | --- | --- |
| 创建（含镜像） | **89ms** | 124,002ms（冷启动） | **~1400×** |
| 启动 | **291ms** | 55-79ms | - |
| **总（到 exec 就绪）** | **396ms** | 冷 124s+ / 热 160ms | **冷启动 ~300×** |
| 运行中就绪 | — | 42ms | OrbStack 常驻优势 |

**结论**：e2b-local 的按需创建/销毁模式（每次新建 sandbox）下，sbx 的启动速度优势是**数量级**（microVM 内核预加载 + Sailor 快速 VM 拉起）。OrbStack 仅在机器常驻复用时有优势（42ms），但不符合 sandbox 按需模型。

### 4.2 资源占用

- sbx：microVM 按需销毁（stop 即清理 shim + state），无常驻开销
- OrbStack：机器常驻（~800MB/台，实测 `e2b-bench 796.5 MB`）

---

## 5. 关键实现细节

### 5.1 VM 生命周期（docker.sock）

```
POST /containers/create {"Image":"...","Cmd":["/bin/sh","-c","sleep 300"],"Tty":true}
→ 201 {"Id":"..."}（89ms）

POST /containers/{id}/start → 204（291ms，VM 拉起）
POST /containers/{id}/exec {"Cmd":["/bin/sh"],"AttachStdin":true,"AttachStdout":true,"Tty":true}
→ 201 {"Id":"..."}
POST /exec/{id}/start {"Detach":false,"Tty":true} → 200 + hijack raw-stream

POST /exec/{id}/resize?h=40&w=120 → 200（query 参数，运行中）
POST /exec/{id}/start {"Detach":true} → 后台长运行
DELETE /containers/{id}?force=1 → 204
```

**注意**：
- `Path` 为空，Cmd 是 exec 风格（`["/bin/sh","-c","sleep 300"]` 正常）
- 单命令直接跑会 exit 127（需 sleep 类长命令或 entrypoint）
- entrypoint 模式：`"Entrypoint":["/bin/sh"],"Cmd":["-c","..."]` 可常驻服务

### 5.2 PTY（exec hijack）

- 交互式 shell：提示符、双向流、TTY（`/dev/pts/0`）全通
- resize：`?h=..&w=..`（query，运行中）
- 断线重连：重新 start → 200 + raw-stream
- 多轮命令：稳定（4 轮实测全过）

### 5.3 端口暴露（反向隧道，D5）—— 【待定方案，评估中】

> **状态：可行性已验证，实现方案待定。** `sbx ports` 在 nerdbox 上不转发数据（实测：端口发布成功但只回 4 字节 stream 头），需反向隧道替代。2026-03 社区仍无官方修复。

**架构（全部 Go 实现，不混入 python）：**

```
宿主侧（Go，e2b-local 内嵌）:
  TunnelRelay: 监听 0.0.0.0:<port>，接受 VM 反向连接 + 用户连接，双向桥接
  端口分配: 固定范围（如 40000-41000），每个 sandbox 一个 relay

VM 侧（Go 静态二进制，随 entrypoint 注入）:
  sbx-tunnel: 反向连接宿主 relay（循环重连保持），
              把 relay 的字节流转发到 VM 内 127.0.0.1:<target_port>
```

**关键实现细节：**
- 宿主 relay **必须绑定 0.0.0.0**（127.0.0.1 绑定不可达，实测）
- VM 侧 `sbx-tunnel` 是 **Go 静态编译**（`CGO_ENABLED=0`，~5MB），通过以下方式注入：
  - 方案 A：宿主编译后 `docker cp`/exec 写入 VM（需登录后 docker.sock 通道）
  - 方案 B：作为 entrypoint 的一部分随镜像构建（自备镜像时预装）
- VM 侧重连循环：断线后 1s 重试（实测 detach exec 可保活，`while true; do ...; sleep 1; done` 模式）
- VM 内目标端口转发：`sbx-tunnel` 把 relay 连接转发到 `127.0.0.1:<envd_port>`
- 认证：relay 仅监听 loopback（127.0.0.1 用户侧），VM 侧连接用 token 验证（可选增强）

**备选方案（未验证）：**
- `POST /sandbox/{name}/ssh`（HTTP upgrade）→ SSH 隧道：sandboxd 暴露了端点（400 校验），但 nerdbox 转发不工作（实测）
- 社区 workaround `ssh-over-tls`（2024）：与本方案同思路，但引入 TLS 复杂度

**测试数据（2026-08-11）：**
- VM 反向连接宿主成功：`('172.26.176.70', 55707)`，数据 `PY-REVERSE-OK` 送达（当时用 python 验证可行性）
- shell-docker 模板 VM 内无 `nc`，但有 `python3` + `socat` → **Go 静态二进制是跨模板最可靠方案**
- `sbx ports` 发布端口后数据不通（4 字节 stream 头 `\x01\x00\x00\x00`）→ 不可依赖

### 5.6 GetSandboxLogs（待定方案，评估中）

> **状态：sandboxd 无 logs API（全路径探测 404），需替代采集。方案待定。**

**方案 A：VM 内日志采集（推荐，Go 实现）**
```
e2b-local（Go）:
  GetSandboxLogs(info) → 通过 exec 通道执行:
    journalctl --no-pager -n <limit>（systemd 模板）
    或 cat /var/log/syslog /var/log/xxx（非 systemd）
  → 解析为 []SandboxRuntimeLogEntry
```
- 优点：走已验证的 exec 通道，零额外组件
- 缺点：VM 内无 journald 时需 fallback 到文件；大日志分页需游标

**方案 B：事件流累积（sandboxd /events）**
```
GET /events（NDJSON）→ 持续累积为 sandbox 日志
```
- 优点：认证后可用，标准通道
- 缺点：只有 sandboxd 事件（创建/停止等），无 VM 内命令输出

**方案 C：VM 内日志代理（Go，随 entrypoint 部署）**
```
sbx-logd（Go 静态二进制）: VM 内 tail 日志文件 → 反向隧道推送到宿主
宿主: 接收并存储 → GetSandboxLogs 直接读
```
- 优点：实时流式日志，与反向隧道共用基础设施
- 缺点：复杂度高，需与 sbx-tunnel 集成

**推荐**：M1 用方案 A（exec 采集），M3 评估方案 C（实时流式）作为增强。

### 5.4 快照（D7 相关）

```
docker stop {id} → docker commit {id} {repo}:{tag} → 镜像
docker save/load → 本地 tar 备份
```
- 需 stop 后 commit（运行中 409）
- volume 不随 commit 保留（实测：`/data` 文件丢失，预期）

### 5.5 指标（D8）

```
GET <vm_dir>/metrics.sock/metrics → Prometheus 文本
51 项：guest_cpu_*_jiffies / guest_mem_*_pages / guest_net_* / virtio_* / fuse_* / balloon_* / vcpu_exits_*
```

---

## 6. 网络模型（gVisor）

| 方向 | 状态 | 说明 |
| --- | --- | --- |
| VM → 宿主（0.0.0.0） | ✅ | 反向连接（隧道基础） |
| VM → 外网 HTTP | ✅ | 默认 egress |
| VM → 外网 HTTPS | ⚠️ 间歇 | MITM 证书影响 |
| VM → 其他 VM | ❌ | 命名空间隔离 |
| 宿主 → VM | ❌ | 无入站 |
| DNS | ✅ | 虚拟 DNS（172.17.0.0） |

**安全边界**：sandbox 只能通过反向连接出站，入站必须宿主绑 0.0.0.0 + relay —— 严格可控。

---

## 7. 配置（config.yaml 草案）

```yaml
runtime: "sbx"

sbx:
  # 默认要求登录（D1）：未登录时阻塞并引导 sbx login
  require_login: true
  # 未登录时是否允许降级（false = 强制登录，不降级）
  allow_degraded: true
  # 登录命令提示
  login_hint: "sbx login"
  # MITM 环境的证书修复提示（可选）
  cert_hint: "SSL_CERT_FILE=$HOME/.sbx/duguanjia-live.pem sbx login"
  # 反向隧道 relay 端口范围
  tunnel_port_range: [40000, 41000]
  # 默认镜像（自备 OCI，D7）
  default_image: "alpine:3.22"
  # metrics 采集间隔
  metrics_interval: "5s"
```

---

## 8. 风险与缓解

| 风险 | 影响 | 缓解 |
| --- | --- | --- |
| docker.sock 免认证是现状非契约 | 未来 Docker 可能加认证 | 登录模式兜底；监控版本变化 |
| HTTPS 出站间歇（MITM） | sandbox 内 HTTPS 不稳 | 提示用户配置 VM 内 CA；或登录后走官方 proxy |
| nerdbox 端口转发缺陷 | `sbx ports` 不可用 | 反向隧道替代（D5） |
| 游离 VM 不被 `sbx ls` 管理 | 官方 CLI 看不到 | 用 docker.sock 统一管理；文档说明 |
| VM 内 exec 后台进程被杀 | envd 无法 exec 启动 | entrypoint 常驻（实测可用） |

---

## 9. 里程碑

1. **M1（原型）**：SbxBackend 骨架 + docker.sock 生命周期 + PTY（exec hijack）
2. **M2（网络）**：反向隧道（Go 实现，§5.3 待定方案落地）+ 端口暴露 + metrics.sock 采集
3. **M3（登录）**：登录检测 + 引导 + sandboxd API 集成（pause/resume/诊断）
4. **M4（完整）**：快照/volume/镜像管理 + **logs 采集（§5.6 方案落地）** + 配置 + 文档
