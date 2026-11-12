package overlay

import (
	"context"
	"errors"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
	"github.com/rclone/rclone/lib/larkcli"
)

// SetDeletionCommit binds filesystem admission before background tasks start.
// The callback wraps only the local transaction, never the network scan.
func (f *Fs) SetDeletionCommit(commit func(context.Context, model.Entry, func() (bool, error)) error) {
	f.deletionCommit = commit
}

func (f *Fs) reconcileMissing(ctx context.Context, parent model.Entry, before []model.Entry, items []larkcli.DriveItem) error {
	if account, ok := f.source.(*accountSource); ok && account.accountSynthetic(parent.Token) {
		return nil // Discovery snapshots/categories are not complete cloud scans.
	}
	present := make(map[string]bool, len(items))
	for _, item := range items {
		present[item.Token] = true
	}
	for _, entry := range before {
		if entry.Token == "" || present[entry.Token] || f.LocalOnly(entry.Remote) || entry.Kind == model.KindFolder {
			continue
		}
		commit := func() (bool, error) {
			_, removed, err := f.store.ObserveRemoteDeletion(ctx, entry)
			return removed, err
		}
		var err error
		if f.deletionCommit != nil {
			err = f.deletionCommit(ctx, entry, commit)
		} else {
			_, err = commit()
		}
		// New edits/reservations win this scan. A subsequent complete scan
		// reconciles their newer generation after native handles have closed.
		if errors.Is(err, model.ErrGenerationChanged) || errors.Is(err, model.ErrVersionChanged) || errors.Is(err, store.ErrBusy) || errors.Is(err, store.ErrFrozen) {
			continue
		}
		if err != nil {
			return err
		}
	}
	return nil
}
