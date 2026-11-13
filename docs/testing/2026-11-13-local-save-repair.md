# 2026-11-13 本地保存修复验证记录

本轮把编辑器保存拆为本地持久日志确认、后台 Git/Store 归档及周期云端同步。前台保存不等待云端合并、上传或图片补齐。最终模块、竞态与原生复验均已通过；安装结果在完成升级后补录。

## 保存与同步规则

- 成功保存首先持久写入本地日志，随后可立即读取。归档、网络故障或云端冲突不撤回已经成功保存的正文。
- 后台从工作进程启动时起，每五分钟进行一轮云端同步。每个文件只处理该轮开始时可用的最后持久版本；轮中接受的新修改留到下一轮。持续编辑不会重置周期。
- 本地历史保留中间版本。只合并可证明未发出的云端任务；已经发出或结果不明的调用保留其凭据并先复核，避免重复创建或覆盖。
- 人工重试与需要上传完毕的安全停止可立即触发同步；用于维护升级的保留队列停止不强制联网上传。
- 目录元数据刷新、按需下载、已缓存文件读取独立运行，不等待五分钟。冷文件首次读取仍需取得完整正文。
- Typora/macFUSE 的打开旧文件→隐藏旧 inode→发布新文件→关闭旧 inode 路径保留原文档 ID/token。真实删除仍同步删除云端，不能把一般删除误当成编辑器替换。

## 已取得的真机证据

实际 macFUSE kernel/local 挂载使用合成 provider，不读取账号凭据、不修改用户云端文件。全部路径以 `.superpowers/nonblocking-local-files/` 为基准。

| 验证 | 结果 | 证据 |
| --- | --- | --- |
| Typora 连续保存与重新打开 | 后台归档暂停约125秒，真实 Typora 连续保存三次、关闭再打开，最新正文正确，目录无残留临时文件。恢复归档后原 ID/token 保持，最后正文成功同步到合成云端。 | `native/final-editor/editor-proof.json`、`native/final-editor/run.log` |
| 100轮原生保存 | 同一挂载运行100次普通保存、临时文件原子替换、热读取和目录读取，前台零失败；另覆盖旧 OS reader 跨替换保持可读。 | `native/final-editor/nonblocking-native-57045091-76f1-4e57-9944-fef6e7469f5b.json` |
| 冷打开与后台故障恢复 | 冷占位→读取正文→准确大小通过。注入后台归档 EIO/ENOSPC 时 OS fsync/close 成功、最新本地正文可读；解除注入后无需重写即可归档。此故障不代表物理磁盘不可写。 | `native/final-editor/native-701c5171-3472-476a-8b54-51dff6b66591.json` |
| Markdown 合并与冲突处理 | 普通 Markdown 非重叠修改自动合并；重叠修改保留冲突，经人工选择后成功同步；token 保持。 | `native/final-editor/plain-markdown-native.json` |
| 五分钟调度 | 假时钟覆盖周期边界、多次保存只发最后版本、上传中新修改推迟、边界后修改不抢入本轮、显式重试与安全停止、结果不明任务不被丢弃。 | `cadence-affected.log`、`cadence-deletion-test.log` |
| 隐藏旧 inode 安全边界 | 真删除、失败替换回滚、无关同名文件、已绑定替换、延迟归档、云端重扫与发布均有回归。已记录失败复现→修复通过。 | `soft-remove-focused.log`、`native/open-reader-red/run.log` |
| 打包检查 | 50项 Python 测试通过；desktop tag 编译与 tray/UI 测试通过。 | `packaging-tests.log`、`desktop-tag-tests.log` |

真机100轮延迟（未同时运行全量测试）：

| 操作 | p95 | p99 |
| --- | ---: | ---: |
| 单独 fsync | 3.78 ms | 5.62 ms |
| 打开、写入、fsync、关闭 | 13.28 ms | 16.65 ms |
| 创建临时文件、保存、原子替换 | 115.29 ms | 133.23 ms |
| 热读取 | 9.16 ms | 13.85 ms |
| readdir | 0.49 ms | 1.28 ms |

普通保存、读取和目录操作已处于毫秒级；原子替换低于150ms p99，但仍未达到最初提出的50ms p95目标。没有把该目标标记为全部达成。测试不能保证物理磁盘故障时仍保存成功。

## 实现与兼容边界

