# 桌面客户端实施与验证状态

更新：2026-11-11；工作分支 `feature/feishu-transparent-sync`。最终验收仍未通过，尚未发布完整三平台安装版。

## 2026-11-11 实机续验点

- 已安装源码 `6dabe69ef`：系统 Installer 升级成功，原账号保留；19 个既有数据库和内容文件哈希未变化。Finder 向真实 FSKit 卷复制 MD/TXT 后，飞书网页回读正文成功，队列与冲突均归零。
- 同一飞书原生文档已验证本地 `local-2` 写回，以及网页等长 `cloud-3` 自动回读本地。普通 TXT 第二版正文回写正确，但 TextEdit 原子保存导致云端 Token 改变，不能判为保留身份的覆盖通过。
- FSKit 出现长度缓存截断：规范化 MD 从120变124字节，Overlay/VFS/cmount返回124，TextEdit读取旧120；重挂载恢复完整。上游 [#1222](https://github.com/macfuse/macfuse/issues/1222) 与 [#1207](https://github.com/macfuse/macfuse/issues/1207) 记录了该限制。用户已选择 kernel 后端，并在系统设置批准 macFUSE；系统要求重启，尚未执行 kernel 挂载验收。
- 待安装候选源码 `a6f56d2a5` 已构建完整 macOS arm64 desktop/cmount 的 pkg、dmg、tar.gz，仍为 unsigned 开发版。新增安全保存事务保留原 EntryID/Token，未完成 replacement 的备份保持显式 pending；配置读取不再探测运行中的挂载根，启动前仍检查选中目标，私有目录检查保留。Bridge/CLI 全包（cmount 标签）测试通过；Windows 桌面及 Linux 双架构 CLI 交叉编译通过。
- 正常 CLI 停止及后台恢复已在 `6dabe69ef` 验证；随后独立冲突挂载的目录读取和管理请求陷入等待，已采样并修复配置读取重入挂载路径的问题，待新候选安装复测。未强制删除待上传或冲突数据。
- 所有旧测试挂载的桌面 desired 已禁用；准备了未启动的 `Kernel 验收 20261111`（kernel，write-back=120s）。重启后应先安装修复候选，只启动该挂载，再验证长度变化、TextEdit 身份保持和两份真实文档的 diff/合并。
- 仍未通过：原生托盘视觉与登录自启动、全部共享目录自动发现、真实多文件合并、Windows/Linux 原生安装与挂载、签名公证。单元测试与交叉编译不能替代这些验收。

逐步说明和真实截图见 [操作手册](../../manual/操作手册.html)，对应 [验证记录](../../manual/验证记录.json)。以下为2026-11-10的历史实现及验证记录。

## 本轮实现

| 块 | 已实现内容 | 验证 |
|---|---|---|
| D1 | 当前 OS 用户唯一桌面实例、带身份校验的私有 IPC、持久启动/停止意图、旧 per-mount 自启动迁移、维护安装退出协调 | Manager 模块与组合 race 通过；新增测试确认退出托盘不停止同步 worker |
| D2 | macOS/Windows/Linux 原生托盘适配、状态图标、菜单动作、本地资源、默认 CLI stub、Linux 显示与指标服务探测 | macOS 原生编译、Windows Win32 交叉编译、托盘窄测试/race 通过；Linux 原生环境缺失 |
| D3 | 每用户登录自启动、双二进制桌面包、macOS .app、Windows 无控制台 EXE/快捷方式、Linux 应用入口、原生能力打包门禁 | startup 测试、32 项 packaging 测试、shell 语法通过；真实安装/自启动未执行 |
| D4 | desktop start/status/stop/capabilities/自启动命令、UI Host 复用、状态聚合、有限退避恢复、打开配置目录、产品 RC/metrics 隔离 | 桌面/CLI/Manager 定向组合测试、vet 与 race 通过；5 次实际脱离终端启动/认证退出循环通过 |

管理界面采用已声明的推荐路径：本机浏览器页面。本轮没有引入独立 WebView。已知旧上传、Markdown 兼容范围及飞书非 CAS 的覆盖窗口仍沿用原设计，不因桌面壳而消失。

## 行为验证

- 空配置可启动本机管理服务；重复实例不能抢占原 UI Host。不同配置不能复用已有桌面身份。
- 仅确认无脏缓存、待上传、未知结果、待发布和冲突时显示“已同步”。单个挂载状态失败不在托盘中抹掉其他挂载。
- CLI/管理页停止意图持久化；自动恢复重新检查意图。最多连续自动尝试 5 次，鉴权/身份错误不自动重试。
- 手动重试与自动恢复并发的回归测试先复现了负移位 panic；提交 `a4917d6e8` 用 generation 丢弃旧结果，保留活动状态，定向 race 通过。最终四个目标构建也在该提交通过。
- 真子进程测试先复现了退出过程中 descriptor 移除引起 CLI 误报，修复为关闭确认响应完整发送、状态在预留/退出窗口返回未就绪。之后连续 5 次启动/退出通过。
- 带 `RCLONE_RC=true`、无认证开关和本机测试地址的独立进程验证，没有开启遗留 RC 监听。
- 安装取得全局 lifecycle lease 后等待桌面 UI 和托盘释放所有权，之后才进入替换阶段；已有 worker 保留原安全停机与队列规则。

## 具体执行结果

1. `go test -race -p 4 ./internal/bridge/desktop ./internal/bridge/desktopstartup ./internal/bridge/manager ./internal/bridge/tray ./internal/bridge/ui ./internal/bridge/auth ./cmd/desktop ./cmd/feishu -count=1 -timeout=90s`：提交 `556edd005`，退出 0，17.20 秒。
2. 后续“退出托盘保留 worker”新增测试的 Manager 定向 race 通过；并发手动重试修复后 `go test -race -p 4 ./internal/bridge/desktop -count=1 -timeout=30s` 通过。
3. 最终 native 绑定检查 `go test -race -p 4 -tags desktop ./internal/bridge/desktop ./internal/bridge/tray -count=1 -timeout=45s` 通过；本机链接器有 LC_DYSYMTAB 警告，退出 0。
4. `go vet -p 4` 覆盖 desktop/desktopstartup/manager/tray/desktop command：退出 0。
5. `python3 -m unittest discover -s packaging/drivebridge/tests`：32 项通过；`bash -n packaging/drivebridge/*.sh` 通过。
6. 最终提交 `a4917d6e8`：macOS arm64 原生托盘（未编入 cmount）、Windows amd64 `desktop,cmount` GUI EXE、Linux amd64/arm64 CLI 全部构建通过。Linux CLI 构建不能当作原生 Linux 托盘构建证明。

macOS arm64 合成配置预览进程实际运行，自动打开本机浏览器管理页，并经私有 IPC 正常退出。Cua 无法获取该菜单栏 App/系统菜单栏，屏幕捕获返回 ScreenCaptureKit -3811，浏览器随后出现用户并发交互，因此停止继续操作。图标可见性、菜单点击及视觉验收**未通过**；没有使用其他 UI 自动化绕过。预览没有账号或挂载，未注册真实登录自启动，未安装驱动或程序。

## 本轮全项目测试

提交 `317cac7bc` 执行 `go test -p 4 -parallel 4 ./... -count=1 -timeout=15m`，退出 1，221.87 秒。所有 Bridge 包、飞书 backend、桌面/飞书命令、VFS/cache 通过。失败包：

- `backend/ftp`、`hdfs`、`seafile`、`sftp`、`sia`、`smb`、`swift`、`webdav`：外部服务测试脚本返回 `flock: command not found`，未能启动测试服务。这些包和 fstest 从 C 块到本轮源码没有变更；不把未运行的服务集成当成通过。
- `cmd/nfsmount`：本机 macOS 挂载返回 `operation not permitted`。

全量后发现并发恢复问题，只重跑修复影响的桌面 race/native 绑定与构建，没有重复执行整套项目测试。全项目结果不能标为全绿，也不能把较早 C 块仅 NFS 失败的记录替代本轮结果。git-annex 缺失与未开启的 native/live 验收仍为跳过。

## 尚未完成的原生交付条件

- macOS：当前缺少 macFUSE `fuse.h`；完整 `--desktop` 挂载包构建明确阻止，没有用 CLI-only 绕过。实际驱动、登录启动、升级退出、签名与公证需要原生验收。
- Windows：桌面及挂载代码交叉编译通过；WinFsp、实际托盘、用户任务、MSI/EXE 安装升级与签名未在 Windows 执行。本机没有 PowerShell。
- Linux：本机没有 pkg-config/GTK/Ayatana 构建环境；本地 Docker CLI 存在但 daemon 不可用，未启动 Docker 或接入远程上下文。原生 GUI 编译、不同桌面的图标可见性、真实 FUSE3 和登录启动未执行。
- 安装 lease 的原生环境/子进程存活传播仍为 `lifecycle_lease_unverified`，系统级 Linux 安装多用户协调及独立卸载路径保留原有发布限制，不通过托盘代码绕过。

证据摘要见 `evidence/2026-11-10/`。本机私有详细日志保存在忽略目录 `.superpowers/sdd/development/`。没有推送、发布或注册真实启动项。
