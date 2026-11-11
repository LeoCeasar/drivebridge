//go:build cgo && cgofuse_bridge_test

// This explicit-tag test utility inspects native bitfields without mounting a
// filesystem. It is excluded from normal builds.
package fuse

/*
#include <fuse.h>
static unsigned int testFileInfoFlags(struct fuse_file_info *fi) {
 unsigned int flags = (fi->direct_io ? 1 : 0) | (fi->keep_cache ? 2 : 0);
#if !defined(__NetBSD__)
 flags |= fi->nonseekable ? 4 : 0;
#endif
#if defined(__APPLE__)
 flags |= (fi->purge_attr ? 8 : 0) | (fi->purge_ubc ? 16 : 0);
#endif
 return flags;
}
*/
import "C"

func fileInfoBridgeProbe(info FileInfo_t) (uint64, uint32) {
	var native c_struct_fuse_file_info
	c_hostAsgnCfileinfo(&native, c_bool(info.DirectIo), c_bool(info.KeepCache), c_bool(info.NonSeekable), c_bool(info.PurgeAttr), c_bool(info.PurgeUBC), c_uint64_t(info.Fh))
	return uint64(native.fh), uint32(C.testFileInfoFlags(&native))
}
