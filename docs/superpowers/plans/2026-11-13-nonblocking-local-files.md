# Nonblocking local files implementation plan

> **For agentic workers:** Use superpowers:executing-plans. Verify each independent block with focused tests, then run the complete affected suites and real mount acceptance. The user approved the repair design in the conversation and requested continuous implementation without extra approval gates.

**Goal:** Background archive, cloud refresh and media backfill must not delay successful local saves or make cached files/directories unavailable.

**Architecture:** Durable local data and namespace publication are the foreground boundary. Immutable generations, background archive and cloud acknowledgement are separate; cached readers retain a complete readable generation. Cloud conflicts never discard or roll back a successful local save.

**Tech Stack:** Go, bbolt, Rclone managed VFS, macFUSE; existing desktop packaging.

**Spec:** Approved repair design in conversation, extending `docs/superpowers/specs/2026-11-13-local-save-pipeline.md` to include editor atomic replacement, removal and cached read availability.

## Global constraints

- Preserve existing cloud tokens, pending journals, accepted snapshots and Git history.
- Foreground save must not perform cloud requests or wait for background full-body archive.
- Never acknowledge a namespace change before its recovery information is durable.
- Old upload acknowledgements must not overwrite a newer local generation.
- Existing cached Markdown remains readable when image backfill fails.
- Confirmed readonly/virtual resources remain protected; cloud conversion restrictions apply to upload, not editor temporary operations.
- No production data or cache deletion as an upgrade strategy.
- User-confirmed cloud cadence: one background cycle every five minutes; take
  the newest durable version of each file at that boundary. Intermediate saves
  remain local/Git history and must not each trigger a cloud merge. New saves
  during a cycle wait for the next one. Explicit manual sync may bypass the wait.

## Review focus

- Atomic replacement during a stalled archive; renamed journal recovery after interruption.
- Cloud publication overlapping an already open reader and new directory opens.
- Multiple acknowledged saves followed by a stale upload acknowledgement.
- Legacy accepted Markdown with missing image bodies while offline.
- Tombstones and duplicate cloud tokens during index migration and rename.

## Task 1: Indexed catalog and cached policy

Files: store blob-free metadata lookup/index code, overlay hidden shared policy.

- [x] Add tests for token/parent lookup after insertion, rename, tombstone, reopen and duplicate token.
- [x] Maintain derived indexes transactionally and rebuild on initial migration.
- [x] Use transactional indexes for policy lookups; omit additional policy memoization because the indexed lookup removes the measured scan cost without adding invalidation state.
- [x] Run full store tests and relevant overlay tests; record results.

## Task 2: Local cached document reads

Files: overlay doc_cache.go, doc_assets.go, lifecycle; doccache.

- [x] Reproduce cached text denied by offline image backfill.
- [x] Make normal read preparation local; bounded background repair owns missing media and shutdown.
- [x] Avoid XML parsing and blob checks on every warm read/listing.
- [x] Run overlay/doccache tests including image acquisition and legacy repair.

## Task 3: Save and namespace independence

Files: VFS managed rename/remove, vfscache journal publication, overlay/store namespace interfaces.

- [x] Turn diagnostic archive-stall and publication-busy probes into desired-behavior regressions.
- [x] Separate durable namespace inputs from background archive completion; retain/recover immutable inputs across rename and replacement.
- [x] Remove global archive waits from foreground namespace; fence stale stage results by identity and generation.
- [x] Cached readers and ancestor directory opens coexist with background publication.
- [x] Preserve local save generations and defer cloud namespace actions while their content is pending.
- [x] Verify repeated saves, replacement, removal, failure injection and restart recovery.

## Task 4: Integration and real machine acceptance

- [x] Run complete VFS, bridge, Feishu backend, CLI/desktop and operations suites, plus race tests for changed concurrency components.
- [x] Run frontend tests and Windows compile checks for changed shared interfaces.
- [x] One final focused whole-change review; fix material findings with regression coverage.
- [ ] Build candidate without replacing current app; isolated real mount tests against controlled temporary data.
- [x] Measure warm opens/listing and repeated normal/safe saves under stalled background work; initial targets p95 50ms, p99 150ms for small warm files on this machine.
- [ ] Upgrade local app preserving pending data, verify actual mounted Typora workflow, recovery and sync.
- [ ] Record actual evidence and any unmet acceptance criteria; do not claim completion on partial evidence.

## Task 5: Fixed cloud sync cycles (user clarification)

- [x] Separate five-minute upload/reconciliation cycles from directory refresh.
- [x] Snapshot eligible generations once per cycle; process each file once.
- [x] Coalesce only provably undispatched work; preserve ambiguous/in-flight
  receipts and every accepted local history generation.
- [x] Test repeated saves within the interval and new saves during an upload,
  using a fake clock rather than five-minute test sleeps.
- [x] Keep explicit sync/retry available and cover the cadence with integration
  tests. Local save must remain independent of the cloud cycle.

Measured final evidence: [validation record](../../testing/2026-11-13-local-save-repair.md). Ordinary saves p9513.28ms; safe-save p95115.29ms/p99133.23ms. Safe-save p95 remains above the initial50ms target; do not mark that target met.
