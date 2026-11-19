# macOS 服务与文件权限排查验证

2026-11-18，本机 arm64，已通过维护租约把 `/Applications/DriveBridge.app` 升级到 0.2.26（unsigned test）。未提交或推送。

## 修复与测试

- 复现：Finder 的“服务”菜单包含两个同名“复制飞书分享链接 (DriveBridge)”。外层和 payload 内层 Info.plist 均声明该服务，共用 NSPortName。
- 修复：保留 payload app 为唯一提供者。递归服务数测试修复前失败 `2 != 1`，修复后通过；打包前后 payload 所有文件 SHA-256 相同。
- 升级迁移：维护租约停止后台后，安装器捕获并正常退出属于此次安装的旧 Swift 启动器，再强制刷新外层和内层注册；不依赖安装后打开选项。新外层启动器启动后台后退出，内层服务继续驻留。
- Python 打包测试 53/53；Swift Core 11 项检查、LinkService 4 项检查通过，Swift 6 launcher/installer 编译成功。
- 一次独立只读审阅发现升级后旧服务驻留（Important），已新增回归测试并修复。无 Critical 或 Minor，不重复审阅。

## 本机升级结果

- 安装 worker 成功退出，原有 Swift 启动器已退出。
- LaunchServices 注册库中该分享链接服务的唯一 service id 数量从 2 变为 1；外层与内层 app 均注册为 0.2.26。
- 链接 CLI 对已上传测试文档返回有效 HTTPS URL；该查询使用缓存、无待同步修改。真实菜单点击与剪贴板结果尚未补验。
- 后台恢复运行，queued=0、inflight=0、conflicts=0、last_error 为空、cloud_state=synced，upload status 无待处理任务。
- config.json 与 conversion-rules.json 哈希未变；未修改账号、分享权限或系统安全设置。

## 文件操作权限的已知范围

- 挂载根目录、个人目录及所查文件均属于当前用户，基本读写权限正常。
- 在个人目录创建自用测试文件和空目录（本地临时文件规则），Finder 重命名和移动成功，没有管理员密码/Touch ID 弹窗。
- Finder 删除测试文件出现“立即删除、无法撤销”的确认，已取消；没有管理员验证。随后经内容校验，仅清理自建测试文件和空目录，后台最终无待处理任务。
- 运行日志中没有发现 permission denied、EPERM 或 .Trashes 相关失败记录。不代表用户此前触发的操作不存在。
- 已请求具体触发项目，尚未收到；未能确认管理员验证的根因，未修改权限行为。
- 后续本机锁屏（IOConsoleUsers 显示 CGSSessionScreenIsLocked=Yes），CUA 无法取得 Finder 窗口。解锁后需补验唯一菜单及复制出的实际链接。

私有检查记录位于忽略目录 `.superpowers/macos-services-permissions-20261118/`，不包含在版本提交中。
