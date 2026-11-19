# Windows Link Parity Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [x]`) syntax for tracking.

**Goal:** 对齐 Windows 的静默复制、非阻塞提示和查询期间剪贴板保护。
**Architecture:** CLI 在 Manager 创建前准备每次调用的剪贴板写入器；Windows 使用序号检查与独占锁，错误显示原生无激活短暂窗口。
**Tech Stack:** Go、golang.org/x/sys/windows、Win32、Python installer tests。
**Spec:** docs/superpowers/specs/2026-11-19-windows-link-parity-design.md

## Global Constraints

- 单选、成功静默、错误约 6 秒、不激活窗口。
- 同一次剪贴板独占锁内比较序号并写入；Manager 初始化前捕获。
- 当前功能分支保留全部已有修改；不提交、推送、发布。
- Windows 原生结果与 Mac 交叉编译明确分开。

## Review Focus

- Manager 初始化或查询期间用户改写剪贴板：新内容保留。
- 获取剪贴板锁期间变化：锁内再次检查。
- 请求取消或剪贴板忙碌：不清空既有内容。
- 无图形窗口站、序号为零：安全失败，不绕过保护。
- 提示不激活、不无限等待、关闭后释放原生资源。

### Task 1: 受保护的单次复制

**Files:** cmd/feishu/link.go, cmd/feishu/link_test.go, internal/bridge/platform/link_clipboard*.go
**Interfaces:** `NewLinkClipboardWriter(context.Context) (func(string) error, error)`；`ErrLinkClipboardChanged`；CLI 可注入 `prepareCopy`。

- [x] 写并运行失败测试：初始化前捕获、不同调用重捕获、序号变化/锁内变化/取消不写入。
- [x] 实现共用守护流程、Windows Unicode 原生适配器、非 Windows 兼容适配器。
- [x] 执行 CLI/platform 全套测试；交叉编译 Windows 专属测试。

### Task 2: 静默成功与无激活错误提示

**Files:** internal/bridge/platform/link_feedback*.go, Windows 专属测试。
**Interfaces:** `LinkFeedback(ctx, message, failed)`；内部窗口创建函数返回 HWND，以支持原生样式与生命周期断言。

- [x] 测试成功无提示、错误调用短暂窗口、取消不等待；Windows 原生测试检查实际窗口不激活及自动关闭。
- [x] 用 Win32 无激活 popup 替换 Toast/MessageBox，只在失败时展示。
- [x] 执行平台测试，交叉编译 Windows 测试。

### Task 3: 打包验证与交付记录

**Files:** .github/workflows/drivebridge-windows-package.yml, docs/drivebridge-desktop.md, docs/testing/2026-11-19-windows-link-parity.md。

- [x] 确保 Windows CI 执行本轮原生测试，打包模板测试覆盖现有单选入口。
- [x] 执行共用桥接套件、UI/打包测试，构建正式参数的 Windows CLI/桌面程序。
- [x] 记录当前源码、产物 SHA-256、构建参数与实机/安装器待验边界；根据可用工具生成可用产物。
- [x] 最终独立代码审查，修复重要问题并运行覆盖测试。

## 执行边界

本地实现、共用回归、Windows 交叉编译和二进制产物已完成。Windows 原生七项测试、完整 EXE/MSI 构建和 Explorer 实机验收仍待 Windows 环境执行；勾选表示本地任务完成，不表示原生已验收。详见 docs/testing/2026-11-19-windows-link-parity.md。
