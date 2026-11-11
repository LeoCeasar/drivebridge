# 当前浏览器复用登录：实现计划

> 本方案已退役，不属于当前交付。用户已选择应用自有原生登录，见 [当前设计](owned-login-design.md)。扩展、Native Messaging 注册及其安装步骤仅保留为历史设计，不要求用户继续执行。
> **当前方向已变更（2026-11-11）：** 用户选择应用自有登录窗口、无需浏览器扩展。以下扩展流程尚未安装验收，已由 [应用自有登录设计](owned-login-design.md) 替代。正式替换前不声称新接入已完成。

> 2026-11-11 用户明确选择“当前浏览器＋轻量扩展”；原目录扫描、补录和账号盘继续复用。沿用已有 feature worktree、多 agent 和模块窄测试/块级集成验证，不另设重复审批或审核环节。

## 目标与边界

点击登录后在已经使用的 Chrome/Edge 中打开或选中飞书页面，复用该浏览器的会话。扩展在飞书页面中执行同源只读请求，将目录响应经 Native Messaging 交给本机应用；Cookie 不离开浏览器。不再默认启动独立 Chromium 配置。浏览器关闭时标记等待浏览器，保留目录和待上传数据；官方 CLI 的文件读写与同步不依赖浏览器继续运行。

Chrome/Edge 为首批目标；Safari/Firefox 的扩展交付不在本块内。扩展先交付可核查的开发目录/ZIP与加载说明，商店发布另行处理，不声称已上架。三平台注册 Native Messaging host 均按当前系统用户处理。

## 固定协议

- 扩展 ID：`mpcbleimmgigonbhigohceapnjjcfhec`。开发公钥来自本地公开 DER；私钥仅在 ignored scratch 中，禁止提交或打包。
- Native host：`org.drivebridge.browser`；独立 helper `drivebridge-browser-host[.exe]`，不得把 rclone 日志写入协议 stdout。
- Native frame：4字节小端长度+UTF-8 JSON；每帧上限256KiB。协议版本 `v:1`。
- app → 扩展命令：`{v:1,kind:"open"|"fetch",id:<UUID>,profile_id:<UUID>,origin:<HTTPS租户origin>,path?:<allowlist path>,query?:<encoded URL query>}`。
- 扩展 → app：先 `hello {v:1,kind:"hello"}`；响应内容用 `{v:1,kind:"chunk",id,seq,data:<base64 UTF-8 bytes>}`，每块至多64KiB原始字节；结束 `{v:1,kind:"reply",id,status:"ok"|"needs_login"|"request_failed",chunks:<count>,http_status?:number,content_type?:string}`。open响应chunks=0。目录响应总上限16MiB，校验归属/顺序/大小/重复完成/取消。
- 只允许现有三条 read-only 路径：个人folder、个人obj、shared folder list；query只允许length/asc/rank/hidden/last_label。不得提供任意URL、脚本执行、上传或 Cookie API。
- 本机 Broker 为 `127.0.0.1` 随机端口，私有 endpoint/key，拒绝浏览器 Origin 请求。Native host从私有 locator读取连接，只连接合法loopback地址。每个OS用户最多一个活跃broker（多账号共享），配置不匹配返回busy。
- native helper HTTP：POST `/connect` body `{}` 返回 `{client_id}`；GET `/poll?client_id=…` 返回单个命令或204，long-poll最多20s；POST `/response?client_id=…` 传回单个chunk/reply；POST `/disconnect?client_id=…`。所有请求Bearer私有key、无Origin、严格Host/JSON/大小限制。客户端ID是UUID，断开取消其全部请求。

## 模块与接口

### 1. Broker 与 Native host（Go）

创建 `internal/bridge/browserbridge/{broker,session,host,protocol}.go`及测试、`cmd/browser-host/main.go`。

