# Single Feishu Link Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [x]`) syntax for tracking.

**Goal:** 单选右键复制现有飞书链接。
**Architecture:** Manager 负责路径及对象身份，官方 CLI 补齐缺失链接；Swift Services 与 Windows 静态 verb 只负责单选入口、剪贴板和反馈。
**Tech Stack:** Go/Cobra、现有 clipboard 依赖、Swift/AppKit、Python/WiX/Inno。
**Spec:** `docs/superpowers/specs/2026-11-18-single-feishu-link-design.md`

## Global Constraints

- 只接受一个绝对路径，沿用原权限，不上传、不下载正文、不支持批量。
- 查询失败保留原剪贴板；本机 IPC 沿用既有认证和 scope 校验。
- macOS「服务」与 Windows「显示更多选项」是已接受的入口。
- 不合并、不推送；本次在 feature branch 中留出可审阅的工作区变更。

## Review Focus

- 路径前缀碰撞、大小写和 Unicode 路径不得指向错误对象。
- 缓存 URL、查询结果必须属于目标对象，查询中重命名/替换需要拒绝。
- Windows 错误必须在无控制台调用时可见，多选不出现菜单。
- macOS 服务冷启动不得因 Launcher 退出而丢失请求；两种包布局都可用。
- 安装、升级、payload 白名单与卸载必须包含菜单/新启动器。

### Task 1: 路径解析与链接查询

**Files:** `internal/bridge/manager/link.go`, `link_test.go`, `ipc.go`, `status.go`; `lib/larkcli/link.go`, `link_test.go`。
**Interfaces:** `Manager.Link(context.Context,string) (LinkResult,error)`；`Drive.ResourceURL(context.Context,string,string) (string,error)`。
- [x] 先写并运行失败测试：缓存链接、缺失链接、挂载边界、未上传、合成目录、对象变化、危险 URL。
- [x] 实现元数据查询、解析及 worker IPC，运行相关 Go 测试。

### Task 2: CLI 与单次复制

**Files:** `cmd/feishu/link.go`, `link_test.go`, `feishu.go`; platform feedback 文件。
**Interfaces:** `feishu link --path PATH --json [--copy]`，复用 Task 1。
- [x] 写失败测试：严格单路径、失败不复制、成功与待同步提示。
- [x] 实现命令、剪贴板和 Windows 可见反馈；运行 CLI/平台测试。

### Task 3: 系统入口与打包

**Files:** `native/macos-installer/Launcher.swift`, shared services/core tests；`packaging/drivebridge/{package.py,macos_app.py,common.sh}` 及 tests。
**Interfaces:** 服务调用 Task 2 JSON；Windows verb 调用 Task 2 --copy。
- [x] 写失败打包测试：服务声明、配套启动器、Windows Single 注册及安装器条件。
- [x] 实现双布局 Swift provider、payload launcher、Windows 安装器注册。
- [x] 验证 Swift 单选处理、失败保留剪贴板，运行全部打包测试、Go 相关测试、macOS 编译和 Windows 交叉编译。
- [x] 更新使用说明并独立代码审阅，记录实机验收边界。
