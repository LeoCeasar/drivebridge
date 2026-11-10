# T11 migration/recovery core handoff

Status: **T11 core implemented; full migration lifecycle remains unwired pending T09/auth/installer composition.**

Worktree `/Users/rrong/.codex/worktrees/7b0a/drivebridge`, branch `feature/feishu-sync-migrate`.

Scoped commits, in dependency order:

1. `87d4269d6` — Store.Validate reference/integrity auditor, new validate.go and validate_test.go only.
2. `2d7e954b4` — Store.ValidateDirectory read-only offline audit, same two files only.
3. `6f7fc8bf222ac000d91dbb7dbe3e3c40d4308a67` — migration/recovery core, six files under internal/bridge/migrate only.

Controller explicitly authorized the two store-owned auditor files; existing Store implementations, canonical models, manager/auth, legacy scripts and module files were not modified. No account, CLI, installation, startup or system action was executed. Working tree clean. This ignored handoff report is outside the commits.

## Concrete behavior

- Explicit legacy preview and approved apply. ReadLegacy parses macOS's generated literal shell assignments as data without sourcing, expansion or command execution, and Windows JSON settings without executing stored commands. Standalone shell syntax and unquoted expansion are rejected. Executable/RC/cache settings are ignored, not copied into managed configuration.
- ReadRcloneRemote reads only the selected INI section and whitelisted type/root resource token. Other options (including command and credentials) are ignored. Existing rclone config is never modified. Legacy non-root RemotePath requires an explicitly resolved folder resource token; paths are not invented tokens.
- PreviewImport selects an already verified managed profile (AuthRef+IdentityHash required), creates new UUID remote/mount namespaces, checks config/path overlap, fixes managed defaults and leaves Autostart disabled. No legacy account/credential or queue adoption occurs. Preview is read-only and carries before-config SHA and a digest of its reviewed proposed configuration.
- ApplyImport requires that exact digest, rejects modified preview/current-config CAS, enters lifecycle quiescence, validates all current scope/reference bindings, publishes a complete backup first, then uses config.SaveWithPaths atomic save. ConfigChanged and NeedsStartupHandoff are explicit, including a visible replacement followed by config directory-sync error. It never executes imported commands or enables both managers.
- No invented schema transition exists. Known current config/store/manifest schema is 1. PrepareUpgrade means validate and publish a complete prerequisite backup; future schema is refused without rewriting it. Actual future schema transformations need a separately versioned implementation.
- Complete backup contains exact config.json, consistent Store.Backup bbolt images, and the entire corresponding immutable blob trees. It includes original Before and copied Files SHA-256/size/path inventories plus bound Scope metadata and a manifest digest. Files/directories are private, file and directory entries synced, and the destination is staged and atomically published only after complete verification. Existing destinations/source-store/config overlaps are rejected. Source credentials and VFS caches are not part of this bundle.
- VerifyBackup is read-only, requires an absolute path, checks schema/digest, canonical relative inventory, exact file set/byte hashes, symlink/nonregular rejection, config references and every store's exact identity binding and semantic references. Store.ValidateDirectory opens bbolt read-only and never runs recovery/rebind/unknown transitions.
- Restore requires an exact expected Scope map and publishes only a verified new unused destination bundle. It does not overwrite an existing namespace, rebind old queues to a new identity, update active configuration, activate mounts, or remove the original backup. Activation/swap/reopen is a future manager composition step.
- Recovery returns existing typed Store jobs/conflicts/namespace/create/publication state. ExportRecoveryBlob uses Store's active read lease. GC is a guarded facade around Store.Validate and Store.GC, with no duplicated retention implementation. Pending/unknown/conflict data remains referenced and survives cleanup. No cloud retry or mutation is performed.
- Uninstall is a quiesced preserve-data decision/handoff, not an installer remover. It never removes config, databases, blobs, cache or credentials and requires startup/installation handoff. Explicit destructive purge is not provided by this core.

