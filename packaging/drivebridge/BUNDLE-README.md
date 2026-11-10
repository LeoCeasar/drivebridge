# DriveBridge

Unofficial Feishu/Lark client. User data, credentials, pending saves and conflicts are stored separately from this application directory. Uninstall preserves them.

Run `bin/drivebridge feishu --help` (`.exe` on Windows). The official pinned CLI is `libexec/lark/lark-cli`; its Windows executable ends in `.exe`. Windows also includes Node.js and the official package launchers. No account is bundled. Configure and authenticate as the current desktop user using the managed CLI.

Read `manifest.json` before use. `unsigned_test` packages are test artifacts, not releases. `mount:false` is CLI-only: it cannot provide the promised writable mount. Install the platform's supported macFUSE / WinFsp / FUSE3 driver separately; the package never installs drivers or elevates a worker.

Before upgrade, as each user, run `bin/drivebridge feishu mount stop-all --wait-upload 60s`. A failure requires resolving outstanding work before replacement. Back up the user configuration and state using the migration/manager workflow. Installers refuse replacement while a DriveBridge process is still running. They do not force-kill workers or clear caches. Do not overwrite a running portable bundle.

Startup is opt-in: `bin/drivebridge feishu mount enable-autostart NAME`; remove with `disable-autostart NAME`. The manager registers a launchd user agent, systemd user unit, or interactive user task. Do not register the worker as root or SYSTEM. Stop mounts and disable their startup entries before uninstall. Installer packages never create a second startup entry.

Licenses and project notices are in `licenses/`.
