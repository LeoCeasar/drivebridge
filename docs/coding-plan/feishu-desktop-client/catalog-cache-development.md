# 目录清单、正文缓存与 Finder 云盘体验修正

2026-11-11，基于用户对 Google Drive 使用体验的反馈。沿用既有 Rclone VFS、持久 Store、官方 CLI 与应用内登录，不改变文件写回协议。本文区分源码实现、模块测试、本机 kernel 探测与正式安装载荷验收；正式载荷升级、Finder「位置」窗口观察及本次合成 Markdown 自动上传已取得验证结果。

脱敏验证记录：[catalog-cache-verification.json](evidence/2026-11-11/catalog-cache-verification.json)。本记录汇总本轮实际测试、kernel挂载、应用升级及Computer Use观察结果。

## 用户行为与边界

- 账号盘展示个人目录和共享目录。链接、名称搜索是补录方式，不是第三种目录归属；补录来源标记可保留。
- 归属必须有当前个人目录树或共享入口关系的证据；无法核验时提示重新扫描，不猜测，不让同一资源产生两个可写路径。已覆盖的嵌套目录复用已有路径。
- 目录元数据持久保存在本机。首次获取完整元数据后，重复浏览、重启和暂时离线先展示缓存的文件名；列目录与 metadata-only getattr 不获取正文。
- 首次未缓存的目录仍需完成一次元数据请求。后台逐目录预热，每轮有界、跨轮继续；已知目录数量不等于云盘目录总数，不承诺首次扫描前或某一轮之后整个目录树都已完整。
- 文件正文在实际 Open 时按需获取，并可复用安全缓存。Finder、Spotlight 或其他进程的真实 Open 也可能触发下载；不能把“用户没有双击”视为“系统没有打开”。
- macOS 卷使用友好名称，保留现有用户目录挂载路径。kernel 后端增加本地卷参数，程序不修改 Finder 磁盘可见性等偏好，不创建需提权的 `/Volumes` 目录，不迁移缓存或待上传数据。FSKit 不增加未经验证的 kernel 参数。

## 目录缓存与独立索引

原 `Fs.List` 在全局锁内总是刷新个人根，再列目标目录，慢请求会拖住已缓存目录。现在使用持久目录完成标记、缓存优先和逐目录刷新；网络 I/O 不占全局目录锁。后台发现的新名称先发布到本地目录清单，再使 VFS/系统目录缓存失效；通知是刷新提示，不承诺每个 Finder 窗口立即重绘。

同步引擎不再串行等待整个目录刷新。元数据索引独立后台运行，立即启动、按配置间隔继续；默认目录刷新间隔为 30 秒。正文清理是另一条独立后台任务，每 5 分钟执行，不能与慢元数据扫描放在同一个串行循环。退出时各任务取消并等待完成，再关闭其使用的 Store。

`MountStatus.directory_index` 展示已知、已缓存、正在刷新和失败计数；目录索引进度与上传确认分别展示。旧版本存在未确认数据的手动目录先兼容保护，不能通过隐藏或强制迁移丢失它；空旧分组不展示。

## 正文缓存清理策略

默认保留最近使用的正文 **7 天（168 小时）**，总容量软上限 **5 GiB（5,368,709,120 字节）**。旧配置未填写年龄时沿用 7 天；年龄设置为 0 时仅按容量清理。清理不删除目录清单、资源身份或文件占位，后续实际打开可重新获取正文。

候选仅来自已同步且可重建的干净正文。打开的读写句柄、dirty VFS 缓存、待上传或待确认工作、冲突及其草稿/副本、恢复记录、活动 Blob 读取和其他受保护引用均不得为满足容量而强制删除。共享 Blob 必须等最后一个可释放引用消失才能回收；候选的预计释放字节为 0 不等于可以跳过该引用。

清理先做受保护 GC，再合计 Store 保留的 Blob 字节与 VFS 当前已获取的数据字节；VFS 使用即时重算，不依赖可能陈旧的周期统计。此计量不包含文件系统分配与元数据开销。候选按实际 Open 的最近访问时间排序，同时间按路径稳定排序；stat、列目录不延长正文使用时间，VFS 缓存命中的实际 Open 仍更新使用时间。

超过年龄，或合计数据仍超过容量时，逐条尝试释放。VFS 先为该路径保留更新窗口并检查打开/dirty 状态，Store 再核验 generation、候选访问时间及完整引用保护。期间发生新的访问、写入或版本变化时不使用旧候选强制删除；busy 项跳过后继续其他候选。成功后发布本地占位对象、GC 并重新计算合计，不能只减预计大小。已经提交的占位转换须在取消或后续清理错误后仍完成到活跃 inode 的发布。

**24 小时 prepared Blob grace 保留。** 新准备或尚受保护的数据可能使实际占用暂时高于 5 GiB；这是软上限，不能以硬删未确认数据、冲突或恢复材料换取达标。所有写回与冲突保护沿用原有协议。

## 冷占位的 stat/Open 修正

原 `cmount.lookupNode → VFS.Stat → Dir.Stat → MaterializeNode` 会在 Finder 查询属性时获取正文，fh getattr 还会显式 materialize。新增 `VFS.StatMetadata/Dir.StatMetadata` 后，managed cmount 的 lookup/getattr 使用纯元数据路径；旧 `Stat` 语义继续保留，实际 Open 仍先取得完整、精确大小的正文。

