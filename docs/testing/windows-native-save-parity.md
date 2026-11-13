# Windows native save gate

The `windows-save-tests` job in `drivebridge-windows-package.yml` runs on
`windows-2022`. Dispatch that existing workflow against the implementation
branch with `tests_only=true` to run the gates without producing installers:

```sh
gh workflow run drivebridge-windows-package.yml \
  --ref fix/windows-save-parity -f version=0.2.10 -f tests_only=true
```

The job installs WinFsp 2.1.25156 after checking its pinned MSI SHA-256 and
prepares the private Windows Git runtime using `git_runtime.py` and the
repository lock file. It runs the Windows Store, Overlay, Sync, History,
Integration, VFS and cmount suites with `CGO_ENABLED=0` and `-tags cmount`.
History tests use that private Git executable. A separate Go overlay run must
fail the process-attribute regression against the pre-fix Git command sources;
an assertion failure is required, and a compilation failure does not qualify.

The native step requires all three `TestWindowsNative*` tests to pass. It
enables `DRIVEBRIDGE_NATIVE=1`, `DRIVEBRIDGE_REQUIRE_NATIVE=1` and
`DRIVEBRIDGE_NATIVE_ALLOW_MOUNT=1`. Missing drivers, failed mounts and skipped
required tests fail the gate. Mounts use automatically assigned free drive
letters and a synthetic provider with private temporary state; they never load
account credentials or write production cloud data.

- `SaveParity`: 100 direct saves and 100 `MoveFileExW` replacements while
  background Store archival is paused; old reader content after explicit POSIX replacement;
  restart/replay of an accepted, unarchived checkpoint; preserved document
  identity and latest bytes.
- `DeleteParity`: ordinary `DeleteFileW` and empty-directory
  `RemoveDirectoryW`, with managed backend observations showing that the
  driver's reader remains open at deletion; shared old reader content after
  unlink; absent deleted paths and fresh content after recreation; deletion
  denied while an application writer is open despite `FILE_SHARE_DELETE`.
- `RemoteGeneration`: exact native read bytes and file sizes after same-size,
  growing and shrinking synthetic remote updates, with a 60-second native
  attribute timeout to exercise invalidation.

The artifact `drivebridge-windows-save-<run-id>-<attempt>` retains installation
logs, the WinFsp/Git/runner versions, Git runtime inventory, RED and GREEN Go
JSON event logs and three native evidence JSON reports. A runtime result is
established only by a completed job and its reports. Cross-compilation on
macOS is a separate compilation check. These gates exercise headless Windows
OS APIs; they do not establish GUI editor acceptance, physical disk failure
behavior or power-loss durability. The paused dependency is background Store
archival, while the local checkpoint journal and physical disk remain usable.

## Local regression evidence (2026-11-13)

The shared Go module suites, affected race suites, 20 Node UI tests, and 50
packaging tests passed on macOS. The full Windows amd64 `cmount,desktop`
configuration compiled with `CGO_ENABLED=0`. Three synthetic native macOS
mount/save/merge gates passed; the installed application and user cloud data
were not changed by these tests.

The final review reproduced a reader-construction race: an admitted read that
had not pinned its cache handle could resurrect a pathname after unlink. A
regression now holds that construction boundary, requires delete preflight
and unlink to return busy until the handle is ready, then verifies that the
completed old reader survives deletion without republishing the name. All six
delete regressions and the full VFS/integration race suites passed afterward.

The upstream unmanaged `cmd/cmount.TestMount` suite returned EPERM in a macOS
temporary mount directory. The same failure was reproduced against the
pre-change source via a Go overlay; it is an existing local environment
limitation. The remaining cmount adapter tests passed. This suite is not
reported as passing or removed from the Windows gate.

## Windows rename semantics

The initial native run passed all 200 foreground saves and both deletion/cache
refresh gates, but legacy `MoveFileExW` rejected replacement while an explicit
destination reader remained open. The save gate now includes the same NTFS
control and exercises `FileRenameInfoEx` with `REPLACE_IF_EXISTS` and
`POSIX_SEMANTICS` for replacement with surviving old readers. Ordinary
`MoveFileExW` remains the API used by the 100 normal safe-save iterations.
This distinction follows the [Microsoft rename contract](https://learn.microsoft.com/en-us/windows-hardware/drivers/ddi/ntifs/ns-ntifs-_file_rename_information);
the application cannot grant POSIX semantics to a third-party editor that uses
only the legacy API. GUI editor acceptance remains a separate test.
