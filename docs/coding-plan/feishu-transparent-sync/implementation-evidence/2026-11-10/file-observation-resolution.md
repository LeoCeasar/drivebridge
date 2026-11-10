# Ordinary-file metadata resolution before T02

2026-11-10. Read-only technical evaluation of the current worktree and saved evidence. No CLI/account/remote calls, credentials, dependency changes, product/document/index edits, or tests were performed. This report is the only authored file. T01 files were read but not changed. Paths below are repository-relative; official CLI source is the existing `third_party/larksuite-cli` clone cited by protocol-preflight.

## Recommendation and scope

**Use an explicitly typed observed-download snapshot as a second ordinary-file route.** It can satisfy exact *local* size, stable reads, and preservation of the bytes actually obtained before a write/delete, without claiming a server version or current-head identity. This is an engineering inference from the implementation and approved best-effort semantics, not a newly proven upstream guarantee. It needs a small explicit contract revision before implementation: the present design 6.5 and T02 brief reject this route, and cannot truthfully be reported as already satisfied unchanged.

The controller clarified that completeness here means a transport-completed, successful authenticated full-endpoint response and exact immutable local bytes, not independent proof of the server's entire intended file. Under that scope, the recommendation is viable; missing currentVersion does not disable ordinary reads.

Keep `FileMeta(ctx, token)` strict if it means “metadata for a proven current server version”: absent linkage it still returns `metadata_unknown`. Add a separate full-download observation helper; do not squeeze its output into `FileMeta.Version`. Missing current-version metadata then blocks version-dependent operations, **not ordinary access that can obtain and durably bind a complete local observation**. Known-version and observation capabilities remain separate.

The observation is “bytes returned by a successful full-file endpoint request during [start, finish]”, not “global head at finish”, a revision, a remote lock, or proof of every intermediate remote edit. A content hash is a local identity, not a server API argument. This route does not establish an exact point-in-time remote snapshot during a concurrent server write; if that stronger property is mandatory, available evidence cannot enable it.

## Source-backed findings and actual evidence

| Evidence | What it establishes | What it does not establish |
| --- | --- | --- |
| `shortcuts/drive/drive_download.go:245–284` | `+download` issues GET `/open-apis/drive/v1/files/<token>/download`, without a version or Range parameter, streams the body to a file, reports `saved_path` and copied `size_bytes`. An explicit relative output path is supported. | No returned server version, ETag, independent checksum, response length, or head linkage. |
| `internal/vfs/localfileio/localfileio.go:70–88`; `atomicwrite.go:24–35,94–143` | Body is copied to temporary file, read/write errors propagate, file is synced/closed then renamed. | `SaveOptions.ContentLength` is ignored by this layer; that does not disable HTTP transport framing checks. Parent directory is not synced here. CLI output is not a durable store adoption. |
| `internal/client/client.go:175–291` | Streaming HTTP request, error status handling, context and body handling are implemented. | No success requirement for exactly HTTP 200 in this path; shortcut output cannot let the adapter independently reject an unexpected 206/Content-Range. |
| Saved `evidence/2026-11-10/24-download-overwritten.json`, `25-overwrite-check.json` | One latest-endpoint readback after same-token overwrite was 81 bytes and matched known intended SHA-256. | General large-file/empty-file/concurrent-read completeness or a stable server version. |
| SDD `raw-history-readonly-probe.json` | Explicit `only_tag=false` sample returned present integer sizes 81/45 and version strings. | Current-head link, ordering as head, or size of a separate latest-endpoint response. |
| SDD `version-download-readonly-probe.json` | Existing V1/V2/V1 downloads had sizes 45/81/45, distinct old/new SHA and repeat-stable old SHA. | General immutable-version behavior after rename/revert/deletion/retention, or which version is global head. |
| `backend/feishu/feishu.go:357–367,491–502,625–649` | Existing ordinary objects start at size 0; every Open downloads latest anew, updates size afterwards, then applies local ranges. | Exact first attributes or consistent bytes across multiple Open/Range calls. This code cannot be reused unchanged for snapshot-bound reads. |
| `vfs/file.go:432–453`; SDD VFS preflight section 5 | File.Size cannot fail and clamps unknown object size to zero. Error-returning target materialization must precede attributes/Open. | Setting Size=-1 alone cannot prevent premature EOF. |

Transport chain is source-backed: `shortcuts/common/runner.go:552–556` routes DoAPIStream to APIClient.DoStream; `internal/client/client.go:215–238` creates an authenticated net/http request and calls httpClient.Do; its Body goes to io.Copy in AtomicWriteFromReader. The inspected local Go source `/opt/homebrew/opt/go/libexec/src/net/http/transfer.go:862–865` returns io.ErrUnexpectedEOF when a declared Content-Length body ends early. Thus a detectable short/framing-failed body propagates as a copy/save error, not successful adoption. This source inspection is not a live failure-injection test or proof of the exact deployed CLI build/runtime. For this route, buffered DoAPI/SDK response parsing is not the download path and should not be confused with it. A close-delimited body without an independent expected length can legitimately terminate at EOF; no generic client can distinguish that from a semantically short service response.

