# 单选复制飞书分享链接验收

## 自动验证

```sh
go test ./cmd/feishu ./internal/bridge/manager ./lib/larkcli ./internal/bridge/platform
python3 -m unittest discover -s packaging/drivebridge/tests
xcrun swiftc -swift-version 6 native/macos-installer/LinkService.swift native/macos-installer/LinkServiceTests.swift -o /tmp/drivebridge-link-service-tests
/tmp/drivebridge-link-service-tests
xcrun swiftc -swift-version 6 native/macos-installer/InstallerCore.swift native/macos-installer/LinkService.swift native/macos-installer/Launcher.swift -o /tmp/drivebridge-link-launcher
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -tags cmount,drivebridge_feishu,desktop -ldflags '-H=windowsgui' -o /tmp/drivebridge-link-windows.exe .
```

Swift 测试使用命名测试剪贴板，不读取或覆盖用户通用剪贴板。Go CLI 测试注入剪贴板动作；官方 CLI 元数据测试使用子进程协议夹具，不连接用户账号。路径用例包含中文、引号、空格、逗号、shell 元字符、Unicode 分解形式、挂载前缀碰撞和查询期间重命名。Windows 的大小写用例需在 Windows 上运行 Go 测试。

## 必须实机验证的项目

本次代码测试与交叉编译不能证明以下项目已通过；未为此次开发安装或替换用户现有应用。

1. macOS 图形安装包与 pkg/portable 两种布局：应用安装升级后，“服务”中出现入口；首次冷启动调用只复制链接，不打开管理页，不启动同步；重复调用仍可用。测试 macFUSE 与 FSKit 挂载。
2. macOS 多选两项拒绝执行，通用剪贴板保留原内容；单选文件、真实文件夹成功。服务禁用时启用后恢复可见。
3. Windows MSI 和 Inno 桌面包安装：文件与目录各有一个菜单，Windows 11 从“显示更多选项”可用，多选时不显示。命令没有控制台闪烁，错误有可见提示。
4. 两端分别验证普通文件、docx 投影的 `.md`、只读在线资源 `.url`、共享文件夹入口与子目录。链接在浏览器打开到对应对象，未更改原访问权限。
5. 未上传文件、挂载外路径、合成分类、已删除项目、无权限或网络失败：失败且剪贴板不变。已有缓存时断网仍能复制，提示未核验当前访问状态。
6. 本地存在待同步修改：链接可复制且提示云端当前内容，待同步队列和文件内容不变。停止 worker 后已缓存对象仍可从 store 查询；自定义配置按文档指定。
7. 安装升级后注册项指向新版本；Windows 在既有生命周期保护允许卸载时仅移除 DriveBridge 的两个菜单项，用户配置、凭据及缓存保留。macOS 移除应用后系统服务缓存刷新可移除入口。

## 2026-11-18 自动验证结果

- Go：CLI、Manager、Platform、Desktop、Store、Overlay、Sync、Cloud、larkcli 相关测试通过；单选链接与 shell 配置发现的 race 检查通过。
- 打包：53 项 Python 测试通过，包含 Services 声明、payload 启动器、Windows Single verb、卸载注册项声明与 Inno UTF-8 BOM。
- Swift：命名剪贴板服务测试和既有安装器核心测试通过；原生启动器编译通过。
- 构建：macOS CLI 与 Windows x64 GUI（cmount、desktop）交叉构建通过，Windows PE GUI subsystem 检查通过。
- 独立审阅提出的 6 项重要问题均已用回归测试修复；另修复了失败子进程输出成功 JSON 的剪贴板问题。完整相关 Go 回归已通过。
- 本节不代表下一节列出的原生菜单、真实飞书权限、签名安装与卸载验收已完成。
