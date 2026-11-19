# Context Block Alignment Implementation Plan

> **For agentic workers:** Use superpowers:executing-plans; one final fresh-context review, preserve existing worktree changes.

**Goal:** Resolve legitimate equal-content blocks by unique context while retaining identity and writeback guards.
**Architecture:** Shared bounded anchor partitioning wraps conservative plain-gap and projected-gap alignment. The existing planner binds current cloud IDs and owns readback, capability, resource and version guards.
**Tech Stack:** Go, existing Markdown codec/planner, existing native maintenance installer.
**Spec:** docs/superpowers/specs/2026-11-18-context-block-alignment-design.md

## Global Constraints

- Never match only by ordinal across ambiguous repeated content; do not merge/delete distinct equal-content objects as deduplication.
- No relocation guessing or protected-content flattening. No reset/replay of unknown writes. Preserve source snapshots and latest user saves.
- No new configurable format rule required for a core correctness fix; macOS and Windows use the shared matcher.
- Bound total partition/DP work and stack depth. Scope writeback to uniquely bound current cloud IDs.

## Tasks

- [x] RED regression cases: equal tables in different sections, repeated headings with parent context, preserved duplicate tables, ambiguous deletion/reorder/cross-section movement, projected rich protection.
- [x] Implement shared bounded context partitioner; preserve conservative leaf matchers and hard-ambiguity fallback guard. Run focused and complete markdown suites.
- [x] Full real-case offline operation/readback replay and sync regression with immutable original captures/newer saves. Run affected Go/race suites and Windows build; one independent review and one test-backed correction pass.
- [x] Build macOS local test version, upgrade through maintenance lease, normally retry the diagnosed document, verify original tables/local body/config and true remote synchronization. Record limits and outcome.
