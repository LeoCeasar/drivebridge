# DriveBridge 飞书透明同步 v1.1

本工作项已完成需求澄清、真实接口实验、设计与任务化开发文档；尚未开始产品实现。

| 文档 | 用途 |
|---|---|
| [design.md](design.md) | 最终功能边界、每用户配置、数据模型、VFS/overlay/同步协议和验收 |
| [development.md](development.md) | 13 个实施任务、文件职责、共享接口、依赖与验证命令 |
| [validation-results.md](validation-results.md) | 真实飞书接口结果、反例及可复查证据 |
| [review.md](review.md) | 独立上下文复核、4 项关键意见及修订复验 |
| [recon.md](recon.md) | 用户确认、源码事实和原问题的处置历史 |
| [readiness.json](readiness.json) / [development-gate.json](development-gate.json) | 写作前的信息闭合度记录；不是测试通过率 |
| [document-validation.json](document-validation.json) | 最终文档摘要、绑定校验和未执行的发布条件 |
| [handoff.md](handoff.md) | 可选实施启动文案；尚未创建 Goal 或开始编码 |

当前范围：本地 Markdown 默认映射飞书在线文档，保存后透明双向同步；普通文件覆盖；无法自动合并时保留副本并弹窗/diff；不同用户和电脑独立配置。

安全边界已经用户明确确认：尽力合并、检测与恢复，不承诺网页和本地同段并发修改时绝对不覆盖。三平台实际运行和签名仍是实施/发布门槛。

实施时先读设计再读开发文档；不得以文档复核通过代替功能验证。实验目录和凭据不能进入产品默认值或安装包。
