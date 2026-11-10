# DriveBridge native packaging

These scripts create native packages; native installation and writable mount support remain **not verified** until the corresponding runner produces evidence. They do not install tools/drivers, authenticate accounts, modify system services, sign without explicit keys, or publish releases. Existing `packaging/macos` and `packaging/windows` are legacy launchers; these builders reuse their rclone/CLI/Node licensing requirements, not their account or global-npm discovery. The old macOS NFS fallback is deliberately absent: managed NFS is read-only.

| Native runner | Full build requirements | Outputs | Install location |
|---|---|---|---|
| macOS amd64 / arm64 | Go, Python 3.9+, Xcode SDK, macFUSE `fuse.h`, pkgbuild, productbuild, hdiutil, codesign | pkg, dmg containing pkg, tar.gz | current user's `~/Applications/DriveBridge` |
| Windows x64 | Go, PowerShell 7, Python 3.9+, WiX 4, Inno Setup 6, WinFsp, pinned Node ZIP | msi, installer exe, zip | `%LOCALAPPDATA%\Programs\DriveBridge` |
| Linux amd64 / arm64 | Go, Python 3.9+, dpkg-deb, rpmbuild, tar, FUSE3, `/dev/fuse` | deb, rpm, tar.gz | `/opt/drivebridge`; workers always run as each user |

Build each architecture on its own native runner. Cross-compilation does not pass the architecture gate. Full macOS builds require macFUSE headers; set `DRIVEBRIDGE_FUSE_INCLUDE` only if needed. A missing driver/tool is an error. `--cli-only --unsigned` (Windows `-CliOnly -Unsigned`) explicitly creates test artifacts with `mount:false` and blocks release; the complete installer toolchain is still required. Default builds require signing settings, and full builds verify the compiled `feishu` and platform mount command before staging. No fallback switches to NFS.

## Pinned public runtime inputs

