package overlay

import (
	"context"
	"errors"
	"github.com/rclone/rclone/internal/bridge/store"
	"time"
)

// TouchManagedAccess is called after a successful managed open has materialized
// its contents. It also observes VFS-cache hits without fetching remote data.
func (f *Fs) TouchManagedAccess(ctx context.Context, remote string) error {
	entry, err := f.store.EntryByRemote(ctx, remote)
	if errors.Is(err, store.ErrNotFound) {
		return nil
	} // New file not accepted yet.
	if err != nil {
		return err
	}
	if err = f.store.TouchBodyAccess(ctx, entry.ID, time.Now().UTC()); err != nil {
		return err
	}
	f.touch(entry)
	return nil
}
