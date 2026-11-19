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

桌面构建新增单选“复制飞书分享链接”：macOS 在 Finder 中选择一个文件或文件夹，使用“右键 → 服务 → 复制飞书分享链接”；Windows 在资源管理器中右键使用同名命令，Windows 11 通常需先打开“显示更多选项”。Windows 菜单只在单选时显示；macOS 服务收到多选时拒绝执行。轻量菜单可能在挂载外也显示，点击后会检查是否属于 DriveBridge。

复制的是飞书已有访问链接，访问权限保持原样。已有对象有待同步修改时可以复制，链接指向云端当前内容；新建项目首次上传完成前不能复制。账号盘根目录及“个人目录／共享目录／手动添加”等分类入口不作为真实文件夹分享。优先使用已缓存链接，此时不实时核验访问状态；缓存缺失时才请求飞书元数据，不读取正文。查询失败不修改剪贴板。

缓存链接必须带有对应云端 token 的归属记录；旧版本缓存第一次使用时可能需要在线查询或刷新目录。云端已确认删除、尚有删除冲突的项目不会返回旧链接；恢复并重新上传后会获取新对象的链接。

入口随完整桌面包安装，旧包需要升级。macOS 若暂未显示服务，可正常启动一次 DriveBridge，并在系统设置的服务快捷键列表检查是否启用；Swift 启动器会保持后台运行以接收服务请求。Windows 静态菜单由 MSI/Inno 桌面安装器注册，portable ZIP 不自动写注册表。现有安装器生命周期与卸载限制继续适用。两端菜单实际可见性仍需按[单选链接验收步骤](testing/2026-11-18-single-feishu-link.md)实机验证。

也可通过 CLI 获取单个项目的链接：

```sh
drivebridge feishu link --path '/本地挂载/方案.md' --json
drivebridge feishu link --path '/本地挂载/方案.md' --copy
```

省略 `--bridge-config` 时，链接命令会通过认证后的桌面实例定位当前配置；桌面未运行时使用默认配置。自定义配置且仅 worker 运行时，请显式传入 `--bridge-config`。

macOS 的链接入口由独立的 `DriveBridge Link Service.app` 提供，与托盘进程使用不同的应用标识。已有链接直接读取本地元数据；首次补查成功会按云端对象身份持久缓存，之后无需重复在线查询。文档改名或移动后，token 不变可继续复用；删除重建、对象类型或账号变化时不会使用旧对象链接。缓存链接不代表实时核验过访问权限。补查不会改变虚拟资源的本地投影内容。

链接查询在后台执行，macOS 的服务握手仍限 3 秒，异步首次补齐使用 15 秒请求预算及 20 秒原生子进程上限（另有有限退出清理时间）。复制成功不弹通知；失败仅显示六秒自动关闭、不抢焦点的原生提示，不打开脚本编辑器；若等待期间剪贴板已经变化，本次结果不会覆盖新内容。CLI 的 `feishu link --timeout 15s` 可指定查询预算。首次在线补齐超时单独显示“缓存尚未补齐”，不与一般网络错误混淆。后台日志记录耗时、白名单结果码与缓存命中，不记录路径、token、URL 或凭据。

Windows 的 `feishu link --copy` 默认查询预算为 20 秒。复制成功同样保持静默；失败显示最多六秒的无激活原生窗口，无需点击关闭。查询开始前记录剪贴板序号，写入时在同一次独占锁内复核；如果期间复制了其他内容，保留新内容并提示重新复制链接。不需要额外管理员权限。该行为已加入 Windows 原生测试门禁；本机交叉编译不代表 Windows 实机或完整安装器验收通过，见 [Windows 对齐记录](testing/2026-11-19-windows-link-parity.md)。

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
