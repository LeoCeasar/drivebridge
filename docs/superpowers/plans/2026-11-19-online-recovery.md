# 在线诊断与恢复 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development and test-driven-development. 独立文件任务可并行，集成由主执行者完成，不重开设计审批。

**Goal:** 同步优化 macOS/Windows 在线错误分类、持久诊断、只读恢复与账户冷却。
**Architecture:** model 保存通用诊断；online 纯分类；onlinecooldown 管理用户域 canonical identity 冷却；store 保持原操作日志，sync 分离观察和云端写入；manager/UI 统一展示。
**Tech Stack:** Go、bbolt、已有 flock/平台私有文件 API、浏览器 JS、Go/Node/Python 测试。
**Spec:** docs/coding-plan/online-recovery/design.md（本轮用户已批准）。

## Global Constraints

- 保留五分钟新保存节奏、原输入/代次/token/CAS/未知写入保护。
- 观察接口不能调用云端 mutation；原任务剩余副作用交正常准入。
- 网络/授权/限流失败不消耗有效核验预算；历史计数不伪造。
- macOS/Windows 共用分类；凭据/剪贴板/文件系统不扩权限。
- 不改真实账号/问题文件，不自动部署、发布或推送新版本。

## Review Focus

- unknown_result 与 auth_required 并存：原因准确且不重发。
- 原创建完成但改名未完成：只读核验不能偷偷改名。
- 旧 NeedsAttention 和累计次数：保持历史并明确未记录字段。
- 相同 canonical 身份不同 profile/config：共享冷却且较短等待不能覆盖较长等待。
- 重启/重复点击/取消来源/协议失败：预算与暂停不得被绕过。

### Task 1: 共用类型与分类（独立）

**Files:** 新建 model/online.go、online/*.go；不改 model/types.go。
**Interfaces:** `online.Capture(ctx, err, operation, phase, outcome string, at time.Time) model.OnlineFailure`；`online.Cause(error) string`；`online.Message/PhaseLabel/ActionLabel(string) string`；`online.WithCancelOrigin(ctx,origin) context.Context`；`online.AtPhase(err,operation,phase) error`。model.OnlineRecord 包含 First/Last/History，OnlineFailure 包含 SchemaVersion/ID/Operation/Phase/Cause/Outcome/Action/APICode/At/RetryAfterSeconds/CancelOrigin。
- [x] 表驱动失败测试：授权/凭据/身份/权限/网络/协议/API/取消/未知，复合错误不丢 unknown outcome，机器字段脱敏。
- [x] 实现并运行 online/model 套件。

### Task 2: canonical 账户冷却（独立）

**Files:** 新建 onlinecooldown/*.go；只依赖标准库、已有 flock/platform。
**Interfaces:** `New(root, identityHash, authRef string, now func() time.Time) (*Coordinator,error)`；`Check(ctx) error`；`Record(ctx, delay time.Duration) error`；WaitError 提供 `RetryAt() time.Time`，错误字符串不带身份/路径。记录身份、事件、原时长、UTC下限，短锁原子写入，不睡眠。
- [x] 失败测试：相同身份跨实例/进程、不同账号、长短更新、重启/时钟、取消锁、坏文件/符号链接。
- [x] 实现、竞态与 Windows 测试交叉编译。

### Task 3: store 诊断与恢复周期（主执行）

**Files:** model/types.go、store/jobs.go、engine.go、create_recovery.go、namespace*.go、新 online_diagnostic.go。
**Interfaces:** `RecordJobOnlineFailure(ctx,jobID,failure)`；`RecordNamespaceOnlineFailure(ctx,id,failure)`；Journal/NamespaceWork/SyncIssue 增加 Online；`RecordCreateObservation` 以有效/请求失败分计，保留 legacy attempts；恢复周期和暂停持久化，重新诊断仅观察授权。
- [x] 失败测试：首次/最近与八条历史，重启、旧记录、三次网络不进入人工、多候选即时人工、三次有效缺失、未知协议持久暂停。
- [x] 追加字段与 CAS，保持业务阶段/原历史；运行 store 套件。

### Task 4: 只读观察、统一错误与传输冷却接入（主执行）

**Files:** sync/create*.go、errors.go、retry.go、dispatch.go、docx.go、namespace.go；larkcli/runner.go；manager/native.go/link.go。
**Interfaces:** 只读 `CreationObserver` 的方法集合不含 mutation；观察新入口与正常 mutation 准入分开；共享诊断 `AtPhase` 标记；Runner 的 before/after 请求钩子用于冷却，身份必须来自已绑定管理环境。
- [x] 失败测试：丢响应只观察、核验后不改名、网络计数隔离、阶段准确、原代次保存、持久 due/暂停、授权/取消类别、冷却不能使未派发请求变未知。
- [x] 接入分类、观察、预算与当前调度，保留原写入保护。
- [x] 运行 sync/larkcli/cloud/manager 套件及竞态。

### Task 5: 统一界面与诊断导出（依赖类型；独立 UI 文件）

**Files:** 新 online-view.js、UI app.js/index.html、manager/status.go、UI Node 测试。
**Interfaces:** issue.online 的安全字段；首次/最近阶段原因与 legacy 次数分开；复制诊断排除路径/正文/身份，服务等待显示真实下一步。
- [x] 失败测试：具体授权而非网络，unknown outcome 同时显示、缺字段旧数据、等待/人工动作、导出脱敏。
- [x] 实现并验证 UI/manager。

### Task 6: 集成验证与审查

- [x] 运行项目桥接完整 Go 套件、相关竞态、Node UI、打包测试。
- [x] 构建 macOS 和完整参数 Windows CLI/桌面程序，Windows 专属测试交叉编译；真实 Windows 执行未跑则明确保留边界。
- [x] 独立代码审查、一次修正重要问题，记录证据与剩余边界；用户授权的工作全部完成后汇报，不每任务求确认。

验证及真实平台边界见 `docs/coding-plan/online-recovery/verification.md`；勾选表示实现和列明的本机验证完成，不表示 Windows 原生运行或新版本发布。
