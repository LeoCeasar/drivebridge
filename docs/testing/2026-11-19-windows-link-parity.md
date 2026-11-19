# Windows 飞书分享链接对齐记录（2026-11-19）

## 结果与范围

当前 `feat/single-feishu-link` 工作区已经补齐 Windows 链接交互。改动保留在本地，未提交、推送、发布，也未修改本机 macOS 安装。

| 能力 | 当前源码状态 |
| --- | --- |
| 单选文件/目录右键复制 | 保留现有 HKCU MSI/Inno 菜单及 Single 选择模型 |
| 链接缓存、身份校验、首次查询超时分类 | 共用 Go 核心已包含本轮及此前修复 |
| 成功复制 | 静默，不调用 Toast、MessageBox 或外部程序 |
| 查询或复制失败 | Win32 原生无激活提示，最多六秒，无需点击关闭 |
| 等待期间用户复制新内容 | Manager 初始化前捕获序号；独占打开剪贴板后复核，拒绝覆盖新内容 |
| 取消、忙碌、序号不可用 | 不清空既有剪贴板；写入内存准备后、EmptyClipboard 前再次检查取消 |

写入使用隐藏消息窗口作为剪贴板所有者，在固定 OS 线程上执行 OpenClipboard / Unicode 写入 / CloseClipboard。内存先分配填充再清空剪贴板；成功提交后将内存所有权转交 Windows。

错误窗口使用 NOACTIVATE / TOOLWINDOW，SetWindowPos 显示以避免首次 ShowWindow 被隐藏启动参数覆盖。标题只绘制一次，处理每条窗口消息时都检查期限，避免繁忙消息队列延长提示寿命。窗口销毁与创建位于同一线程。

## 本机验证

- `go test ./lib/larkcli ./backend/feishu ./cmd/feishu ./cmd/desktop ./internal/bridge/... ./vfs/... -count=1`：27 个有测试的包全部通过。
- `go test -race ./cmd/feishu ./internal/bridge/platform -count=1`：通过；macOS 链接器仍输出 LC_DYSYMTAB 警告，无测试或竞态失败。
- 打包测试 54 项、界面行为测试 27 项通过。
- Windows `platform.test.exe`、带 cmount 的 `feishu.test.exe` 交叉编译成功。
- Windows x64 CLI 和桌面程序按正式参数构建成功：`CGO_ENABLED=0`，CLI 标签 `cmount,drivebridge_feishu`，桌面标签 `cmount,desktop,drivebridge_feishu`，桌面链接参数 `-s -w -H=windowsgui`。
- 已校验 CLI PE Console 子系统为 3、桌面 PE GUI 子系统为 2。ZIP 内程序 SHA-256 与编译产物一致。
- 两条修改后的工作流 YAML 解析通过，`git diff --check` 通过。

新增回归包括：开始初始化前捕获、同一命令再次执行重捕获、查询或等锁期间剪贴板变化、序号检查期间取消、忙碌/取消保持内容、准备失败时不初始化 Manager 且只报告一次剪贴板错误。

首次较广套件在测试先行阶段捕获 `TestLinkClipboardCaptureFailureStopsBeforeManager` 的预期失败（误报挂载错误）；修复后完整命令重新运行通过。独立审查发现的取消、隐藏启动、消息队列期限及测试读取器问题均已修正；原生部分只完成编译，未宣称 Windows 运行通过。

## Windows 原生门禁

`drivebridge-windows-package.yml` 与 `drivebridge-release.yml` 均强制以下七项测试逐项 pass，缺失或 skip 会阻止门禁通过：

1. `TestWindowsLinkClipboardUnicodeAndChanged`
2. `TestWindowsLinkClipboardBusyPreservesContent`
3. `TestWindowsLinkClipboardCancelledWritePreservesContent`
4. `TestWindowsLinkErrorWindowDoesNotActivate`
5. `TestWindowsLinkErrorWindowHiddenLaunch`
6. `TestWindowsLinkFeedbackSilentAndBounded`
7. `TestWindowsLinkFeedbackDeadlineUnderMessages`

在隔离 Windows 会话中执行（测试会写入该会话的剪贴板并短暂显示测试窗口）：

```powershell
$env:DRIVEBRIDGE_TEST_LINK_NATIVE = '1'
go test -count=1 -timeout 2m -run '^TestWindowsLink' ./internal/bridge/platform
```

Native reader 独立调用 GetClipboardData / GlobalSize / GlobalLock，并正确处理最后一次 GlobalUnlock 返回零且 NO_ERROR 的情况，不使用会误判此返回值的旧第三方读取实现。

API 依据：[剪贴板序号](https://learn.microsoft.com/en-us/windows/win32/api/winuser/nf-winuser-getclipboardsequencenumber)、[OpenClipboard 独占及所有者规则](https://learn.microsoft.com/en-us/windows/win32/api/winuser/nf-winuser-openclipboard)、[SetClipboardData 内存所有权](https://learn.microsoft.com/en-us/windows/win32/api/winuser/nf-winuser-setclipboarddata)、[ShowWindow 首次显示规则](https://learn.microsoft.com/en-us/windows/win32/api/winuser/nf-winuser-showwindow)。

## 产物及待验边界

本地 `.superpowers/windows-alignment-20261119/` 包含当前源码编译的 `drivebridge.exe`、`drivebridge-desktop.exe`，以及 `drivebridge-windows-amd64-binaries.zip`。`BUILD-INFO.json` 记录 538 个生产依赖源文件的整体指纹、HEAD 加 dirty 状态、构建参数及各程序 SHA-256；`SHA256SUMS` 和 ZIP 内容已校验。

此 ZIP **仅含二进制，不是完整安装包**，不包含官方 CLI/Node/私有 Git/登录组件/驱动，不应直接替换运行中的程序或绕过升级协调器。Windows 安装工作流的默认目标版本已更新为 `0.2.30`，尚未执行该构建。

本机为 macOS，未发现可运行的 Windows 环境。本轮未生成完整 EXE/MSI，也未验证 Windows 实机剪贴板、焦点、Explorer 菜单、真实账号和安装器升级。本地此前找到的完整 Windows 包仍是 0.2.21，不包含本轮功能；不能将它作为新版分发。

## 后续 GitHub 原生构建

同日用户授权发布后，Windows runner 的七项链接原生测试、共享套件及三项真实 WinFsp 门禁均通过，并构建完整 0.2.30 EXE/MSI/ZIP。此结果补齐了上文本机交叉编译阶段的原生执行边界；人工 Explorer、真实账号和安装器升级验收仍未完成。详见 [0.2.30 构建与发布记录](2026-11-19-windows-0.2.30-release.md)。
