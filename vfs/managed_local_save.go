package vfs

import "context"

// ManagedLocalSaveFailures identifies failed local WAL confirmations.
func (v *VFS) ManagedLocalSaveFailures() []string {
	if v.cache == nil {
		return nil
	}
	return v.cache.ManagedLocalSaveFailures()
}

// RetryManagedLocalSaves retries closed failed saves without rewriting bodies or
// contacting the provider. Active writers remain responsible for their saves.
func (v *VFS) RetryManagedLocalSaves(ctx context.Context) error {
	if v.cache == nil {
		return nil
	}
	return v.cache.RetryManagedLocalSaves(ctx)
}
