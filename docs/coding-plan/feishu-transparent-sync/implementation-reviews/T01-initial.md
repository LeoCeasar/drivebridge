# Task 1 / T01 独立审核

## 结论

- Spec verdict：**Issues**。规范模型、JSON 字段和确认的公开 API 已实现；Windows 默认目录组合无法保存配置，Windows junction 祖先还可绕过递归/引用路径预检。
- Quality verdict：**Needs fixes**。F1 为 P1 阻塞问题；F2 为 P2，应在基础路径层修正后再交付给后续任务。
- 本次为独立上下文的同模型源码审核；没有派生代理，没有修改产品代码、索引或 HEAD。只写本报告。

## 范围和依据

工作树：`/Users/rrong/.codex/worktrees/5b46/drivebridge`。

基线：`01b729faa684ed6d93f95690f259c0d931c53300`；目标：`50bd47b886cc4d9201c51a571ba158644f96bce1`。

读取 `.superpowers/sdd/development/task-1-brief.md`（包含 API、descriptor 和责任边界澄清）、`task-1-report.md` 及提供的 `review-01b729faa..50bd47b88.diff`。实际逐文件检查全部 11 个变更文件：`config/{config.go,paths.go,config_test.go}`、`model/{types.go,types_test.go}`、`platform/{paths.go,paths_darwin.go,paths_linux.go,paths_windows.go,paths_unix.go,paths_test.go}`。适用路径上的 AGENTS.md 检查没有返回额外规则；初始 `git status --short` 无输出。

没有扩展到 backend/store/CLI/UI，也没有检查旧验证工具目录。为两项具体 Windows 风险，额外只查本机 Go 1.25.6 标准库的 `os/file.go`、`os/stat_windows.go`、`os/types_windows.go`；项目 `go.mod` 要求 Go 1.25.0。该源码检查用于确认 API/模式语义，不是 Windows 原生实测。

## Findings

### F1 — P1：Windows 默认状态目录位于默认缓存根内，所有正常配置保存被拒绝

位置：`internal/bridge/config/paths.go:30`；相关位置 `internal/bridge/platform/paths_windows.go:12`、`internal/bridge/config/config.go:236`、`internal/bridge/config/config.go:138`、`internal/bridge/config/config.go:380`。

普通 Windows 环境中，`os.UserCacheDir()` 返回 `%LocalAppData%`，`KnownFolderPath(FOLDERID_LocalAppData)` 返回相同当前用户目录。因此当前默认值为：

```text
CacheDir = <LocalAppData>/DriveBridge
StateDir = <LocalAppData>/DriveBridge/State
```

`DefaultPaths()` 只检查绝对路径，所以返回成功；但 `Validate` 无条件拒绝状态和缓存根包含关系。结果是首次 `Save("", Default())` 即返回 `state and cache directories overlap`，带挂载的正常默认配置也一样；已存在的 schema-1 默认配置在 `Load` 的最终校验时同样被拒绝。缺失配置的 `Load` 提前返回默认值，不能证明其可以保存。

这不是待测 ACL 或驱动行为，而是默认值与纯路径校验的确定冲突。证据是 Go 1.25.6 `os/file.go:511-515`、本任务 Windows `StateDir` 和重叠判定。不能仅删除重叠校验：保存先认领并创建 State，随后认领品牌缓存父目录时，`claimPrivateRoot` 又会把已有 State 子目录当成未认领的占用内容拒绝；同时独占整个品牌父目录过宽。

处理建议：由控制器确认统一、独立的可淘汰缓存子目录（例如品牌目录下 Cache，与 Windows State 同级），保持持久 VFS 在 State 下；同时更新相应路径契约和精确默认值测试。保留对用户覆盖路径的状态/缓存递归冲突拒绝。

核验：源码确定；未执行 Windows 二进制。阻塞：是。状态：open。

### F2 — P2：Windows junction 祖先绕过“无符号链接”检查，可隐藏真实目录重叠

