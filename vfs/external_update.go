package vfs

import (
	"context"
	"errors"
	"fmt"
	"os"
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

// ManagedAccessTracker records actual opens, including reads satisfied by the VFS
// cache. Attribute queries do not count as body use.
type ManagedAccessTracker interface {
	TouchManagedAccess(context.Context, string) error
}

// ManagedOpenValidator rejects per-path write intent before materialization,
// cache mutation or creation. It is optional and used only in managed mode.
// Implementations must perform local validation without network or cache I/O.
type ManagedOpenValidator interface {
	ValidateManagedOpen(context.Context, string, bool) error
}

func (v *VFS) validateManagedOpen(ctx context.Context, remote string, write bool) error {
	if v.managed.Load() == nil {
		return nil
	}
	validator, ok := v.f.(ManagedOpenValidator)
	if !ok {
		return nil
	}
	err := validator.ValidateManagedOpen(ctx, remote, write)
	if errors.Is(err, fs.ErrorPermissionDenied) || errors.Is(err, os.ErrPermission) {
		return EPERM
	}
	return err
}

// ManagedReadPreparer completes coupled resources before a file read. It runs
// after normalization and managed admission, and may perform cache/network I/O.
type ManagedReadPreparer interface {
	PrepareManagedRead(context.Context, string) error
}

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
	namespacePublications map[*reservation]chan struct{}
	nativeNotify          func(string)
	paused                int
	mu                    sync.Mutex
	handles               map[*admission]struct{}
	reservations          map[*reservation]struct{}
	readPublications      map[*reservation]chan struct{}
}
type admission struct {
	m         *managedState
	path      string
	write     bool
	directory bool
	ready     bool // cache handle construction has completed under this admission
	retired   bool // an unlinked immutable generation no longer reserves its old name
}
type reservation struct {
	m     *managedState
	paths []string
}

func newManagedState() *managedState {
	return &managedState{handles: map[*admission]struct{}{}, reservations: map[*reservation]struct{}{}, readPublications: map[*reservation]chan struct{}{}, namespacePublications: map[*reservation]chan struct{}{}}
}
func managedPath(s string) string { return strings.TrimPrefix(path.Clean("/"+s), "/") }
func related(a, b string) bool {
	return a == b || a == "" || b == "" || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}
func (m *managedState) admit(name string, write, directory bool) (*admission, error) {
	if m == nil {
		return nil, nil
	}
	name = managedPath(name)
	for {
		m.mu.Lock()
		if m.paused > 0 {
			m.mu.Unlock()
			return nil, ErrExternalUpdateBusy
		}
		var wait <-chan struct{}
		for r := range m.reservations {
			publication := m.readPublications[r]
			if publication != nil && !write {
				continue
			}
			for _, p := range r.paths {
				if related(name, p) {
					if namespace := m.namespacePublications[r]; namespace != nil {
						if directory && !write && name != p && (name == "" || strings.HasPrefix(p, name+"/")) {
							continue
						}
						wait = namespace
						break
					}
					if publication != nil {
						wait = publication
						break
					}
					m.mu.Unlock()
					return nil, ErrExternalUpdateBusy
				}
			}
			if wait != nil {
				break
			}
		}
		if wait != nil {
			// Publication callbacks only commit prepared local metadata. A writer
			// waits for that atomic change, without holding admission exclusion.
			m.mu.Unlock()
			<-wait
			continue
		}
		a := &admission{m: m, path: name, write: write, directory: directory}
		m.handles[a] = struct{}{}
		m.mu.Unlock()
		return a, nil
	}
}

// finishOpen publishes a pinned handle before namespace operations may detach
// its generation. Admission alone cannot keep a not-yet-created cache item safe.
func (a *admission) finishOpen() {
	if a == nil {
		return
	}
	a.m.mu.Lock()
	a.ready = true
	a.m.mu.Unlock()
}

func (a *admission) release() {
	if a == nil {
		return
	}
	a.m.mu.Lock()
	delete(a.m.handles, a)
	a.m.mu.Unlock()
}

// Directory enumeration holds a snapshot, not every descendant's contents.
// A strict ancestor directory reader can coexist with child changes; exact-path
// and descendant handles retain their original exclusion semantics.
func (a *admission) blocks(name string, readers bool) bool {
	if a.retired {
		return false
	}
	if a.directory && !a.write && a.path != name && (a.path == "" || strings.HasPrefix(name, a.path+"/")) {
		return false
	}
	return related(name, a.path) && (readers || a.write)
}

func (m *managedState) reserve(paths []string, readers bool) (*reservation, error) {
	return m.reserveWithPause(paths, readers, false)
}
func (m *managedState) reserveWithPause(paths []string, readers, allowPaused bool) (*reservation, error) {
	return m.reserveWithPauseMode(paths, readers, allowPaused, false)
}