The latest-endpoint evidence predates these new read-only probes. The new version probes must not be represented as new latest-head/full-download evidence.

## Minimal contract and ownership changes

### T02 transport/cloud

Retain strict version metadata and explicit historical download APIs. Add a cohesive helper such as `ObserveFile(ctx, token) (FileObservation, error)` (names illustrative), whose output owns a completed private temporary file/cleanup lease, token, typed observation ID, actual byte count, SHA-256, observation start/finish, and provenance. The helper does **not** create a store baseline or return a temp pathname as durable storage.

A small tagged identity should distinguish:

- `server_version`: a nonempty genuine server version, with separately recorded selection/head-link evidence. A known historical version does not get an `IsCurrent=true` merely because downloading succeeded.
- `observed_download`: nonempty local observation ID and content SHA-256; **no server-version field is present**. Record the exact route/evidence and observation time. SHA may repeat when content repeats; observation ID distinguishes observations and does not imply remote chronology.

Prefer a validated discriminated DTO (one allowed variant) over ambiguous `Version string` with an empty value or a hash stuffed into Revision. Local content size/hash belongs to both routes and describes the actual stored bytes. Unknown metadata, failed acquisition, and a validated empty observation must be distinct states. `ModTimeSource` can mark persisted first-observed time when remote mtime is unavailable; do not recreate `time.Now()` on each call.

ObserveFile runs a **full** request in a fresh private directory, requires successful process/envelope/expected identity and present valid nonnegative integer `size_bytes`, validates `saved_path` containment and regular-file/no-symlink ownership, reads/counts/hashes to EOF, and requires reported/stat/read counts to agree. Ignore any output file from unsuccessful/canceled commands. No range requests, no fallback from a requested historical V to latest, no raw maps above the adapter. Guard arithmetic/limits and stream large files instead of buffering. Reuse Runner identity/credential locks, including reads.

Treat this as acquisition of bytes, not cheap metadata. A caller needing exact size must retain the same acquisition for subsequent reads, not discard it and repeat +download in Open. Cache identities include identity domain/profile + token + snapshot identity; never use token alone for content reuse. Concurrent requests may be coalesced, but each successful publication names a specific immutable result.

### T03 durable model/store seam (not a T01 silent change)

`model.Snapshot` currently has only `Revision`, XML/Markdown/BlockMap refs, Size and ModTime (`internal/bridge/model/types.go:130` vicinity). It has no honest ordinary-byte observation identity. Record an explicit, owner-reviewed additive ordinary-file payload/reference plus the tagged identity/provenance. Preserve Docx revision rules. Do not put binary contents in a misleading XML field or use the observation SHA as Revision. The exact field names belong in the shared contract update before T02/T03 consume them; no T01 files were edited here.

Add/define one store adoption operation: acquire outside locks → stream into store blob and verify count/hash → sync blob, rename, sync directory → short transaction binds observation, blob, exact size, and expected entry/token/generation. Only then return a materialized object. Acquisition cancellation, identity mismatch, generation/freeze mismatch or durability failure publishes nothing. Unreferenced prepared blobs follow existing GC grace; accepted observations/checkpoints/recovery refs and in-use read leases cannot be collected.

Adoption must distinguish initial clean materialization from remote refresh and from job confirmation. First materialization initializes the visible/local baseline of that clean entry; it must never overwrite pending local edits or advance a newer generation. Refresh and job confirmation retain the existing OfferRemote/Confirm/publication rules and dual checkpoints. Persist the observation referenced by backup/job before any destructive remote call. Restart Open uses the adopted blob; it does not redownload just because no server version exists.

### T05/T06 materialization and native scope

Extend the existing `ManagedMaterializer.MaterializeManaged(ctx, remote) (fs.Object, error)` behavior to **unmaterialized ordinary files as well as document projections**. No signature expansion or bridge imports into VFS are necessary. Its returned ordinary object has exact adopted-blob size; Open and all Range/Seek calls read that blob under a lifetime lease. Remote refresh cannot replace bytes under an existing reader. Target Stat/Lookup/Getattr/Open routes use the error-returning helper before attributes/cache sizing; read-only directory enumeration does not trigger full downloads. The existing cmount readdir-plus issue in VFS preflight still needs its explicit adapter treatment.

This is a managed-overlay solution, not an automatic repair of all native rclone metadata paths. Native NewObject can reuse target full acquisition and an object-owned snapshot for exact requested-object size/Open, with synchronous native upload semantics retained. Native metadata-only List cannot supply unknown remote sizes accurately without a trustworthy metadata source or downloading each child; leave that limitation explicit, never fake zero or silently make listing eager. Native mounts need their own applicable target materialization/error handling before accurate attrs can be claimed; the managed-only helper cannot be assumed to fix them.

## Backup, verification and remaining races

