# 官方 CLI 与 LarkFS 复用审核

审核日期：2026-11-10。对象：DriveBridge 设计/开发文档 v1.1，主仓库 HEAD `b2271fdfda822ecf98ce7a321ddfa232dc8ccb66`。这是方案审核，尚未实现产品功能。

**结论：可以简化飞书协议接入和授权编排；不能用现成同步命令替代 Markdown 协作引擎。当前方案需要先修订 4 项契约，再按任务实施。** 此前的 95 分是当时信息闭合度；本轮新增源码证据后，不能继续据此宣称当前方案已无关键未知。

机器可读问题、验证记录及独立复核见 [audit.json](audit.json)，离线可筛选页面见 [audit.html](audit.html)。HTML 中的“需修复/可合并”是通用审核模板用语，本次指设计能否进入实施，不是产品分支合并结论。

## 1. 本地源码和检查边界

| 仓库 | 本地目录 | 固定提交 |
|---|---|---|
| 官方 larksuite/cli | `third_party/larksuite-cli` | `1bfdcc2c1a0ed0296ddb6349d251bdd75145bc5a` |
| IchenDEV/larkfs | `third_party/larkfs` | `67b474915035b57f43926152caf323a3813e833a` |

两个仓库均为浅克隆；根 `.gitignore` 已加入 `/third_party/`。`git check-ignore` 命中，`git ls-files -- third_party` 为空。没有提交、暂存、推送第三方代码。复现时应检出上述提交，不能用后续 main 的行为替换本次证据。

检查包括官方认证/配置/凭据存储、错误信封、Drive 传输/同步/版本信息、Docx 更新、扩展接口，以及 LarkFS adapter/doctype/naming/cache/retry/mount/桌面入口和部分测试。没有穷尽两仓库；没有登录、读取真实凭据、调用飞书业务接口或修改云端文件。实际执行的离线测试见 JSON，不能据此声称真实挂载或在线协作通过。

官方检出源码 `package.json` 标注 1.0.99，与前轮实验的 CLI 版本号相同；本次没有证明该提交与已安装发行二进制逐字节对应。前轮真实接口实验仍是独立证据，本轮不把源码测试当作重跑了那些实验。

## 2. 官方 CLI 能省掉什么

| 能力 | 建议接入 | DriveBridge 仍负责 |
|---|---|---|
| 应用配置、device flow、用户 token、刷新及安全存储 | 后台调用官方 `config` / `auth`，产品仅编排与展示授权页面 | 每用户配置、产品 profile、验证身份、共享凭据生命周期、授权失败恢复 |
| 普通文件上传/下载、同 Token 覆盖 | 复用官方命令与已实测 `--file-token` 路线 | 本机持久快照、代次、备份、unknown 回读、上传核验 |
| 目录查询与分页 | 首版显式逐页；不把聚合成功当完整性证明 | DTO、稳定身份、异常/截断及重复 cursor 检测 |
| Docx 读取、XML 和块级修改 | 使用 `docs +fetch` / `+update` 作为协议适配，逐操作做 capability 表 | Markdown 子集、块对齐、三方合并、保真和冲突判断 |
| JSON 错误与限流信息 | 解析官方稳定 `type/subtype/code/retryable/retry_after_seconds` | 业务错误映射、脱敏、重试预算、判断写入结果是否明确 |
| 原生三平台可执行文件 | 由打包流程固定版本/校验和，后台调用绝对路径 | FUSE/WinFsp、进程管理、安装签名及升级保留队列 |

授权和协议复用在 v1.1 已有安排；本轮主要把边界补实，并非发现了可以删掉整套 T02/T08 的 SDK。`internal/auth`、`internal/core` 等 Go 包不能从 DriveBridge 模块直接导入。官方有公开 `extension/credential`、`extension/platform`，可做独立 wrapper，但它是 CLI 扩展机制，需要额外维护构建、状态和升级，并非可直接嵌入长期运行服务的完整 Drive/Docx SDK。首版继续用固定版本子进程适配最合适。

官方 npm 入口最后执行原生二进制。因此可评估直接打包该二进制以减少运行时依赖；这只是安装方案优化候选，当前 Windows 的 CLI/Node 打包约定没有在本次变更中修改，也没有验证无 Node 安装包。

分页还有一个必须防范的细节：官方聚合器在后续页发生传输错误时可能停止并返回已取页面；后续页业务错误还可能被格式化成成功且 has_more=false。`--page-limit 0` 只取消页数上限，不是完整成功证明。T02 首版采用显式 cursor 循环，逐页处理错误与重复 cursor；详细源码路径见 F05。

