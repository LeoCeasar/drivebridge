# v1.1 历史文档复核记录

当前 v1.2 已修订契约，独立文档复核 **已通过**（见 [review-v1.2.md](review-v1.2.md)，云端/平台能力门槛仍未通过）。以下是 v1.1 历史记录，不适用于 v1.2；当前裁决见 [implementation-decisions.md](implementation-decisions.md)。

日期：2026-11-10。对象：design.md v1.1 和 development.md v1.1。结论：文档契约已完成自查、独立上下文复核和受影响协议复验；产品代码及计划中的行为测试尚未实施。

## 模式与范围

- 主作者自查：需求覆盖、源代码边界、状态机、任务依赖、共享接口、验证条件和引用。
- 实施者复核：`/root/builder_check`，同模型、独立上下文（fork_turns=none）；读取两份原始文档及相关真实源码，不只阅读作者摘要。
- 首轮文档摘要见 [review-initial-digests.json](review-initial-digests.json)。复核者没有修改产出物或派生复核者。
- 首轮提出 3 个关键问题；修订后复验发现 1 个调度窗口；再次修订并针对性复验后没有重要遗漏。没有用多个赞成意见代替证据。
- 核查模式为文档/源码静态边界审核；未跑新增产品测试。文档 review_status=passed 不等于实现 ready。

## 意见、核验与处置

| ID | 原始观察与影响 | 核验依据 | 文档处置及复验 |
|---|---|---|---|
| RV-01 | 通用 Move 先删目标，仅在 overlay.Move 里保 Token 不足；source o==nil 也可能绕过事务 | `vfs/file.go` 调 `operations.Move`；`fs/operations/operations.go` 在调用后端 Move 前 DeleteFile(dst) | 成立。设计6.3、T05/T06 增 ManagedRenamer、通用 Move 前拦截、源/目标本机快照、namespace journal；明确不调用目标 Remove，覆盖 o==nil。首次复验确认主体关闭 |
| RV-02 | 把持久错误放在 Release，真实编辑器可能收不到；仅测 Go Close 不够 | `vfs/read_write.go` Flush 原先返回成功；`cmd/mount2/file.go` 注明 Release 返回被内核忽略；主作者还核实 `cmd/cmount/fs.go` Fsync 是 no-op、Item._save 未同步元数据 | 成立。设计6.1、T06/T13 明确 Flush/Sync、mutation/durable seq、原子元数据、cmount 托管转发和真实 close/fsync ENOSPC/EIO 验证；未证明 NFS 稳定写入时托管只读。复验确认关闭 |
| RV-03 | Store 契约缺创建 entry/命名空间/tombstone/create journal；普通首次 upload 不是独立创建空资源 | 本文首轮 Store 仅有已存在 Entry 的 Stage；原 `backend/feishu` 的 upload 同时上传并创建 | 成立。设计5.2/6.3、开发3.3/T03/T05/T07 增 NamespaceOp/CreateOp、EnsureEntry、namespace 事务和 PrepareCreate/BindCreated/FinishCreate；普通首次上传真实输入及唯一临时名。复验确认关闭 |
| RV-04 | Flush 先 Stage 源，立即入队的引擎可能在 rename 事务前创建临时资源；VFS admission 不约束网络调度 | 首次修订的 Begin/Flush 顺序尚无 Store 调度冻结；根据设计“入队立即调度”可构造确定窗口 | 成立。Begin 在 Flush 前 PrepareNamespace 冻结，Job.Prepare 与冻结同事务；源 merging/uploading/unknown 时 busy；Seal 固定代次，Finish 后解冻；重启先 namespace。T06 增 barrier 唤醒引擎及 unknown 重启用例。最终复验确认关闭 |

这些“关闭”指实施所需决定与接口闭合，未声称测试已执行或代码缺陷已修复。

## 主作者补充自查

- SC-01：无 mount worker 时首次配置页无宿主。已明确每用户 `feishu gui` host 独立承担 config/auth，挂载数据只走 worker IPC；关闭 host 不停止挂载。Windows 不在 SYSTEM/session 0 创建用户盘符。
- SC-02：在线 Markdown 投影字节大小不能从普通文件元数据获得。已明确未物化为 -1、目标 Getattr/Lookup/Open 经 ManagedMaterializer 取得准确大小、readdir 不取所有正文，并补内核零大小 EOF 的行为测试。最终复验认为与原范围一致。
- SC-03：配置字段和 CLI 覆盖参数需统一。已在设计5.1 定义 JSON，使用 --bridge-config 避免与 Rclone --config 冲突，T01 对应落实。
- SC-04：零字节普通文件可能被服务端拒绝。已规定本地保留和 blocked/empty_file_unsupported，不解释为同步成功；后续真实能力验收。
- SC-05：真实 +version-history limit=1 返回最新 size_bytes=81；补证据35，与 limit=2 和下载大小一致。只用于大小元数据契约，不声称全历史分页完成。
- SC-06：g1 上传期间保存 g2，LocalCheckpoint 与 RemoteCheckpoint 分开，Prepare 重绑定、Confirm 不清新代次、Publish CAS；计划包含相应确定时序用例。复核者未发现需新增用户决定的重要遗漏。
- SC-07：任务不能预先依赖后续才创建的 GUI/集成包。已将 feishu gui 的具体入口归 T10，T09 仅提供服务/IPC；T12 自有 smoke 且只测已存在的包，T13 后补 integration CI，保持任务依赖无回环。

## 覆盖与实际验证

需求 R-01–R-13 均映射到开发任务；T01–T13 均有依赖、文件、接口、行为用例、命令和完成条件。首版语法、默认参数、内部 overlay、发布屏障、三方合并页等标为工程决定，用户可以在实施前审阅。

已执行：独立测试账号/目录上的真实接口实验、观测断言、文件大小元数据探测、文档记录格式/引用/摘要校验。

尚未执行：所有新增产品行为测试、故障注入、真实三平台挂载、桌面通知/diff、升级迁移、大文件/零字节远端能力及签名发布。不得将这些未执行项包装为通过；对应任务及发布门槛保留。
