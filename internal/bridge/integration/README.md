# Functional acceptance harness

The default package composes real bbolt/blob Store, Overlay, `vfs.NewManaged`, Engine, and a Publisher that calls `TryExternalUpdate` before local publication. Its provider is synthetic, with offline-only capability grants. Cache roots are temporary and the existing process cache override is restored after each sequential fixture. No live account, browser, notification or driver is used by default.

```sh
DRIVEBRIDGE_NATIVE=0 DRIVEBRIDGE_LIVE=0 go test -p 4 ./internal/bridge/integration -count=1
```

Native/live skips mean **unverified**, not release passes. Release orchestration must opt into required gates, archive their evidence and fail missing dependencies.

## Real managed driver smoke

Only an explicitly authorized operator should run this. It mounts the same synthetic Store/Overlay/VFS stack into a private temporary directory, without any account. It checks actual kernel Stat/read/write/fsync/close/rename, resource identity, and injected Store EIO/ENOSPC surfaced through OS fsync/close followed by a byte-exact retry. Those injected errors are not proof of a physically full disk.

Required environment:

- `DRIVEBRIDGE_NATIVE=1`
- `DRIVEBRIDGE_REQUIRE_NATIVE=1`
- `DRIVEBRIDGE_NATIVE_ALLOW_MOUNT=1`
- Optional `DRIVEBRIDGE_NATIVE_ENGINE=cmount` or `mount2` (OS default otherwise).
- Optional `DRIVEBRIDGE_NATIVE_EVIDENCE_DIR` absolute private directory to retain JSON evidence; otherwise evidence lives in the test temporary tree.

Run `go test -p 4 ./internal/bridge/integration -run '^TestNativeMount$' -count=1 -timeout=3m`. On platforms using cmount, build with the `cmount` tag and required cgo/native driver support. Missing compiled method or driver is a failure once opted in; no automatic elevation or installation is attempted. The test's adapter is real, its remote provider is synthetic.

## Parent/operator-only live observation helper

`TestLiveFeishuFileObservations` uses manager/auth's typed managed runner and cloud client. It does not read credential contents, run shell strings, enroll/login/logout, or adopt another identity. The operator must explicitly select an already verified profile and a disposable parent folder. It creates a unique named child folder and verifies small, 16 MiB and empty files by an independent whole download/count/SHA, plus a newly created document body projection. Unknown writes are never repeated; evidence retains the unique resource names for reconciliation. Cleanup only removes independently listed returned resources under that unique folder; unexpected contents keep the run folder.

Required environment:

- `DRIVEBRIDGE_LIVE=1`, `DRIVEBRIDGE_REQUIRE_LIVE=1`
- `DRIVEBRIDGE_LIVE_ALLOW_MUTATIONS=1`, `DRIVEBRIDGE_LIVE_CONFIRM_ISOLATED=1`
- `DRIVEBRIDGE_LIVE_CONFIG`: absolute product config path.
- `DRIVEBRIDGE_LIVE_CLI`: absolute official CLI executable.
- `DRIVEBRIDGE_LIVE_PROFILE`: explicit profile UUID or name.
- `DRIVEBRIDGE_LIVE_FOLDER_TOKEN`: explicit authorized disposable parent resource.
- Optional `DRIVEBRIDGE_LIVE_EVIDENCE_DIR`: absolute private directory to retain evidence.
- Optional `DRIVEBRIDGE_LIVE_KEEP=1`: retain independently verified test resources.

Run only the selected helper with `go test -p 4 ./internal/bridge/integration -run '^TestLiveFeishuFileObservations$' -count=1 -timeout=7m` after authorizing that folder. An empty upload rejection is an unconfirmed helper case and fails rather than pretending an observed empty resource exists. The separate default synthetic test verifies the product's durable blocked behavior for an unverified empty upload.

This helper does **not** satisfy the complete live LV-01–LV-05 matrix: credential sharing/revocation across installations, independently verified per-operation document mutation capability, real multi-page boundaries, inaccessible history and two-client race acceptance remain operator gates. `DRIVEBRIDGE_REQUIRE_LIVE_FULL=1` intentionally makes the full-acceptance gate fail until that broader plan has genuine evidence. Do not treat a helper pass plus skipped full gate as a platform release pass.

## Coverage limits

The default tests prove application composition and durable bytes under controlled synthetic failures; they are not kernel, account, provider atomicity, request-causality, signing or installer evidence. A default suite success cannot authorize any production mutation capability. Human-readable conflict copies are independent immutable files under the Store's conflict UUID/version directory. Upgrade fixtures verify backed-up conflict source bytes can regenerate those copies; user-edited export files are not claimed to be part of the current config/DB/blob-only migration bundle, and original state is never removed by the restore helper.
