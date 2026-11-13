package vfs

import (
	"context"
	"errors"
	"github.com/rclone/rclone/fs"
	"strings"
)

type managedRemover interface {
	ManagedCheckpointBinder
	BeginManagedRemove(context.Context, string) (string, error)
	CommitManagedRemove(context.Context, string) error
	FinishManagedRename(context.Context, string) error
	AbortManagedRename(context.Context, string) error
}

type managedRemoveValidator interface {
	ValidateManagedRemove(context.Context, string) error
}

// CheckManagedDelete is a side-effect-free preflight for native adapters whose
// eventual deletion callback cannot return an error (WinFsp Cleanup). The actual
// removal still repeats admission and durable namespace validation.
func (v *VFS) CheckManagedDelete(remote string) error {
	m := v.managed.Load()
	if m == nil {
		return nil
	}
	if v.Opt.ReadOnly {
		return EROFS
	}
	name := managedPath(remote)
	if name == "" {
		return EPERM
	}
	r, err := m.reserveRemove(name, true)
	if err != nil {
		return err
	}
	defer r.release()
	if validator, ok := v.f.(managedRemoveValidator); ok {
		return validator.ValidateManagedRemove(v.ctx, name)
	}
	return v.validateManagedOpen(v.ctx, name, true)
}

// A checkpoint-backed unlink detaches the old cache generation. Existing
// readers can therefore finish after the name is removed, including WinFsp's
// own deletion handle, which is released after its Unlink callback.
func (v *VFS) canDetachManagedRemoval() bool {
	_, ok := v.f.(managedRemover)
	return ok && v.cache != nil && v.cache.UsesManagedCheckpointArchive()
}

// The open handle still owns its cache generation and participates in shutdown,
// but must not exclude a newly created file with the same visible name.
func (m *managedState) retireRemovedReaders(name string) {
	name = managedPath(name)
	m.mu.Lock()
	defer m.mu.Unlock()
	for a := range m.handles {
		if !a.write && (a.path == name || strings.HasPrefix(a.path, name+"/")) {
			a.retired = true
		}
	}
}

func (f *File) removeManaged() (err error) {
	f.mu.RLock()
	v := f.d.vfs
	f.mu.RUnlock()
	r, e := v.managed.Load().reserveRemove(f.Path(), v.canDetachManagedRemoval())
	if e != nil {
		return e
	}
	defer func() {
		if !committed(err) {
			r.release()
		} else {
			r.failNamespaceWaiters()
		}
	}()
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
	v.managed.Load().retireRemovedReaders(name)
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
