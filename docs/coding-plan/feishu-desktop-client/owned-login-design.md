# 应用自有登录窗口与会话：当前决定

2026-11-11：用户明确选择“首次在应用提供的登录窗口中登录飞书”，不要求安装 Chrome/Edge 插件。此决定替代 browser-extension-development.md 的默认接入方向。此前的扩展安装确认已作废；不得根据迟到的旧确认回复继续加载扩展。

## 用户体验

安装一个 DriveBridge 应用；应用提供带真实地址展示的飞书网页登录窗口。首次登录后，由本机应用保存该账号的网页会话。关闭登录窗口只隐藏窗口，不视为退出账号；目录发现由托盘进程按需驱动。用户正常使用的 Chrome、Edge、Safari 可以关闭，不是后台目录发现的依赖。

完全退出应用、系统退出登录、休眠或断网时，不承诺继续联网同步。保持原有持久队列与恢复机制；不删除未上传数据。会话过期、SSO/MFA或企业安全策略要求重新验证时，托盘提示用户主动打开登录窗口，不承诺永久免登录。

## 明确保留的边界

- 官方 CLI 的 API 授权与飞书云盘网页登录分别管理，初次设置可能包含两步。官方授权继续走官方流程，必要时打开系统浏览器；不能把网页登录 Cookie 当作官方 API token。
- 网页会话按 OS 用户及产品账号隔离，配置 JSON 不包含 Cookie、账号密码或 token。
- 网页只用于目录发现；文件下载、写回与冲突处理继续使用已实现的官方 CLI/同步模块。
- 不能用“内置窗口”推导出额外目录权限或全部共享目录的服务端保证；沿用个人、可见共享、隐藏共享来源、分页、补录和重新扫描。
- 不修改日常浏览器配置，不要求扩展，不导入日常浏览器 Cookie，不借用用户已有浏览器的登录。

## 正式接入规格（macOS 原型已验证，安装后验收待填）

采用独立的应用自有登录 helper，承载原生 WebView，避免与现有托盘的原生主线程/退出事件循环互相干扰。当前交付只实现 macOS WKWebView，最低 macOS 14；Windows/Linux 的 Go 包保持可编译和既有 CLI/同步功能，但应用内网页登录明确不可用。WebView2、WebKitGTK 属于后续适配方向，不能写成已提供的运行时或已验证能力。

Swift 6 源码为 `native/session-host/main.swift`；安装包路径固定为 `libexec/DriveBridge Session.app/Contents/MacOS/drivebridge-session-host`。helper 的 Bundle ID 保持 `org.drivebridge.sessionhost`，与已验证原型一致；Info.plist 标明 macOS 14.0、普通应用激活方式及高分辨率支持。打包生成并校验这些字段，签名构建在清单生成前单独签名 helper app。应用状态、WebKit 会话、Cookie 与账号数据不进入安装载荷。

Manager 的 discoverySession（Login/Get/SealLogin/Close）和 discovery.Scan 保留，默认会话接入 `internal/bridge/ownedlogin`；更换获取目录响应的提供者，不改 VFS、队列或云端写回协议。helper通过受控私有IPC接收“打开登录窗口、按固定接口读取目录、隐藏、关闭”请求；不暴露任意脚本执行、任意URL请求或 Cookie 导出接口。

身份以官方 CLI 获取的 user_id 与本轮网页资源 owner_id 核对，不以页面文字或“看起来登录成功”作为依据。任意跨账号或未验证状态保留旧清单，阻止接纳新共享入口。

## 验证状态

macOS 当前账号原型验证已完成：官方页面登录、12 个去重入口、关闭窗口后读取、helper 与完整验证程序重启后免重复登录、独立存储隔离。云盘正文存在空白/加载骨架问题，不能声称整站内嵌兼容。详见 [实机验证记录](owned-login-verification.md)。Windows/Linux 原生登录尚未实现；长期过期、其他企业 SSO，以及正式安装包的登录、重启复用、账号隔离和账号盘联调结果仍待填。

## 需要验证的实际影响

1. macOS当前飞书租户的扫码/密码、重定向、可能的企业 SSO 是否能在 WKWebView 中完成，且同源目录请求正常。未实测前不得宣称兼容。
2. 首次完整登录后关闭窗口、退出并重开应用，网页会话是否能复用；每账号是否真正隔离。
3. 隐藏窗口后的目录请求是否能按需唤醒；过期是否明确提示、保留清单，不反复弹窗。
4. Windows/Linux 当前返回登录不可用。未来如实现 WebView2/WebKitGTK，应另行验证运行库部署、代理、证书与图形环境；当前 Go 交叉编译不能替代这些系统的原生登录验收。
5. Native helper 退出前清理与托盘退出顺序保持；升级不清空会话、配置、Blob、冲突副本或待上传队列。

## 本轮交付与待补证据

本轮正式接入 Manager 默认原生会话、UI 登录入口、Swift helper app 和 macOS 打包；不交付扩展或 Native Messaging host，也不要求用户操作浏览器开发者模式。旧扩展源码由主任务移入 ignored 退役参考，不再是运行依赖。

模块、协议与打包检查和编译属于源码验证；正式安装后的登录、关闭窗口继续扫描、应用重启复用、账号隔离、账号盘刷新及退出清理需另行记录。原型通过项保留为原型证据，不自动填入产品验收表。详见[使用说明](discovery-usage.md)及[验证记录](owned-login-verification.md)。

参考：
- https://developer.apple.com/documentation/webkit/wkwebsitedatastore
- https://learn.microsoft.com/en-us/microsoft-edge/webview2/concepts/user-data-folder
- https://learn.microsoft.com/en-us/microsoft-edge/webview2/concepts/distribution
- https://webkitgtk.org/reference/
- https://open.feishu.cn/document/client-docs/build-login-free-system-

## 2026-11-11 正式接入结果

正式 Manager 默认使用应用内 WKWebView helper，扩展交付与注册已退出当前方案。显式打开登录窗口后，每3秒验证一次、最多5分钟；自动完成账号核验、入口扫描与账号盘创建启动。失败保留清单，可在管理页重新验证。当前 macOS 开发载荷已实测12个入口、原生完成页、Finder分组、隐藏后新读取及 Manager/helper 整体重启复用，162个受保护同步文件升级后不变。Windows/Linux原生窗口、双真实账号隔离及签名公证仍需后续验收，详见独立正式验证记录。
