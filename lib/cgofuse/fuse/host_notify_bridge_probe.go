//go:build darwin && cgo && cgofuse_bridge_test

// Explicit-tag bridge test utility; no mount or system-library call is made.
package fuse

/*
#include "host_notify_darwin.h"
static int testNotifySuccess(struct fuse *f, const char *path) { return 0; }
static int testNotifyUncached(struct fuse *f, const char *path) { return -ENOENT; }
static int testNotifyFailure(struct fuse *f, const char *path) { return -EIO; }
static int testNotifyBridge(int outcome) {
 cgofuse_invalidate_path_fn fn = 0;
 if (outcome == 1) fn = testNotifySuccess;
 if (outcome == 2) fn = testNotifyUncached;
 if (outcome == 3) fn = testNotifyFailure;
 // Only the local stubs above are called. NULL is never passed to libfuse.
 return cgofuse_notify_invalidate_path(fn, 0, "/directory");
}
*/
import "C"

func notifyBridgeProbe(outcome int) bool { return C.testNotifyBridge(C.int(outcome)) != 0 }
