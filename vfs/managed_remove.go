package vfs

import (
	"context"
	"errors"
	"github.com/rclone/rclone/fs"
)

type managedRemover interface {
	ManagedCheckpointBinder
	BeginManagedRemove(context.Context, string) (string, error)
	CommitManagedRemove(context.Context, string) error
	FinishManagedRename(context.Context, string) error
	AbortManagedRename(context.Context, string) error
}

func (f *File) removeManaged() error {
	f.mu.RLock()
	v := f.d.vfs
	f.mu.RUnlock()
	r, e := v.managed.Load().reserve([]string{f.Path()}, true)
	if e != nil {
		return e
	}
	defer r.release()
	return f.removeManagedReserved()
}
func (f *File) removeManagedReserved() error {
	f.mu.RLock()
	v, d := f.d.vfs, f.d
	f.mu.RUnlock()
	name := f.CachePath()
	if remover, ok := v.f.(managedRemover); ok && v.cache != nil && v.cache.UsesManagedCheckpointArchive() {
		return f.removeCheckpointManaged(remover, name)
	}
	if v.cache != nil {
		release := v.cache.BlockManagedArchives()
		defer release()
	}
	if v.cache != nil {
		if _, e := v.cache.FlushManagedPath(f.ctx, name, func(o fs.Object) { f.setObject(o); f.setSize(o.Size()) }); e != nil {
			return e
		}
	}
	o := f.getObject()
	if o != nil {
		if e := o.Remove(f.ctx); e != nil {
			return e
		}
	}
	if v.cache != nil {
		if e := v.cache.InvalidateClean(name); e != nil {
			return e
		}
	}
	d.delObject(f.Name())
	return nil
}

func (f *File) removeCheckpointManaged(remover managedRemover, name string) (err error) {
	f.mu.RLock()
	v, d := f.d.vfs, f.d
	f.mu.RUnlock()
	id, err := remover.BeginManagedRemove(f.ctx, name)
	if err != nil {
		return err
	}
	committedState := false
	defer func() {
		if err != nil && !committedState {
			err = errors.Join(err, remover.AbortManagedRename(f.ctx, id))
		}
	}()
	refs, release, err := v.cache.PinManagedNamespaceTree(f.ctx, name)
	if err != nil {
		return err
	}
	defer func() {
		if !committedState || err == nil {
			release()
		}
	}()
	if err = remover.BindManagedNamespace(f.ctx, id, refs); err != nil {
		return err
	}
	if err = remover.CommitManagedRemove(f.ctx, id); err != nil {
		return err
	}
	committedState = true
	if err = v.cache.DetachManagedTree(name); err != nil {
		return &managedCommittedError{err}
	}
	d.delObject(f.Name())
	if err = remover.FinishManagedRename(f.ctx, id); err != nil {
		return &managedCommittedError{err}
	}
	return nil
}

func (d *Dir) removeAllManagedReserved() error {
	nodes, e := d.ReadDirAll()
	if e != nil {
		return e
	}
	for _, n := range nodes {
		switch x := n.(type) {
		case *File:
			e = x.removeManagedReserved()
		case *Dir:
			e = x.removeAllManagedReserved()
		default:
			e = EINVAL
		}
		if e != nil {
			return e
		}
	}
	if e = d.f.Rmdir(d.vfs.ctx, d.Path()); e != nil {
		return e
	}
	d.parent.delObject(d.Name())
	return nil
}
