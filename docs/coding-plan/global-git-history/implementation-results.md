# 全局 Git 历史：实现与验证记录

日期：2026-11-12。开发验证后，按用户指示继续完成了 macOS 本机升级与真实飞书同步测试，当前安装 **0.2.5**。没有提交或推送工作区。最新实机证据见 [real-machine-acceptance.md](real-machine-acceptance.md)。

## 已实现

| 模块 | 行为 |
|---|---|
| 本机保存 | 正文和持久历史事件在 Store 同一事务接受；保存链路不等待 Git 或云端。无法写入事件时拒绝提交该事务，避免出现没有历史输入的成功保存 |
| 后台归档 | 每账号私有 Git 裸仓库；不可变正文和资源按哈希归档，连续源水位和幂等回执处理重试。不会为建库遍历下载未打开的云端文件 |
| 全局历史 | 管理页“修改历史”提供账号/文件筛选、分页、正文、版本 diff、固定版本和恢复。待归档正文可从持久队列直接读取；Git 故障单独显示 |
| 安全恢复 | 恢复为新的本地修改，进入现有同步检查；校验账号、绑定、文件代次、路径、父目录及同名占用，拒绝覆盖并发修改。重复请求使用同一请求 ID |
| 图片与资源 | 保存按版本/正文角色绑定的资源；恢复历史正文时同步恢复对应图片引用，不向用户目录生成比较副本 |
| 保留与清理 | 默认保留 30 天、每文件至少 10 个不同版本、5 GiB 软预算。固定版本、未确认事件、未上传修改、冲突和正在读取的版本受保护；超预算可见而不强删受保护内容 |
| 后台维护 | 挂载 worker 自动归档；桌面后台也消费停用映射的持久队列。每天维护，失败退避重试。新 epoch 校验后原子切换，旧 epoch 延迟 7 天回收 |
| CLI | `drivebridge feishu history list/show/diff/restore/status/sync/pin/unpin/policy/compact`，与 GUI 使用同一服务和仓库 |
| 运行时 | 安装包携带独立 Git，按目标架构和清单哈希校验，不要求用户安装开发工具。源码构建的 macOS Git 最低系统为 14.0，仅链接系统库 |

Git 清理采用单进程内建 `prune`，配合 epoch 重建；没有启动取消后可能继续存活的自动 `gc/repack` 后台子进程。

## 验证证据

日志位于工作区 `.superpowers/global-git-history-implementation/`，不提交测试产物或用户数据。

- `final-suite.log`：history、store、manager、ui、overlay、sync、integration、desktop、config、cmd/feishu、vfs 共 11 个相关 Go 模块通过。
- `race-final.log`：history/Store 的归档、重放、压缩和历史相关测试通过竞态检测。
- `packaging-final.log`：46 项打包单元测试通过。
- `js-final.log`：17 项前端测试通过；新增历史脚本和应用入口语法检查通过。
- `windows-build-final.log`、`linux-amd64-build-final.log`、`linux-arm64-build-final.log`：Windows amd64 桌面构建、Linux amd64/arm64 CLI 交叉编译通过。
- 图片恢复集成测试：旧版 MD 与图片归档后，恢复旧版、产生后续版本并运行源缓存清理，离线仍能读取旧图片。
- 故障测试：回执失败重放不重复归档；缺正文返回错误而合法空正文返回零字节；未归档正文不被源 GC 删除；写事件失败导致保存事务回滚；二进制篡改被检测。
- 压缩测试：固定版本和读租约保留；租约释放后可过期；源旧事件在源正文已回收时补回执，不重放内容；清理后可重新打开仓库。
- `bundled-git-final.log`：使用最终包同源的 Git 2.56.1 运行完整 history 模块通过，包括分支丢失/回退拒绝和运行时哈希/架构校验。
- `package-verify-final.log`、`cli-help-final.log`：最终 tar.gz 解包后清单校验通过；内置 Git 和打包的 `feishu history --help` 可直接运行。

## 测试安装包

macOS Apple Silicon、0.2.5、未签名测试版，最低 macOS 14：

`.superpowers/global-git-history-implementation/artifacts-0.2.5/drivebridge-0.2.5-darwin-arm64-desktop-unsigned-test.dmg`

同目录包含 PKG、tar.gz、payload 清单和 `artifacts.json`（各产物 SHA-256）。本机使用 DMG 中的“安装 DriveBridge.app”完成升级和验收；较早测试构建已过时。原始 PKG 的维护校验不支持脱离协调器直接双击升级。

## 兼容性和当前边界

- Store schema 从 1 迁移到 2。旧程序不支持新 schema，不能通过清空状态解决降级；升级前应保留完整应用数据备份。
- 历史是本机已经观察/保存的检查点，不是云端完整审计、每次按键或多文件原子快照。多台电脑的历史独立。
- 普通二进制保存原字节，但网页不提供通用二进制编辑；文本 diff 有大小和行数上限。Git 不可用时的队列文本 diff 限额更小，界面显示受限状态。
- 源事件元数据和回执仍保留用于恢复/幂等；正文由引用图和 Git 保留策略回收。5 GiB 为软预算，不承诺应用总目录严格不超限。
- 首版未提供整账号历史导出/导入或跨设备迁移工具；不应直接复制正在运行的 Store 并作为独立新生产者使用。
- 尚未完成 Windows/Linux 实机安装与挂载、签名公证、真实断电和长期大规模性能验收。交叉编译及软件故障注入不替代这些验收。
- 本轮安装包为未签名测试构建；已按用户后续指令安装到本机，原有账号、配置和待处理数据保留。
