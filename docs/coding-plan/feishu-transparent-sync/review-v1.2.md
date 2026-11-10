# Task 0 独立契约与文档质量复核

结论：**Spec compliant；Code quality Approved（本次为文档质量批准）**。未发现 Critical、Important 或 Minor 问题。批准范围仅为 Task 0 的 v1.2 文档契约；不表示 T01–T13 已实施、真实云端能力已验证或平台可以发布。

## 范围与方法

- 工作区：`/Users/rrong/.codex/worktrees/5b46/drivebridge`。
- 基线：`b2271fdfda822ecf98ce7a321ddfa232dc8ccb66`；目标：`02437a6cedd322ae469d3027b6963f14d9edfa7f`。
- 阅读任务 brief、作者报告、preflight 及提供的 diff 包；审核设计、开发文档、实施裁决和状态记录的相关完整段落。历史 audit HTML/JSON 按任务约束作为原样复制的历史报告，不要求其旧结论动态改变；作者报告明确记录了七个文件的逐字节一致性检查。
- 模式：独立同模型上下文、静态契约复核。只读固定版本本地 CLI 源码以核对指定风险，没有进一步分派。
- 未运行产品测试、validator、真实账号调用或既有检查的重复验证。作者报告的退出码作为作者执行证据阅读，不冒充本复核执行结果。唯一写入是本报告。

## 需求与任务保留

R-01–R-13 均保留于 `docs/coding-plan/feishu-transparent-sync/design.md:34` 起的需求表。T01–T13 及其需求映射、文件责任与验证步骤仍在开发文档中；本次只修订受到审核影响的契约及必要可选模型/协调器扩展，没有把需求删除或改成“已实现”。

| 检查项 | 状态 | 依据 |
|---|---|---|
| 原生 Rclone/direct 兼容 | compliant | `design.md:290`、`development.md:228` 显式保留 direct 的既有配置/官方锁，不强制产品 authRef；managed 不可降级 direct。 |
| R-10 并发边界 | compliant | `design.md:259`、`implementation-decisions.md:3` 不承诺服务端 CAS/exactly-once；本机锁、版本重读与本机 ID 未被提升为服务端保证。 |
| 产品配置与产物不含凭据 | compliant | `design.md:108`、`:290`、`:298` 仅保存引用/验证身份，禁止 token/secret/device_code 进入产品配置、日志、浏览器；官方 CLI 管理秘密。 |
| unknown/partial/error 保留数据 | compliant | `design.md:263`–`:278`、`development.md:298` 未知写入先核验，歧义暂停；部分列表不替换完整快照、不推断删除。 |
| ApplyDoc 持久操作边界 | compliant | `development.md:165`–`:167` 保留方法签名，首版严格一个逻辑 mutation；T07 每项持久 RecordOperation 后回读/remap，cloud 不执行未记账批循环。 |
| T01–T13 与共享所有权 | compliant | `development.md:64`、`:108`–`:120`、`:391` 确立 T01 模型、T02 协议/接口、T08 实现、T09 组装；preflight 逐任务和全部任务对记录具体交接，PF-01–PF-10 没有被干净结论掩盖。 |

上述 compliant 指 Task 0 的规格保留和文档修订符合要求，不是产品实现状态；产品需求均仍待相应任务验证。

## 指定风险的定向核对

| 风险 | 裁决与证据 | 结论 |
|---|---|---|
| F01 实际凭据共享域 | `design.md:286`–`:290`、`implementation-decisions.md:11` 规定同 AppID+OpenID 共用 canonical root，不同用户分 root，brand 不分官方凭据锁；所有 managed child 先 gate，再 AppID/身份锁，覆盖自动刷新和 AppSecret/TAT，外部 CLI 留在明确恢复边界。固定 CLI 的 `internal/auth/token_store.go`、`token_lock.go`、`internal/keychain/keychain_darwin.go` 及 credential provider 源码支持“凭据键域与 configDir 锁域不同、环境 provider 优先”的修订理由。 | 文档闭合；真实旋转/生命周期仍属 LV-01，未假称已修复产品。 |
| 注册与用户授权 | `design.md:294`–`:298` 对应 `cmd/config/init.go` 的 `--new` 阻塞路径、保存/裸 JSON 输出后才 probe；用户授权对应 `cmd/auth/login.go:366` 的裸 device_authorization 事件及 `login_result.go` 的 authorization_complete/scopes。没有虚构注册 split/JSON flag，也不把敏感 device_code 放 argv。未知 AppID gate、最终退出码/身份/scopes、取消 reap 后解锁和重启残留核验均有明确所有者。 | 可实施；无需用户手工终端操作；CLI 输出夹具与真实流程仍需 T02/T08 验证。 |
| F02 操作能力与未知结果 | `design.md:261`–`:270` 每操作列 route/idempotency/reconciliation；固定 CLI `shortcuts/doc/docs_update_v2.go` 的参数/body 没有 client_token，append 映射 insert，与文档一致。`development.md:93` 明确逻辑操作由 T04 产出、路由/能力由 T07 在 RecordPlan 前补齐。 | 文档闭合；本机 ID 不被误用为服务端去重；原生 docx 逐方法证明前关闭。 |
| F03 精确大小与版本 | `design.md:274`–`:276` 将 raw history 定义为候选，presence/精确整数、only_tag=false、current 关联和不可变已知版本完整下载分别设门槛；请求回显不构成版本遵循证据。固定 CLI `shortcuts/drive/drive_version.go` 确有 only_tag=true 和 GetFloat 归零路径；raw API 入口存在，`internal/client/response.go:196`–`:197` 使用 Decoder.UseNumber，支持候选路线保留精确数字的实现基础。 | 文档闭合；不能证明 current 就 metadata_unknown，不伪造 0/空版本。LV-03 仍未验证。 |
| F05 完整分页 | `design.md:278`、`development.md:225` 强制单页 cursor 循环、逐页检查、重复 cursor 失败、最终页后原子发布。固定 CLI `internal/client/client.go:422` 附近后续页错误中断和 `pagination.go:60` 附近末页状态合并，支持禁用聚合完整性假设的理由。 | 文档闭合；错误/取消/部分列表保留旧完整快照。 |
| F04 嵌套 DTO | `development.md:226`、`implementation-decisions.md:14` 指定 data.document.content/document_id、presence 和必需 ID/版本拒绝；固定 CLI fetch/create 相关测试保留 document.content/document_id 嵌套形状。 | 已归入 T02 回归职责；未冒称本次实现或运行。 |

## 证据与放行边界

`implementation-decisions.md:25` 起列出 LV-01–LV-05 的动作、所有者和失败门槛；真实凭据、逐操作失败语义、current/version 关联、真实分页、固定发行二进制契约均保持 not-run。平台驱动、安装、签名和升级保留数据也明确未通过。本次可批准继续做配置、存储、codec、状态机和带失败门槛的协议实现；启用或发布对应能力仍须取得所列真实证据。

readiness/development-gate 不沿用历史 95/passed，当前 pending 和 critical_unknowns 与作者报告一致；require-ready/handoff 的预期拒绝被如实记录。旧 audit 的 open/needs-fixes 与 v1.2 文档裁决并不矛盾：前者是不可变历史事实，后者只解决设计取舍。

## 发现

Critical：无。Important：无。Minor：无。

本复核未发现需要先修改 Task 0 文档才能开始实施的缺口。真实能力未验证属于明确保留的实施/发布门槛，不应被改写成“产品已通过”，也不需要据此阻断无依赖的离线任务。
