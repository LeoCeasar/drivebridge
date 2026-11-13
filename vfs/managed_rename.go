package vfs

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/internal/bridge/model"
)

// A RenameManaged error after durable namespace commit must implement
// interface{ Committed() bool }; such a transaction must never be aborted.
type ManagedRenamer interface {
	BeginManagedRename(context.Context, string, string) (string, error)
	RenameManaged(context.Context, string) (fs.Object, error)
	FinishManagedRename(context.Context, string) error
	AbortManagedRename(context.Context, string) error
}

type ManagedCheckpointBinder interface {
	BindManagedNamespace(context.Context, string, []model.ManagedCheckpoint) error
}
type managedNamespaceCommitter interface {
	CommitManagedRename(context.Context, string, []model.ManagedCheckpoint) (fs.Object, error)
}
type managedCommittedError struct{ error }

func (e *managedCommittedError) Committed() bool { return true }
func (e *managedCommittedError) Unwrap() error   { return e.error }
func committed(err error) bool {
	var c interface{ Committed() bool }
	return errors.As(err, &c) && c.Committed()
}
func (m *managedState) reserveRename(src, dst string, allowReaders bool) (*reservation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.paused > 0 {
		return nil, ErrExternalUpdateBusy
	}
	src, dst = managedPath(src), managedPath(dst)
	for a := range m.handles {
		if a.blocks(dst, !allowReaders) || a.blocks(src, false) {
			return nil, ErrExternalUpdateBusy
		}
	}
	for r := range m.reservations {
		for _, p := range r.paths {
			if related(p, src) || related(p, dst) {
				return nil, ErrExternalUpdateBusy
			}
		}
	}
	r := &reservation{m, []string{src, dst}}
	m.reservations[r] = struct{}{}
	if allowReaders {
		m.namespacePublications[r] = make(chan struct{})
	}
	return r, nil
}

// managedRename owns the complete namespace publication reservation, including
// cache/node publication and Finish. The generic Move path is never entered.
func (d *Dir) managedRename(oldName, newName string, dest *Dir) (err error) {
	if d.vfs.Opt.ReadOnly {
		return EROFS
	}
	src, dst := path.Join(d.Path(), oldName), path.Join(dest.Path(), newName)
	if src == dst {
		return nil
	}
	m := d.vfs.managed.Load()
	_, checkpointNamespace := d.vfs.f.(ManagedCheckpointBinder)
	checkpointNamespace = checkpointNamespace && d.vfs.cache != nil && d.vfs.cache.UsesManagedCheckpointArchive()
	r, e := m.reserveRename(src, dst, checkpointNamespace)
	if e != nil {
		return e
	}
	committedState := false
	defer func() {
		if !committedState || err == nil {
			r.release()
		} else {
			r.failNamespaceWaiters()
		}
	}()
	renamer, ok := d.vfs.f.(ManagedRenamer)
	if !ok {
		return fmt.Errorf("managed filesystem lacks rename transaction interface")
	}
	n, e := d.stat(oldName)
	if e != nil {
		return e
	}
	if n.IsDir() && strings.HasPrefix(dst, src+"/") {
		return EINVAL
	}
	tx, e := renamer.BeginManagedRename(d.vfs.ctx, src, dst)
	if e != nil {
		return e
	}
	defer func() {
		if err != nil && !committedState {
			err = errors.Join(err, renamer.AbortManagedRename(d.vfs.ctx, tx))
		}
	}()
	cache := d.vfs.cache
	binder, bindsCheckpoints := d.vfs.f.(ManagedCheckpointBinder)
	committer, atomicCommit := d.vfs.f.(managedNamespaceCommitter)
	var inputs []model.ManagedCheckpoint
	fastNamespace := bindsCheckpoints && cache != nil && cache.UsesManagedCheckpointArchive()
	// Exclude background imports through the complete namespace transaction,
	// including Finish and cache publication, not merely the snapshot drain.
	if cache != nil && !fastNamespace {
		release := cache.BlockManagedArchives()
		defer release()
	}
	if fastNamespace {
		for _, p := range []string{src, dst} {
			refs, release, err := cache.PinManagedNamespaceTree(d.vfs.ctx, p)
			if err != nil {
				return err
			}
			defer func() {
				if !committedState || err == nil {
					release()
				}
			}()
			inputs = append(inputs, refs...)
		}
		if !atomicCommit {
			if e = binder.BindManagedNamespace(d.vfs.ctx, tx, inputs); e != nil {
				return e
			}
		}
	} else if cache != nil {
		for _, p := range []string{src, dst} {
			e = cache.FlushManagedTree(d.vfs.ctx, p, func(name string, o fs.Object) {
				if f, ok := d.vfs.root.cachedNode(name).(*File); ok {
					f.setObject(o)
					f.setSize(o.Size())
				}
			})
			if e != nil {
				return e
			}
		}
	}
	var o fs.Object
	if fastNamespace && atomicCommit {
		o, e = committer.CommitManagedRename(d.vfs.ctx, tx, inputs)
	} else {
		o, e = renamer.RenameManaged(d.vfs.ctx, tx)
	}
	if e != nil {
		committedState = committed(e)
		return e
	}
	committedState = true
	if n.IsFile() && o == nil {
		return &managedCommittedError{errors.New("committed file rename returned nil object")}
	}
	if cache != nil {
		resolve := func(name string) (fs.Object, error) {
			if n.IsFile() {
				return o, nil
			}
			return d.vfs.f.NewObject(d.vfs.ctx, name)
		}
		if e = cache.RenameManagedTree(src, dst, resolve); e != nil {
			return &managedCommittedError{e}
		}
	}
	switch x := n.(type) {
	case *File:
		destPath := dest.Path()
		x.mu.Lock()
		x.d = dest
		x.dPath = destPath
		x.leaf = newName
		x.o = o
		x.pendingRenameFun = nil
		x._setIsLink()
		x.mu.Unlock()
		x.setSize(o.Size())
	case *Dir:
		if e = x.repathManaged(dest, dst); e != nil {
			return &managedCommittedError{e}
		}
	}
	d.delObject(oldName)
	dest.addObject(n)
	m.migrate(src, dst)
	if e = d.SetModTime(time.Now()); e != nil {
		return &managedCommittedError{e}
	}
	if e = renamer.FinishManagedRename(d.vfs.ctx, tx); e != nil {
		return &managedCommittedError{e}
	}
	return nil
}

// Preserve live nodes and Sys references when renaming a directory. Native rename
// may forget children; managed readers retain stable inode/admission identity.
func (d *Dir) repathManaged(parent *Dir, newPath string) error {
	d.mu.Lock()
	d.parent = parent
	d.path = newPath
	d.entry = fs.NewDirCopy(d.vfs.ctx, d.entry).SetRemote(newPath)
	d.read = time.Time{}
	nodes := make([]Node, 0, len(d.items))
	for _, n := range d.items {
		nodes = append(nodes, n)
	}
	d.mu.Unlock()
	for _, n := range nodes {
		switch x := n.(type) {
		case *File:
			name := path.Join(newPath, x.Name())
			o, e := d.vfs.f.NewObject(d.vfs.ctx, name)
			if e != nil {
				return e
			}
			x.renameDir(newPath)
			x.setObjectNoUpdate(o)
			x.setSize(o.Size())
		case *Dir:
			if e := x.repathManaged(d, path.Join(newPath, x.Name())); e != nil {
				return e
			}
		}
	}
	return nil
}