## Exact APIs

```go
type Quiescer interface {
    WithQuiescence(context.Context, func(context.Context) error) error
}
type Mount struct { Store *store.Store; Dir string }
type Options struct { Paths config.Paths; Guard Quiescer; Mounts []Mount }
func New(Options) (*Service, error)

func (s *Store) Validate(context.Context) error // package store
func store.ValidateDirectory(context.Context, string, store.Scope) error
// Store.ErrIntegrity wraps validation causes.

type LegacyFormat string
const LegacyMacOS LegacyFormat = "macos"
const LegacyWindows LegacyFormat = "windows"
type Legacy struct {
    Backend, Remote, RemotePath, Mountpoint string
    Autostart bool
    Ignored []string
}
type LegacyRemote struct { Name, Type, FolderToken string; Ignored []string }
func ReadLegacy(io.Reader, LegacyFormat) (Legacy, error)
func ReadRcloneRemote(io.Reader, string) (LegacyRemote, error)

type ImportOptions struct {
    ProfileID model.ProfileID
    FolderToken, Mountpoint, Name string
}
type ImportPlan struct {
    BeforeSHA256 string
    After model.Config
    LegacyRemote, LegacyPath string
    Warnings []string
    Digest string
}
type ApplyResult struct {
    Backup Manifest
    ConfigChanged, NeedsStartupHandoff bool
}
func (*Service) PreviewImport(context.Context, Legacy, LegacyRemote, ImportOptions) (ImportPlan, error)
func (*Service) ApplyImport(context.Context, ImportPlan, string, string) (ApplyResult, error)
// Final strings: approved preview digest, new backup destination.

type InventoryFile struct { Path string; Size int64; SHA256 string }
type MountBackup struct { Scope store.Scope; Directory string }
type Manifest struct {
    SchemaVersion int
    CreatedAt time.Time
    ConfigSchema int
    Mounts []MountBackup
    Before, Files []InventoryFile
    Digest string
}
func (*Service) PrepareUpgrade(context.Context, string) (Manifest, error)
func (*Service) VerifyBackup(context.Context, string) (Manifest, error)
func (*Service) Restore(context.Context, string, string, map[model.MountID]store.Scope) (Manifest, error)
// Restore strings: existing backup, new unused destination bundle.

type RecoveryView struct {
    Scope store.Scope
    Jobs []model.Job
    Conflicts []model.Conflict
    Namespace []model.NamespaceOp
    Creates []model.CreateOp
    Publications []model.PublishCandidate
}
func (*Service) Recovery(context.Context, model.MountID) (RecoveryView, error)
func (*Service) GC(context.Context, model.MountID, store.GCOptions) (store.GCResult, error)
func (*Service) ExportRecoveryBlob(context.Context, model.MountID, model.BlobID, io.Writer) error

type UninstallResult struct {
    PreserveData, NeedsStartupHandoff bool
    ConfigFile, StateDir, CacheDir string
}
func (*Service) Uninstall(context.Context) (UninstallResult, error)
```

Errors: ErrUnknownSchema, ErrNotQuiescent, ErrPreviewChanged; config.ErrUnknownSchema and model.ErrIdentityChanged propagate where applicable. Manifest JSON has explicit lowercase schema/path/hash fields. Manifest digest is integrity inventory metadata, not a signature or independently verified identity credential.

## Guard and inventory gates

Guard is mandatory; there is no built-in no-op/default production guard. It must synchronously execute the supplied callback exactly once, hold exclusive lifecycle admission for the entire callback, reject configuration changes, flush accepted VFS cache state, stop worker dispatch, wait for active handles/mutations or fail timeout, and release only afterward. Package checks callback execution and preserves operation errors even if a broken guard discards them. A guard that calls fn without real exclusion still violates its contract; only manager/OS lifecycle integration can establish that exclusion.

