package vfs

import (
	"context"
	"errors"
	"fmt"
	"path"
	"strings"
	"sync"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/vfs/vfscache"
	"github.com/rclone/rclone/vfs/vfscommon"
)

var ErrExternalUpdateBusy = errors.New("external update busy")
var ErrManagedSyncDisabled = errors.New("managed sync disabled")

// ManagedMaterializer binds an unknown-size object to one complete local snapshot.
type ManagedMaterializer interface {
	MaterializeManaged(context.Context, string) (fs.Object, error)
}

// ManagedNamespaceReplayer must call reconcile only for durably committed
// namespaces, before Finish; failed reconciliation must leave the journal frozen.
type ManagedNamespaceReplayer interface {
	ReplayManagedNamespace(context.Context, func(string, string) error) error
}

type managedState struct {
	paused       int
	mu           sync.Mutex
	handles      map[*admission]struct{}
	reservations map[*reservation]struct{}
}
type admission struct {
	m     *managedState
	path  string
	write bool
}
type reservation struct {
	m     *managedState
	paths []string
}

func newManagedState() *managedState {
	return &managedState{handles: map[*admission]struct{}{}, reservations: map[*reservation]struct{}{}}
}
func managedPath(s string) string { return strings.TrimPrefix(path.Clean("/"+s), "/") }
func related(a, b string) bool {
	return a == b || a == "" || b == "" || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}
