# SbxBackend 实现交接文档（给实施 Agent）

> **交接人**：逆向调研 Agent（已完成全部调研与设计）
> **接收人**：实施 Agent（负责 M1 原型实现）
> **日期**：2026-08-11
> **状态**：设计定稿（§5.3 反向隧道 / §5.6 logs 为待定方案），可开始 M1

---

## 1. 任务概述

为 e2b-local 新增 **`sbx` runtime backend**：利用 Docker Sandboxes 的 microVM（nerdbox + Sailor）作为 sandbox 运行时，**默认要求用户 Docker 登录**（`sbx login`），主通道为 sandboxd.sock，docker.sock 作辅助。

**里程碑**：M1（原型：生命周期 + PTY）→ M2（反向隧道）→ M3（登录集成）→ M4（完整）

---

## 2. 必读文档（按顺序）

| 文档 | 内容 | 作用 |
| --- | --- | --- |
| `docs/sbx-backend-design.md` | **设计定稿**（决策、能力矩阵、架构、性能、实现细节、配置、里程碑） | 设计依据，先读这个 |
| `docs/research/sandboxd-protocol.md` | sandboxd 私有协议完整记录（9 章：socket、API schema、VM 能力、网络模型、认证） | 协议契约，实现时对照 |
| `docs/research/sbx-binary-analysis.md` | 逆向全过程（符号表、认证边界、免认证通道、登录修复、三轮扫描） | 背景与证据 |
| `docs/research/docker-sbx-e2b-local.md` | 原始调研（Docker Sandboxes 官方能力） | 背景 |
| `docs/research/diagrams/e2b-sbx-architecture.html` | 架构图（浏览器打开） | 视觉参考 |

---

## 3. 关键设计决策（必须遵守）

| # | 决策 |
| --- | --- |
| D1 | **默认要求登录**：未登录时阻塞并提示 `sbx login`（`require_login: true`），仅用户明确选择才降级 |
| D2 | 双模式：登录（100%）/ 免登录降级（90%，pause→stop/start） |
| D3 | **主通道 sandboxd.sock**（登录，认证 API：生命周期/exec/files/SSH） |
| D4 | **辅助通道 docker.sock**（免认证：PTY hijack、metrics.sock、免登录降级） |
| D5 | 端口暴露用**反向隧道**【待定方案，Go 实现】（`sbx ports` 不工作） |
| D6 | 免登录时 Pause/Resume 映射 Stop/Start；登录用 sandboxd 原生 |
| D7 | 镜像：自备 OCI 优先；官方模板登录后可用 |
| D8 | 指标用 metrics.sock（`docker stats` 是 501） |
| ★ | **全部用 Go 实现，禁止混入 python**（VM 内工具也必须是 Go 静态二进制） |

---

## 4. 核心接口（必须实现）

`internal/gateway/runtime.go` 的 `SandboxRuntime` 接口（5 方法）+ 可选接口：

```go
type SandboxRuntime interface {
    CreateSandbox(ctx, SandboxRuntimeCreateRequest) (SandboxRuntimeInfo, error)
    ListTemplates(ctx) ([]SandboxRuntimeTemplate, error)
    DeleteSandbox(ctx, SandboxRuntimeInfo) error
    PauseSandbox(ctx, SandboxRuntimeInfo) error
    ResumeSandbox(ctx, SandboxRuntimeInfo) (SandboxRuntimeInfo, error)
}
// 可选（建议实现）：
// SandboxRuntimeInspector.InspectSandbox
// SandboxRuntimeRestorer.RestoreSandboxes
// SandboxRuntimeMetrics.GetSandboxMetrics（metrics.sock）
// SandboxRuntimeSnapshotter.CreateSandboxSnapshot/ListSnapshots（commit）
// SandboxRuntimeLogger.GetSandboxLogs（§5.6 方案 A）
// VolumeRuntime / VolumeContentRuntime（docker.sock /volumes）
```

注册方式（参照 orbstack backend）：
```go
func init() {
    gateway.RegisterSandboxRuntimeFactory("sbx", func(cfg gateway.Config, logger *log.Logger) (gateway.SandboxRuntime, error) {
        return NewSbxRuntime(cfg.Sbx, logger)
    })
}
```
文件建议：`internal/backends/sbx/`（runtime.go / sandboxd_client.go / docker_client.go / tunnel.go / config.go / 测试）

---

## 5. 关键协议要点（实现时对照 sandboxd-protocol.md）

### 5.1 sandboxd.sock（主通道，登录后）

```
Socket: ~/.sbx/run/d/sandboxd.sock（HTTP over unix socket）
认证:   Authorization 头（OAuth JWT，登录后 secretskit 提供）
User-Agent: sbx-cli/v0.38.0 (darwin/arm64)

POST /sandbox {"agent":"shell","workspace":"/tmp","name":"x"}  → 201（创建即 running）
GET  /sandbox                                               → 列表
GET  /sandbox/{name}                                        → inspect（status/labels）
POST /sandbox/{name}/start|stop                             → 200（原生 pause/resume）
DELETE /sandbox/{name}                                      → 200 deleted
POST /sandbox/{name}/exec {"cmd":[...]}                     → 200 {"exit_code","stdout","stderr"}（同步）
POST /sandbox/{name}/exec（tty:true）                        → hijack 流（PTY，注意：实测挂起，PTY 走 docker.sock）
GET  /sandbox/{name}/files?path=...                         → tar 流（文件读写）
POST /sandbox/{name}/save {"tag":...}                       → 快照
POST /sandbox/{name}/ssh（Connection: Upgrade）              → SSH（nerdbox 转发不工作，备选）
GET  /daemon/diagnostics                                    → 105KB 诊断
GET  /events                                                → NDJSON 事件流
GET  /policy/network/*                                      → 策略管理
GET  /daemon/settings                                       → 18 项设置（免认证）
GET  /daemon/health /daemon/info                            → 免认证健康检查
```

