# 本地保存与 Git 归档优化实施计划

**Goal:** 保存只等待本地修改可靠落盘，完整快照、Git 归档和云端同步后台执行。

**Architecture:** 托管缓存使用追加式写入日志保存 write/truncate 与保存标记；不可变基础正文和日志区间共同提供读取，每个保存标记固定自己的内容。Git 是已归档历史正文、图片、版本比较与恢复的来源；同步 Store 和保存日志只承接活动同步及尚未归档的数据。

**Tech Stack:** Go、现有 Rclone VFS、bbolt、私有账号 Git 仓库。

**Spec:** [本地保存、Git 归档与云端同步](../specs/2026-11-13-local-save-pipeline.md)。

## 约束与边界

- 保留本地持久化错误；保存不能谎报成功。未缓存部分不能解释成零。
- 保存不调用云端、不复制或哈希整文件、不调用 Git。
- 连续保存的版本不能混杂；Git 不可用时保留待归档内容。
- 未处理日志、未上传修改、冲突不允许被移除挂载、缓存清理和升级误删。
- 兼容旧缓存和旧历史的外置资源引用；原生 Rclone 行为不变。
- 原子重命名覆盖必须遵守现有 namespace 事务的完整快照契约，不能绕过事务宣称成功；单独验证它与异步归档的交接。

## Review Focus

- 截断后扩展必须为零，不能重新暴露旧尾部。
- 保存后再次覆盖、重命名及强退不能改变已经接受的版本。
- 日志尾部不完整可恢复到提交边界；已提交内容损坏必须报错保留。
- 大正文与图片从 Git 流式读取，不能被命令输出的 32 MiB 上限截断。
- 归档持久化与回执顺序一致，垃圾回收不能删除未提交的 Git 对象。

## Task 1: Git 归档来源统一

- [x] 先增加二进制、图片、大正文归档及重开读取测试，验证旧行为失败。
- [x] `history/repository.go` 直接流式导入 Git blob，删除新归档的额外正文副本；`commit.go` 将正文和媒体 OID 都纳入 Git 树。
- [x] `history/query.go` 流式读取 Git 正文，旧 Asset 引用继续可读；`compact.go` 保留及压缩覆盖所有 OID。
- [x] 运行 `go test ./internal/bridge/history ./internal/bridge/manager ./internal/bridge/overlay`。

## Task 2: 托管缓存保存日志

- [x] 增加断网、后台阻塞期间连续保存、局部改写、截断、恢复与损坏测试，先验证失败。
- [x] 新建 `vfs/vfscache/managed_journal*.go`，实现追加数据和检查点、不可变区间读取、后台 staging 与恢复。
- [x] `overlay` 提供已缓存不可变基础正文的本地读取接口；`item.go` 与 `managed_flush.go` 接入仅本地保存。
- [x] 普通 Rclone 与未实现本地快照接口的 backend 保留旧路径。
- [x] 小范围运行新增测试，然后运行 `go test ./vfs/... ./internal/bridge/...`。

## Task 3: 生命周期与交接

- [x] 日志纳入在线/离线 pending、缓存字节、清理、关闭、重命名与 namespace 恢复。
- [x] 日志失败在状态中可见，后续写入不能覆盖未确认版本。
- [x] 验证完整相关模块、race 检测和 Windows 交叉编译；记录原子替换路径的实际边界。

## 执行记录

- 使用现有 checkout 的独立分支 `feat/local-save-journal`，不新建 worktree，不改动已安装应用和用户文件。
- 按用户偏好分块验证，不做重复的界面复核；本轮不推送、不打包。

- 完成：全相关模块 `go test ./vfs/... ./internal/bridge/... ./cmd/feishu ./cmd/desktop ./backend/feishu ./lib/larkcli ./fs/operations` 返回 0。
- 完成：`go test -race ./vfs/vfscache ./internal/bridge/history ./internal/bridge/integration ./internal/bridge/overlay` 返回 0；前端 20 项测试通过。
- 完成：Windows amd64 的缓存、历史、Store、overlay、manager 测试程序交叉编译通过，使用空执行器，不代表 Windows 实机测试。
- 回归：新建文件的零长度 truncate 曾绕过日志，已补齐 VFS 级失败→通过测试；新文件在 outbox 不可用时重启仍能读到本地保存。
- 回归：持久化基础引用防止跨重启 GC；尚未进入 outbox 的本地保存阻止云端刷新/删除推进基线；首次日志写入前提高 Store 最低读取版本到 3。
- 回归：测试缓存目录改用真实物理路径，保留生产环境的防符号链接策略。一个永不完成的目录刷新测试将本地事务截止时间由 100 ms 调整为 5 s；仍能检测错误等待目录网络，同时避免 race/并行磁盘负载造成误报。
- Ruling: 原子重命名、覆盖和删除保留完整本地快照交接 — 现有 namespace 事务依赖不可变完整输入，绕过会损害恢复一致性 — 大文件的原子保存仍可能等待本地整理，普通 Flush/Sync/Close 不等待此交接。
- Ruling: Git 保存已归档版本，活动同步和未归档正文仍保留在 Store — 上传回读、未确认版本及 Git 故障恢复需要独立的持久化交接 — 归档前存在临时缓存占用，不能直接删除所有 Store 正文。
- 最终状态：保留开发分支；不安装、不打包、不推送，未改动用户挂载盘内的真实文件。
