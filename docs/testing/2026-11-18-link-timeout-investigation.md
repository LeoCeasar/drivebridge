# 偶发链接超时：排查与修复

2026-11-18，macOS arm64。排查起始版本 0.2.28，最终本地测试版本 0.2.29。未提交、推送或发布。

## 复现证据

用户反馈同一文件重试有时成功、有时失败。只读确认 Finder 当前选中一份 HTML，未修改 Finder 选择或剪贴板。

该文件状态为 synced，旧 SourceURL 存在且包含当前 token，但 SourceURLToken 为空，无法确认旧缓存的对象绑定，fallback 缓存也不存在。因此按现有身份规则走首次在线补齐。

旧版 CLI 使用与原生服务相同的 `--timeout 3s` 连续测试：

| 请求 | 耗时 | 结果 |
|---|---:|---|
| 当前 HTML 第一次 | 3020ms | network，退出码 5 |
| 当前 HTML 第二次 | 3022ms | network，退出码 5 |
| 当前 HTML 第三次 | 2265ms | 在线成功，cached=false，并条件写回 |
| 随后同一 HTML 五次 | 19–20ms | cached=true，全成功 |
| 已缓存文档八次对照 | 19–28ms | cached=true，全成功 |

共 13 次缓存命中无失败。对同目录另一未缓存文件，用 12 秒诊断预算得到 4349ms 在线成功。3 秒总预算会截断正常但稍慢的身份验证、元数据查询和缓存补齐流程。

这些结果说明本次复现由过短预算与首次缓存补齐叠加引发，不能据此推断每一次超时都由相同子阶段造成。账号验证、凭据锁等待、云端请求的各自占比未被现有版本日志完整记录。一次直接子进程采样未捕获阶段，不能把空采样视为没有执行提供方 CLI。

## 修复

- Services 握手保留 3000ms，服务方法仍异步提交查询并返回。
- 原生调用的 Go 请求预算改为 15 秒，Swift 兜底改为 20 秒，另保留有限退出清理时间。
- 只有首次未缓存查询实际返回 context.DeadlineExceeded 才分类为 link_lookup_timeout；一般网络错误、授权错误和取消保留原分类。错误可重试，退出码 5。
- 新错误提示说明首次获取超时、缓存未补齐；不取消 token/类型/账号身份校验。
- 原生记录耗时、白名单结果码及缓存状态，未记录路径、token、URL 或凭据。
- 缓存成功写回、迟到剪贴板保护与提供方子进程取消机制保留。

## 自动验证

- 新 Swift 回归：尊重 CLI 预算的模拟 6 秒补齐，旧 3 秒预算失败 RED，新 15/20 秒预算在约 6.3 秒成功 GREEN。原生短截止仍终止进程。
- manager 新回归：首次查询预算耗尽从 network RED → 独立错误 GREEN；即使截止已到，也不把真实授权错误改成超时（RED → GREEN）。
- manager、store、cmd/feishu、lib/larkcli 完整相关套件通过。
- Swift 八类检查、launcher Swift 6 编译通过；Python 打包 54/54。
- Windows amd64 桌面交叉编译与 git diff --check 通过；未做 Windows 实机验收。
- 一次独立只读审阅，无 Critical/Important。Minor 保留：诊断耗时用 Date 墙上时钟，系统校时可能造成偏差；实际超时使用单调时钟。

## 本机升级与验证

- 通过维护租约升级 `/Applications/DriveBridge.app` 到 0.2.29，旧 helper 已退出，注册库仅有一条复制链接服务。
- 基于旧索引第一次挑选额外样本时，遇到 pending，81/28/26ms 即返回；这属于当时对象状态，未当作查询预算回归。随后通过认证 IPC 先检查当前可用状态，再选取另一未缓存 HTML。
- 新样本：100ms 测试预算在 118ms 返回 link_lookup_timeout、退出码 5；15 秒正常预算在 1894ms 得到有效 HTTPS URL，cached=false；再次查询 22ms，cached=true。
- 最终 running=true、queued=0、inflight=0、conflicts=0、last_error 为空、cloud_state=synced；配置与转换规则 SHA-256 与升级前一致。
- 没有调整系统权限、重置账号或读取云端正文。正常查询会条件补齐所测文件的本地链接元数据。

## 边界

没有承诺网络请求永不超时；本次修复的是 3 秒总预算过早终止首次补齐。原生错误面板与真实剪贴板没有在 0.2.29 单独重演，服务方法和剪贴板保护路径已有对应 Swift 检查。本地 unsigned 测试不代表正式签名、公证或跨平台发布验收。

私有日志、只读 store 副本及运行记录在忽略目录 `.superpowers/link-timeout-20261118/`，真实文件名与路径未写入本报告。