### 5.2 docker.sock（辅助通道，免认证）

```
Socket: ~/.sbx/run/d/docker.sock（标准 Docker Engine API，docker-next 实现）
免认证: 所有端点无需 token

POST /containers/create                                     → 201（89ms）
POST /containers/{id}/start                                 → 204（291ms，VM 拉起）
POST /containers/{id}/exec {"Cmd":[...],"Tty":true}          → 201 exec Id
POST /exec/{id}/start {"Detach":false,"Tty":true}            → 200 + hijack raw-stream（PTY 通道！）
POST /exec/{id}/resize?h=40&w=120                            → 200（query 参数，运行中）
POST /exec/{id}/start {"Detach":true}                        → 后台长运行（保活）
GET  /containers/{id}/json                                  → inspect
DELETE /containers/{id}?force=1                             → 204
POST /images/create?fromImage=...                            → pull（匿名可用）
POST /commit?container=...&repo=...&tag=...                  → 快照（需 stop 后）
GET  /networks /volumes                                     → 列表
501: logs / stats / top / changes / export / restart
```

### 5.3 metrics.sock（免认证）

```
路径: ~/Library/Application Support/com.docker.sandboxes/sandboxes/sandboxd/containerd/state/io.containerd.runtime.v2.task/docker/<cid>/vm/metrics.sock
注意: 路径超长（>104 字符），需 chdir + 相对路径连接
GET /metrics → Prometheus 文本（51 项：guest_cpu/mem/net/virtio/fuse/balloon/vcpu）
```

### 5.4 PTY 注意事项

- **PTY 走 docker.sock exec hijack**（sandboxd TTY 挂起，实测）
- 交互 shell + resize + detach/重连 全部验证通过
- VM 内进程保活：用 detach exec（`{"Detach":true}`），不用 exec 内 `&` 后台（会被杀）
- 常驻服务用 entrypoint（`"Entrypoint":["/bin/sh"],"Cmd":["-c","..."]`）

---

## 6. 待定方案（M2/M4 处理，评估中）

### 6.1 端口暴露（§5.3）—— 反向隧道【待定】

- **宿主侧**：`TunnelRelay`（Go）监听 `0.0.0.0:<port>`，双向桥接 VM 连接与用户连接
- **VM 侧**：`sbx-tunnel`（**Go 静态编译**，`CGO_ENABLED=0`）反向连接宿主 + 转发到 `127.0.0.1:<envd_port>`
- 注入：docker cp / entrypoint 预装
- 必须绑 0.0.0.0（127.0.0.1 不可达，实测）
- VM 内无 nc（ubuntu 模板），Go 静态二进制是跨模板方案

### 6.2 GetSandboxLogs（§5.6）—— 3 方案【待定】

- A：exec `journalctl`/文件采集（推荐，M1 可做）
- B：sandboxd /events 事件流累积
- C：sbx-logd（Go）VM 内 tail → 隧道推送（实时）

---

## 7. 性能基准（供参考）

| 指标 | 值 |
| --- | --- |
| create | 89ms |
| start（VM 拉起） | 291ms |
| 到 exec 就绪 | 396ms |
| 对比 OrbStack 冷启动 | ~300× 优势 |

---

## 8. 验证清单（M1 完成标准）

- [ ] `sbx` runtime 注册成功（`gateway.RegisterSandboxRuntimeFactory("sbx", ...)`）
- [ ] 未登录时提示 `sbx login`（阻塞 + 允许降级）
- [ ] 登录后 CreateSandbox → sandboxd.sock `POST /sandbox` 201
- [ ] DeleteSandbox → `DELETE /sandbox/{name}` 200
- [ ] Pause/Resume → sandboxd stop/start（登录）/ docker.sock stop/start（降级）
- [ ] exec → sandboxd exec（同步）/ docker.sock hijack（PTY）
- [ ] ListTemplates → 自备镜像枚举
- [ ] 单元测试 + 集成测试（参照 orbstack/docker backend 的测试风格）

---

## 9. 环境信息（本机验证用）

- sbx v0.38.0，docker-next v0.28.0，daemon 常驻
- 登录已生效（`sbx ls` 可用）
- MITM 环境证书：`SSL_CERT_FILE=$HOME/.sbx/duguanjia-live.pem`（Go TLS 需要）
- sandboxd socket：`~/.sbx/run/d/sandboxd.sock` + `docker.sock`
- 测试镜像：`alpine:3.22`（13MB，已本地）
- **禁止**：`git push`（只 commit）；混入 python 实现