位置：`internal/bridge/platform/paths.go:48`；相关位置 `internal/bridge/config/config.go:232`、`internal/bridge/config/config.go:255`、`internal/bridge/config/config.go:279`、`internal/bridge/config/paths.go:87`。

`CheckNoSymlinks` 只拒绝 `info.Mode() & os.ModeSymlink != 0`。Go 1.25 Windows 实现仅给 `IO_REPARSE_TAG_SYMLINK` 设置 `ModeSymlink`，给 `IO_REPARSE_TAG_MOUNT_POINT`（junction）设置 `ModeIrregular`。所以一个 junction **祖先**不会被此检查拒绝。把最终 State 目录放在 junction 的普通子目录内，也不会触发 `EnsurePrivateDir` 对叶子 `IsDir` 的拒绝。

例如真实挂载目录为 `<root>/mount`，另有 `<root>/alias` junction 指向它，覆盖 `state_dir=<root>/alias/state`。字符串重叠检查认为它与 `<root>/mount` 分离，检查祖先又接受 junction，实际却会把持久状态写到挂载树内。同样，身份路径中 `auth` 或 `identities` 的 junction 祖先可以让受限制的相对 `auth_ref` 解析到私有状态根之外。此处不需要攻击者在检查后替换路径，也不依赖尚未实现的 T09 运行时占用判定；是现存别名路径的结构预检缺口。

证据：本机 Go 1.25.6 `os/types_windows.go:149-156,177-229` 与 `os/stat_windows.go:130-141`，结合本任务按 `ModeSymlink` 过滤和仅按词法路径判重的实现。没有声称在本机执行了 Windows junction 复现。

处理建议：Windows 私有状态/缓存/身份路径的每个现存组件应拒绝 name-surrogate reparse points，或使用可靠的规范化目标路径并重新验证根包含关系。挂载盘符根应保留独立、仅结构性的处理，避免把正常活跃盘符挂载当作私有路径链接拒绝。补上 junction 祖先和身份根逃逸的 Windows 原生用例。

核验：Go 类型映射和代码接受路径已作源码核验；原生 junction 端到端复现未运行。阻塞：此基础层交付前应修复。状态：open。

## 逐项规范对照

| 要求 | 状态 | 实际依据 |
|---|---|---|
| 单一 canonical model、固定 Config/Profile/Remote/Mount JSON | implemented | `model/types.go:49` 起；核心 DTO 与 brief 字段/类型逐项对照，无相邻实现副本 |
| 所有指定状态、kind、可 errors.Is 的规范错误 | implemented | `model/types.go:19-47` |
| Local/Visible/双 checkpoint、Job/Conflict/Namespace/Create 恢复引用 | implemented | `model/types.go:106-189`，未合并或丢失引用字段 |
| Operation.RawMessage 及可选 ID/Route/CapabilityID | implemented | `model/types.go:193-209`；缺省不授予派发能力，运行时 gating 留给 T02/T07 |
| OperationPayload、三个逻辑 kind、DocView 可选 source/reference context | implemented | `model/types.go:224-231,276-288`；保留原始载荷，不加入未验证传输行为 |
| `DefaultPaths() (Paths,error)`、`Load(path) (Config,error)`、`Save(path,c) error` | implemented | `config/paths.go:17`、`config/config.go:50-63`；WithPaths 为补充接口 |
| 无默认实验账号/路径/凭据；产品 flag 为 bridge-config | implemented | 空业务默认配置与参数默认值，`config/config.go:23-37`；实际 CLI 注册不属 T01 |
| 当前 OS 用户路径、profile UUID 与 mount UUID 命名空间 | partial | 路径来自用户 API，UUID 派生目录不使用名称；Windows 默认缓存/状态组合有 F1 |
| 持久 VFS 与可淘汰下载分离 | partial | `config/paths.go:61-62` 的 VFS 属 State；Windows 根布局有 F1 |
| UUID、重复 ID、profile/remote 引用、挂载参数 | implemented | `config/config.go:183-229,240-275`；身份未授权空引用被明确允许 |
| 相对身份引用、验证 DTO、同 AppID/UserOpenID 唯一 canonical root | implemented（路径别名另见 F2） | `config/paths.go:73-94`、`config/config.go:296-323`；身份键不含 brand，绑定时比对 brand/hash/ref，失败返回 ErrIdentityChanged |
| 覆盖路径私有性、独占性、挂载递归预检 | partial | ownership marker、锁、占用拒绝已实现；Windows junction 别名缺口 F2 |
| 未来 schema 只读、Save 加锁后重读 | implemented | `config/config.go:81-90,128-136`；拒绝未来和损坏旧文件 |
| 原子私有保存 | implemented（原生 Windows 语义待平台 gate） | 同目录临时文件、0600/受保护 ACL、文件 Sync、替换、目录同步；Unix SyncDir 错误传播；Windows write-through 与目录同步 no-op 均如实说明 |
| Windows Known Folder 与 ACL、Linux XDG | implemented（原生运行待验） | 三平台 StateDir 和受 build tag/文件名约束的 Windows API，未将 cross-build 当作原生证明 |
| 测试覆盖与报告 | implemented，历史执行结果非本审核重新实测 | 真实 TempDir/JSON、未来 schema、持锁重读、并发 JSON、权限、隔离、递归、身份、默认值、operation JSON 测试均已阅读；未覆盖 F1/F2 |
| 范围边界：无登录/令牌/进程/挂载/业务存储实现 | implemented | 全部变更局限 model/config/platform |