- 元数据 token/父目录/路径以及待处理命名空间使用事务索引；不再因查找单个文件反复扫描整个账号。
- 本地命名空间与归档通过持久 checkpoint/journal ID 路由，重启可重放；旧异步结果只能进入其历史代，不能覆盖新正文。
- 已缓存 Markdown 的正文读取不等待缺失图片修复；首次取得文档时仍准备正文及图片。
- 新状态使用 reader floor4。旧版0.2.9不能读取升级后的新路由，禁止直接降级并继续打开该状态。升级保留账号、配置、缓存、WAL和 Git 历史。
- Darwin 新增阻塞声明正确的 FULLFSYNC 包装，保留持久性；同进程 FUSE 自测不再卡住 Go 调度器。
- Windows只完成交叉编译，尚无本轮 Windows真机运行证据。

## 最终验收状态（待补齐）

- 受影响模块全量通过：`accepted-modules.log`；八个并发相关包竞态检查通过：`accepted-race.log`。Darwin 链接器存在 LC_DYSYMTAB 提示，未报告数据竞态。
- 最后一次全部原生用例通过：`native/release-final/run.log`，三组实际内核测试共44.499秒；覆盖最终防护代码。
- Windows amd64 最终交叉编译通过：`accepted-windows-compile.log`；该检查不执行 Windows测试。
- 0.2.10安装包与本机替换：下一步执行。

## 0.2.10 打包准备

本机输入已按仓库锁文件验证，记录在 `.superpowers/nonblocking-local-files/packaging-preflight.json`：

- Git 2.56.1：`.superpowers/global-git-history-implementation/git-runtime-darwin-arm64-final`，完整 runtime inventory 校验通过。
- 官方 lark-cli 1.0.99 包：`.superpowers/macos-package-682abc635/runtime/lark-package`，全部锁定包文件校验通过。
- 官方 arm64 原生归档：`.superpowers/macos-package-682abc635/runtime/cli-native.tar.gz`，归档和内部可执行文件 SHA-256 均匹配 `runtime-lock.json`。
- macFUSE 头文件：`/usr/local/include/fuse/fuse.h`。

最终源码就绪后，在仓库根目录执行以下构建；目前仅准备命令，未执行：

```sh
DRIVEBRIDGE_FUSE_INCLUDE=/usr/local/include/fuse \
  bash packaging/drivebridge/build-macos.sh \
  --arch arm64 --version 0.2.10 --edition feishu --desktop --unsigned \
  --lark-package "$PWD/.superpowers/macos-package-682abc635/runtime/lark-package" \
  --lark-native-archive "$PWD/.superpowers/macos-package-682abc635/runtime/cli-native.tar.gz" \
  --git-runtime "$PWD/.superpowers/global-git-history-implementation/git-runtime-darwin-arm64-final" \
  --output "$PWD/.superpowers/installers-0.2.10/macos-arm64"
```

构建输出必须不存在；脚本拒绝覆盖。该命令生成完整 Feishu 桌面、cmount、Git 运行时的 unsigned-test 包，不是签名公证发布。

安装应沿用 0.2.9 的 DMG 内安装器及维护租约流程。挂载最终只读 DMG、核对实际挂载点后，使用候选包自带的 `upgrade.sh` 调用同包安装器；不直接覆盖应用目录，不使用 sudo，不直接运行 detached pkg。若实际挂载点为 `/Volumes/DriveBridge 0.2.10`，相应命令为：

```sh
db_candidate_installer='/Volumes/DriveBridge 0.2.10/安装 DriveBridge.app'
db_candidate_app="$db_candidate_installer/Contents/Resources/DriveBridge.app"
db_candidate_payload="$db_candidate_app/Contents/Resources/payload"
/bin/sh "$db_candidate_payload/upgrade.sh" \
  "$db_candidate_payload/bin/drivebridge" -- \
  "$db_candidate_installer/Contents/MacOS/drivebridge-installer" \
  --install-worker "$db_candidate_app" /Applications/DriveBridge.app
```

该安装器核对应用 inventory，由维护协调器停止任务并持有安装租约，只替换应用文件。账号、缓存、日志和待上传数据位于应用外；仍需安装前后实际对照身份、挂载配置和待处理状态。CLI 安装 worker 本身不重新配置登录启动项，安装后应核验现有登录启动及桌面运行状态。
