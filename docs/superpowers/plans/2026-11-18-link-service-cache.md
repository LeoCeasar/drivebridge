# Link Service Cache Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** 修复服务身份冲突，持久缓存链接并保证 Finder 与剪贴板交互可靠。
**Architecture:** 独立 Swift helper 提供唯一 Services；Go manager 通过现有认证 IPC 与 store 条件写回元数据。
**Tech Stack:** Swift 6/AppKit、Python 打包、Go/bbolt。
**Spec:** docs/superpowers/specs/2026-11-18-link-service-cache-design.md

## Global Constraints

只支持单选；不变更分享权限、不读取正文、不提交推送、不覆盖既有工作区。仅使用当前安装自己的进程和状态。

## Review Focus

- 托盘运行与服务冷启动的 bundle 身份隔离。
- DMG/pkg/便携布局、签名顺序和文件白名单。
- 缓存绑定对象，拒绝查询中身份变化与已删除对象。
- 异步结果不覆盖后来剪贴板，超时不遗留子进程。
- 升级迁移仅触及本安装的 Swift helper，保留同步队列和账号。

### Task 1: 条件缓存写回

**Files:** internal/bridge/{store/link_cache.go,manager/link.go,manager/ipc.go} 及相关测试。
**Interfaces:** Store.CacheSourceURL(ctx, expected model.Entry, virtualType, url string) error；认证 /link/cache。
- [x] 回归测试 RED：补查一次后再次命中缓存，运行与停止模式，身份变更拒绝写入，队列不变。
- [x] 最小实现 store CAS、IPC 和 manager 写回；运行 store/manager/link 测试。

### Task 2: 独立服务与可靠复制

**Files:** packaging/drivebridge/{package.py,macos_app.py,build-macos.sh} 及测试；native/macos-installer/{Launcher.swift,LinkService.swift,InstallerCore.swift} 与测试。
**Interfaces:** 唯一 helper org.drivebridge.linkservice；fetchLinkReply(...timeout: 5)；copyLinkReply(...expectedChangeCount:)。
- [x] 回归测试 RED：独立服务 bundle、helper CLI 路径、超时和后来剪贴板保护。
- [x] 创建 helper，显式注册端口，修复升级注册/退出名单；实现超时与非模态反馈。
- [x] 打包全部测试、Swift 与 Go 相关套件、Windows 编译；一次独立只读审阅并修复重要发现。
- [x] 构建 0.2.27、安全升级，真实服务冷启动，用户确认剪贴板可复制；最终按新增反馈升级到 0.2.28，验证同步与配置保留。

## Ledger

- Final review: 一次独立只读审阅，无 Critical，3 Important，1 Minor；不重复审阅。
- Final: fixed 取消时遗留提供方子进程 — TestLinkSIGTERMCleansProviderDescendants SIGTERM 导致父进程直接退出 RED → 取消并回收进程组 GREEN；TestLinkDeadlineCoversManagerConstruction 未识别 timeout RED → 覆盖构造阶段的 deadline GREEN。
- Final: fixed 虚拟资源投影与补查缓存混用 — TestLinkVirtualCacheSurvivesURLLessObservationWithoutChangingProjection 投影被改动 RED → 独立缓存字段并保留离线复用 GREEN。
- Final: fixed pkg 缺少升级迁移 — test_pkg_postinstall_verifies_lease_before_migrating_services 缺失脚本 RED → 先验证维护租约再执行包内 updater GREEN；CoreTests 验证当前用户目标和拒绝 symlink。
- Final: minor (deferred): 忙碌或非法多选仍使用 Services error 指针交给系统反馈；异步查询错误已使用非模态通知。
- Final: Ruling: 审阅未判断真实 Finder/队列状态 — 以安装后实机补验决定是否完成；Developer ID 签名与公证不在本地 unsigned 测试中宣称通过 — 若未经后续验收发布会承担平台兼容风险。

- Steering: 用户实测 0.2.27 可复制，但成功通知点击打开编辑器；已按用户反馈移除 AppleScript 通知，成功静默，异步失败使用短暂非激活原生提示。构建与验收版本调整为 0.2.28。
- Task 1/2 complete: 54 项 Python 测试、Swift Core/Link 测试、六个相关 Go 包套件通过；store/manager 链接 race 测试通过（LC_DYSYMTAB 工具链告警已记录）；Windows 桌面交叉编译成功。
- Runtime: 0.2.28、系统唯一服务数=1；旧 helper 已退出、AppleScript 通知字节已移除；running=true/queued=0/inflight=0/conflicts=0/cloud_state=synced，配置与规则哈希保留，缓存查询 21ms。
- Evidence: docs/testing/2026-11-18-link-service-cache.md，保留真实剪贴板确认版本、提示面板和正式签名未验收范围，不扩张完成声明。