## 进一步核验与非问题裁决

- 已检查身份 canonicalization：注册表以 `(AppID, UserOpenID)` 为键，brand 不分割 auth root；重复身份必须复用既有记录，UUID 重复按不区分大小写拒绝；不同身份的 hash 冲突拒绝。没有发现第二份逻辑身份根或误把 brand 当凭据锁命名空间的问题。F2 是路径层问题，不能由 registry 字符串唯一性解决。
- 已检查活跃 Windows 挂载风险：裸盘符会先规范为根再参与路径比较；配置代码没有空目录/可写性/盘符占用要求。`CheckNoSymlinks` 仍会调用 `Lstat` 并传播非 NotExist 错误，但未取得 WinFsp 原生盘符返回行为证据，故不把“正常活跃盘符必定失败”列为缺陷，也不宣称已经验证其可用。后续 T09 原生用例应包含配置 Load/Save/Validate 在已启动盘符上的往返。
- 没有把 `BridgeConfigFlag` 常量当作已注册 CLI flag，也没有要求 T01 实现授权、registry 持久化、运行时派发、blob/GC、进程环境清理或 bbolt worker 锁。
- Save 可能在配置发布前留下锁/新目录/ownership marker；目录 Sync 失败时替换可能已经可见。实现报告已明确这些语义，未声称消除了 adversarial TOCTOU 或实现跨层事务，本审核不据此另立推测性问题。

## 验证边界

按派发要求，没有重跑报告中已通过的测试、race、vet 或九项 cross-build，也没有运行全量 upstream 测试。未新增或修改测试。实际新执行的核验是目标源码/补丁及上述 Go 标准库的读取、初始工作树状态检查、`go env GOROOT`（返回本机 Go 1.25.6 路径）。报告中的历史红/绿输出和最终成功命令只能作为提交者记录；本审核没有独立历史终端日志可确认其执行顺序，因此不把这些命令标为本轮实测通过。

Windows 原生 Known Folder、DACL、MoveFileEx 的文件系统语义、活跃驱动盘符、junction 复现，以及 Linux 原生 XDG/权限/目录同步均未在本轮执行。此限制保留为平台 gate 的待验项，不自动转成缺陷或批准依据。F1 的默认路径自相矛盾与 F2 的 Go 模式过滤缺口均有具体源码依据；质量结论由这两项实现问题决定。
