# Markdown Writeback Compatibility Implementation Plan

> Use superpowers:executing-plans inline. User requested scoped tests per block and one affected suite at the end; no repeated approval/review loops.

**Goal:** Remove false manual conflicts from supported unilateral Markdown edits and simple HTML breaks.
**Architecture:** Existing Markdown AST, separate local/remote checkpoints, verified block mutations, existing Git history.
**Spec:** docs/superpowers/specs/2026-11-13-markdown-writeback-compat-design.md

## Constraints and review focus

Keep all local bytes/history; no production cloud mutations during diagnosis. Preserve code literals and unknown HTML. Relative paths remain readable without being unsafe active links. Unilateral table restructuring requires unchanged identity/content and a fully supported old/new table. Never bypass concurrent edits, rich resource protection, or generation guards. No foreground save/network changes.

## A. Compatibility

- [x] Add RED tests for paragraph/table breaks, code literals, safe relative links and unsafe URL/HTML denial.
- [x] Update AST rendering at the smallest boundary; validate semantic roundtrip and retained path text.
- [x] Run focused tests and full Markdown suite.

## B. Automatic planning

- [x] Add RED regressions for unchanged-remote row/column changes with stable surrounding blocks and real step/readback verification.
- [x] Add tests for concurrent edits, protected rich/merged cells and unchanged drafts.
- [x] Permit only verified unilateral supported table rebuilding; keep current block IDs/Token and staged readbacks.
- [x] Exercise current private captured documents in local-only diagnostics; commit only synthetic fixtures.

## C. State and integration

- [x] Route unsupported conversion to actionable blocked state instead of manual text merge, keeping immutable captures and genuine conflicts.
- [x] Provide a guarded retry for previously captured compatibility failures without consuming edited human drafts.
- [x] Run affected Markdown/sync/store/UI tests, concurrency checks and Windows compile where code requires it.
- [x] Record exact evidence and remaining unsupported cases; integrate only verified changes. Do not overwrite existing user documents to force success.

## Evidence

Private captures (not committed): both stored remote bodies equal base; real Git merge-file returns exit0 and exact local bytes. Before fix, codec rejects RawHTML/relative URL. Improved local-only planning plus independent context verification completes both captured documents in2 and68 synthetic operations. These are simulations, not API proof.

Markdown/sync/store/UI/manager complete suites passed. Race passed for Markdown/sync/UI/manager; a Store test fixture incorrectly compared pre-conflict status and was corrected, then the full Store race suite passed. Windows amd64 compilation passed for Markdown/sync/store/manager. One focused final review found no important defects and identified live list serialization as the remaining verification boundary. Disposable synthetic online document probe is running; production documents have not yet been changed by the diagnostic.
