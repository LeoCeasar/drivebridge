# T12 installation lease followup

Scoped commit **7556a39ae** on `feature/feishu-install-lease`, isolated `/4351/drivebridge`. Parent should cherry-pick this commit only after T09 dependencies **d5f66bc66** and **0080b0025** (local reference copies 22aed4f6a and 60e26b71d). The only existing command registration edit is the peer-authorized `installMaintenanceCommands(root, o)` line. Existing manager/worker/auth files remain peer-owned. Report is ignored and intentionally not committed.

## Implemented contract

`Manager.WithInstallationLease(ctx context.Context, fn func() error) error` takes the same-OS-user global auth lifecycle exclusive lease. It inventories validated historical ownership records across all config hashes, including removed mounts; includes current mounts; takes deterministic sorted config manager/config locks, runtime locks, then actual Store `worker.lock` ownership. It never uses Store.Open for validation because Open writes metadata; it invokes readonly Store.ValidateDirectory under actual writer exclusion. Missing/wrong roots, malformed/unknown ownership, lock-only records with no recoverable root reference, corrupt Store/cache, or unaccepted cache bytes block the callback. Valid accepted pending/awaiting-verification/conflict bytes remain unchanged. Locks release in reverse on error or callback completion. Callback must not reacquire manager/config/Store locks.

Locked live workers are located through strict authenticated historical `readEndpointAt`/`callEndpoint`, then requested through `/recovery/stop` to preserve pending state. Closed readers/writers/reservations and zero VFS pending are required. Runtime lock acquisition, not PID existence, proves stop. No PID kill or queue clearing. The lease itself requests no cloud/auth operation. Existing worker synchronization may continue while its local handles close; peer intentionally keeps that behavior so a refused stop does not leave a still-mounted worker with a permanently canceled engine. Before callback, worker engine is canceled/joined and all Store ownership is held.

Hidden CLI:

```
feishu maintenance exec -- /absolute/synchronous-installer arg1 arg2
feishu maintenance verify
```

Exec requires `--`, calls exec.Command with exact argv and no shell, and waits child completion even after caller context cancellation. A short-lived random bearer-authenticated loopback endpoint is created only inside the held callback; same-user verify checks bounded endpoint/proof identity and liveness without constructing a Manager or reacquiring ownership. Proof closes when child completes. This is coordination between cooperative product processes; it is not an anti-tampering boundary against the same user replacing binaries or forging a localhost endpoint. No editable success file or manifest grants permission.

`upgrade.sh /absolute/candidate/bin/drivebridge -- /absolute/synchronous-installer ...` and PowerShell 7+ `upgrade.ps1 -Candidate C:\candidate\bin\drivebridge.exe -Installer C:\Windows\System32\msiexec.exe -InstallerArguments @('/i','C:\candidate\drivebridge.msi','/qn')` copy the candidate helper outside the installed binary tree and retain it until exit. PowerShell uses ProcessStartInfo.ArgumentList and UseShellExecute=false; legacy PowerShell quoting is rejected. Detached launchers/GUI `open` are unsupported. No automatic elevation.

macOS pkg Scripts embeds the actual candidate verifier; MSI embeds it in BinaryRef with impersonation; Inno embeds a dontcopy/noencryption temp resource. None execute an arbitrary env-selected executable. All require live proof even for fresh installs, preventing existence-check bypasses. Missing environment propagation or changed OS-user identity refuses. Linux system package install/upgrade/removal explicitly refuses until multi-user coordination exists; Inno uninstall refuses until its separate trusted verifier lifetime is implemented. Per-user portable layout can use the synchronous wrapper. User data is never deleted.

Manifest `upgrade_guard:lifecycle_lease_unverified` distinguishes implemented source coordination from native validation. Release gate still rejects this value. Signing, native identity/environment propagation and actual installer lifetime must be proven before enabling release; builder source changes do not imply platform acceptance. README links the official WiX CustomAction and Inno temporary-resource documentation consulted.

## Verification

- Test-first manager run failed with undefined WithInstallationLease before implementation. CLI tests were added before implementation (initial compile dependency blocker was the pending historical IPC helpers). Packaging whitelist test failed before adding wrappers to staged allowed payload.
- Focused Go tests pass: five installation test groups plus maintenance argv/lifetime/proof/error cases; retained queued/unknown/conflict state and blobs, actual writer-lock ownership, historical removed mount, another live config, concurrent start/config write exclusion including a new config, raw DB byte equality for offline pending state, missing/wrong/dirty/corrupt state rejection, canceled acquisition, callback error cleanup, open handle refusal, forged/expired/wrong-user/remote-endpoint proof refusal, canceled-parent child completion.
- **One affected-package race run**: `GOFLAGS=-p=4 go test -race ./internal/bridge/manager ./cmd/feishu -count=1 -timeout=180s` passes, manager **4.462s**, CLI **3.722s**. Host linker emitted existing LC_DYSYMTAB warnings but exit was zero. No full-suite or external re-review.
- **22 Python stdlib packaging tests** pass, including embedded verifier source, release hard gate, exact literal shell-wrapper argv/copy/cleanup. bash/sh syntax passes. `git diff --check` passes. Host has no pwsh, so PowerShell AST/native execution remains unverified.
- Actual updated unsigned CLI-only macOS arm64 build succeeded with `GOFLAGS=-p=4`, version 0.1.0, existing checksum-verified public runtime inputs `/tmp/drivebridge-t12-runtime-arm64`, output `/tmp/drivebridge-install-lease-arm64-unsigned-cli-only`. No additional runtime download. Because pkg Scripts and staged wrapper changed, this is one necessary rebuild; prior T12 artifacts do not represent the new lease templates.
- `pkgutil --expand-full` only (no installation) produced `/tmp/drivebridge-install-lease-expanded-pkg`. Expanded payload passes package.py verify and final JSON schema; embedded Scripts verifier bytes exactly equal the staged application; guard invokes `feishu maintenance verify`. Manifest mount=false, unsigned_test, lifecycle_lease_unverified. `hdiutil verify` checksum VALID; image was not attached.

Actual artifact bytes / SHA256:

- pkg 66,134,662 — `ddd93d7d8bcbea69137423054a26378a2d6c3f545211a378d772b9563cda7a8e`
- dmg 66,714,999 — `ee57c3f50a86538a4a3e1695d4adf3f6cb7ec23270b9aed02ff4d3f3c47bec72`
- tar.gz 40,035,394 — `269fbbc3fb45862bd85f84996488ff60f696213391017b0fea1c49f3c6e25e1f`

No installer/system change, upgrade/uninstall, signing/notarization, real mount, account/auth operation, Windows/Linux native builder or push executed. Native release remains blocked as described above.
