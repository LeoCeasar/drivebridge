# 链接服务与持久缓存优化

用户确认实施前一轮分析中的优化。系统实测 Go 托盘 PID 78205 的 bundle ID 为 org.drivebridge.desktop，与 0.2.26 的 Services 提供者相同；Finder 等待该应用的服务端口，但托盘未注册服务。

采用独立 `libexec/DriveBridge Link Service.app`，标识和端口均为 `org.drivebridge.linkservice`，可执行文件为 drivebridge-link-service。只由此 app 声明 NSServices，正常启动和服务启动均只接收链接请求，不启动托盘或管理页。运行时显式注册同名端口，服务握手超时 3000ms。外层及桌面 app 无服务声明。安装后更新三个 bundle 的注册并正常退出属于本安装的旧 Swift 服务。

链接仍在异步任务中查询：Go 请求预算 3 秒，覆盖配置发现和查询；响应 SIGTERM 取消并回收飞书 CLI 进程组。原生子进程最长等待 5 秒并保留短暂退出清理时间。用户在实机确认复制成功后反馈通知点击会打开脚本编辑器。因此移除 AppleScript 通知：成功静默完成；失败使用六秒自动关闭、不激活应用的原生 NSPanel，不调用通知脚本或模态错误框。异步结果不得覆盖用户在查询期间重新复制的内容。

目录扫描已有 URL 继续缓存；首次在线补查成功后，以账号作用域、entry ID、kind、token、路径和虚拟类型进行条件写回。补查结果独立保存在 CachedLinkURL/Token/Kind，不改变虚拟文件的 SourceURL 投影字段，不受无 URL 的目录刷新影响。并发改名/替换/删除不得写入失效结果。后续复制复用 URL，无时间到期策略，不联网核验访问权限，不改变分享权限。云端 token 改变后旧 URL 不再命中。无需下载正文即可复制。

高级 pkg 的 postinstall 再次验证当前用户维护租约，使用包内签名 updater，仅迁移当前用户 ~/Applications/DriveBridge 的 Services。验证属主、目录类型、bundle ID，拒绝符号链接；不切换用户、不提升权限。

验收：托盘运行时独立服务能冷启动、Finder 恢复交互、剪贴板为所选对象的 HTTPS URL；唯一入口；查询一次后离线缓存命中；运行/停止两种 store 路径均可写回；失败不污染缓存或剪贴板；原同步队列与账号配置不变。
