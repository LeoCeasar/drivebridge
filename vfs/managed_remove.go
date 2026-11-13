package vfs

import "github.com/rclone/rclone/fs"

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