输出：`Start(ctx, Options{RuntimeDir,ConfigKey string})(*Broker,error)`；`Broker.NewSession(profileID,origin string)(*Session,error)`；Session实现 `Login/Get/SealLogin/Close` 与现有发现接口一致；`Broker.Connected() bool`、`Broker.Close() error`；`RunNativeHost(ctx, in io.Reader,out io.Writer,args []string) error`。常量 `ExtensionID`、`NativeHostName`。缺连接返回`ErrDisconnected`，未登录使用discovery.ErrNeedsLogin，非法响应ErrProtocol。

RuntimeDir固定由platform.StateDir()/browser-bridge提供，endpoint.json含schema_version/config_key/address/key/PID/instance。只允许私有规范路径，原子替换，锁禁止多个owner。关闭不关闭用户浏览器或既有标签页。Session每次fetch不需要单独创建浏览器。

测试：真实loopback和模拟native传输覆盖双向RPC、分块、错误归属、断连/取消、host过滤、独立账号隔离；所有错误不包含上游响应或密钥。

### 2. 浏览器扩展与用户引导

创建 `browser-extension/drivebridge/{manifest.json,background.js,content.js,popup.*}`、Node行为测试。manifest使用公开key；权限为nativeMessaging/storage/alarms及feishu.cn/larksuite.com host域名，不请求cookies/debugger/全站权限。content script ISOLATED，无页面postMessage任意执行入口。

open优先激活同租户既有标签页，否则在当前浏览器创建页面；fetch在同源content script内credentials include、redirect error、35s timeout、流式16MiB限制。没有页面时可创建后台辅助页，不导航或关闭用户既有文档。用户关闭浏览器后native port断开；onStartup/扩展点击重连，断开alarm重试，避免忙循环。只传目录元数据，不读写文件内容。

修改现有UI说明/状态/按钮与安装步骤，展示已连接/待安装扩展/浏览器未连接；不再提示独立窗口登录。仍保留重新扫描、名称搜索与链接补录。

测试：凭据内请求、非飞书/任意路径拒绝、未知消息来源、分块上限、页选择、断开重连及不关闭用户页。

### 3. 每用户注册与打包

创建 `internal/bridge/browserbridge/registration*.go`。输出 `RegisterHost(ctx, helperPath string) error`；仅注册允许的固定扩展ID；Chrome/Edge/Chromium私有用户manifest位置与Windows HKCU registry，绝不系统级写入。路径必须绝对普通文件，禁止覆盖foreign manifest。增加helper与extension静态文件白名单，更新各平台builder与packaging fixture，保留离线安装和原CLI能力。

### 4. Manager 接入（主 agent）

默认Session工厂切换到Broker；`StartDiscoveryLogin`准备host/broker、在当前浏览器打开页面，等待扩展连接。缺扩展显示明确的安装引导；已有账号的目录快照不迁移、不清空。`DiscoveryView.Browser`提供extension_id/extension_path/connected供UI展示（纯读取，不启动服务）。背景scan断连标记browser_disconnected且保留旧清单。

Manager.Close先阻止新会话、关闭broker/已拥有session，再结束生命周期；不得关闭当前浏览器。沿用已修复的托盘终止前清理。个人owner_id仍逐扫描比对官方CLI user_id，不把扩展的“已登录”文字当身份验证。

## 验证与交付

按模块窄测试后运行Bridge/CLI/desktop集成测试及三平台编译；构建候选包后再加载扩展实测。扩展安装权限需在具体可检查产物准备好后由用户确认/操作。实机检查复用当前登录、个人/可见共享/隐藏共享清单、名称补录、重新扫描、关闭浏览器后已有文件同步、重开浏览器恢复发现。截图/操作手册只记录实际观察结果，开发包与正式商店发布状态分别说明。

参考：[Chrome Native Messaging](https://developer.chrome.com/docs/extensions/develop/concepts/native-messaging)、[Content scripts](https://developer.chrome.com/docs/extensions/develop/concepts/content-scripts)、[默认浏览器配置的调试限制](https://developer.chrome.com/blog/remote-debugging-port)。
