# v1.2 实施裁决与能力门槛

日期：2026-11-10。状态：文档裁决完成，作者自查完成，独立复核 pending；产品能力未实现、未实测。本记录关联 [design.md](design.md) 和 [development.md](development.md)。用户已授权开始开发与多 agent，本次 Task 0 只修订文档。R-01–R-13、T01–T13 和公共方法签名保留；仅增加明确所有者的可选 Operation/能力字段。并发始终是 R-10 的尽力检测/合并/恢复，无服务端 CAS 或 exactly-once 承诺。

## 审核处置

[audit.json](audits/2026-11-10-reference-reuse/audit.json) 和同目录所有证据保持原样；其中 open/needs-fixes 是 v1.1 当时结论。本记录只关闭设计取舍，不能把历史 finding 改成 fixed，也不表示产品已经修复。

| ID | v1.2 文档裁决 | 所有者 / 消费者 | 实施与发布仍须验证 |
|---|---|---|---|
| F01 | 产品 UUID 业务状态隔离；相同 AppID+OpenID 复用 canonical auth root，不同用户不同 root。官方键无 brand；所有可自动刷新的 child invocation 使用 AppID→AppID+OpenID 跨进程文件锁（前置 OS 用户 enrollment gate：未知 AppID 注册独占，全部其他托管 child 共享），AppID 级独占保守串行，覆盖 AppSecret/TAT。仅存引用/验证身份；过滤 CLI 覆盖环境；外部独立 CLI 异常须显式恢复 | T01 定义引用/身份 DTO；T02 定义 coordinator/enrollment/流式授权接口及白名单事件 DTO，Runner 环境/注入边界；T08 实现注册表/锁/生命周期；T09 组装；T11 迁移；T13 验收 | 离线并发/环境故障用例；LV-01 真实旋转/撤销及系统凭据域证据 |
| F02 | 每种操作按设计 6.4 选路线。docs_ai XML shortcut 没有 client_token；本机 Operation.ID 仅 journal 身份。unknown 先回读，唯一证据才确认，歧义 paused/conflict，不盲重试。原生 docx 每方法验证前关闭 | T01 规范模型可选字段；T02 传输/能力 DTO；T04 逻辑操作；T03 原样持久；T07 路由/回读/恢复 | LV-02 每个启用操作的真实结构/失败语义，单方法的 token 证明不能扩展到其他方法 |
| F03 | 不再使用 shortcut limit1+非负值证明当前大小。候选 raw history 保留 presence/精确整数，only_tag=false 仍待验证；须有已验证 current-version 关联。否则仅在证明版本遵循后完整下载已知 V；无法证明 current 就明确 metadata_unknown | T02 协议/元数据；T05 消费准确大小；T07 版本核验；T13 实验 | LV-03 当前版本来源、过滤/排序、空/缺字段和不可变版本下载证据；version-get 的请求回显不构成证据 |
| F04 | 不移植 LarkFS 平铺 Docx 解析。固定 data.document.content 与 data.document.document_id 嵌套 DTO；presence 区分合法空正文与缺字段，缺必需 ID/版本拒绝 | T02 两条嵌套回归/必需字段用例；T13 固定发行版契约 | LV-05 固定 CLI 发行二进制的响应夹具与版本/校验和；本轮不修第三方代码 |
| F05 | 首版只用单页 cursor 循环。每页校验进程/信封/业务码/items/has_more/cursor；末页成功才提交完整 namespace snapshot，任何页失败保留旧快照且不能据部分列表删除 | T02 逐页 DTO/循环；T05 完整列表视图；T07 刷新与删除抑制；T13 验收 | 第二页错误缺 data、断网、取消、重复 cursor 的离线故障测试；LV-04 真实多页边界 |

首次配置/授权裁决：支持的 `config init --new --brand` 是阻塞注册，无 split flag；AppID 未知时取得全局 enrollment gate 独占，全部 managed 调用先在共享侧参与。T02提供流式 stderr URL 提取及脱敏最终裸JSON、Wait/CancelAndWait 句柄；T08保留内存flow。用户授权选 retained `auth login --scope ... --json` 裸NDJSON事件，避免 --device-code argv-only；不加 --as。最终事件/配置保存可能早于失败退出，必须收割退出码并验证身份/scopes；取消先reap再解锁，残留状态unknown核验，注册无服务端expiry时明确未知（本机10分钟上限另标）。详见设计7.1.1。

