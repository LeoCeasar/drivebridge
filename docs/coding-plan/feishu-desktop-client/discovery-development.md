# 自动发现与补录目录实施计划

> **当前方向已变更（2026-11-11）：** 用户选择应用自有登录窗口、无需浏览器扩展。以下扩展流程尚未安装验收，已由 [应用自有登录设计](owned-login-design.md) 替代。正式替换前不声称新接入已完成。

> 沿用已有 feature worktree、多 agent、模块窄测试与块级集成测试，不逐项重复审核，不动主工作区。

**当前方案：** 用户随后选择“首次在应用提供的登录窗口中登录”。本计划的会话接入由[应用自有登录设计](owned-login-design.md)取代此前的浏览器扩展方向。本轮采用 macOS 14+ WKWebView helper，不交付扩展或 Native Messaging host；不保留独立 Chromium/CDP 或浏览器扩展备用模式。

**目标：** 应用首次提供原生飞书登录窗口，后续按 OS 用户及产品账号保存自己的 WebKit 会话，用于个人、可见共享和隐藏共享入口扫描及后台刷新。官方 CLI 账号授权、目录核验、统一账号盘、按名称/链接补录、VFS 与云端写回继续复用。

**依据：** [原接口方案与证据](discovery-options.md)、[当前自有登录设计](owned-login-design.md)及[macOS 原型验证](owned-login-verification.md)。官方 CLI/schema 和原型实测结论保留，不能自动升级为正式安装 App 的端到端验收。

## 边界

- macOS 原生登录最低为 14.0。Swift 6 helper 位于 `libexec/DriveBridge Session.app`，Bundle ID 固定 `org.drivebridge.sessionhost`，与原型一致；不把会话状态随包分发。
- Windows/Linux Go 包保持可编译，既有 CLI/同步能力保留，但应用内网页登录明确不可用。WebView2/WebKitGTK 后续适配不能写成已交付。
- 不要求浏览器插件，不导入日常浏览器 Cookie。应用会话只传受限目录响应及个人资源归属核验元数据；文件正文和写回仍用官方 CLI。
- 官方 CLI 授权与原生网页登录分别管理；每次扫描比较当前 owner_id 与官方 user_id，历史快照不替代本次归属证明。
- 关闭登录窗口只隐藏；目录后台请求可继续。会话过期需用户重新验证；失败、入口缺失或退出不删除旧清单和待上传数据。
- 一个 token 只有一个可写路径，管理页保留多来源关系。统一账号盘与原有单目录挂载并存，不迁移或清空已有挂载。
- 三路入口分页完成不证明所有子树或任意链接可访问目录的全集；云盘网页正文并非本机管理页，不宣称整站内嵌兼容。

## 分块

1. **目录扫描：** 沿用 discovery.Binding/Entry/Snapshot 与 Scan；严格 JSON、当前 owner_id、三路分页及补录保留。个人文件夹为空时以当前个人资源第一页验证归属；没有当前证据时阻止新增共享入口。
2. **自有原生会话：** `internal/bridge/ownedlogin` 管理 `native/session-host/main.swift` helper；受控 JSONL 协议提供 Login/Get/Complete/SealLogin/Close。完成后显示本机成功视图并隐藏云盘网页，不在账号/SSO 输入过程中自动导航。按账号隔离 WebKit 存储，生命周期清理在原生托盘终止前完成。
3. **Manager 与统一盘：** 默认接入原生 provider，保留官方 CLI 名称查询、目录链接核验、手动补录和每五分钟后台刷新。扫描完成后创建/复用账号盘并挂载；后台扫描不启动显式停止的挂载。
4. **UI/托盘：** 提供打开原生登录、完成并挂载、重新扫描、最多 30 字符的名称搜索/候选添加与链接/token 补录。展示 helper 可用性、过期和平台不可用，不再展示扩展目录或安装开发模式。
5. **打包与验证：** macOS 构建 Swift 6、macOS 14 目标 helper，生成固定 Bundle ID/普通激活/最低系统版本 plist，检查文件白名单及架构；签名构建在清单前签名 helper app。Windows/Linux 无 helper 依赖。模块、协议、打包与编译检查后，再以正式安装 App 验证登录、隐藏、重启、隔离、退出和账号盘联调。

## 证据与执行约定

macOS 当前账号原型已读取 12 个去重入口，完成隐藏窗口、helper/完整验证程序重启后的新读取与独立存储隔离，见[验证记录](owned-login-verification.md)。正式安装结果另表待填；不把原型、合成测试或交叉编译当作正式登录与三平台验收。

系统登录、密码、扫码或 MFA 由用户在应用提供的真实网页登录窗口中完成，不向聊天发送凭据。沿用当前 feature worktree，按用户要求由主任务在验证后提交并集成 main；本模块工作不自行提交或发布。正式签名、公证、Windows/Linux 原生登录和长期会话条件均不虚构通过。
