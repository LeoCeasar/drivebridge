# Authorization Recovery Status Implementation Plan

> **For agentic workers:** Use superpowers:executing-plans inline, task by task.

**Goal:** Distinguish missing credentials from changed identities and explain authorization-owned mount blocking with an actionable current status.
**Architecture:** Preserve all account guards. Classify the existing CLI diagnostic DTO with typed safe errors; expose a read-only in-memory authorization snapshot on mount views; render that snapshot ahead of stale mount errors.
**Tech Stack:** Go, existing embedded vanilla JavaScript, Node test fixtures.
**Spec:** docs/superpowers/specs/2026-11-17-auth-recovery-status.md

## Global Constraints
- Preserve identity binding, cache, pending work, guard ownership and cloud verification.
- No raw credentials, diagnostic free text, or authorization URLs persisted.
- No official CLI changes or replacement of the running installed application.

## Review Focus
- Missing verification fields never imply verified identity or a real identity switch.
- Actual identity mismatch must still block even alongside a credential error.
- Progress must be scoped per profile and survive opening another management tab.
- Previous mount-level errors must not hide current progress; per-file issues remain visible.
- Expiry and completion must not leave an unusable action or fabricate synced state.

### Task 1: Credential failure classification
**Files:** lib/larkcli/identity.go, errors.go, runner_test.go; internal/bridge/auth/auth.go, auth_test.go; manager/status.go; sync/errors.go; associated tests.
**Interfaces:** Produce typed larkcli credential-required, credential-storage and verification errors; auth preserves causes and maps missing credentials to ErrUserMissing. Manager and sync publish safe classified codes.
- [ ] Add CLI subprocess fixtures/tests for missing, storage, unverified, real mismatch and secret redaction; watch RED.
- [ ] Implement typed classification, preserving real identity checks and safe machine-only causes; test GREEN.
- [ ] Cover auth status/reconcile and manager/sync classification propagation; run focused package tests.
- [ ] Commit the completed classification change.

### Task 2: Observable authorization and actionable mount UI
**Files:** manager/automatic_login.go; ui/types.go, manager_adapter.go, assets/app.js, assets/auth-view.js, assets/recovery-view.js; Go and Node regression tests.
**Interfaces:** Read-only AutomaticLoginForProfile returns an immutable snapshot. MountView.authorization carries sanitized progress; UI reads it per mount, with safe link only while waiting.
- [ ] Add backend and rendered-UI regressions for waiting, new tab, second account, timeout/retry, successful recovery and retained per-file issues; watch RED.
- [ ] Implement safe progress projection and state-based mount actions, separate bound-account copy and historical mount error display; test GREEN.
- [ ] Run manager/UI and all Node tests; commit the completed behavior.

### Task 3: Product verification and review
**Files:** Evidence/report under docs/superpowers; no live credential changes.
- [ ] Run product Go suite and race checks appropriate to the changed packages, all Node tests and macOS mount-enabled build.
- [ ] Preview UI with synthetic auth states and visually inspect; do not interrupt running mounts.
- [ ] Review complete diff, resolve findings with regression tests, record evidence and commit.
