# DriveBridge

Unofficial Feishu/Lark client. User data, credentials, pending saves and conflicts are stored separately from this application directory. Uninstall preserves them.

Run `bin/drivebridge feishu --help` (`.exe` on Windows). The official pinned CLI is `libexec/lark/lark-cli`; its Windows executable ends in `.exe`. Windows also includes Node.js and the official package launchers. No account is bundled. Configure and authenticate as the current desktop user using the managed CLI.

Read `manifest.json` before use. `unsigned_test` packages are test artifacts, not releases. `mount:false` is CLI-only: it cannot provide the promised writable mount. Install the platform's supported macFUSE / WinFsp / FUSE3 driver separately; the package never installs drivers or elevates a worker.

Use the candidate bundle's `upgrade.sh /absolute/candidate/bin/drivebridge -- /absolute/synchronous-installer [args...]`, or PowerShell 7+ `upgrade.ps1 -Candidate C:\candidate\bin\drivebridge.exe -Installer C:\Windows\System32\msiexec.exe -InstallerArguments @('/i','C:\candidate\drivebridge.msi','/qn')`, as the current desktop user. The wrapper copies the candidate helper outside the installation and holds the global same-user installation lease until the child exits. Do not use `open`, detached launchers, or overwrite a running portable directory. Pending accepted saves/conflicts are retained; open handles and unaccepted/corrupt cache block replacement. No worker is killed and no cloud upload is requested by the installer. Existing worker synchronization may complete while handles close.

These installer templates still require native lease propagation and signing validation before release. A missing proof or changed OS-user identity fails closed, including fresh install. Linux system package install/upgrade/removal is blocked pending multi-user coordination; use a separate per-user portable layout. Inno uninstall is blocked until its trusted verifier path is validated. Uninstall never removes configuration, credentials, cache or pending saves.

Startup is opt-in: `bin/drivebridge feishu mount enable-autostart NAME`; remove with `disable-autostart NAME`. The manager registers a launchd user agent, systemd user unit, or interactive user task. Do not register the worker as root or SYSTEM. Stop mounts and disable their startup entries before uninstall. Installer packages never create a second startup entry.

Licenses and project notices are in `licenses/`.

If `manifest.json` contains `desktop:true`, launch `DriveBridge.app` on macOS, `bin/drivebridge-desktop.exe` or the desktop shortcut on Windows, or `sh launch-desktop.sh` on Linux. Keep the full bundle together; the macOS `.app` shares its sibling CLI and runtime and cannot be moved alone. Linux portable users can run `sh install-desktop.sh` to add a current-user application launcher. It does not enable login startup.

The macOS desktop DMG instead provides **安装 DriveBridge.app**, with visible installation-location, login-startup and open-after-install options. The installed `/Applications/DriveBridge.app` (or `~/Applications/DriveBridge.app`) contains this complete bundle under `Contents/Resources/payload`; open the outer app directly. Login startup is selected by default in that graphical installer and remains controllable from the tray. The raw pkg is reserved for the coordinated wrapper described above; do not use it as the normal graphical installation entry.

Desktop controls: `bin/drivebridge desktop start|status|stop|enable-autostart|disable-autostart` (use `.exe` on Windows). Exiting the tray or `desktop stop` keeps sync workers running; stopping all sync is a separate action. Login startup belongs to the current user: macOS LaunchAgent, limited Windows interactive logon task, or Linux XDG graphical session autostart. Disable it before moving or removing this directory. Portable bundles require opt-in; the graphical macOS installer enables it when its default-selected option is kept. Build scripts never enable it.

Desktop native tray, login startup and upgrade exit require platform evidence in addition to the existing mount and lifecycle gates. A desktop binary in an archive is not proof that those native checks passed.

## Application-owned login and directory discovery

On macOS 14 or later, the complete bundle includes `libexec/DriveBridge Session.app`, an application-owned WKWebView login helper with bundle ID `org.drivebridge.sessionhost`. Use **账号与目录 → 账号盘与自动目录发现** to finish the official CLI authorization. The app automatically opens its native login window, verifies the signed-in identity and mounts the account drive. Check its displayed HTTPS address. There is no required completion click; **打开应用登录窗口** remains available when another login is needed. Personal Chrome/Edge/Safari sessions and browser extensions are not required, read or imported.

The login window is for authentication and directory discovery. The helper runs as an accessory app and does not create a separate Dock tile or application-switcher entry. Closing it hides the window; background directory requests can continue while the desktop runs. Reopen it from the management page when needed. Website data remains outside the installation, isolated per OS user and product account. Cookie values are not exported to configuration. Session expiry or SSO/MFA can require logging in again. The cloud-drive page body may render only partly; this helper does not promise full website browsing compatibility. File reads, synchronization and writes continue through the official CLI.

Use **重新扫描** in the management page or tray to refresh later-added folders, or search a folder name (up to 30 characters) and select its verified result. Folder links/tokens can also be verified and saved. Missing or failed list entries never imply cloud deletion. One token has one writable path, with its observed memberships retained. The desktop checks eligible catalogs every five minutes without starting stopped mounts.

Personal, visible shared and hidden shared entrance pagination does not prove all descendants or every link-accessible directory have been inventoried. Current ownership is checked each scan; a personal resource page may provide evidence when the folder list is empty. No current personal evidence means new shared entries remain blocked and old entries are retained.

Windows/Linux packages preserve existing CLI and sync functions, but application-owned web login is currently unavailable there. No nonexistent helper or browser extension is required. The macOS compatibility prototype passed read-only login/restart/isolation checks; formal installed-app login, account-disk integration, signing and other-platform validation require separate evidence. An unsigned development package is not a release.