`runtime-lock.json` pins CLI 1.0.99 to the [official npm metadata](https://registry.npmjs.org/@larksuite/cli/1.0.99), its SHA-512 tarball integrity, individual package file hashes, and all five native release asset SHA-256 values from that package's `checksums.txt`. The native archives come from [Lark's official release](https://github.com/larksuite/cli/releases/tag/v1.0.99). Windows Node 24.22.2 is a packaging input selected from the [official distribution index](https://nodejs.org/dist/index.json), pinned to the [official SHASUMS256 file](https://nodejs.org/dist/v24.22.2/SHASUMS256.txt) for both ZIP and executable. These pins do not establish Windows runtime compatibility; native validation is still required. Every native executable was also read with `go version -m` without execution: module `github.com/larksuite/cli v1.0.99`, VCS commit `8d8357d39dd15ac5e9aa815d4ab2639d56bd3239`, `vcs.modified=false`, and expected OS/architecture. This actual artifact source commit is distinct from any later inspected source-checkout HEAD. Runtime updates must update checksums and repeat native evidence.

`fetch-runtime.py` downloads only those public inputs, verifies hashes, and extracts named regular package files. It does not execute npm hooks, install globally, or inspect an existing account. Alternatively supply those exact files from an offline cache. Builders verify the package files and native archive again before copying. An arbitrary globally installed CLI or Node executable is insufficient.

```sh
python3 packaging/drivebridge/fetch-runtime.py --platform darwin --arch arm64 --output /tmp/db-runtime
bash packaging/drivebridge/build-macos.sh --arch arm64 --version 0.1.0 --unsigned \
  --lark-package /tmp/db-runtime/lark-package \
  --lark-native-archive /tmp/db-runtime/cli-native.tar.gz
# On Linux use --platform linux above and build-linux.sh, on the native architecture.
```

```powershell
python packaging/drivebridge/fetch-runtime.py --platform windows --arch amd64 --output "$env:TEMP/db-runtime"
pwsh -File packaging/drivebridge/build-windows.ps1 -Arch amd64 -Version 0.1.0 -Unsigned `
  -LarkPackage "$env:TEMP/db-runtime/lark-package" `
  -LarkNativeArchive "$env:TEMP/db-runtime/cli-native.zip" -NodeArchive "$env:TEMP/db-runtime/node.zip"
```

Output defaults to `dist/drivebridge-VERSION-PLATFORM-ARCH[-unsigned-test]/`. Existing output is never removed. The build uses a private temporary staging directory, `go build -trimpath`, then publishes the artifact directory only after all native packaging/signing commands succeed. Go dependencies must be provisioned or accessible through the normal Go toolchain. No private validation runtime is copied.

Payload layout is fixed: `bin/drivebridge[.exe]`, `libexec/lark/lark-cli[.exe]`, `licenses/`, `README.md`, and `manifest.json`. Windows additionally carries `libexec/node/node.exe` and the verified official package at `libexec/lark/package/` (`scripts/run.js` is its actual launcher). The managed runner resolves the native CLI from the application installation directory, never a hardcoded developer path. Node is retained for package compatibility; workers invoke the native CLI directly. The official CLI's own self-update changes its hash and therefore requires renewed capability validation; it is not silently trusted by the application.

The payload is an allowlist. Unexpected config/account files, symlinks, startup entries, missing licenses, and embedded absolute user-home/developer paths fail the build. The byte-verified official CLI embeds upstream `/home/runner/go/pkg/mod/` and `/home/runner/work/` Go source paths plus literal `/Users/.../` and `/home/.../` documentation placeholders. Only those exact prefixes are allowed in the checksum-verified vendor executable roles, recorded in `vendor_metadata`; our executable and all text still reject real developer paths. Our binary also contains exact public upstream help samples from `cmd/mountlib/rc.go`, `fs/config/rc.go`, `vfs/rc.go`, `backend/sftp/sftp.go`, and `cmd/bisync/rc.md`; only the seven full path literals recorded in `product_help_examples`, with token boundaries, are allowed. They do not allow source-directory prefixes. Vendor executables are not rewritten to conceal this upstream metadata. `manifest.schema.json` describes relative paths, per-file SHA-256 and size, Go/CLI/Node/install-tool versions, architecture, mount engine and signing status. `artifacts.json` records container hashes and the exact manifest hash without recursive self-hashing. The embedded manifest always says native verification is outstanding: evidence is a separate hash-bound result produced after installation. A `signed` manifest alone is never a release assertion.

## Installer and lifecycle boundaries

macOS Distribution enables only `currentUserHome`; its relative `Applications/DriveBridge` payload stays separate from `Library/Application Support/DriveBridge` state. Windows MSI uses `Scope=perUser`; Inno uses `PrivilegesRequired=lowest`. Linux package management writes `/opt` through the normal package manager, but installs no root service. Installers do not run workers, enable login startup or write credentials. The CLI owns the one opt-in startup definition: `feishu mount enable-autostart NAME` / `disable-autostart NAME` uses launchd user agents, systemd --user, or an interactive user task. No worker runs as SYSTEM. No second startup template is installed by packaging.

Before upgrade: stop admissions and drain each user's mounts with `feishu mount stop-all --wait-upload 60s`, then use the manager/migration workflow to back up config/state and validate retained pending data. A stop failure blocks replacement. Installers refuse if any `drivebridge` process is still running; they never force-kill it. Current builders emit `upgrade_guard:process_check_only`, and **release verification rejects it even if external evidence claims success**. A future concrete manager lifecycle-lease integration must change that declaration and prove the lock before release. This process check is a guard, not a cross-user race-free upgrade lock: native upgrade evidence must verify the manager admission/lifecycle lock and startup coordination. MSI prevents downgrade; Inno checks its recorded version; database future-schema handling remains the application's responsibility. Do not mix MSI and Inno installations or overwrite a running portable directory.

Uninstall through MSI/Inno/deb/rpm removes only tracked application files; user configuration, credentials, blobs and caches are outside those paths. macOS pkg has no automatic uninstaller: stop mounts and disable their startup items, then remove **only** `~/Applications/DriveBridge`. Portable removal has the same stop/disable requirement. There is no “clear user data” installer action.

## Signing and release gates

Omit `--unsigned` only on a provisioned signing runner:

- macOS: `DRIVEBRIDGE_CODESIGN_IDENTITY` (Developer ID Application), `DRIVEBRIDGE_INSTALLER_IDENTITY` (Developer ID Installer), `DRIVEBRIDGE_NOTARY_PROFILE` (preprovisioned notarytool keychain profile). The script first verifies the official archive **and unpacked native hash**, then signs that exact CLI with hardened runtime/timestamp. It calculates the post-sign hash, compiles our app with manager `ReleaseCLIArtifactSHA256` and `ReleaseCLIUpstreamSHA256` via `-X`, and only then signs our app. Runtime requires the compiled upstream hash to match the fixed official OS/arch map; there is no editable manifest/config capability grant or normalization of an unknown binary. The optional manifest `cli_binding` field records provenance only. Unsigned builds keep raw vendor bytes unchanged. pkg and dmg are notarized and stapled. The tar carries the same signed executable bytes as the notarized pkg; it has no staple-able tar container.
- Windows: `DRIVEBRIDGE_SIGN_CERT_SHA1`, `DRIVEBRIDGE_TIMESTAMP_URL`, and signtool. Our binary, MSI, setup EXE and Inno uninstaller are signed. Vendor binary signatures are preserved. ZIP contains the signed payload.
- Linux: `DRIVEBRIDGE_GPG_KEY`, gpg, dpkg-sig and rpmsign. deb/rpm receive package signatures and all three containers get detached signatures. Trust roots/public keys must be provisioned independently for verification.

Missing signing secrets fail; only explicit unsigned test builds are allowed. Scripts do not import/export credentials. No publishing action is included.

`smoke.sh` / `smoke.ps1` require payload, artifact directory, an explicitly supplied disposable real mounted filesystem, and native evidence. They verify hashes, native OS/arch, driver, commands, write/fsync/read/rename on that mount, and release signatures when requested. The mount probe deletes only its own newly created directory. It does **not** inject disk errors or install/uninstall by itself. Those actions, stop feedback, startup and upgrade retention must be proven by the provisioned native harness; absent evidence fails instead of returning a successful skip. T13 adds deeper end-to-end fixtures.

The runner-owned harness receives `--artifacts DIRECTORY --result FILE`. It must perform installation, user startup, pending/conflict upgrade retention, stop, uninstall retention, driver and error feedback tests, then leave a disposable mount and installed payload. Result JSON contains absolute local `payload`, `mountpoint`, `evidence` paths. Evidence JSON has this structure (all checks must be `passed`, not copied from an example):

```json
{
  "platform": "linux", "architecture": "amd64", "runner": "native-runner-identifier",
  "payload_sha256": "SHA256 of canonical sorted-key compact JSON manifest.files",
  "checks": {
    "driver": "passed", "install": "passed", "mount_read_write_rename": "passed",
    "flush_error_feedback": "passed", "stop": "passed", "upgrade_preserves_pending": "passed",
    "uninstall_preserves_userdata": "passed", "user_startup": "passed"
  }
}
```

Use `package.payload_digest(manifest['files'])` to derive the binding. Retain the actual runner logs with the evidence; the evidence is an attestation input from that controlled runner, not a substitute for running tests.

The workflow's native matrix uses five provisioned self-hosted runners labelled `drivebridge-native` plus OS/CPU. `NATIVE_HARNESS` is a protected environment variable path. Signing keys live only in protected platform environments. Native jobs only run on manually selected default branch/protected release tags, never PR code. A missing runner remains unverified/queued; missing driver, harness, evidence, key or failed check blocks that platform and the final release gate. An unsigned artifact is always named `unsigned-test`. Unit jobs reference only existing packages. The workflow does not publish or imply that an unavailable platform passed.

## Local verification

```sh
python3 -m unittest discover -s packaging/drivebridge/tests
bash -n packaging/drivebridge/*.sh
```

Windows authoring follows the [WiX package schema](https://docs.firegiant.com/wix/schema/wxs/package/) and [impersonated custom action schema](https://docs.firegiant.com/wix/schema/wxs/customaction/); source rendering is not native MSI validation.

On Windows parse all `.ps1` through `System.Management.Automation.Language.Parser.ParseFile` before native builds. Native validation of WiX/Inno/macOS installer behavior, signing, installation/upgrade/uninstall, real mounts, and runtime compatibility requires the corresponding runner. Do not report script syntax checks as that evidence.
