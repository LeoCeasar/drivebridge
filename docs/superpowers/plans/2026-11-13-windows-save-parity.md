# Windows Save Parity Implementation Plan

> **For agentic workers:** Use superpowers:executing-plans for the root task and independent bounded agents. User authorized implementation after the audit; avoid repeated approval/review loops.

**Goal:** Repair concrete Windows gaps and obtain evidence for the native save/delete boundaries.

**Architecture:** Keep common checkpoint, namespace and sync engines. Reuse immutable reader generations for local deletion, existing background process creation for Git, and visible durable-local-save error state for failed WAL checkpoints. Native Windows tests own disposable mounts only.

**Tech Stack:** Go, bbolt, managed VFS, cgofuse/WinFsp, Git, GitHub Actions.

**Spec:** `docs/superpowers/specs/2026-11-13-windows-save-parity-design.md`

## Global Constraints

- Preserve identities, WAL, conflict bodies, Git history and five-minute cloud cadence.
- Never acknowledge a durable save after a failed local checkpoint; no network on save.
- Keep active-writer and namespace exclusion intact when admitting existing readers.
- No account credentials or production cloud writes in native CI.
- Do not label cross-compilation as Windows runtime verification.

## Review Focus

- WinFsp deletion callback precedes its own Release, including empty directories.
- Old reader after unlink/recreate must retain its own generation and never resurrect the deleted inode.
- Concurrent writable handles must retain protection; verify the Windows delete preflight contract.
- Close-only local WAL failure must be visible even if WinFsp ignores callback return values.
- Current bundled Git and WinFsp must execute; absent drivers/native evidence must fail explicitly.

## Task A: Git background processes

Files: `internal/bridge/history/git.go`, `runtime.go`, Windows process tests.

- [x] Add a Windows command-boundary regression covering version probe, archive and streaming Git body process startup.
- [x] Verify RED, reuse `platform.BackgroundCommand`, run focused history tests and Windows compile.

## Task B: Local deletion with reader generations

Files: `vfs/managed_remove.go`, `vfs/dir.go`, managed admission helpers, cmount/integration deletion tests.

- [x] Turn the isolated Open→Unlink→Release probe into desired-behavior regression; include old reader content, absence after reopen, and writer protection.
- [x] Permit readers only for checkpoint-backed local namespace deletion; retain transactional publication and recoverable committed errors.
- [x] Cover empty-directory driver reader; keep remote-deletion policy unchanged.
- [x] Run complete affected VFS/Store/Overlay/Integration packages after focused GREEN.

## Task C: Local checkpoint failure visibility

Files: vfscache local journal/checkpoint and status tests; VFS/manager status forwarding and UI issue mapping as needed.

- [x] Inject local WAL fsync failure and assert local-unsaved/error state rather than archived/synced success.
- [x] Retry a closed failed save locally, keep bytes/history, clear error only after successful checkpoint.
- [x] Keep background archive errors distinct and cover explicit flush failures.

## Task D: Windows native and CI gates

Files: `internal/bridge/integration/*windows*test.go`, Windows test workflow and evidence documentation.

- [x] Add real WinFsp tests for repeated saves, open-reader replacement/deletion, directory deletion, cache generation refresh, and restart/recovery where supported.
- [ ] Run current Windows Store/Overlay/Sync/History/VFS suites with actual packaged Git.
- [x] Configure a hosted Windows driver gate with pinned WinFsp and explicit opt-ins; preserve native evidence artifacts.
- [ ] Execute the remote Windows job if available, inspect failures and fix them; GUI editor/manual and power-loss gaps remain explicit.

## Task E: Integration

- [x] Run affected module suites, concurrency checks and full Windows driver compilation.
- [x] One focused final review, record fixes and remaining native environment limits.
- [ ] Commit validated code and test evidence; do not claim unexecuted Windows GUI acceptance.
