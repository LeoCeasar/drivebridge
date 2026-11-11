# DriveBridge cgofuse compatibility patch

This is the complete `fuse/` package from
`github.com/winfsp/cgofuse v1.6.1-0.20260126094232-f2c4fccdb286` (commit
`f2c4fccdb286`), not a reference checkout. The root module uses it through a local
`replace`. The original module, license, README and changelog are retained;
examples and build-container files are not needed by this dependency.

The local patch exposes `FileInfo_t.PurgeAttr` and `PurgeUBC`. Both flags pass
through OpenEx/CreateEx into macFUSE's `fuse_file_info.purge_attr/purge_ubc` fields.
Other platforms ignore them; Windows C and no-CGO ABI layout remains unchanged.
Direct I/O is not enabled by this patch, so normal kernel caching and mmap remain
available. Managed opens request the one-shot UBC push/invalidation and attribute
refresh after materialization, including same-size external updates.

References:

- https://github.com/macfuse/library/blob/master/include/fuse_common.h
- https://github.com/macfuse/library/blob/master/lib/fuse_lowlevel.c
- https://github.com/osxfuse/kext/blob/master/osxfuse/fuse_vnops.c

The archived kernel implementation handles `FOPEN_PURGE_UBC` with
`UBC_PUSHALL | UBC_INVALIDATE`, then handles `FOPEN_PURGE_ATTR` with a fresh
file-handle getattr and `ubc_setsize`. It does not set the direct-I/O vnode flags.
Actual current-driver read/mmap behavior must still be verified on a mount.

Narrow bridge test (does not mount anything):

```
cd lib/cgofuse
go test -tags=cgofuse_bridge_test ./fuse -run '^(TestFileInfoBridge|TestHostNotifyOptionalMac)'
```

The bridge probe utility is compiled only with the explicit test tag. Upstream
mount tests are retained unchanged; running the whole upstream suite mounts a
synthetic filesystem, so use the narrow command above for this patch.

## Optional macOS cache notifications

`FileSystemHost.Notify` also uses macFUSE's `fuse_invalidate_path` when that
symbol exists in the already loaded library. It is resolved with optional
`dlsym`, so its absence does not prevent older libraries from loading. Native
return values `0` and `-ENOENT` both mean the notification was handled; other
errors or a missing symbol return false. Windows and other Unix paths retain
their upstream behavior. This does not use the obsolete `fuse_invalidate` API.

Call notifications only from a background queue outside filesystem callbacks.
The caller must stop and drain that queue before unmount, because the upstream
host's `fuse` pointer must remain valid throughout the native call. Invalidation
is a kernel-cache hint rather than a synchronous frontend refresh guarantee.
The explicit-tag tests use local native stubs for absent/success/uncached/error
outcomes; they never call a system-library function with a dummy FUSE pointer.
