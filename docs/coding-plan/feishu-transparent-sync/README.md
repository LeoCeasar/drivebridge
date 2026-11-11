# DriveBridge 飞书透明同步 v1.2

当前实现（2026-11-11）：CLI、后台桌面管理、Markdown 同步与冲突处理已实现；macOS 已接入应用内登录、目录发现与账号盘。使用与证据见 [操作手册](../../manual/操作手册.html)、[账号盘说明](../feishu-desktop-client/discovery-usage.md)及[原生登录验证](../feishu-desktop-client/owned-login-verification.md)。Windows/Linux 原生登录与实机验收、正式签名公证仍未完成。

以下为 v1.2 设计阶段记录：Task 0 已把参考审核结论纳入设计和任务契约，独立文档复核见 [review-v1.2.md](review-v1.2.md)。历史文档通过不代替产品实测或发布验收。

| 文档 | 用途 |
|---|---|
| [implementation-decisions.md](implementation-decisions.md) | F01/F02/F03/F05 文档裁决、F04 回归、任务所有者及 LV-01–LV-05 能力门槛 |
| [design.md](design.md) | v1.2 的 R-01–R-13、身份/凭据协调、路线、精确元数据、完整分页和同步边界 |
| [development.md](development.md) | 保留 T01–T13、文件/模型所有者、接口、依赖和实际验证命令 |
| [参考审核](audits/2026-11-10-reference-reuse/README.md) | v1.1 的 4 项契约缺口（F01/F02/F03/F05）和 F04 兼容性反例；历史证据原样保留 |
| [validation-results.md](validation-results.md) | 前轮真实接口结果与反例；不能证明本轮新增能力通过 |
| [review.md](review.md) | v1.1 历史独立复核；v1.2 状态为 pending |
| [readiness.json](readiness.json) / [development-gate.json](development-gate.json) | 当前缺口和信息门槛；不沿用历史 95 分，不等于实现/发布许可 |
| [document-validation.json](document-validation.json) | 当前摘要绑定、本地检查与 not-run 真实条件 |
| [recon.md](recon.md) | 用户确认和原问题处置历史 |
| [handoff.md](handoff.md) | 当前普通执行授权；未创建 Goal |

范围保持：本地 Markdown 默认映射飞书在线文档，普通文件原 Token 覆盖，保存后后台同步；无法安全自动合并时保留副本并弹窗/diff。R-10 为尽力合并、检测与恢复，网页和本地同段并发仍有覆盖窗口。账号、凭据、实验路径不能进入产品默认值或安装包。

当前实施工作区为 `/Users/rrong/.codex/worktrees/5b46/drivebridge`，分支 `feature/feishu-transparent-sync`。先读设计再读开发文档；共享文件按所有者交接，独立研究/复核可并行。不自动安装驱动、远端写入、推送、发布或创建 Goal。

Task 0 独立文档复核：[review-v1.2.md](review-v1.2.md)。通过仅允许按能力失败门槛继续实施，不代表云端/平台已验证。