本机 macFUSE kernel 探测发现第二个问题：未知大小占位先被内核缓存为 0 后，仅让 Open 取得真实大小仍不足够，原先冷 read 返回 0，mmap 失败。`KeepCache=false` 只控制数据缓存，并不保证刷新 vnode 属性；全卷或默认冷句柄 direct I/O 又有 mmap/高层应用兼容风险。

本轮通过本地 cgofuse 兼容桥暴露 macOS 的 `PurgeAttr/PurgeUBC`，managed Open 在正文就绪后请求一次属性与 UBC 刷新，保留普通 I/O；普通非 managed 挂载不取得该策略。本轮真实 kernel 探测中，修后 read 和 mmap 均完整取得 **5,174 字节**且内容一致。此结果只覆盖该探测，不等于所有编辑器、文件类型或正式载荷都已验收。

## 卷名称与 Finder「位置」

原 `VolumeName` 未设置，回退到含内部 UUID 的 Fs 名称。现在账号盘采用账号友好名称，单目录盘采用挂载名称，并对显示名做安全处理。kernel 后端添加去重的 `local` 标志；不引入自定义文件系统 bundle 或仅为外观加载额外图标模块。

先前 CUA 合成挂载已实际观察到 Finder「位置」入口，说明该路径不必迁到 `/Volumes`。macFUSE 官方仍将 `local` 列为实验性选项，并指出它会带来 `.Trashes` 等本地卷行为；系统元数据不得因此成为新增的云端业务文件。验收时经 View → Show Sidebar 恢复了此前隐藏的侧栏；没有更改磁盘可见性或系统安全设置。

最新正式安装载荷升级后，CUA 窗口接口已恢复，Finder「位置」已看到采用账号友好名的 DriveBridge 卷；进入个人目录及其验收子目录后正常列出 1 个 Markdown 文件。截图：[61-account-volume-locations.png](../../manual/assets/61-account-volume-locations.png)。这是一项正式载荷的 Finder 入口与列目录观察，不是 File Provider 徽标或全平台验收。

## 验证记录与未完成项

本轮完整 bridge、飞书 backend 和 VFS 测试通过。cmount 排除上游真实挂载用例后的单元测试通过（0.853 秒）；cgofuse 显式标记桥测试通过（0.689 秒）；packaging 39 项测试通过。缓存块的窄测试覆盖年龄/LRU、合计计量、busy 跳过、访问时间 CAS、共享零字节候选、软上限和取消后占位发布。

上游 `cmd/cmount TestMount` 的临时 non-local 合成挂载收到 `EPERM`，该用例未通过；同一命令在未修改的 `35f3cb67e` main 基线上也复现相同权限错误，确认不是本轮新增行为；不能因此记录“全 cmount 测试通过”，也不把排除该用例后的通过当作其问题已修复。

最新本机 kernel probe 已观察到后台新增名称经 OS readdir 可见，目录元数据列举没有触发正文 materialize。正式载荷升级成功：171 个受保护数据文件的 hash 没有变化，安装后的 binary 与候选匹配，原先两个 active 挂载均恢复 running。真实账号盘根目录仅有个人/共享两个分组；本次个人根列出 7 项，共享列出 9 个入口。抽查 3 个个人子目录分别为 1、0、5 项，首次耗时分别为 0.001、0、0.337 秒，重复进入均为 0 秒。以上是本次样本的列目录结果，不能外推任意共享链接全集或全子树完整性。

真实验收目录中新增的 `test/drivebridge-cache-acceptance-20261111-161719.md` 为 110 字节合成 Markdown，本地保存与读回一致。沿用验收挂载已有的 120 秒 write-back 延迟后自动上传，engine 回读核验在 `2026-11-11T08:20:45.916925Z` 确认该验收挂载 `cloud_state=synced`、queued/inflight/conflicts 均为 0；该计数不是所有历史挂载的汇总。120 秒不是产品全局默认，本项也不额外声称经过独立飞书网页比对。

Windows no-CGO cmount 交叉构建与 Linux amd64 CLI 构建均退出 0，仅证明相应构建完成，不替代实机挂载与编辑器验收。

尚未完成 Windows/Linux 实机、正式签名与公证、任意共享链接的可访问资源全集，以及所有编辑器/格式的冷打开验收。Google Drive 的 File Provider 入口、云图标、逐文件下载徽标和 App 退出后仍可访问的完整系统行为，不因本轮 FUSE 改造就视为已实现。

参考：[macFUSE 挂载参数](https://github.com/macfuse/macfuse/wiki/Mount-Options)、[macFUSE 文件信息标志](https://github.com/macfuse/library/blob/master/include/fuse_common.h)、[Google Drive 的 File Provider 与传统挂载差异](https://support.google.com/drive/answer/12178485?hl=en)。

验收补充：TextEdit 已打开同步完成的合成 Markdown 并显示完整正文，见[截图62](../../manual/assets/62-synced-test-markdown-open.png)。新 MD 投影为飞书原生文档，回读可能规范化 Markdown 转义；本项验证不代表 Markdown 字节保真。