For same-token overwrite, preserve the accepted local base and a fresh successfully obtained remote observation durably before upload. If fresh acquisition fails, retain local work and block that mutation rather than calling an old snapshot “current backup”. The backup is the remote content actually observed, not a guarantee to include another writer's later changes. For delete, similarly preserve the obtainable remote bytes and journal the operation before deleting; never infer deletion from incomplete listing.

After upload, obtain another full observation and compare actual SHA-256/size to the captured job input, then adopt that actual readback. Equality supports “desired content was observed after upload” for this job, not proof this request caused the state, exactly-once execution, or a guarantee that the remote remains equal. On timeout/ambiguous result retain awaiting_verification; nonmatching/missing observations must not cause blind overwrite/retry or rollback. Ordinary files remain opaque whole-file replacements with no new content merge algorithm.

A refreshed observation with different hash detects content drift. Same hash cannot detect ABA edits, rename-only changes, or edits that were overwritten between polls. Record namespace metadata separately; don't infer version equality from content equality. The existing statement “remote comparisons use explicit revision” must be narrowed to version-bearing routes/Docx; observation routes compare observed content and preserve that limitation.

Residual races include remote change during fetch, after fetch but before local adoption, after backup but before overwrite/delete, and after verification but before publication. Local generation checks/publication gates protect local state only. No recheck loop or credential lock turns this into server CAS. Local stable bytes and size remain exact even when the observation is stale; do not describe staleness as metadata corruption.

## Capability and test gates

Separate capabilities: (1) candidate history parsing; (2) known-version selection and completeness; (3) current-version linkage; (4) successful full latest-endpoint observation; (5) durable local adoption/read binding; (6) overwrite/delete and post-write verification. Failure of (3) must not disable a validated (4)+(5). Existing samples support limited (2)/(4), not a blanket verified current-version capability or all-file-size coverage.

**Completeness distinction must be explicit in the contract revision.** A full request, successful authenticated envelope, clean process exit and completed body/file read establish a transport-completed observation. Count+SHA then establish exact identity/size of those received bytes; they do not independently prove that the service returned its entire intended file. The prior preflight overstates the blocker if ignoring SaveOptions.ContentLength is taken to mean declared-length HTTP truncations are undetectable: net/http framing still detects those errors, and io.Copy propagates them. Keep missing expected server checksum/length as an integrity-evidence limitation, not a reason to disable observation-based ordinary reads. An unexpected semantically partial but otherwise successful service response, or coherent remote snapshot semantics during a concurrent mutation, remains a remote-release integrity test/limitation. Never generate partial-range observations locally. The API does not expose response status/headers in this shortcut output, so independent 200-versus-206 checking is unavailable at the adapter boundary; record this honestly rather than claiming that check is implemented.

The contract revision should supersede the earlier requirement for independent server-completeness/head proof **only for this explicitly observation-typed route**, in accordance with the clarified local guarantee. Known-version/current metadata claims still require their stronger evidence. No dependency patch or new transport is necessary for the local observation contract.

Before enabling the route, required offline tests include:

1. Tagged DTO validation: absence versus actual zero; wrong/null/fractional/negative/overflow counts; reject ambiguous identity variants; history never becomes head; observation ID/hash never goes to `--version` or revision parameters.
2. Controlled real subprocess tests: valid empty/full output; nonzero/malformed/identity mismatch/cancel after writing file; path escape/symlink; copied/stat/reported-count mismatch; known independent hash mismatch; cleanup and no adoption on failure. Fixture success proves adapter decisions, not server semantics.
3. Store crash/failure boundaries: blob sync/rename/directory sync and transaction failure; restart retrieves adopted bytes; generation drift/freeze prevents replacement; pending/recovery/read refs survive GC.
4. Target Stat then multiple Open/Range calls while fake remote changes: all bound to adopted bytes/size until safe refresh; materialization failure propagates instead of zero; repeated list does no content calls; native-specific behavior stays explicit.
5. Overwrite/delete journal ordering: durable observed backup before mutation; known local generation remains isolated; lost response→readback-match may confirm only captured job; mismatch remains unresolved; no automatic destructive retry or rollback.

Remaining release integrity validation (not a substitute for local correctness tests, and not a universal currentVersion gate) should cover known empty/nonempty/larger contents through latest full-download, expected independent SHA/length, overwrite and rename, cancellation/truncation transport behavior where testable, and concurrent changes without claiming global atomicity. Version-retention/revert/deleted-version gates remain exclusively on the stronger known-version route. No such experiments were executed for this report.

## Bounded decision

Proceed with the observation design for the approved best-effort product, after updating only the relevant shared DTO/materialization/metadata and evidence wording. It removes an unnecessary dependency on unverifiable current-version metadata while preserving exact local bytes and durable recovery. Do not mark current head or independent upstream completeness as proven. The design/protocol wording must be reconciled before dispatch so implementers do not apply the stronger server-version gate to this route. The implementation must never hide that distinction in an empty Version field.
