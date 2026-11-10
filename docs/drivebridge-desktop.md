# DriveBridge 桌面常驻客户端

开发版，2026-11-10。桌面入口、后台控制与平台打包代码已实现；真实三平台挂载、安装升级及签名验收尚未完成。本文描述当前实现，不代表已有正式发布的完整安装包。

## 日常使用

安装完整桌面包后，双击 DriveBridge。在 macOS 菜单栏、Windows 通知区域或 Linux 支持的托盘中查看状态。点击“打开管理页”进入本机浏览器界面，配置自己的飞书账号、共享文件夹和挂载位置。之后直接在本地目录编辑，后台 worker 负责同步。

托盘提供：

- 汇总状态与每个挂载的待处理、冲突数。
- 打开管理页、打开已配置的本地目录。
- 启动同步、停止同步、重试。
- 启用或停用“登录时启动”。
- “退出托盘（继续同步）”及单独的“停止全部同步”。

“登录时启动”需要启用一次，之后在当前用户登录图形桌面时生效。当前实现不在安装过程中自动注册，也不在其他账号或 SYSTEM 下运行。macOS/Linux 关闭这一选项只影响后续登录，不结束当前托盘或同步。

关闭浏览器页面不停止同步。退出托盘会关闭它自己拥有的管理服务，并保留独立 worker；如果管理服务原先由单独的 `feishu gui` 启动，则仍归该命令所有。要停止同步，请使用“停止同步”或“停止全部同步”。停止有打开文件或待同步数据时可能超时，界面会显示操作未完成；数据不会因此清空。

未知上传结果、云端待核验、本机待发布、VFS 待保存和冲突都会阻止显示“已同步”。文件夹没挂载、进程失联或状态不可读时同样不会显示成功。后台仅恢复明确启用的挂载；CLI/管理页主动停止会持久记录，桌面轮询不会把它立即重新拉起。

macOS 支持按挂载保存 `macos_backend`：留空保持 macFUSE 默认行为，`fskit` 使用用户空间后端。管理页的 macOS 挂载方式选项与 CLI `feishu mount add ... --macos-backend fskit` 共用配置。FSKit 需要兼容的 macOS/macFUSE，并在系统设置“通用 → 登录项与扩展 → 文件系统扩展”启用 macFUSE 扩展。安装了驱动但扩展关闭时，真实挂载仍会失败；切换后端不会自动绕过系统授权。

## CLI 控制桌面程序

```sh
# 有完整桌面构建时：后台启动并打开管理页
drivebridge desktop start

# 查看桌面进程是否就绪
drivebridge desktop status

# 当前用户登录后启动；取消时用 disable-autostart
drivebridge desktop enable-autostart

# 只退出托盘和它拥有的管理服务，保留同步 worker
drivebridge desktop stop

# 停止某个挂载；team 替换为自己的配置名称
drivebridge feishu mount stop team --wait-upload 60s

# 检查二进制是否编入原生托盘；不读取账号或配置
drivebridge desktop capabilities
```

`--bridge-config <绝对路径>` 和 `--lark-cli <路径>` 是 desktop 命令的持久参数。桌面实例按 OS 用户唯一；不同配置的另一个实例不会接管已有实例，需先使用原配置退出桌面。现有 worker 的归属和身份仍由 Manager 检查。

无图形环境或只做本机服务验证时可使用 `drivebridge desktop start --headless`。这会运行管理服务与监督器，不会产生托盘图标；随后 `drivebridge feishu gui --no-open-browser` 可取得同一配置的本机管理地址。不要把 headless 模式描述成原生托盘验收。

## 平台交付

| 平台 | 桌面入口 | 登录自启动 | 运行条件 |
|---|---|---|---|
| macOS Intel / Apple Silicon | `DriveBridge.app` 菜单栏程序 | 当前用户 LaunchAgent | 完整构建需要 macFUSE 头文件，实际挂载需要可用驱动 |
| Windows x64 | 无控制台 `drivebridge-desktop.exe`、用户快捷方式 | 当前用户、有限权限、交互式登录任务 | 实际挂载需要 WinFsp |
| Linux amd64 / arm64 | 桌面应用入口或 `launch-desktop.sh` | XDG 图形桌面自启动 | FUSE3；原生托盘需要 GTK3、GIO、Ayatana AppIndicator 及可用的桌面托盘服务 |

Linux 没有图形显示或 StatusNotifierWatcher 时，会保留管理服务和监督器并降级为无托盘模式；CLI 和管理入口仍可使用，不能宣称图标已显示。各桌面环境的图标可见性需要单独验证。

macOS 当前安装布局为一个 DriveBridge 目录，其中 `.app` 与 `bin/drivebridge`、`libexec/lark` 共用运行时；不能只把 `.app` 移出安装目录。Windows 同样保留独立 CLI，桌面 EXE 用于双击运行。Linux portable 包可用 `sh install-desktop.sh` 安装当前用户应用入口；这一步不启用登录自启动。

## 构建与验收边界

纯 CLI 构建不导入原生图形库。安装脚本使用 `--desktop`（Unix）或 `-Desktop`（Windows）增加桌面程序，不能与 CLI-only 测试模式混用。打包会执行只读的 `desktop capabilities`，拒绝没有编入原生托盘的二进制。

完整构建参数、依赖、签名与安装保护见 [打包说明](../packaging/drivebridge/README.md)。macOS Apple Silicon 完整桌面挂载包已构建，当前用户 pkg 安装已通过实机验证；真实挂载、托盘可见性、登录自启动、签名及升级仍按操作手册的实际记录验收。Windows/Linux 原生安装与自启动仍待对应平台验证。Linux 系统级 deb/rpm 更新及部分卸载路径仍受既有多用户/安装生命周期保护限制；不能用桌面壳绕过这些条件。升级过程中不会清理待上传、未知结果、冲突或恢复数据。

本轮实机步骤、截图和未通过项见 [操作手册](manual/操作手册.html) 及 [验证记录](manual/验证记录.json)。