func (m *managedState) admit(name string, write bool) (*admission, error) {
	if m == nil {
		return nil, nil
	}
	name = managedPath(name)
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.paused > 0 {
		return nil, ErrExternalUpdateBusy
	}
	for r := range m.reservations {
		for _, p := range r.paths {
			if related(name, p) {
				return nil, ErrExternalUpdateBusy
			}
		}
	}
	a := &admission{m, name, write}
	m.handles[a] = struct{}{}
	return a, nil
}
func (a *admission) release() {
	if a == nil {
		return
	}
	a.m.mu.Lock()
	delete(a.m.handles, a)
	a.m.mu.Unlock()
}
func (m *managedState) reserve(paths []string, readers bool) (*reservation, error) {
	return m.reserveWithPause(paths, readers, false)
}
func (m *managedState) reserveWithPause(paths []string, readers, allowPaused bool) (*reservation, error) {
	if m == nil {
		return nil, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.paused > 0 && !allowPaused {
		return nil, ErrExternalUpdateBusy
	}
	for i := range paths {
		paths[i] = managedPath(paths[i])
	}
	for _, p := range paths {
		for a := range m.handles {
			if related(p, a.path) && (readers || a.write) {
				return nil, ErrExternalUpdateBusy
			}
		}
		for r := range m.reservations {
			for _, q := range r.paths {
				if related(p, q) {
					return nil, ErrExternalUpdateBusy
				}
			}
		}
	}
	r := &reservation{m, paths}
	m.reservations[r] = struct{}{}
	return r, nil
}
func (r *reservation) release() {
	if r == nil {
		return
	}
	r.m.mu.Lock()
	delete(r.m.reservations, r)
	r.m.mu.Unlock()
}
func (m *managedState) migrate(old, new string) {
	if m == nil {
		return
	}
	old, new = managedPath(old), managedPath(new)
	m.mu.Lock()
	defer m.mu.Unlock()
	for a := range m.handles {
		if a.path == old || strings.HasPrefix(a.path, old+"/") {
			a.path = new + strings.TrimPrefix(a.path, old)
		}
	}
}

// EnableManagedSync enables an unused, unshared instance. NewManaged is required
// when startup cache replay matters. A reused native instance remains disabled;
// callers can check ManagedSyncEnabled rather than affecting another mount.
func (v *VFS) EnableManagedSync() {
	activeMu.Lock()
	defer activeMu.Unlock()
	if v.managed.Load() != nil {
		return
	}
	if v.Opt.CacheMode != vfscommon.CacheModeFull || v.cache == nil || v.inUse.Load() != 1 || v.shared.Load() || v.nativeAccess.Load() {
		return
	}
	if v.cache != nil && !v.cache.EnableManagedSync() {
		return
	}
	v.managed.Store(newManagedState())
}
func (v *VFS) ManagedSyncEnabled() bool { return v.managed.Load() != nil }

// TryExternalUpdate reserves the path before checking its cache. commit must only
// publish already prepared local metadata: no network, VFS recursion or blob copy.
func (v *VFS) TryExternalUpdate(ctx context.Context, remote string, commit func() (fs.Object, error)) error {
	m := v.managed.Load()
	if m == nil {
		return ErrManagedSyncDisabled
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	name := managedPath(remote)
	r, e := m.reserveWithPause([]string{name}, true, true)
	if e != nil {
		return e
	}
	defer r.release()
	if v.cache != nil {
		if e = v.cache.InvalidateClean(name); e != nil {
			if errors.Is(e, vfscache.ErrBusy) {
				return ErrExternalUpdateBusy
			}
			return e
		}
	}
	if e = ctx.Err(); e != nil {
		return e
	}
	o, e := commit()
	if e != nil {
		return e
	}
	if o == nil {
		return fmt.Errorf("managed publication returned nil object")
	}
	// A successful local commit must reach the live inode even after cancellation.
	if f, ok := v.root.cachedNode(name).(*File); ok {
		f.setObjectNoUpdate(o)
		f.setSize(o.Size())
		f.mu.RLock()
		d := f.d
		f.mu.RUnlock()
		d.mu.Lock()
		d.read = time.Time{}
		d.mu.Unlock()
	} else {
		parent, leaf := path.Split(name)
		if d := v.root.cachedDir(strings.TrimSuffix(parent, "/")); d != nil {
			d.addObject(newFile(d, d.Path(), o, leaf))
			d.mu.Lock()
			d.read = time.Time{}
			d.mu.Unlock()
		}
	}
	return nil
}

// MaterializeManaged is fallible and target-only; Size and ReadDirAll stay pure.
func (f *File) MaterializeManaged(ctx context.Context) error {
	f.mu.RLock()
	v := f.d.vfs
	f.mu.RUnlock()
	m := v.managed.Load()
	if m == nil {
		return nil
	}
	a, e := m.admit(f.Path(), false)
	if e != nil {
		return e
	}
	defer a.release()
	return f.materializeManaged(ctx)
}
func (f *File) materializeManaged(ctx context.Context) error {
	f.materializeMu.Lock()
	defer f.materializeMu.Unlock()
	f.mu.RLock()
	o, v := f.o, f.d.vfs
	f.mu.RUnlock()
	if o == nil || o.Size() >= 0 {
		return nil
	}
	mm, ok := v.f.(ManagedMaterializer)
	if !ok {
		return fmt.Errorf("managed filesystem cannot materialize unknown size")
	}
	o, e := mm.MaterializeManaged(ctx, f.Path())
	if e != nil {
		return e
	}
	if o == nil || o.Size() < 0 {
		return fmt.Errorf("managed materializer returned unknown size")
	}
	f.setObjectNoUpdate(o)
	f.setSize(o.Size())
	return nil
}

// MaterializeNode is used by adapters holding an existing inode.
func MaterializeNode(ctx context.Context, n Node) error {
	if f, ok := n.(*File); ok {
		return f.MaterializeManaged(ctx)
	}
	return nil
}

type managedHandle struct {
	Handle
	token  *admission
	mu     sync.Mutex
	closed bool
}

func (h *managedHandle) finish(release bool) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		if release {
			return nil
		}
		return ECLOSED
	}
	h.closed = true
	defer h.token.release()
	if h.Handle.Node().IsDir() {
		return h.Handle.Close()
	}
	if release {
		return h.Handle.Release()
	}
	return h.Handle.Close()
}
func (h *managedHandle) Close() error   { return h.finish(false) }
func (h *managedHandle) Release() error { return h.finish(true) }

// PauseManagedAdmission blocks new opens and namespace mutations, but permits
// prepared external publication so the worker can drain confirmed results.
// Existing handles must finish; callers stop/join the engine before unmount.
func (v *VFS) PauseManagedAdmission() (func(), error) {
	m := v.managed.Load()
	if m == nil {
		return nil, ErrManagedSyncDisabled
	}
	m.mu.Lock()
	m.paused++
	m.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { m.mu.Lock(); m.paused--; m.mu.Unlock() }) }, nil
}
func (v *VFS) ManagedActivity() (readers, writers, reservations int) {
	m := v.managed.Load()
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for a := range m.handles {
		if a.write {
			writers++
		} else {
			readers++
		}
	}
	reservations = len(m.reservations)
	return
}
func (v *VFS) ManagedCachePending() bool { return v.cache != nil && v.cache.ManagedPending() }
func (v *VFS) ManagedCachePaths() (dataRoot, metaRoot string) {
	if v.cache == nil {
		return
	}
	return v.cache.ManagedPaths()
}
