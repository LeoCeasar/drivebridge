package vfs

// ManagedCacheBytes returns the currently fetched managed VFS data bytes,
// including protected open/dirty cache entries, without evicting any content.
func (v *VFS) ManagedCacheBytes() int64 {
	if !v.ManagedSyncEnabled() || v.cache == nil {
		return 0
	}
	return v.cache.ManagedBytes()
}
