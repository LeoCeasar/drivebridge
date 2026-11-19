# 单选复制飞书分享链接

用户已确认：单选文件或真实文件夹，右键复制飞书已有访问链接；接受 macOS「服务」和 Windows「显示更多选项」入口。沿用原权限，不上传正文、不修改分享权限、不支持多选或清单导出。

公共接口为 `Manager.Link(ctx, absolutePath)`，CLI 为 `feishu link --path <absolutePath> --json`；`--copy` 供 Windows 无控制台桌面程序使用。按挂载边界解析路径，通过现有 worker IPC/withStore 查询投影索引；优先使用可信的 Metadata.SourceURL，缺失时通过绑定账号查询官方元数据。查询后重新核对对象身份。新建未上传、已删除、合成分类目录和挂载外路径返回明确错误，解析失败不得改剪贴板。已有云端对象的本地待同步修改不阻止复制，结果附带提示。

macOS：现有 Swift Launcher 同时作为 NSServices provider，单次只接受一个 file URL，异步调用配套 CLI 后写入 NSPasteboard。普通启动继续启动 Go 桌面进程；服务启动保持 AppKit 事件循环。payload app 和图形安装的外层 app 共用启动器与服务声明，兼容两种布局。服务声明随应用安装更新，无额外持久安装项。

Windows：复用 drivebridge-desktop.exe，注册 HKCU 文件/目录静态 verb，MultiSelectModel=Single，调用 `feishu link --copy --path "%1"`。MSI/Inno 条件性注册和移除。失败可见、成功提示，只有链接解析成功才写剪贴板。

考虑过 Finder Sync 与 IExplorerCommand，因首版允许子菜单入口，选用较小的系统集成。原生菜单实际可见性、签名安装和 WinFsp/macFUSE/FSKit 行为必须分别实机验收，构建/单元测试不能代替。
