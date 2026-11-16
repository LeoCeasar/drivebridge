//go:build linux

package vfs

import (
	"context"
	"errors"
	"path"
	"time"
)

// WaitManagedRenameReady waits for managed namespace admission without beginning
// a rename transaction. Linux FUSE close can return before final Release removes
// its writer admission. A still-open duplicate remains a writer until Release.
// Callers must supply a bounded context; expiry preserves the busy error.
// Readiness is advisory: Rename takes its own reservation and can reject a new
// writer admitted after this probe releases its reservation.
func (d *Dir) WaitManagedRenameReady(ctx context.Context, oldName, newName string, dest *Dir) error {
	m := d.vfs.managed.Load()
	if m == nil {
		return nil
	}
	src, dst := path.Join(d.Path(), oldName), path.Join(dest.Path(), newName)
	if src == dst {
		return nil
	}
	_, allowReaders := d.vfs.f.(ManagedCheckpointBinder)
	allowReaders = allowReaders && d.vfs.cache != nil && d.vfs.cache.UsesManagedCheckpointArchive()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if err := ctx.Err(); err != nil {
			return errors.Join(ErrExternalUpdateBusy, err)
		}
		r, err := m.reserveRename(src, dst, allowReaders)
		if err == nil {
			r.release()
			return nil
		}
		if !errors.Is(err, ErrExternalUpdateBusy) {
			return err
		}
		select {
		case <-ctx.Done():
			return errors.Join(ErrExternalUpdateBusy, ctx.Err())
		case <-ticker.C:
		}
	}
}