func (m *managedState) reserveWithPauseMode(paths []string, readers, allowPaused, allowReads bool) (*reservation, error) {
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
			if a.blocks(p, readers) {
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
	if allowReads {
		m.readPublications[r] = make(chan struct{})
	}
	return r, nil
}
func (r *reservation) release() {
	if r == nil {
		return
	}
	r.m.mu.Lock()
	delete(r.m.reservations, r)
	if done := r.m.namespacePublications[r]; done != nil {
		delete(r.m.namespacePublications, r)
		close(done)
	}
	if done := r.m.readPublications[r]; done != nil {
		delete(r.m.readPublications, r)
		close(done)
	}
	r.m.mu.Unlock()
}

// A committed namespace requiring recovery remains reserved, but opens must
// receive an error rather than wait forever for a publication that cannot finish.
func (r *reservation) failNamespaceWaiters() {
	r.m.mu.Lock()
	defer r.m.mu.Unlock()
	if done := r.m.namespacePublications[r]; done != nil {
		delete(r.m.namespacePublications, r)
		close(done)
	}
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
	return v.tryExternalUpdate(ctx, remote, commit, false)
}

// TryExternalReconcile uses the same open/dirty-file admission barrier as
// publication, but a nil object represents a committed remote deletion.
// commit must be local-only and must retain local edits on a failed CAS.
func (v *VFS) TryExternalReconcile(ctx context.Context, remote string, commit func() (fs.Object, error)) error {
	return v.tryExternalUpdate(ctx, remote, commit, true)
}

func (v *VFS) tryExternalUpdate(ctx context.Context, remote string, commit func() (fs.Object, error), allowRemoval bool) error {
	m := v.managed.Load()
	if m == nil {
		return ErrManagedSyncDisabled
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	name := managedPath(remote)
	r, e := m.reserveWithPauseMode([]string{name}, true, true, true)
	if e != nil {
		return e
	}
	defer r.release()
	var publishCache func() error
	if v.cache != nil {
		var releaseCache func()
		publishCache, releaseCache, e = v.cache.PrepareReadPublication(name)
		if e != nil {
			if errors.Is(e, vfscache.ErrBusy) {
				return ErrExternalUpdateBusy
			}
			return e
		}
		defer releaseCache()
	}
	if e = ctx.Err(); e != nil {
		return e
	}
	o, e := commit()
	if e != nil {
		return e
	}
	// Reader construction binds its cache generation and object under m.mu.
	// The callback runs outside this lock, so reads never wait for publication work.
	m.mu.Lock()
	defer m.mu.Unlock()
	if o == nil && !allowRemoval {
		return fmt.Errorf("managed publication returned nil object")
	}
	if publishCache != nil {
		if e = publishCache(); e != nil {
			return e
		}
	}
	if o == nil {
		if allowRemoval {
			parent, leaf := path.Split(name)
			if d := v.root.cachedDir(strings.TrimSuffix(parent, "/")); d != nil {
				d.delObject(leaf)
			}
			return nil
		}
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
	a, e := m.admit(f.Path(), false, false)
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
	// Bind local cache observations to the same generation switch as new readers.
	m := v.managed.Load()
	if m != nil {
		m.mu.Lock()
	}
	if v.cache != nil {
		if local, size, ok := v.cache.ManagedLocalFile(f.CachePath()); ok {
			if local != nil {
				f.setObjectNoUpdate(local)
			}
			f.setSize(size)
			if m != nil {
				m.mu.Unlock()
			}
			return nil
		}
	}
	if m != nil {
		m.mu.Unlock()
	}
	if o == nil || o.Size() >= 0 {
		return nil
	}
	mm, ok := v.f.(ManagedMaterializer)
	if !ok {
		return fmt.Errorf("managed filesystem cannot materialize unknown size")
	}
	before := o
	o, e := mm.MaterializeManaged(ctx, f.Path())
	if e != nil {
		return e
	}
	if o == nil || o.Size() < 0 {
		return fmt.Errorf("managed materializer returned unknown size")
	}
	if m != nil {
		m.mu.Lock()
		defer m.mu.Unlock()
	}
	// A completed publication owns the inode if materialization finished later.
	if f.getObject() != before {
		return nil
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
	return v.managedActivity(true)
}

// ManagedStopActivity excludes directory enumeration from the pre-unmount drain.
// File handles and namespace reservations still drain before normal OS unmount;
// the caller must require that unmount to succeed before reporting a stop.
func (v *VFS) ManagedStopActivity() (readers, writers, reservations int) {
	return v.managedActivity(false)
}

func (v *VFS) managedActivity(includeDirectoryReaders bool) (readers, writers, reservations int) {
	m := v.managed.Load()
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for a := range m.handles {
		if a.write {
			writers++
		} else if includeDirectoryReaders || !a.directory {
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

// SetManagedNativeNotifier installs the mount adapter's native invalidation hook.
// The owner drains managed background work before tearing down the native mount.
func (v *VFS) SetManagedNativeNotifier(notify func(string)) {
	if m := v.managed.Load(); m != nil {
		m.mu.Lock()
		m.nativeNotify = notify
		m.mu.Unlock()
	}
}

// NotifyManagedDirectoryChanged expires a locally refreshed directory and informs
// native consumers. Call only outside filesystem callbacks and store/VFS locks.
func (v *VFS) NotifyManagedDirectoryChanged(remote string) {
	m := v.managed.Load()
	if m == nil {
		return
	}
	remote = managedPath(remote)
	v.root.ForgetPath(path.Join(remote, ".drivebridge-refresh"), fs.EntryObject)
	m.mu.Lock()
	notify := m.nativeNotify
	m.mu.Unlock()
	if notify != nil {
		notify("/" + remote)
	}
}
