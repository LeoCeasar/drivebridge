# macOS Services and Permissions Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** 修复重复链接服务，定位文件操作管理员验证。
**Architecture:** payload app 提供唯一服务，外层启动刷新两个当前安装 bundle 的注册，保留签名和清单。
**Tech Stack:** Python、Swift/AppKit/CoreServices、Go/macFUSE。
**Spec:** docs/superpowers/specs/2026-11-18-macos-services-permissions.md

## Global Constraints

不提交、不推送，保留既有工作区；不变更远端分享或系统安全设置。

## Review Focus

- DMG 嵌套 app 注册重复。
- pkg/便携布局仍可复制。
- 旧注册刷新不得删除应用、配置或凭据。
- 不修改已签名 payload 字节或使 manifest 失效。
- 删除/移动权限不可用 root 绕过；测试仅操作自建数据。

### Task 1: 服务唯一性修复与本机验证

**Files:** packaging/drivebridge/macos_app.py、tests/test_macos_installer.py；native/macos-installer/Launcher.swift、LinkService.swift、LinkServiceTests.swift。
**Interfaces:** assemble(...) 输出唯一服务；正常启动刷新当前安装 app 注册。

- [x] 递归服务计数及 payload 原样复制测试，观察 RED 2 != 1 后 GREEN。
- [x] 移除外层服务并刷新当前注册，53 项 Python 打包测试和 Swift 链接/安装器测试通过。
- [ ] 完成文件操作排查，记录未复现范围，只修复证实原因。
- [x] 一次独立只读审阅，修复重要发现。
- [ ] 构建 0.2.26，通过维护租约升级，实机验证唯一入口、复制链接和同步状态。

## 验证与审阅

- Task 1: 递归服务计数 RED 2 != 1 → GREEN，payload 哈希保持一致。
- Final: fixed 升级后可能继续使用旧 Swift 服务 — 新增本安装进程筛选及强制注册刷新测试，缺失接口 RED → GREEN；Swift Core 11 项检查通过，launcher 与 installer Swift 6 编译成功。
- Final review: 1 次只读审阅，无 Critical/Minor；唯一 Important 已修复，不重复审阅。
- Final: Ruling: Finder 实机菜单/请求接收与权限弹窗无法由只读审阅判断 — 留待本机验证；未复现的管理员验证不得宣称解决 — 若遗漏会造成用户继续遇到弹窗，须以具体文件继续追踪。
- Task 1: 本机 0.2.26 构建与维护租约升级成功，旧 Swift 启动器正常退出，LaunchServices 唯一服务数=1，链接 CLI 返回有效 HTTPS，后台 running=true/queued=0/inflight=0/conflicts=0/cloud_state=synced，配置和转换规则哈希未变。
- Pending: 具体触发管理员验证的文件尚待用户提供；Finder 后续因本机锁屏无法访问，须解锁后补验菜单点击及真实剪贴板，不能声明这些步骤已验收。
- Evidence: docs/testing/2026-11-18-macos-services-permissions.md。
