package overlay

import (
	"context"
	"errors"
	"github.com/rclone/rclone/fs"
	"io"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
)

// These methods deliberately expose only already durable local content. The
// managed VFS can retain an immutable base while a new save is still archiving.
func (f *Fs) ManagedSnapshotBase(ctx context.Context, remote string) (string, error) {
	e, err := f.admitLocalWrite(ctx, remote)
	if err != nil {
		return "", err
	}
	return string(e.Visible), nil
}

func (f *Fs) OpenManagedSnapshot(ctx context.Context, id string) (io.ReadCloser, error) {
	return f.store.ReadBlob(ctx, model.BlobID(id))
}

func (f *Fs) RetainManagedSnapshot(ctx context.Context, key, remote, id string) error {
	e, err := f.store.EntryByRemote(ctx, remote)
	if err != nil {
		return err
	}
	return f.store.RetainManagedEntry(ctx, key, e.ID, model.BlobID(id))
}

// Admit metadata before the first local mutation. In particular, an editor's
// causal backup must restore its parked identity before another rename can see
// the path. No body is read, hashed or converted here.
func (f *Fs) admitLocalWrite(ctx context.Context, remote string) (model.Entry, error) {
	e, err := f.store.EntryByRemote(ctx, remote)
	if errors.Is(err, store.ErrNotFound) {
		if err = f.requireParent(ctx, parentPath(remote)); err != nil {
			return e, err
		}
		existing, err := f.store.Entries(ctx, parentPath(remote))
		if err != nil {
			return e, err
		}
		for _, x := range existing {
			if foldName(x.Remote) == foldName(remote) {
				return e, errors.New("local name collides after normalization")
			}
		}
		var resumed bool
		e, resumed, err = f.store.ResumeSafeSave(ctx, remote, newKind(remote))
		if err == nil && !resumed {
			e, err = f.store.EnsureEntry(ctx, remote, newKind(remote))
		}
	}
	if err != nil {
		return e, err
	}
	if e.Kind == model.KindFolder {
		return e, fs.ErrorIsDir
	}
	return e, f.requireWritable(ctx, e)
}
func (f *Fs) ReleaseManagedSnapshot(ctx context.Context, key string) error {
	return f.store.ReleaseManagedBase(ctx, key)
}
func (f *Fs) ReconcileManagedSnapshots(ctx context.Context, live map[string]bool) error {
	return f.store.ReconcileManagedBases(ctx, live)
}

// Body preparation is outside Store namespace transactions. Acceptance resolves
// the journal's current identity inside the same transaction as rename/replace.
func (f *Fs) ArchiveManagedCheckpoint(ctx context.Context, key string, seq uint64, in io.Reader, src fs.ObjectInfo) (fs.Object, error) {
	b, err := f.store.PutBlob(ctx, in)
	if err != nil {
		return nil, err
	}
	current, err := f.store.ManagedCheckpointEntry(ctx, key)
	if err != nil {
		return nil, err
	}
	localOnly := f.localOnlyPath(current.Remote, current.Kind)
	e, err := f.store.AcceptManagedCheckpoint(ctx, key, seq, b, localOnly)
	if err != nil {
		if errors.Is(err, store.ErrFrozen) || errors.Is(err, store.ErrBusy) {
			return nil, managedArchiveRetry{err}
		}
		return nil, err
	}
	m, err := f.store.Metadata(ctx, e.ID)
	if err != nil {
		return nil, err
	}
	// History-only generations intentionally have no visible path. The worker
	// owns their immutable bytes but must not resurrect an overlay object.
	e.Local, e.Visible = b, b
	return &Object{f: f, e: e, m: m, size: src.Size()}, nil
}

type managedArchiveRetry struct{ error }

func (managedArchiveRetry) Temporary() bool { return true }
func (e managedArchiveRetry) Unwrap() error { return e.error }

func (f *Fs) BindManagedNamespace(ctx context.Context, id string, inputs []model.ManagedCheckpoint) error {
	return f.store.BindManagedNamespace(ctx, id, inputs)
}

func (f *Fs) CommitManagedRename(ctx context.Context, id string, inputs []model.ManagedCheckpoint) (fs.Object, error) {
	if err := f.validateManagedNamespaceCommit(ctx, id); err != nil {
		return nil, err
	}
	e, err := f.store.CommitManagedNamespace(ctx, id, inputs)
	if err != nil {
		return nil, err
	}
	if e.Kind == model.KindFolder {
		return nil, nil
	}
	o, err := f.object(ctx, e)
	if err != nil {
		return nil, &CommittedError{Err: err}
	}
	return o, nil
}
