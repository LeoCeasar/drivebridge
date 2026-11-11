#ifndef CGOFUSE_HOST_NOTIFY_DARWIN_H
#define CGOFUSE_HOST_NOTIFY_DARWIN_H

#include <errno.h>

struct fuse;
typedef int (*cgofuse_invalidate_path_fn)(struct fuse *, const char *);

/* The optional API's return convention differs from hostNotify's boolean. */
static inline int cgofuse_notify_invalidate_path(cgofuse_invalidate_path_fn notify,
	struct fuse *fuse, const char *path)
{
	if (0 == notify)
		return 0;
	int result = notify(fuse, path);
	/* ENOENT means there was no cached inode to invalidate. */
	return 0 == result || -ENOENT == result;
}

#endif
