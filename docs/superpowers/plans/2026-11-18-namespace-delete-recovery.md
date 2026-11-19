# Namespace Delete Recovery Implementation Plan

> **For agentic workers:** Use superpowers:executing-plans with regression tests before implementation. Preserve the existing uncommitted single-link feature.

**Goal:** Recover a local delete from verified directory absence, isolate unrelated namespace work, and classify pending-parent uploads as waiting.
**Architecture:** Engine reads complete provider listings; Store performs identity-checked atomic reconciliation. Namespace scheduling preserves overlapping path/resource dependencies while continuing independent operations. Status/UI distinguish waiting and unknown deletion from network failures.
**Tech Stack:** Go/BoltDB, existing CLI transport and JS UI; existing macOS maintenance/install workflow.
**Spec:** User-authorized fix following live diagnosis in `.superpowers/runtime-errors-2026-11-18/diagnosis.json`.

## Global Constraints

- No blind replay of a dispatched deletion, no deletion by filename, no clearing unknown journals without fresh proof.
- Complete parent listing absence fulfills removal from the original synchronized directory, not proof of permanent deletion throughout Feishu. Do not pursue an object moved elsewhere. The existing pre-dispatch missing-resource path already uses this scope-based rule; the new recovery uses stronger identity and parent CAS checks for dispatched work.
- Failed/incomplete listings and permission errors never establish absence. Preserve recovery bodies, history and task records.
- Keep overlapping paths/resource identities ordered; independent work can continue after failures and bounded operation timeouts.
- Do not overwrite the user's files, credentials or conversion settings during local update; preserve and verify pending data.

## Review Focus

- Token absent versus a same-name replacement, token still present, listing failure, parent rename/rebind, entry generation change.
- Unknown delete cannot be dispatched again; dependent deletes/moves must not bypass unresolved predecessors.
- Waiting for an unpublished parent must remain queued rather than trigger network advice; namespace uncertainty stays visible.
- Installation while uploads are queued must use the maintenance lease and preserve state.

### Task 1: Identity-checked delete recovery

Files: `internal/bridge/store/namespace_delete.go`, tests; `internal/bridge/sync/namespace.go`, `namespace_recovery_test.go`.
Interface: Store reconciliation accepts captured namespace work, current source entry and captured original parent. Engine supplies only a successfully completed validated listing's absence.
- [x] Test missing original token/no replay, same-name replacement, still-present token, failing listing, stale parent/source, legacy unknown task.
- [x] Implement readback recovery and run store/sync focused checks.

### Task 2: Independent namespace scheduling

Files: `internal/bridge/sync/namespace.go`, recovery tests.
- [x] Reproduce unknown delete followed by independent mkdir; verify overlapping parent delete remains pending and slow operations are bounded.
- [x] Preserve dependency ordering while continuing independent operations; run sync regression tests.

### Task 3: Waiting state and visible uncertainty

Files: `internal/bridge/sync/{create.go,errors.go,retry.go}`, model/status/UI and tests.
- [x] Test pending parent classification, normal queued status, explicit delete-result-pending UI without blind retry.
- [x] Implement waiting counters, metadata-ready retry admission and targeted status/UI checks.

### Task 4: Verification and local repair

- [x] Run affected Go suites, race checks and UI tests; obtain one fresh independent code review and fix findings with regression tests.
- [x] Build the macOS desktop package, update via maintenance lease, then verify the live pending directories and 20 files progress while preserving local modifications.
- [x] Record remaining uncertainty accurately; do not claim global remote deletion from a parent-directory observation.