Mount sources must match config namespace roots and bound Profile/Mount/Remote/identity/root folder. Missing initialized configured stores are rejected rather than silently omitted. No auth registry/credential backup is claimed. Before contains source-image hashes; Files describes the consistent copied database image and blob bytes. Verification audits every Store-owned reference, including entries/checkpoints, immutable snapshots/FileData, job local/base/plan/desired/operation results, conflict/draft, namespace preimages/sealed references, create input, recovery and publications. Referenced blobs are streamed through SHA-256, and typed file size/identity checked. The auditor does not requeue, retry, classify or confirm anything.

## Actual verification

- Initial migration compile failed on missing public APIs.
- Runtime TestLegacyPreviewRed failed because the parser scaffold returned no selected remote; implementing literal parsing made it pass. Temporary scaffold/test removed.
- Auditor runtime red: missing blob, missing snapshot and tampered-byte tests initially returned nil; after real reference/hash auditing all pass. Unknown journal state is unchanged.
- Runtime TestGuardCannotSilentlySkipExclusiveOperation initially returned success from a guard that never invoked its callback; the enforced callback gate now returns ErrNotQuiescent.
- Focused auditor command: `go test ./internal/bridge/store -run '^TestValidate' -count=1` -> pass (final auditor run 1.320s), five tests. Offline test proves merging state remains merging rather than silently rewritten by validation.
- Final migration command: `go test ./internal/bridge/migrate -count=1 -timeout=30s` -> `ok github.com/rclone/rclone/internal/bridge/migrate 2.799s`, exit 0.
- Required one migration race run: `go test -race ./internal/bridge/migrate -count=1 -timeout=60s` -> `ok github.com/rclone/rclone/internal/bridge/migrate 3.865s`, exit 0, no race reports.
- 13 migration tests use real private TempDir/config/bbolt/blobs plus a fixture guard using a real flock; no live worker/account is involved. Pending, awaiting_verification and conflict bytes survive complete backup/new-namespace restore. Tests cover future config/manifest/database schema refusal, unchanged future DB bytes under read-only audit, malicious literal commands with a writable temporary canary, rejected standalone shell commands, explicit preview digest and stale-config CAS, identity mismatch, preservation on uninstall, skipped/denied guard, tampered/symlink bundle, inventory verification, canceled backup, recovery export and Store GC retention.
- `git diff --cached --check` passed before scoped commit. No external review agent, full-project test or actual installer/manager/system operation was run. Parent performs block integration.

## Remaining wiring / not a full T11 completion claim

T09 must compose a real quiescence guard and identity verification service, flush/stop workers before backup, reconcile namespace recovery before mount activation, close/reopen DB handles for installation replacement, and explicitly activate a verified restored bundle. Restored config preserves its original settings; manager must resolve any relocated state/cache paths before activation. Uninstall result reports Options.Paths roots; manager should present effective Config.StateDir/CacheDir overrides when showing preserved data locations.

Legacy forwarding changes remain intentionally unedited per controller instruction:

- `packaging/macos/app/drivebridge-manager.sh`: currently sources drivebridge.settings, runs mount directly and registers com.drivebridge.mount.plist; replace action routing with managed entrypoint, expose explicit import preview/apply, disable old launch agent only through reviewed handoff, never source settings during import or delete cache/queue.
- `packaging/windows/app/rclone-feishu-manager.ps1`: route existing Menu/Mount/Startup/Status/Uninstall actions to the managed service, import rclone-feishu.settings.json as data, remove the old startup entry after successful handoff, preserve user state/auth by default. `.cmd` launchers should forward to that entrypoint and retain exit codes.
- Existing legacy uninstall currently removes package directories; ensure installation-only cleanup cannot encompass config/state/cache roots. Do not enable both old and new startup managers.

Actual live stop/flush timeouts, Windows/macOS script forwarding, multi-process startup handoff, restore activation/reopen, credentials revalidation, real power-loss/native ENOSPC and three-platform installer upgrade/uninstall remain explicit integration gates. No automatic destructive purge or server-side history guarantee was added.
