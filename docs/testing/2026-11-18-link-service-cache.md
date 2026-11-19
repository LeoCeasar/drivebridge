# 链接服务与缓存优化验证

2026-11-18，macOS arm64，本地应用 `/Applications/DriveBridge.app` 最终版本 0.2.28，unsigned test。工作区改动保留，未提交、推送或发布。

## 根因与实现

- 0.2.26 的唯一 Services 提供者与 Go 托盘共用 `org.drivebridge.desktop`。`lsappinfo` 确认运行实例实际是 drivebridge-desktop；Finder 日志多次报告应用未在超时前打开 Services 端口。
- 新增独立 `libexec/DriveBridge Link Service.app`，bundle ID/端口为 `org.drivebridge.linkservice`，显式注册服务端口，不启动 Go 托盘或管理页。外层、桌面 app 均不声明 Services。
- Services 握手超时 3000ms；异步链接请求使用 Go 3 秒预算，覆盖配置发现与查询。SIGTERM 取消会回收提供方进程组。Swift 5 秒外层等待与有限退出清理时间防止长时间遗留任务。
- 目录列表的 SourceURL 继续用于投影；在线补查结果独立保存为 CachedLinkURL/Token/Kind，不改变虚拟资源正文，也不被无 URL 的刷新抹掉。条件写回检查 entry/token/kind/path/mount/profile/virtual type、冻结状态和未解决的远端删除冲突。
- 异步结果不覆盖后来改变的剪贴板。
- 用户实测 0.2.27 已可复制，随后反馈成功通知点击会打开编辑器。原因是旧 AppleScript 通知的发送方关联；0.2.28 已去掉该通知路径，成功静默，异步失败用六秒自动关闭的非激活 NSPanel。
- DMG 安装 worker 与高级 pkg postinstall 均迁移当前安装的服务；pkg 先验证维护租约，再用包内 updater 处理当前用户固定安装路径，验证属主/目录/bundle ID 并拒绝符号链接。

## 自动验证

- `python3 -m unittest discover -s packaging/drivebridge/tests`：54/54 通过。
- Swift 6：CoreTests 通过，LinkServiceTests 七类检查通过；launcher、installer 均编译成功。包含成功 CLI → 剪贴板、失败保留、超时回收、后来剪贴板保护、helper 布局和 pkg 路径验证。
- `go test ./internal/bridge/store ./internal/bridge/manager ./cmd/feishu ./lib/larkcli ./internal/bridge/sync ./internal/bridge/desktop -count=1`：六个包均通过。
- 新回归测试观察到 RED → GREEN：补查两次变一次；虚拟资源补查不得改变投影且刷新后可离线复用；SIGTERM 必须清理真实提供方子进程；预算覆盖 manager 构造；pkg postinstall 必须先验证租约。
- 整套验证曾暴露 TestLinkRecreatedObjectDoesNotReuseOldURL：把已解决的历史删除冲突误当活动冲突。修复为仅拒绝未解决冲突后，整套验证通过。
- `go test -race ./internal/bridge/store ./internal/bridge/manager -run 'TestCacheSourceURL|TestLink' -count=1`：通过。macOS Go race 链接器输出 LC_DYSYMTAB 工具链告警，无测试失败。
- Windows amd64 桌面交叉编译成功；未做 Windows 实机复制验收。
- `git diff --check`、构建脚本和 postinstall shell 语法检查通过。

## 本机证据

- 通过维护租约依次升级到 0.2.27、0.2.28；0.2.27 独立服务真实冷启动，用户确认剪贴板可复制。
- 0.2.27 的在运行 helper PID 21197 在 0.2.28 安装后已正常退出，确认升级会接管旧服务。
- 已安装 0.2.28 helper 的可执行文件不包含 `/usr/bin/osascript` 或 `display notification` 通知代码。
- 系统注册库中链接服务唯一 id 数量为 1。
- 最终后台 running=true、queued=0、inflight=0、conflicts=0、last_error 为空、cloud_state=synced；upload status 无待处理任务。
- 配置和转换规则与升级前 SHA-256 一致。已缓存文档返回有效 HTTPS URL，cached=true，实测 21ms。

## 剩余边界

- 用户确认的真实剪贴板复制发生在 0.2.27；0.2.28 的静默成功及失败提示已完成源码、编译和已安装二进制检查，未单独读取用户真实剪贴板或复现原生失败提示面板。
- 本机 Finder 操作与用户手动测试交错时，自动化遵从状态变化提示停止旧动作，未修改用户文档。
- 高级 pkg 迁移通过脚本行为、路径验证与构建验证；未覆盖安装其他用户或 root 域，不在已有旧安装上再做一轮真实 pkg 覆盖。
- Developer ID 签名、公证与 Windows 实机验收不在本次本地测试中宣称通过。
- 一次独立只读审阅的三项 Important 均已修复；Minor 保留：忙碌或非法多选仍由 Services error 指针交给系统反馈，异步查询失败为非模态原生提示。

私有运行记录与应用备份位于忽略目录 `.superpowers/link-service-cache-20261118/`，最终数据为 `after.json`。
