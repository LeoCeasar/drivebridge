# 创建结果未知任务的有限核验与显式恢复

> **For agentic workers:** 分块实施，单块定向测试，最后合跑受影响包并做一次整体复核。

**Goal:** 旧创建任务不再永远显示上传；核验失败后明确需要处理，可重新核验或明确确认后使用最新本地版本重新创建。

**Architecture:** 保持未知 mutation 不盲目重放。创建 journal 增加可选核验次数、时间、原因及候选 token；最多三次失败或十五分钟后停在 blocked/creation_needs_attention。恢复操作带当前代次及正文 hash 校验，逐文件互斥，确认父目录和同名文件，再原子归档旧任务并创建新代次；旧 journal、blob、Git 历史保留。

**Tech Stack:** Go、bbolt、现有 IPC/CLI/管理页、官方飞书 CLI。

## 接口及边界

- `model.CreateRecoveryRequest`: job_id、action(recheck/recreate)、expected_generation、expected_local、confirm_recreate。
- `Store.CreateForJob` 读取创建 journal；`MarkCreateUncertain` 保存首次失败和候选元信息；`RecordCreateCheck` 记录失败并有限升级；`RecheckCreate` 只重新允许核验；`RecreateCreate` 原子归档并建立最新本地输入的新任务。
- `Engine.RecoverCreation` 执行账号、来源、代次/输入及云端完整目录核查。已有候选/已绑定 token、同名/临时文件、权限错误或不完整查询均不能直接重新创建。recheck 不执行新建写入。
- 管理页 `POST /api/mounts/{id}/uploads/{job}/recover` 与 CLI `upload recover` 使用同一模型；恢复必须带代次/hash，recreate 必须明确确认。运行中的挂载通过 IPC `/upload-recover`，不直接打开在线数据库。
- `SyncIssue` 附加创建恢复提示、当前代次/hash。等待核验与正在上传分开；未确认任务可下载原始本地内容。
- 旧未知记录缺少时间时使用已保存 staged_at 判定超时；没有首次错误码时明确报告“旧版本未记录”，不猜测网络或权限。

## 验收

- [x] Store：计数/时间/旧记录、候选保留、原子归档、不同版本拒绝、其他未知写入不可绕过、旧输入继续保护。
- [x] Engine：无候选/多候选/内容不同/网络失败有限升级；成功核验只绑定校验一致的对象；重建前阻止同名/父目录变化，最新本地内容与 Git 保护。
- [x] 管理页/IPC/CLI：准确状态、文件名、原因、重新核验、确认后重新创建、下载副本；会话/CSRF、非法输入、过期版本及停止挂载保护。
- [x] 集成及实机：受影响包、JS、race、Windows 交叉编译；打包更新本机，核查现有历史任务并按本轮明确恢复方案执行，上传回读确认，推送 main。

## 限制

当前父目录未列出旧临时文件不能证明它从未创建或被人移动；重新创建是明确授权的新操作，保留旧证据并提示可能存在其他位置的旧副本。不自动覆盖云端同名文件，不把归档标成已同步。

## 已完成验证（安装前）

- 受影响 Store、Engine、Manager、UI、CLI 整包通过；界面脚本 25 项通过。
- 创建恢复相关 race 检查通过；Windows amd64 受影响包交叉编译通过，不代替 Windows 实机验证。
- 新增回归先复现后修复：超时后停止自动核验、prepared 崩溃窗口、候选内容不符后仍保留身份；覆盖最新保存、同名/父目录/代次变化、归档失败和取消时保留响应身份。
- 一次整体代码复核发现候选身份保留缺口，已补齐；没有其他重要发现。

## 本机验收（macOS arm64，0.2.20）

- 安装包基于 `171b59fb1`，VCS 工作区标记为 clean；安装前后 23 个应用文件哈希完全一致，转换规则文件未改变。
- 通过 maintenance 安装租约替换 `/Applications/DriveBridge.app`，桌面服务启动并自动恢复原挂载。
- 历史创建任务先显示 awaiting_verification / 在途 0；点击管理页“重新核验”后因旧记录超过时间预算且未找到候选，转为 blocked / creation_needs_attention，展示明确原因与恢复入口。
- 使用当前第 4 代正文的 hash 明确确认重新创建；服务重新检查完整父目录、代次及父目录，再归档旧任务并建立第 5 代任务。
- 最终目录列出 1 个正式文件、0 个旧临时文件；独立下载 7,762 字节，SHA-256 与本地完全相同。
- 第 4 代原始正文可从 Git 历史完整取回；第 5 代上传确认已归档。
- IPC、CLI 和管理页最终状态为已同步，队列 0、在途 0、冲突 0，无恢复问题卡片。
- 另观察到升级前已存在的 Git 历史软容量超限与维护超时告警；归档 pending 为 0，未阻塞此次恢复，本轮未改变其清理策略。

**验证边界：** Windows 只完成受影响包交叉编译；本机安装包是未签名开发版，不代表 Windows 实机或 macOS 签名公证验收。
