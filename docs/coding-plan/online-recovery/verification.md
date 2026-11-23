# 验证记录（2026-11-19）

本轮验证针对当前工作区源码；未提交、安装或发布。Windows 原生运行及真实飞书账号故障注入未执行，交叉编译不能替代这两项验收。

## 自动验证

| 检查 | 结果 | 本机证据 |
| --- | --- | --- |
| `go test -p 2 ./internal/bridge/... ./lib/larkcli ./vfs/... ./cmd/feishu` | 全部通过 | `.superpowers/online-recovery-20261119/go-verified.log` |
| `go test -race ./lib/larkcli` | 完整传输层竞态复验通过 | `race-larkcli-verified.log` |
| online / onlinecooldown / auth 完整竞态套件 | 通过 | `race-foundation.log` 中对应包通过行；该轮传输包失败另行复验 |
| store / sync / overlay / manager / UI 新增恢复场景竞态 | 通过 | `race-integration.log` |
| 取消继续、启动与重试准入竞态复验 | 通过 | `race-cancel-verified.log` |
| `node --test internal/bridge/ui/testdata/*.test.cjs` | 42 / 42 通过 | `node-final.log` |
| `python3 -m unittest discover -s packaging/drivebridge/tests` | 54 / 54 通过 | `packaging.log` |
| macOS arm64 CLI 与桌面程序 | 构建通过 | `build/drivebridge-macos`、`build/drivebridge-desktop-macos` |
| Windows amd64 CLI 与桌面程序 | 交叉构建通过 | `build/drivebridge-windows.exe`、`build/drivebridge-desktop-windows.exe` |
| Windows 专属测试程序 | 九个包交叉编译通过，未在 Windows 执行 | `windows-test-compile.log` |
| `git diff --check` | 通过 | 本轮终检 |

上述未列完整路径的证据均在 `.superpowers/online-recovery-20261119/`，属于本机私有验证输出。Windows 测试包为 online、onlinecooldown、auth、store、sync、overlay、manager、ui、larkcli。

构建使用 `cmount,drivebridge_feishu`，桌面额外使用 `desktop`；macOS 启用 CGO 与已有 macFUSE 头文件，Windows 使用 CGO=0，桌面增加 GUI subsystem 链接参数。

## 审查修正

独立集成审查提出七项 Important，逐项增加回归并修正，未发现需要放弃原持久操作日志的原因：

1. 手动重新核验使用观察专用 dispatcher 请求，已有候选被其他观察器先完成时仍不能获得改名权限。
2. 多候选、内容不符和位置/身份漂移的业务暂停，不能被后续请求失败或次数阈值覆盖。
3. 实际 worker / control / 停止入口提供可信取消来源；Start 仅释放用户停止限制，来源不明的取消仍须显式继续。
4. 本地观察输入读取失败持久化修复诊断及到期信息；无法落库的观察失败也有调度退避，避免完成事件造成快速重试。
5. 命名空间派发入口执行取消写入限制；已派发的工作仍可只读核验，显式继续保留原派发证据及业务暂停。
6. 协议暂停重启必须携带当前诊断周期和策略版本 CAS；延迟的旧请求不能重置后来的周期，旧记录只迁移一次。
7. 来源包装保留可选图片下载能力；通过 sync.New 构造的引擎可缓存未命中的文档图片。

另外验证了服务等待极值不会在抖动计算中溢出，历史计数饱和增长，普通重试无需写入无变化的取消状态。

## 失败与复验说明

早期完整测试暴露的旧断言已按新契约修订：观察不立即改名、重启保留等待、未知取消需要显式继续；没有删除原来的不重复创建、内容保留和代次检查。

一次高并发构建/测试同时运行时，50 毫秒准入测试和传输子进程 1.5 秒测试超时。分别连续独立复验通过，随后移除无取消限制时的多余数据库写入，并以较低包并发完整复验通过。未放宽这些测试的期限。macOS race 链接器仍产生既有 LC_DYSYMTAB warning，但对应套件通过。

## 尚未执行

- Windows 真实 WinFsp / 凭据管理器 / 资源管理器交互验收。
- 使用隔离飞书测试账号进行真实服务限流、断网、授权过期与丢响应实验。
- 新安装包签名、发布以及覆盖当前安装。
