# 有界调用、公平调度与安全默认规则验收

日期：2026-11-13。实施计划：`docs/superpowers/plans/2026-11-13-bounded-sync-dispatch.md`。

## 改变的行为

- 普通 CLI 与非交互身份查询含凭据锁等待最多 2 分钟；上传/下载媒体等传输最多 15 分钟，调用方更早截止时间优先。交互登录保留原来的 10 分钟流程。
- 取消会回收调用进程后释放凭据锁；Unix 处理同组子进程和继承管道，Windows 先挂起启动、加入 kill-on-close JobObject 再恢复线程，失败则拒绝执行。Windows 本次仅交叉编译，尚未实机验证。
- 实际启动前失败返回明确未发送；实际启动后的写入超时保持未知结果。九处已知未发送的 journal 证据/状态重置不依赖已取消的请求上下文，避免误变成永久待核验。
- 正常运行的挂载使用独立调度器，五分钟边界捕获最新代次，不等待旧轮完成。最多三个文件并行，同文件串行；在线文档最多三个已回读步骤或约 30 秒后让出并继续原 desired。目录工作独立且有界。
- 每个后台在线文档 pass 总预算 90 秒，普通文件 pass 总预算 20 分钟，交互复核 45 秒。超时保留本地与 journal，不清空未知写入。
- 手动重试只在本地排队，不等待账号云端检查与整个上传完成；CLI 返回 accepted，状态接口负责报告云端确认。
- 策略跳过且没有进展的文件不立即循环入队。真正版本冲突、格式失败、未知结果均保留各自文件的状态，不因其存在暂停其他文件。

## 默认转换

新增四个固定结构策略，共八项有效默认规则：

- basic-inline-html：配对且无属性的 strong/b/em/i 转为原生粗斜体。
- simple-html-paragraph：一个安全 p/div 容器转为原生段落。
- indented-code：缩进代码转为原生代码块，内容保持字面量。
- ordinary-dollar-text：明确的金额和大写变量文本保留，例如 $100 和 $HOME。

无闭合标签、额外属性、图片、注释、复杂嵌套、链接标题与不明确的公式继续保护。TOML 仍拒绝写回，未新增不完整的转换或删除元信息。用户已有替换规则保留。

## 证据

- 定向先复现旧轮阻挡新轮、三槽等待、凭据等待无截止、继承管道/子进程迟到写入、取消丢失未发送证据、无进展文件热循环、手动重试等待身份核验；修复后通过。
- transport/cloud 包全量及 race 通过；auth/Markdown/store/manager/UI 包全量通过；sync 全量修正旧测试对现已支持 div 的假设后通过。CLI 与 UI 最终全量通过。
- 调度、取消证据与交互截止定向 race 通过；25 项 Node UI 用例通过。
- Windows amd64：transport、cloud、auth、Markdown、sync、manager、CLI、UI 交叉编译通过；不等于 Windows 实机验证。
- 飞书临时文档包含粗斜体、简单 HTML 段落、金额/变量与无语言缩进代码：创建→回读语义匹配→编辑→回读及 VerifyOperation 校验通过。临时文档已删除，未使用业务正文构造公开测试。
- 旧 projection 1/2/3 与旧四规则解释对比通过，未改变已有持久化快照的显示或块身份。
- 最终复核只做一轮，两个边界问题均以回归测试修复；没有清空历史、草稿或未知 journal。

原生安装后结果另行追加。

## 本机安装与验收

- 安装 macOS arm64 开发版 0.2.19，源 revision `85d0bfe9b`、VCS modified=false；安装前 23 个清单文件 SHA256 校验通过，维护租约安装返回 0。
- 挂载已恢复：running=true、queued=0、conflicts=0、last_error 为空。原来的一条 awaiting_verification 记录仍保留，未被清空或重新强制发送。
- 安装版 CLI `feishu upload retry ... --json` 实测约 0.049 秒返回 `accepted: true`，后续由后台处理；没有等待旧任务云端确认。
- 当前用户 conversion-rules.json 的升级前后 SHA256 一致；Chrome 实际账号与目录页面显示八项有效默认规则，新增四项说明与只读边界正确。
- 新管理页面已连接本机服务，后台继续运行。安装镜像校验完成后已卸载；未触碰用户缓存、历史和未同步内容。

产物：`.superpowers/installers-0.2.19/macos-arm64/`。这是 unsigned-test 开发版，未声明签名公证或 Windows 真机验收。
