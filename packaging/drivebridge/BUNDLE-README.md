# DriveBridge

Unofficial Feishu/Lark client. User data, credentials, pending saves and conflicts are stored separately from this application directory. Uninstall preserves them.

Run `bin/drivebridge feishu --help` (`.exe` on Windows). The official pinned CLI is `libexec/lark/lark-cli`; its Windows executable ends in `.exe`. Windows also includes Node.js and the official package launchers. No account is bundled. Configure and authenticate as the current desktop user using the managed CLI.

Read `manifest.json` before use. `unsigned_test` packages are test artifacts, not releases. `mount:false` is CLI-only: it cannot provide the promised writable mount. Install the platform's supported macFUSE / WinFsp / FUSE3 driver separately; the package never installs drivers or elevates a worker.

Use the candidate bundle's `upgrade.sh /absolute/candidate/bin/drivebridge -- /absolute/synchronous-installer [args...]`, or PowerShell 7+ `upgrade.ps1 -Candidate C:\candidate\bin\drivebridge.exe -Installer C:\Windows\System32\msiexec.exe -InstallerArguments @('/i','C:\candidate\drivebridge.msi','/qn')`, as the current desktop user. The wrapper copies the candidate helper outside the installation and holds the global same-user installation lease until the child exits. Do not use `open`, detached launchers, or overwrite a running portable directory. Pending accepted saves/conflicts are retained; open handles and unaccepted/corrupt cache block replacement. No worker is killed and no cloud upload is requested by the installer. Existing worker synchronization may complete while handles close.

These installer templates still require native lease propagation and signing validation before release. A missing proof or changed OS-user identity fails closed, including fresh install. Linux system package install/upgrade/removal is blocked pending multi-user coordination; use a separate per-user portable layout. Inno uninstall is blocked until its trusted verifier path is validated. Uninstall never removes configuration, credentials, cache or pending saves.

Startup is opt-in: `bin/drivebridge feishu mount enable-autostart NAME`; remove with `disable-autostart NAME`. The manager registers a launchd user agent, systemd user unit, or interactive user task. Do not register the worker as root or SYSTEM. Stop mounts and disable their startup entries before uninstall. Installer packages never create a second startup entry.

Licenses and project notices are in `licenses/`.

If `manifest.json` contains `desktop:true`, launch `DriveBridge.app` on macOS, `bin/drivebridge-desktop.exe` or the desktop shortcut on Windows, or `sh launch-desktop.sh` on Linux. Keep the full bundle together; the macOS `.app` shares its sibling CLI and runtime and cannot be moved alone. Linux portable users can run `sh install-desktop.sh` to add a current-user application launcher. It does not enable login startup.

Desktop controls: `bin/drivebridge desktop start|status|stop|enable-autostart|disable-autostart` (use `.exe` on Windows). Exiting the tray or `desktop stop` keeps sync workers running; stopping all sync is a separate action. Login startup is opt-in and belongs to the current user: macOS LaunchAgent, limited Windows interactive logon task, or Linux XDG graphical session autostart. Disable it before moving or removing this directory. Build and installer scripts never enable it.

Desktop native tray, login startup and upgrade exit require platform evidence in addition to the existing mount and lifecycle gates. A desktop binary in an archive is not proof that those native checks passed.
