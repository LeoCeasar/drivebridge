# DriveBridge 三平台桌面常驻客户端实施计划

> 执行方式：分块实现，隔离工作区并行；单元小范围测试，集成块统一验证。用户于 2026-11-10 明确要求开始实现 macOS/Windows/Linux 方案，不重复请求开工确认。

**目标：** 双击启动或登录后自动运行，通过菜单栏/系统托盘查看同步状态、打开管理页并安全控制挂载。第一版管理界面采用此前推荐的本机浏览器页面；独立 WebView 窗口不在本轮新增。

**架构：** 可选桌面构建提供托盘；纯 CLI 保留。桌面进程复用 UI Host 和 Manager，worker 继续独立运行。每个 OS 用户只运行一个桌面实例，配置身份不一致拒绝串用。关闭管理页和退出托盘不终止 worker；停止同步是单独操作。未知上传结果、冲突和待发布任务不得被误报为已同步。

**技术：** 当前 Go 1.25；Go systray（版本在引入前锁定），可选 desktop 构建标签。macOS .app、Windows 无控制台 EXE、Linux desktop entry；真实平台运行不以交叉编译代替。

**设计依据：** research-notes.md，用户已要求实施推荐路径。原有透明同步设计和数据保护边界继续有效。

## 全局约束

- 所有账号、配置、队列按 OS 用户存储；不硬编码开发机路径、凭据或共享文件夹。
- 仍使用 Manager 私有 IPC；产品命令不得因继承 RCLONE_RC 环境启动通用 RC/metrics。
- 初版托盘支持就绪/同步中/冲突/授权或错误/不可用/停止；只显示可证明的状态，不编造上传百分比。
- 用户 CLI/GUI 主动停止必须持久化停止意图；监督器不能立即重新拉起。自动恢复有退避、次数限制，并服从安装 lifecycle lease。
- 升级时先退出桌面/UI Host，停止自动恢复，再按现有安装 lease 保护数据和 worker。退出托盘与停止全部同步明确分开。
- 不在开发验证期间安装驱动、注册实际自启动、登录真实账号或运行安装程序。需要实际托盘预览时仅使用合成配置。

## 文件职责与接口

### D1 Manager 桌面控制（独立 agent）

文件：internal/bridge/manager/desktop*.go、新增所需测试；最小修改 worker.go/control.go/install_lease.go。

提供：
- `DesktopMounts(ctx) ([]DesktopMount,error)`，DesktopMount 含 `Mount model.Mount`、`Desired bool`。
- `EnableDesktop(ctx) error`：建立桌面意图状态，初始从现有 Mount.Autostart 取值；去重处理旧挂载自启动，保留纯 CLI 模式兼容。
- `SetDesktopDesired(ctx,id,enabled) error`、`StartDesktopMount(ctx,id) (model.MountStatus,error)`：持久意图和自动启动；后者必须重新检查停止意图。
- 用户 `Start/Stop` 在桌面模式激活后同步意图；自动恢复不能覆盖用户停止。维护停机保留意图。
- `DesktopRegister(ctx,open func(context.Context)error,shutdown func()) (release func(),err error)`、`DesktopOpen(ctx) error`、`DesktopQuit(ctx) error`、`DesktopRunning(ctx) (bool,error)`；`ErrDesktopRunning`。当前 OS 用户唯一、配置身份校验、私有随机 token、loopback、文件锁；锁持有到 UI/tray 完全退出。
- 安装 lease 取得全局独占后，退出桌面实例并等待释放锁，再进入替换 callback。

验证：主动停止与自动恢复并发、跨配置拒绝、重复实例、伪造 token、安装等待 UI 退出、持久意图重启与写入失败。只跑 manager 受影响测试。

### D2 托盘平台适配（独立 agent）