证据：[扩展入口](https://github.com/larksuite/cli/blob/1bfdcc2c1a0ed0296ddb6349d251bdd75145bc5a/extension/README.md)、[插件宿主边界](https://github.com/larksuite/cli/blob/1bfdcc2c1a0ed0296ddb6349d251bdd75145bc5a/extension/platform/README.md)、[错误契约](https://github.com/larksuite/cli/blob/1bfdcc2c1a0ed0296ddb6349d251bdd75145bc5a/errs/ERROR_CONTRACT.md)、[npm 原生入口](https://github.com/larksuite/cli/blob/1bfdcc2c1a0ed0296ddb6349d251bdd75145bc5a/scripts/run.js)、[分页错误处理](https://github.com/larksuite/cli/blob/1bfdcc2c1a0ed0296ddb6349d251bdd75145bc5a/internal/client/client.go#L422)、[聚合分页状态](https://github.com/larksuite/cli/blob/1bfdcc2c1a0ed0296ddb6349d251bdd75145bc5a/internal/client/pagination.go#L60)。

## 3. 两个容易误用的官方功能

### `drive +sync` 是双向文件同步，但不适合托管目录的主循环

源码明确实现两端扫描和同步，不能把它说成仅上传工具。但它只处理 `type=file`，跳过 Docx/Sheet/Base 等在线资源。任意两端内容不等即归为 `modified`，没有持久三方基线；默认 `remote-wins`，也提供 `local-wins/keep-both/ask`。`keep-both` 保留版本副本，没有内容合并。

精确比较需要下载远端文件计算哈希；`--quick` 改用尽力时间比较，即使字节相同也可能写入。它不能满足当前的按需访问、本机持久接受、代次确认和 Markdown→Docx 协作。可借鉴冲突/重复远端预检用例，或作为单独的普通目录导入导出工具；不与产品 worker 同时管理同一目录。

证据：[drive_sync.go:45–79、203–235](https://github.com/larksuite/cli/blob/1bfdcc2c1a0ed0296ddb6349d251bdd75145bc5a/shortcuts/drive/drive_sync.go#L45)。

### `markdown +patch` 不是在线 Docx 协同补丁

它下载普通 Drive Markdown 源文件，在本地执行字符串/正则替换，再同 Token 全量上传；不是按 Docx 块合并，也没有消除读后写竞争。可用于低层普通 `.md` 文件工具，不能替代 T04/T07。

证据：[markdown_patch.go:30–43、117–168、209–218](https://github.com/larksuite/cli/blob/1bfdcc2c1a0ed0296ddb6349d251bdd75145bc5a/shortcuts/markdown/markdown_patch.go#L30)。

## 4. 进入实施前需要修订的 4 项

### F01 · P1 · 配置隔离与官方凭据共享域没有对齐

设计 `design.md:108/261–263`、开发 T08 当前按每 profile 独立 CLI 配置目录/刷新锁编排。实际官方 UAT 按 `AppID:UserOpenID` 存储；macOS 位于当前 OS 用户下固定 service 目录，Windows 位于固定 HKCU 注册表。刷新文件锁却放在 `configDir/locks`。所以两个产品配置绑定同一身份时，可能共享 token 却使用不同跨进程锁；这是基于源码的竞争风险推断，没有在真实账号上制造刷新竞争。不同身份的 UAT key 不同，不应笼统称它们必然串 token；App Secret/TAT 另有 AppID 级共享域。

也不能直接改成“一个 root + 官方 profile”：官方 profile 是应用配置，同一 root 禁止重复 AppID，每应用配置只选一个用户，再次登录会替换该用户。

建议保留产品 profile 独立业务状态和 CLI config root；为同 OS 用户建立**按官方实际键定义的凭据协调域**：AppID 级配置/应用凭据协调、AppID+OpenID 级 UAT 协调。重复身份绑定去重或显式共享，跨 worker 的刷新/重授权/退出走同一协调锁及引用检查；普通业务数据仍按产品 UUID 分开。brand 留作身份校验，但官方存储键不含 brand，不能仅按 brand 分锁。同一账号还被外部独立 lark-cli 管理时，产品锁不能约束外部进程；需明确所有权/可检测的重新授权恢复边界，不能承诺完全隔离。

官方 API 子进程可能自动刷新凭据，因此协调应覆盖可能触发刷新的调用，不能仅在显式 login/refresh 时加锁。具体锁范围和并发代价需在 T02/T08 中定义并验证。

Runner 还需显式清理继承的 CLI 凭据、profile 和 workspace 覆盖环境，再注入派生配置；官方 env credential provider 优先于本地配置，`--as user` 只选身份类型。采用环境白名单并保留必要系统网络配置，身份未验证或不符即停止，不能带错身份请求后才发现。

验收补充：同 AppID 不同用户；同 AppID 同用户两个产品配置；并行过期刷新；任一引用仍有作业时 logout；继承了其他账号环境；外部 CLI 撤销/换账号。不能用 `TestProfileAuthDirectoryIsolation` 一个路径测试代替这些场景。

证据：[token_store.go:46–69](https://github.com/larksuite/cli/blob/1bfdcc2c1a0ed0296ddb6349d251bdd75145bc5a/internal/auth/token_store.go#L46)、[token_lock.go:37–43](https://github.com/larksuite/cli/blob/1bfdcc2c1a0ed0296ddb6349d251bdd75145bc5a/internal/auth/token_lock.go#L37)、[macOS 存储](https://github.com/larksuite/cli/blob/1bfdcc2c1a0ed0296ddb6349d251bdd75145bc5a/internal/keychain/keychain_darwin.go#L52)、[Windows 存储](https://github.com/larksuite/cli/blob/1bfdcc2c1a0ed0296ddb6349d251bdd75145bc5a/internal/keychain/keychain_windows.go#L24)、[官方 profile 限制](https://github.com/larksuite/cli/blob/1bfdcc2c1a0ed0296ddb6349d251bdd75145bc5a/cmd/profile/add.go#L108)、[登录替换用户](https://github.com/larksuite/cli/blob/1bfdcc2c1a0ed0296ddb6349d251bdd75145bc5a/cmd/auth/login.go#L645)、[环境凭据优先级](https://github.com/larksuite/cli/blob/1bfdcc2c1a0ed0296ddb6349d251bdd75145bc5a/internal/credential/credential_provider.go#L192)。

### F02 · P2 · client_token 要区分本机操作 ID 与服务端幂等能力

`design.md:190` 仅要求对已验证的可幂等块操作生成 client_token，这个原则没有错；但 T02/T07 尚未列出每种操作的传输路线/服务端支持。当前 `docs +update` 公共参数及 body 没有该字段。不能把本机持久化的 `Operation.ClientToken` 当成已经传到服务端，也不能凭它重放插入/创建。

建议给每种操作列出 `route / capability / idempotency / unknown reconciliation`：优先 shortcut 的 XML 块编辑；只有确认 raw API 支持且经过实验时才传服务端幂等键。其余操作使用本机 operation ID、提交后回读、块/内容定位和歧义暂停，保留已设计的逐操作 journal。若无法判断上次插入是否成功，保留本地待同步并转人工，不因 token 缺失而整批重试。模拟“提交成功、响应丢失”不能代替真实服务端幂等实测。

证据：[docs_update_v2.go:37–49、263–299](https://github.com/larksuite/cli/blob/1bfdcc2c1a0ed0296ddb6349d251bdd75145bc5a/shortcuts/doc/docs_update_v2.go#L37)。

### F03 · P1 · 版本历史的格式化大小不能无条件视为最新准确元数据

`development.md:205` 固定 `+version-history --limit 1` 并只检查 size_bytes 非负。源码请求强制 `only_tag=true`，且把缺失或非数值 size 通过 GetFloat 转成 0。适配层看到合法非负 0 时无法区分缺字段与真实空文件；源码也不足以证明 tag 过滤结果永远包含最新对象。

前轮 45/81 字节文件实验确实得到正确结果，本轮没有推翻该样本。需要去掉从该样本到所有文件的保证：在 T02 选定可保留原始字段及缺失状态的接口、核对过滤和最新版本语义；必要时使用官方 raw API，但同样要验证契约。大小未知应显式返回 unknown/error，不能冒充空文件。增加缺字段/错误类型/空文件/重命名版本/连续覆盖/过滤或乱序历史测试，缓存绑定已核验版本。

证据：[drive_version.go:68–76、111–133](https://github.com/larksuite/cli/blob/1bfdcc2c1a0ed0296ddb6349d251bdd75145bc5a/shortcuts/drive/drive_version.go#L68)。

### F05 · P1 · 聚合分页可能把后续页错误隐藏成成功

当前 T02 允许显式分页或 page-all 聚合。源码显示：`client.go:430–438` 遇后续页非零业务 code 会把错误页追加后退出；`pagination.go:60–78` 从最后错误页取不到 data/has_more 时设置 false，并复制第一成功页的 code=0；`service.go:765–778` 只检查聚合结果，可能输出成功信封。传输错误也可能返回部分页面，但不能据此认为检查最终 has_more 就足够。这是已核对的源码控制流风险，本轮没有执行该故障场景。

T02 首版明确使用官方单页命令，逐页校验业务错误、字段类型及 cursor；seen-cursor 检测重复，只有最终页明确完整才发布目录快照。任何页失败都保留旧快照并标记刷新失败，不能把缺失项当远端删除。增加第二页网络错误、第二页业务错误无 data/has_more、重复 cursor 及中途取消用例。聚合路线待其逐页错误保留机制经过验证或修复后再启用；exit=0、ok=true、has_more=false 不能单独作为完整证明。

证据：[后续页错误](https://github.com/larksuite/cli/blob/1bfdcc2c1a0ed0296ddb6349d251bdd75145bc5a/internal/client/client.go#L422)、[聚合覆盖字段](https://github.com/larksuite/cli/blob/1bfdcc2c1a0ed0296ddb6349d251bdd75145bc5a/internal/client/pagination.go#L60)、[聚合后的成功检查](https://github.com/larksuite/cli/blob/1bfdcc2c1a0ed0296ddb6349d251bdd75145bc5a/cmd/service/service.go#L765)。

## 5. LarkFS 值得参考的部分

| 原建议源码 | 可参考内容 | 必须调整或保留的边界 |
|---|---|---|
| `pkg/adapter/drive.go` | 类型分派、列表缓存、写后失效 | 不搬 ReplaceFile；其新建→删旧会改变 Token，删除失败还留下新资源 |
| `pkg/doctype/folder.go` | page-all/page-limit 0、分页状态 DTO | 未提供准确 size；has_more 仍需检查；缺时间不能用每次 time.Now |
| `pkg/doctype/types.go` | file/folder/docx 分类 | 首版三类用现有 Kind 即可；未知格式显式只读，不盲回退 |
| `pkg/naming/naming.go` | Token 与显示名分离、后缀在扩展名前、UTF-8 截断 | 补父目录命名空间、casefold/NFC、Windows 保留名、后缀再碰撞；持久映射放唯一 store |
| `pkg/cache/metadata.go` | TTL 与失效接口 | 仅可丢弃元数据缓存；不能用来保存待上传内容或版本基线 |
| `pkg/cli/executor.go` | Runner 抽象、argv 调用、stdout/stderr 分离 | ctx 有值也要有超时；绑定身份环境；验证成功信封；不在错误中记录完整正文 argv |
| `pkg/cli/errors.go` | 结构化错误→业务错误 | 以官方稳定 wire 字段为准，保留细分及 unknown，不做正文匹配主判据 |
| `pkg/errors/retry.go` | 有上限退避的基本形状 | 不复制超时/5xx 全局自动重试；按操作重放安全性及 Retry-After 调度 |
| `pkg/doctype/file.go` | 传输命令封装、fake Runner 测试 | 既有普通文件 Write 返回只读；同 Token 覆盖继续用 DriveBridge 已验证路线 |
| `pkg/vfs/operations_control_replace.go` | 显式返回 token_changed 的可观测性 | upload_delete 是用户可见替换操作，不能变成透明保存 |
| `pkg/doctype/registry.go` | 未来资源处理器注册方式 | 当前无需整套 registry/composite token 框架 |
| `pkg/doctype/drive_delete.go` | 资源类型与删除路由 | 托管删除仍走现有 tombstone/journal 和安全策略 |
| `pkg/cli/parser.go` / `response.go` | 解析代码组织与样本测试 | 不能假定所有官方命令同 JSON 形状；缺必要字段应报协议错误 |
| `pkg/adapter/wiki.go` | Wiki 与 Drive 路由分离 | 留第二阶段，不扩当前范围 |
| `pkg/doctype/docx.go` | `.md` 投影的接入形状 | fetch Markdown→overwrite 没有保真/块基线/合并；旧平铺 JSON 解析已复现不兼容，见 F04 |
| `pkg/doctype/sheet.go` / `bitable.go` | CSV/JSONL 映射的未来参考 | 留第二阶段，不进入首版依赖 |

还可借鉴其桌面 `doctor/status` JSON→状态页模式；不搬它的 SwiftUI/打开终端初始化流程，当前需求需要应用内配置。它的 FileProvider 扩展写操作仍为只读错误，CI 构建也没有 Windows；不能作为双向同步或三平台挂载已完成的证据。

尤其不要复用以下持久性假设：FUSE handle 在内存缓存写入并在 Flush 直接访问远端；未物化/空内容 Getattr 可返回 4096；ContentCache 没有待上传引用保护及崩溃恢复索引。它们不等价于当前的 durable save/blob+journal/VFS 空闲发布屏障。

证据：[Drive adapter](https://github.com/IchenDEV/larkfs/blob/67b474915035b57f43926152caf323a3813e833a/pkg/adapter/drive.go#L126)、[Docx handler](https://github.com/IchenDEV/larkfs/blob/67b474915035b57f43926152caf323a3813e833a/pkg/doctype/docx.go#L26)、[命名](https://github.com/IchenDEV/larkfs/blob/67b474915035b57f43926152caf323a3813e833a/pkg/naming/naming.go#L35)、[执行器](https://github.com/IchenDEV/larkfs/blob/67b474915035b57f43926152caf323a3813e833a/pkg/cli/executor.go#L40)、[FUSE 缓冲和 Flush](https://github.com/IchenDEV/larkfs/blob/67b474915035b57f43926152caf323a3813e833a/pkg/mount/fuse.go#L433)。

### F04 · P1 · 不直接移植 LarkFS 的 Docx JSON 解析

已用 fake Runner 注入当前官方 `data.document.content` / `data.document.document_id` 形状，直接调用 LarkFS 公共 DocxHandler。读取非空正文得到空字节且无错误；创建得到空 Token 且无错误。两个保真断言均失败，退出码 1；这证实参考适配器不能直接复用，不是 DriveBridge 当前代码的新增缺陷。官方源码及前轮已保存的真实响应共同支持输入形状，本轮测试没有访问远端。

原因是 LarkFS 仅解析 `data.markdown/content/text` 和 `data.document_id/doc_id/token` 的平铺字段。T02 应写当前契约 DTO，并将缺少必需字段作为协议错误，不能视为空文档。复现材料保存在 [evidence](evidence/)；修订设计时保留此回归用例。

## 6. 对现有 13 个实施任务的影响

| 任务 | 建议调整 |
|---|---|
| T01 配置/模型 | 保留产品 UUID；新增官方凭据协调域模型，不另建 LarkFS namemap/config 真源 |
| T02 CLI/元数据 | 最主要的薄适配工作；直接消费官方命令/错误契约；补 F01 环境边界、F02 capability 表、F03 准确大小路线及 F05 显式逐页 |
| T03 持久状态 | 保留 blob+bbolt、双 checkpoint、create/namespace journal；不引入第二套 LRU 待上传仓库 |
| T04 Markdown 合并 | 保留子集 codec、块对齐、保守 merge 和 XML 局部补丁；不搬整篇覆盖 |
| T05 overlay | 保留本机接受语义；只参考命名/类型，不引入第二个 adapter 树 |
| T06 VFS | 保留 Flush/Sync 持久性、空闲发布、替换保存；不增加 go-fuse/WebDAV/FileProvider 挂载栈 |
| T07 同步引擎 | 保留逐 entry 状态机与 unknown 回读；业务重试只有一个预算，不叠加参考库全局 retry |
| T08 授权 | 官方 CLI 负责凭据实现；产品处理引导、验证身份、共享凭据引用与锁；按 F01 补测试 |
| T09 管理命令 | 保留后台 worker/真实 ready/IPC；可参考 status DTO，不照搬 Unix fork 或 PID=健康 |
| T10 UI | 保留应用内引导、弹窗和三方 diff；复用管理服务，不增加 Swift 桌面依赖 |
| T11 迁移 | 只迁移 DriveBridge 既有状态；不为参考项目再新增兼容格式 |
| T12 打包 | 固定官方 CLI 二进制与校验和；可评估减少 Node，三平台驱动/签名/队列保留门槛不变 |
| T13 验证 | 参考 fake Runner/临时脚本；增加本轮 4 项契约用例和 F04 兼容回归；真实授权/挂载/故障仍须实测 |

建议实施次序先做 T01/T02/T08 的边界验证，再进入依赖它们的队列/同步任务。此次不重写已批准设计、不改产品代码；本报告作为 v1.1 后续修订输入，旧 passed/readiness 记录保留为历史。

## 7. 验证与使用约束

主审和两个同模型新上下文分别检查官方 CLI、LarkFS 原始源码，再交叉核对问题；没有跨模型或外部人工审批。执行了选定纯函数、mock Runner 和临时脚本测试；缺依赖的测试如实记为未执行。没有将测试通过外推为服务端 CAS、幂等、跨平台或协同编辑可行性证据。

两个仓库 LICENSE 均声明 MIT。若后续实际移植代码，保留对应版权和许可通知；本轮只检查了声明文件，没有进行全量依赖许可审计。当前没有引入任何第三方源码作为产品依赖。
