package vfs

import "context"

func (v *VFS) ManagedLocalSaveStatus() (bool, int, []string) {
	if v.cache == nil {
		return true, 0, nil
	}
	return v.cache.ManagedLocalSaveStatus()
}

// DrainManagedArchives waits for accepted saves to reach the immutable local
// sync outbox. It never accepts unsaved writes or waits for cloud delivery.
func (v *VFS) DrainManagedArchives(ctx context.Context) error {
	if v.cache == nil {
		return nil
	}
	return v.cache.DrainManagedArchives(ctx)
}

// ManagedCacheBytes returns the currently fetched managed VFS data bytes,
// including protected open/dirty cache entries, without evicting any content.
func (v *VFS) ManagedCacheBytes() int64 {
	if !v.ManagedSyncEnabled() || v.cache == nil {
		return 0
	}
	return v.cache.ManagedBytes()
}
