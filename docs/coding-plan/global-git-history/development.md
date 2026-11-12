# 全局 Git 历史实施计划

> 执行方式：当前会话直接执行，按用户要求分块验证，不增加逐任务代理审核。

**Goal:** 在现有托管保存链路中记录持久历史，提供全局查询、无比较副本的 diff 和受保护恢复。
**Architecture:** Store 同事务事件/引用保护；每账号单写 Git 裸仓库；manager 复用 worker IPC 与离线租约；UI/CLI 共用服务。
**Tech Stack:** Go、bbolt、Git plumbing、现有本机 HTTP/原生 JS。
**Spec:** [design.md](design.md)，v1.1。

**执行结果（2026-11-12）：** 五个开发模块已实现，相关模块测试和交叉编译通过；后续真机发现的问题已修复，macOS 本机已安装 0.2.5 并完成指定功能验证。下列清单保留原计划；实际证据、未执行的其他平台/断电验收及功能边界以 [implementation-results.md](implementation-results.md) 和 [real-machine-acceptance.md](real-machine-acceptance.md) 为准，不能把本机功能验证视为全平台发布验收。

## 全局约束

- 继续当前工作区，保留前序未提交改动；不新建 worktree，不自动安装/推送。
- 本机保存不调用 Git/网络；历史输入只取不可变 blob，不读取挂载盘。
- 稳定历史身份、按源连续序号、账号隔离、未确认引用不可清理。
- 读取未知正文不能返回空文件；恢复校验本机代次和目标身份。
- Git 与 Store 双写使用幂等 outbox；仅配置接受不代表断电验收通过。

## Task 1：持久事件与引用图

Files: `model/history.go`, `store/history.go`, `store/history_test.go`, `store/store.go`, `store/gc.go`, `store/cache_eviction.go`, `store/validate.go`。
Interfaces: `HistoryEvents(ctx, after, limit)`, `HistoryReadBlob(ctx, id)`, `AckHistory(ctx, sequence, eventID)`；事件携带 scope/entry/metadata、正文角色和资源引用。
- [ ] 写测试并观察失败：重启/重复保存、空正文、事务失败、缓存/GC 不丢未归档输入。
- [ ] 实现 schema 迁移、保存与命名空间/远端接受的事件捕获、持久序号和资源保护。
- [ ] 相关小测试通过后运行完整 Store 模块。

## Task 2：Git 归档和有界查询

Files: `history/repository.go`, `history/git.go`, `history/query.go`, `history/repository_test.go`。
Interfaces: `Open(Options)`, `Ingest(ctx, Source)`, `List(ctx, Query)`, `Read(ctx, VersionID, role)`, `Diff(ctx, left, right)`。
- [ ] 真 Git 测试先失败：归档/重开/幂等、账号不匹配、无工作副本读旧版、空正文/缺失、不同来源和资源、过时 CAS。
- [ ] 实现受控 Git、账号锁、持久源水位、索引重建、正文/资源完整性与有界 diff。
- [ ] 完整 history 模块测试。

## Task 3：后台和管理服务

Files: `manager/history.go`, `manager/history_test.go`, `manager/worker.go`, `manager/ipc.go`。
Interfaces: `HistoryList`, `HistoryVersion`, `HistoryDiff`, `HistoryRestore`, `HistoryStatus`；worker 自动摄取，离线复用 withStore。
- [ ] 测试自动归档、停用映射、Git 不可用独立报错、恢复目标/代次竞争和幂等。
- [ ] 实现后台 lifecycle、IPC、查询和经过现有发布屏障的本机恢复。
- [ ] 完整 manager/overlay/integration 模块验证。

## Task 4：管理页与 CLI

Files: `ui/history.go`, `ui/history_test.go`, `ui/assets/history.js`, `ui/assets/index.html`, `ui/assets/style.css`, `cmd/feishu/history.go`。
Interfaces: 设计 §15 的受认证 history API；`feishu history list/show/diff/restore/status`。
- [ ] 先验证未授权/跨账号/缺正文/过时代次拒绝，再实现历史列表、正文版本比较和恢复入口。
- [ ] 默认只在网页/终端显示，不自动导出比较文件；错误与归档状态可见。
- [ ] UI/CLI 模块与相关 JS 检查。

## Task 5：保留、兼容与打包

Files: `history/compact.go`, `history/compact_test.go`, `packaging/drivebridge/*` 相关运行时入口。
- [ ] 压缩后旧事件重投递、固定版本/读租约、切换中断测试先失败。
- [ ] 实现 epoch 保留、完整性验证、可恢复发布和资源清理；Git 运行时发现/打包失败明确报错。
- [ ] 跨平台编译，当前平台真实 Git 集成；未获得平台/签名环境的验收如实记录。

## 验证与进度

基础 Store/manager/UI 测试在开发前通过。进度记录于 `.superpowers/global-git-history-implementation/progress.md`。任务结束核对 self-review.md 的 12 类反例；不以交叉编译代替三平台实机验收。
