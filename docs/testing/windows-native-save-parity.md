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
  background Store archival is paused; old reader content after replacement;
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