文件：internal/bridge/tray/**；go.mod/go.sum 依赖只由此任务修改。

接口：`Run(ctx context.Context, initial View, updates <-chan View, actions chan<- Action) error`，`Available() bool`、`ErrUnavailable`、`ErrNoDisplay`。

`View{State,Text string; Mounts []Mount; Autostart,Busy bool}`；`Mount{ID,Name,Mountpoint,State string; Running,Desired bool; Pending,Conflicts int; Error string}`；`Action{Kind,MountID string}`。

动作 Kind：open_manager/open_folder/retry/start/stop/enable_autostart/disable_autostart/quit/stop_all。quit 菜单必须写“退出托盘（继续同步）”；stop_all 是单独操作。“暂停”不用于表示已停止挂载。

desktop 构建调用原生 systray；默认构建为无图形依赖 stub；Linux 无显示返回 ErrNoDisplay。图标和中文状态不依赖外网。UI 操作必须投递事件，不能阻塞原生菜单线程。

验证：菜单模型与动作映射、默认 stub、macOS 原生构建、Windows 编译（如果工具链满足）、Linux 依赖缺失明确记录。禁止运行实际用户自启动/通知。

### D3 自启动及打包（独立 agent）

文件：internal/bridge/desktopstartup/**、packaging/drivebridge/**、.github/workflows/drivebridge-release.yml；不修改 manager/托盘/CLI 实现。

接口：`Spec{Executable,ConfigFile,CLIExecutable string}`；`Set(ctx context.Context,s Spec,enabled bool) error`、`Enabled(ctx context.Context,s Spec)(bool,error)`。入口 argv 为 `desktop _run --bridge-config <file>`，必要时加 `--lark-cli <absolute>`。参数无 shell 拼接执行。

macOS LaunchAgent，Windows 当前用户登录任务，Linux 桌面会话 XDG autostart（有图形环境才显示托盘）；注册与停用不停止已有同步。构建过程不自动注册。

可选 --desktop / -Desktop 构建：保留 CLI，同时添加独立桌面入口。macOS .app LSUIElement；Windows EXE windowsgui；Linux .desktop 及图标。桌面选项不可与 cli-only 混淆。支持 native 平台构建，不能靠 CGO0 假装含托盘。维护原签名、清单和 upgrade gate，不掩盖已有原生验收缺口。

验证：三个平台注册内容和路径转义的纯测试，packaging policy tests，实际 macOS 打包/载荷核对；Windows/Linux 原生安装与自启动无法执行时明确列出未验证。

### D4 编排、命令及集成（root）

文件：internal/bridge/desktop/**、cmd/desktop/**、cmd/all/all.go、rclone.go、cmd/cmd.go 及对应测试。

命令：`drivebridge desktop start [--headless]`（脱离终端，等就绪）、status、stop（仅退出托盘）、enable-autostart、disable-autostart；hidden `_run`。桌面入口双击无参数默认 `_run`；纯 CLI 默认行为不变。desktop _run 支持 headless 降级以验证生命周期，不伪称有托盘。

复用 ui.Start/OpenExisting/BootstrapURL，UI Host 与桌面生命周期绑定；已有手动 GUI Host 可复用但不抢其锁。托盘关闭不会停止 worker。无配置首次打开管理页；已有配置不反复弹浏览器。菜单打开目录只从已配置 MountID 解析路径。

状态轮询 3s，后台恢复退避 2s 至 5min，连续 5 次失败后等待用户重试；鉴权/身份错误不自动重试。同步操作异步执行。所有状态读取有超时；错误不会变成绿色已同步。

测试：状态聚合、多挂载局部失败、停止意图、退避、关闭 UI 继续 worker、重复打开、命令参数和产品 RC 隔离。

## 实施步骤

- [ ] 固定接口与依赖版本，先写有行为价值的失败测试。
- [ ] 各块补实现并运行受影响的小范围测试；交付 scoped commit。
- [ ] root 接入真实 Manager/UI/tray/startup，执行组合测试和竞态检查。
- [ ] macOS 实际托盘合成配置验证；Windows/Linux 编译及静态载荷检查，原生缺口单列。
- [ ] 最终集成块执行一次全项目检查，记录已知环境失败与跳过，不冒充三平台原生通过。
- [ ] 更新用户使用文档与实施状态。

## 易出错的边界

1. queued=0 但 unknown/awaiting_publish/VFS dirty 仍不能显示已同步。
2. CLI stop、GUI stop 和自动恢复并发时以用户停止意图为准。
3. 换配置/换账号不能复用旧桌面或 worker 身份。
4. 安装替换时不能由桌面监督器重新拉起旧进程。
5. Linux 无托盘、Mac/Windows 图形构建依赖不满足时必须可诊断，不输出伪完整安装包。

本文由实施者自查接口/职责一致性；按用户要求不增加逐任务审核与重复复核。外部资料见 research-notes.md；未运行的原生能力不写成通过。
