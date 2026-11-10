# 官方 CLI 1.0.99 的实际产物来源

日期：2026-11-10。检查对象为官方 npm 完整性校验通过的 1.0.99 包及其校验表绑定的五个平台原生产物；详细摘要见同目录 official-cli-native-artifacts.json。

实际二进制的 `go version -m` 均记录源码提交 `8d8357d39dd15ac5e9aa815d4ab2639d56bd3239`、模块 v1.0.99、2026-11-04 构建、工作树未修改。最初参考源码检索使用稍晚的 `1bfdcc2c1a0ed0296ddb6349d251bdd75145bc5a`，不得将该检索提交误报为二进制构建提交。

在已授权的 ignored third_party/larksuite-cli 克隆中取得实际提交后，执行两个提交间的限定 diff：cmd、internal、shortcuts/common、shortcuts/docs、shortcuts/drive。Drive、Docx、认证、配置、客户端、运行时、相关 schema 和 go.mod 没有变化。相关运行时唯一变更为 internal/envvars/read.go 增加 Codex/IDE agent-name 识别提示；托管 runner 的环境允许集合不传这些变量。其他变化属于 Apps 功能和技能/质量检查测试，不作为本轮 Drive/Docx 契约变化。

代码中的 cloud.CLIRevision 已更正为实际产物提交 8d8357d39；InspectedSourceRevision 单独记录 1bfdcc2c。现有 macOS arm64 真实协议实验对应原生文件 SHA-256 a8f652c2a62577a152467a983ec60548570fb0a08715e95fa3f579438c81885b，与本次官方产物校验一致。

这证明产物身份和相关源码契约一致；不证明其他操作系统已实际执行 CLI、挂载或安装。签名会改变可执行文件字节，因此签名发布采用“验证上游原始摘要→先签 CLI→将签后摘要及上游摘要编译进应用→再签应用”的流程。可编辑 manifest、环境变量和用户配置不能授予写操作能力。

来源：[官方 1.0.99 发布](https://github.com/larksuite/cli/releases/tag/v1.0.99)、[实际源码提交](https://github.com/larksuite/cli/tree/8d8357d39dd15ac5e9aa815d4ab2639d56bd3239)、[后续检索提交](https://github.com/larksuite/cli/tree/1bfdcc2c1a0ed0296ddb6349d251bdd75145bc5a)。