首版 `cloud.ApplyDoc(ops[])` 保留签名，但只接受一个逻辑 mutation，零项参数错误、多项 unsupported_batch；T07 每项完成后 RecordOperation 持久、回读/remap，再下一项，cloud 不循环未记录的批操作。

T02 不依赖尚未实现的 T08：协调器接口在 lib/larkcli 定义，离线注入 fake；T08 实现，T09 生产组装，managed 生产缺协调器则拒绝访问凭据。文件锁实现可在 T08 增加 coordinator.go，属于必要文件扩展，不改变公共业务方法。共享 Runner 显式区分 managed 与 legacy/direct：原生 feishu: 保持既有 CLI 配置/官方锁，不要求产品 authRef，不静默迁移或共享 canonical root；其凭据生命周期视同外部 CLI 的产品锁外操作。worker 内若复用原生适配仍属 managed，必须注入协调器，缺失不能降级 direct。T01 的 model.Operation 新增可选 ID/Route/CapabilityID，model.OperationCapability 为可选扩展；cloud.ApplyResult 可选 Capabilities 使用该规范模型，lib/larkcli 协议类型仍独立，不引入 internal 依赖。T04 只产出逻辑操作，T07 派发前补齐并持久化路由，避免 codec 硬编码 CLI。

## 精确的剩余真实发布验证

本轮以下全部 **not-run**。执行条件：用户明确授权隔离账号、目录及远端变更；记录固定发行 CLI SHA-256/版本、OS、合成输入、脱敏请求/响应和实际断言。离线测试可验证本机处理分支，不能替代服务端能力证据。

| 验证 ID | 动作和必须证明的结果 | 任务 / 失败门槛 |
|---|---|---|
| LV-01 | 未知AppID注册独占gate、取消先reap、保存后probe失败/缺scopes事件仍失败、重启残留核验；同 OS 用户下同 AppID+OpenID 两 profile 复用 auth root；同 AppID 两用户 root 分开；并行过期 token 刷新、AppSecret/TAT 写/重新授权、pending 引用 logout；brand 不分锁；继承他人环境不改变身份；独立 CLI 撤销/换账号后暂停并显式重验恢复。验证真实 keychain/HKCU/Linux 对应存储及跨 worker 文件锁 | T08/T09/T13；未证明的系统凭据生命周期不标平台发布通过 |
| LV-02 | 分别对 XML replace/delete/insert/append（若启用）证明输入、非目标保真、回读定位与部分成功。以真实传输故障模拟提交后丢响应；重复内容必须暂停。若选择原生 docx，逐方法验证参数位置/块转换/token 重放作用域；不得从旧 revision 被接受推导 CAS | T02/T07/T13；每种能力单独关闭/开放，歧义不自动重发 |
| LV-03 | raw history 的字段 presence/精确数值、only_tag=false 是否接受、过滤/排序/分页；确认当前 head 来源并与 size/version 关联；重命名、连续覆盖、真空文件、乱序/tag-only 场景；已知 V 版本下载证明返回该版本不可变完整字节，而非响应回显 V | T02/T05/T13；无法证明时 metadata_unknown，阻止相关访问/写入，不伪造 0/空版本 |
| LV-04 | 真实多页目录完整性/参数/cursor 末页语义；同时以可控离线故障分别注入后续业务错误无 data、传输失败、取消、重复 cursor，验证保留旧完整快照和零删除推断。真实试验与故障注入分别记录 | T02/T05/T07/T13；失败刷新不可提交部分列表 |
| LV-05 | 固定 CLI 发行二进制与校验和；读取/创建响应的嵌套 document 内容/ID/版本及合法空正文；字段缺失明确错误，保留旧实验与新能力证据的版本边界 | T02/T12/T13；版本升级重跑契约夹具和受影响真实能力 |

另须执行既定 T06/T12/T13 的各发布 OS/架构真实驱动 close/fsync ENOSPC/EIO、安装/签名/升级保留队列、桌面及无桌面 diff 验收；本轮无新证据。上述门槛允许配置、存储、codec、状态机及其他已验证能力继续开发，不能用于宣称整个云端能力已放行。

## 证据与当前状态

只运行本地文档格式/绑定/引用/范围检查；命令、退出码见 [document-validation.json](document-validation.json) 与执行账本 `.superpowers/sdd/development/task-0-report.md`。历史 v1.1 的 95 分和 passed 不再代表当前版本；新的 readiness/development-gate 明确保留未知，require-ready 预期拒绝。独立复核由主协调者安排，本 Task 0 不冒充已复核通过。
