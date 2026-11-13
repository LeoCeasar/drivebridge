# Git 自动合并与差异体验实施计划

> 沿用用户已指定执行方式：分模块开发/测试，独立模块并行，一次最终集成验证。

**Goal:** Git clean 的文本无需人工合并，JSON 扩展转换规则，预备差异并稳定刷新。
**Architecture:** 已打包 Git merge-file + AST 规则 + 输入指纹缓存 + 稳定 DOM。
**Tech Stack:** Go、Git、原生 JavaScript。
**Spec:** docs/superpowers/specs/2026-11-13-git-merge-rules-fast-diff-design.md

## 约束及重点

不在前台保存增加网络/Git；不清理用户草稿/历史；不借自动合并绕过富资源/未知操作保护；Git 错误与内容冲突区分；规则不改代码/URL；缓存不返回旧来源；部分状态失败不删队列。

## A. Git 合并（主线程）

- [x] 在 history 新增可信 runtime 的有界 MergeText(ctx, base, local, remote)，测试真实 Git clean/conflict、超限、取消与环境隔离。
- [x] Sync 普通 md/在线 docx 接入结果；Git clean 以当前快照规划，无法表示的结构转 blocked 而非 manual conflict；测试同段分行并发及原有保护。

## B. JSON 规则（独立模块）

- [x] Markdown 新增默认 JSON 与可扩展加载器；测试缺省、自定义、非法规则、代码/转义保护；接入原有 AST 兼容边界。
- [x] 管理服务读取用户级 JSON，按规则版本保持写回/验证一致；文档明确字段、路径和生效方式。

## C. 差异预备与稳定展示（独立模块）

- [x] 管理页默认从已捕获本地输入读取，云端刷新显式执行。
- [x] 发现冲突后预备 diff，指纹变化失效、容量/并发有界；测试预热命中及草稿/来源变化。
- [x] 固定列表顺序/行身份，保留编辑状态与滚动；补充有行为断言的 JS/Go 集成测试。

## D. 集成

- [x] 各模块先 RED 再 GREEN；完成 Markdown/History/Sync/Store/Manager/UI 全部相关包、必要 race、Windows 编译。
- [ ] 核对当前实际队列，记录已完成与仍需保护项目；更新本机安装，验证版本与挂载；提交并推送 main。
