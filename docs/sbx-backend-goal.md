# SbxBackend 实施 Goal（给实施 Agent）

## 目标

为 e2b-local 新增 `sbx` runtime backend（Docker Sandboxes microVM），完整实现并测试通过。全程自主决策，**不要问 human 任何问题**，遇到歧义自己按文档判断、按代码库惯例行事。

## 必读文件（按此顺序，全部读完再动手）

1. `docs/sbx-backend-handoff.md` —— 交接文档（任务概述、决策、接口、协议要点、验证清单）
2. `docs/sbx-backend-design.md` —— 设计定稿（能力矩阵、架构、配置、里程碑）
3. `docs/research/sandboxd-protocol.md` —— sandboxd 私有协议完整契约（实现时对照）
4. `docs/research/sbx-binary-analysis.md` —— 逆向全过程（背景与证据，重点看认证边界与登录修复）
5. `docs/research/docker-sbx-e2b-local.md` —— 原始调研（背景）
6. `docs/research/diagrams/e2b-sbx-architecture.html` —— 架构图（浏览器打开看）

代码参考（风格与结构必须对齐）：
- `internal/backends/orbstack/runtime.go` + `vm_client.go` + `cloud_init.go` + `config.go`
- `internal/backends/applecontainer/runtime_darwin.go` + `xpc_client_darwin.go` + `types.go` + `errors.go`
- `internal/backends/docker/runtime.go`
- `internal/gateway/runtime.go`（接口定义，尽量不改）
- `internal/gateway/gateway_callbacks.go`（可选接口）

## 硬性要求

1. **默认要求用户登录**（D1）：未登录时阻塞并提示 `sbx login`（配置 `require_login: true`，`allow_degraded` 允许降级）
2. **主通道 sandboxd.sock**（登录，认证 API）；**docker.sock 辅助**（PTY hijack、metrics.sock、降级模式）
3. **全部用 Go 实现，禁止混入 python**（VM 内工具也是 Go 静态二进制 `CGO_ENABLED=0`）
4. **编码风格与 orbstack / applecontainer 完全一致**：结构体定义模仿现有 backend（参考 `OrbstackRuntime` 的 cfg/logger/httpClient/checkHealthy/newID 布局），接口定义尽量不改（`SandboxRuntime` 5 方法 + 可选接口照实现）
5. **少写冗余防御性代码**：只写有实际意义的错误处理与校验（不能冗余 ≠ 不能防御；对照 orbstack 的防御密度，不超额）
6. 注册方式对齐：`gateway.RegisterSandboxRuntimeFactory("sbx", ...)`，文件放 `internal/backends/sbx/`
7. 协议实现对照 `sandboxd-protocol.md` 的 §5 要点（sandboxd.sock 各端点 / docker.sock 端点 / metrics.sock 路径超长需 chdir / PTY 走 docker.sock hijack）
8. **完成实现后，e2b-local 定义的所有 API 都要测试通过**（参照现有 backend 的 runtime_test.go / runtime_integration_test.go 风格写测试，跑通 `go test ./...`）
9. 待定方案（反向隧道 §5.3、logs §5.6）按文档方案实现，标注清楚

## 最终交付

一切完成（实现 + 测试全绿）后，**跑一个 benchmark**：对比 applecontainer / docker / orbstack / sbx 四个方案的 sandbox 创建到就绪速度（缺依赖就自动安装，如 orbctl/orbstack 未装则用现有环境）。benchmark 结果写进 `docs/research/backend-benchmark.md`（含每个方案的 create/start/就绪耗时，格式自定，清晰即可）。

## 约束

- **禁止 `git push`**（只能 commit，如果 commit）
- 不要问 human 问题；歧义按文档优先、代码库惯例其次、合理工程判断兜底
- 高质量交付：代码可读、测试覆盖核心路径、文档引用正确
