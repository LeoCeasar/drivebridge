# macOS / Windows 0.2.31 预发布构建记录

本轮用户于 2026-11-23 授权打包发布在线诊断与安全恢复更新。发布范围为 macOS Apple Silicon 和 Windows x64 的完整飞书桌面包，保留功能分支，不合并 main，不覆盖本机安装。

## 源码及可追溯性

- 功能提交：`8fee47393ec1eb9882ad5931b6da6a6dd51a7482`。
- 安装包共同源码与发布目标：`351ecaac5d32eb64df74be0823151e0f48be604e`。
- CLI / 桌面二进制的 Go 构建信息均指向该源码，`vcs.modified=false`。
- [Windows 原生门禁及安装器构建](https://github.com/LeoCeasar/drivebridge/actions/runs/45145198656)：两个作业成功。
- 发布页：[0.2.31 macOS + Windows unsigned preview](https://github.com/LeoCeasar/drivebridge/releases/tag/drivebridge-v0.2.31)。

## 验证

- 发布前本地完整桥接、CLI、VFS Go 回归通过；UI 42 项、打包测试 54 项通过。
- Windows 共享套件 21 个模块，2708 个测试通过事件；通用运行的 163 个显式启用类测试跳过，不视为已通过。
- 必需的七项原生剪贴板/提示测试、三项真实 WinFsp 门禁均单独启用并逐项通过，无跳过；包括 200 次直接/安全保存、删除恢复、缓存代际与对象身份保护。
- Windows 近期回归 57 个通过事件，UI 42 项通过。新增 auth / online / onlinecooldown 套件已在 Windows 执行。
- Windows ZIP 的 64 个载荷文件逐项检查，内嵌清单与产物清单一致；PE 子系统 CLI=3、桌面=2。
- macOS 原生构建 DMG / PKG / tar.gz；便携载荷 23 个文件逐项检查，DMG 完整性、图形安装器 27 个文件及其内嵌载荷均通过校验。

首轮 Windows 作业 45142872156 在共享套件中暴露两项测试计时边界：启动夹具在 2 秒准备预算内执行了大量落盘，健康文件也继承了用于阻塞文件的短传输超时。准备数据已移出启动计时，健康文件使用独立预算；启动 2 秒与槽位释放 5 秒断言均保留。manager / sync 完整本地套件及连续 5 次针对性竞态通过，第二轮 Windows 优先回归和完整门禁通过。生产同步逻辑未因该修正而改变。

## 完整产物

| 文件 | 字节 | SHA-256 |
| --- | ---: | --- |
| `drivebridge-0.2.31-darwin-arm64-feishu-desktop-unsigned-test.dmg` | 46857300 | `ca59d3355fc228e40f258d5cd01656336e0ab884387b7bcac3bd797e60d3c1e5` |
| `drivebridge-0.2.31-darwin-arm64-feishu-desktop-unsigned-test.pkg` | 50877235 | `38536841b4db6af9fbb49114298b3b3af4b3b9b445c6c4ab8ceb0ba980bd68b1` |
| `drivebridge-0.2.31-darwin-arm64-feishu-desktop-unsigned-test.tar.gz` | 42101927 | `136b76d9416e5a2e87764f341054f83f8b959d5362da473f691882302bf4c23e` |
| `drivebridge-0.2.31-windows-amd64-feishu-desktop-unsigned-test.exe` | 106394576 | `beb6e86756dca12f39ac1cc7a21edd3fc1ada4cb2e1c1466c32cf2f37d001a7a` |
| `drivebridge-0.2.31-windows-amd64-feishu-desktop-unsigned-test.msi` | 143093760 | `b5723de186f1cd846b7d56a0ecd5edeaa9ee36c78389c10510ab8902637c182a` |
| `drivebridge-0.2.31-windows-amd64-feishu-desktop-unsigned-test.zip` | 136508979 | `3b7fe93f68f3f9119f4b23a006358e52cf782d7c82b87f7859902332d3342d77` |

发布附件另含两个平台的 payload / artifacts / verification 清单、release-manifest.json 和 SHA256SUMS。原始载荷的未验证标记保留；未用交叉编译替代原生测试，也未把自动化测试等同于真实账号或安装器验收。

## 安装和边界

- macOS 仅 arm64，要求 macOS 14+、macFUSE；DMG 内提供图形安装器，PKG 是必须经 `upgrade.sh` 协调的高级入口。
- Windows x64 要求 WinFsp、WebView2。EXE/MSI 必须经 ZIP 中 `upgrade.ps1`、PowerShell 7+ 和既有安装协调锁启动；ZIP 不自动注册右键菜单。
- 未签名、macOS 未公证。真实飞书账号、人工 Finder / Explorer、安装升级、卸载和签名验收均仍未完成。
- 不发布 Intel Mac / Linux 包，不修改真实账号或用户文档，不自动安装本版。
