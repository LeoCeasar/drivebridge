# Link Lookup Timeout Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** 修复首次补齐被过短预算中断并提高错误可诊断性。
**Architecture:** Services 握手与异步查询采用独立预算；manager 标识在线补齐超时，native 记录脱敏耗时。
**Tech Stack:** Go、Swift 6/AppKit。
**Spec:** docs/superpowers/specs/2026-11-18-link-timeout-design.md

## Global Constraints

保持单选、对象身份校验和取消回收；不读取正文、不重置授权、不提交推送。

## Review Focus

- 较长查询不得恢复 Finder 同步等待。
- Swift 外层预算须大于 Go 在线请求预算。
- 缓存命中不走云端，错误分类不得改变其他网络/授权错误。
- 超时/取消仍回收提供方子进程，迟到结果不得覆盖剪贴板。
- 日志不得包含路径、token、URL 或凭据。

### Task 1: 预算与错误诊断

**Files:** native/macos-installer/LinkService.swift、LinkServiceTests.swift；internal/bridge/manager/link.go、status.go、link_test.go。
**Interfaces:** link_lookup_timeout，CLI 15s、原生 20s、握手 3000ms。
- [x] 先测试 6 秒补齐及独立超时错误，观察 RED。
- [x] 最小实现并运行 Swift、manager/store/CLI 相关验证。
- [x] 一次独立只读审阅，修复重要发现。
- [x] 构建并安全升级 0.2.29，核验服务、缓存和同步状态，记录边界。

## Ledger

- Task 1: manager 首次查询截止原来分类为 network，回归 RED → link_lookup_timeout GREEN；普通错误和超过截止返回的授权错误保持原分类。
- Task 1: Swift 模拟 6 秒查询旧预算失败 RED → 新预算成功 GREEN；自定义 1 秒截止、子进程清理和剪贴板保护仍通过。
- Final review: 一次只读审阅，无 Critical/Important；不重复审阅。
- Final: minor (deferred): 耗时日志使用 Date 墙上时钟，校时可能使诊断耗时偏差；实际截止为单调时钟，不影响取消行为。
- Final: Ruling: 审阅没有执行实机升级或测量在线耗时 — 用本机安装后证据补充，无法保证网络永不超时；本地 unsigned 测试不宣称正式签名、公证或 Windows 实机通过 — 未经对应平台验收发布仍存在兼容风险。
- Task 1 complete: 相关四个 Go 包整套、54 项 Python、Swift 八类检查与 launcher 编译、Windows 桌面交叉编译通过。
- Runtime: 0.2.29 已安装，唯一服务数=1，旧 helper 退出；新未缓存样本 100ms 预算返回 link_lookup_timeout（118ms），15s 预算成功（1894ms），随后 cached=true（22ms）。配置与规则保留，后台 0 排队/0 执行/0 冲突，synced。
- Evidence: docs/testing/2026-11-18-link-timeout-investigation.md，保留子阶段采样不足、pending 额外样本和签名/真实剪贴板未复验的限制。
