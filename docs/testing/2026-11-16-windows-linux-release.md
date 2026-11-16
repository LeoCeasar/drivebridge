# Windows / Linux 0.2.21 预发布记录

## 工作区清理

额外 worktree 的 HEAD 已合入 main，唯一提交数为 0，未提交及非忽略未跟踪文件为空。112 个忽略的诊断文件（4,296,098 字节）先归档到主仓库的私有 `.superpowers/worktree-archive-20261116/` 并校验哈希，再删除 worktree 及已合并本地分支。最终只保留 main 工作区。

## 构建与验证

- Windows x64 沿用已验证的 0.2.21 EXE/MSI/ZIP，构建源码 `9dd7774e5`。三个产物重新计算 SHA-256；载荷 64 个文件已校验。Windows 生产构建的 525 个仓库依赖文件均未被本轮 Linux 修复改变。
- Linux amd64、arm64 原生构建源码为 `ce51861ce`。各包均含完整桌面/CLI、官方飞书 CLI、私有 Git；本地重新校验每包 19 个载荷文件及两个 Go 程序的源码版本、clean 状态。
- 各 Linux 架构共享 Go 套件 12 个模块通过，1,463 个通过测试事件、4 个显式启用类跳过；25 项界面测试、52 项打包测试通过。
- 各架构句柄等待测试 100 个通过事件、适配器测试 5 个通过事件、竞态检查通过；托盘编译及无图形会话测试 18 个通过事件。
- 各架构 10 次真实 mount2/FUSE 内核测试通过，无跳过。包含保存、原子替换保持 Token、后台 Store 归档 EIO/ENOSPC 后不重写恢复；使用隔离测试提供方，不加载真实账号。归档故障是注入，不代表物理磁盘断电/写满验收。

[Linux 执行记录](https://github.com/LeoCeasar/drivebridge/actions/runs/43809500920) · [Windows 原生记录](https://github.com/LeoCeasar/drivebridge/actions/runs/43473795684) · [Windows 构建记录](https://github.com/LeoCeasar/drivebridge/actions/runs/43476968614)

## 本轮修复

Linux 原生测试发现 close 返回后最终 FUSE Release 尚未完成，紧接着的原子替换会遇到写句柄保护。新增 Linux 专用就绪探测，最多等待 250 ms，再执行一次真正重命名；探测不创建命名事务，不关闭 Flush 后仍存活的重复描述符，真实写入者仍返回 EBUSY。保留最终重命名原有的原子预留检查。新测试先复现再修复，并执行重复原生检查。

打包验证原本把隐藏的 mount2 命令误判为缺失。现检查目标命令自身 usage，同时保留“未知帮助主题不能算作存在”的回归检查。

## 发布范围

[GitHub 预发布版](https://github.com/LeoCeasar/drivebridge/releases/tag/drivebridge-v0.2.21)

- Windows：推荐完整 ZIP 解压运行。EXE/MSI 需通过配套 upgrade.ps1 安装协调包装器；直接双击会被保护机制拒绝。卸载入口仍受保护，未开放直接卸载。
- Linux：仅发布可用的每用户 TAR.GZ（amd64、arm64），不发布被系统级多用户生命周期保护阻止安装的 DEB/RPM。注册用户桌面入口可运行包内 install-desktop.sh，不自动启用登录自启动。
- Linux 应用内网页登录组件尚未提供；账号授权及目录映射使用 CLI。托盘需要支持 AppIndicator / StatusNotifierWatcher 的图形会话。
- 全部为 unsigned_test 预发布产物。未声称 GUI 编辑器、真实账号登录、安装器升级或签名验收完成；原 payload manifest 的未验证标记保持原样。

发布附件包含各平台 payload/验证摘要、release-manifest.json 和 SHA256SUMS。发布清单分别记录真实构建源码，未将 Windows 二进制来源改写为 Linux 提交。
