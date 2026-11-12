// Package store persists one identity-bound mount. It performs no cloud operations.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/platform"
	bolt "go.etcd.io/bbolt"
)

var (
	ErrUnbound      = errors.New("store scope has not been bound")
	ErrNotFound     = errors.New("store record not found")
	ErrFrozen       = errors.New("entry scheduling is frozen by namespace operation")
	ErrBusy         = errors.New("entry has pending or inflight work")
	ErrInvalidState = errors.New("invalid journal transition")
	ErrSpace        = errors.New("less than 1 GiB available for a large snapshot")
	ErrLocked       = errors.New("mount store is already open")
)

const (
	LargeSnapshotBytes int64  = 10 << 20
	MinimumFreeBytes   uint64 = 1 << 30
	CleanCacheBudget   int64  = 5 << 30
)

var buckets = []string{"meta", "entries", "jobs", "conflicts", "paths", "recovery", "namespace", "createops", "snapshots", "metadata", "publications", "history_events", "history_last", "history_receipts", "history_restore", "history_media_cache"}

type Scope struct {
	MountID      model.MountID   `json:"mount_id"`
	ProfileID    model.ProfileID `json:"profile_id"`
	RemoteID     model.RemoteID  `json:"remote_id"`
	IdentityHash string          `json:"identity_hash"`
	FolderToken  string          `json:"folder_token"`
}
type Metadata struct {
	HistorySnapshot   model.SnapshotID `json:"history_snapshot,omitempty"`
	HistoryMediaBody  model.BlobID     `json:"history_media_body,omitempty"`
	LastBodyAccess    time.Time        `json:"last_body_access,omitempty"`
	DirectoryCachedAt time.Time        `json:"directory_cached_at,omitempty"`
	SafeSavePending   string           `json:"safe_save_pending,omitempty"`
	SafeSaveParked    bool             `json:"safe_save_parked,omitempty"`
	SafeSaveOriginal  model.EntryID    `json:"safe_save_original,omitempty"`
	FirstObserved     time.Time        `json:"first_observed"`
	ModTime           time.Time        `json:"mod_time"`
	SourceURL         string           `json:"source_url,omitempty"`
	VirtualType       string           `json:"virtual_type,omitempty"`
	Tombstone         bool             `json:"tombstone,omitempty"`
}
type Observation struct {
	Remote                      string
	Kind                        model.Kind
	Token, CloudName, SourceURL string
	VirtualType                 string
	ModTime                     time.Time
}

// PersistenceIO isolates the real durability operations; failures prevent database acceptance.
type PersistenceIO interface {
	SyncFile(*os.File) error
	Rename(string, string) error
	SyncDir(string) error
}
type nativeIO struct{}

func (nativeIO) SyncFile(f *os.File) error { return f.Sync() }
func (nativeIO) Rename(a, b string) error  { return platform.ReplaceFile(a, b) }
func (nativeIO) SyncDir(p string) error    { return platform.SyncDir(p) }

// SpacePolicy admits additional snapshots; it must never remove accepted data.
type SpacePolicy func(context.Context, string, int64) error
type Options struct {
	IO          PersistenceIO
	SpacePolicy SpacePolicy
}
type Store struct {
	mu            sync.Mutex
	db            *bolt.DB
	dir           string
	lock          *flock.Flock
	scope         Scope
	bound, closed bool
	changed       chan struct{}
	io            PersistenceIO
	space         SpacePolicy
	leases        map[model.BlobID]int
}

func Open(dir string) (*Store, error) { return OpenWithOptions(dir, Options{}) }
func OpenWithOptions(dir string, o Options) (*Store, error) {
	if o.IO == nil {
		o.IO = nativeIO{}
	}
	if o.SpacePolicy == nil {
		o.SpacePolicy = defaultSpacePolicy
	}
	var newParents []string
	for p := dir; filepath.IsAbs(p); p = filepath.Dir(p) {
		_, err := os.Lstat(p)
		if err == nil {
			break
		}
		if !os.IsNotExist(err) {
			return nil, err
		}
		newParents = append(newParents, filepath.Dir(p))
		if p == filepath.Dir(p) {
			break
		}
	}
	if err := platform.EnsurePrivateDir(dir); err != nil {
		return nil, err
	}
	if err := platform.EnsurePrivateDir(filepath.Join(dir, "blobs")); err != nil {
		return nil, err
	}
	for _, p := range []string{"worker.lock", "store.db"} {
		if err := platform.CheckNoSymlinks(filepath.Join(dir, p)); err != nil {
			return nil, err
		}
	}
	l := flock.New(filepath.Join(dir, "worker.lock"))
	ok, err := l.TryLock()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrLocked
	}
	if err = platform.SecurePrivate(filepath.Join(dir, "worker.lock"), false); err != nil {
		l.Unlock()
		return nil, err
	}
	db, err := bolt.Open(filepath.Join(dir, "store.db"), 0600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		l.Unlock()
		return nil, err
	}
	if err = db.Update(func(tx *bolt.Tx) error {
		if m := tx.Bucket([]byte("meta")); m != nil {
			v := m.Get([]byte("schema_version"))
			if string(v) != "1" && string(v) != "2" {
				return fmt.Errorf("unsupported store schema %q", v)
			}
		}
		for _, n := range buckets {
			if _, err := tx.CreateBucketIfNotExists([]byte(n)); err != nil {
				return err
			}
		}
		if err := initHistoryTx(tx); err != nil {
			return err
		}
		if tx.Bucket([]byte("meta")).Get([]byte("history_media_seeded")) == nil {
			if err := each[snapshotRecord](tx, "snapshots", func(r snapshotRecord) error { return indexHistoryMediaTx(tx, r.Snapshot) }); err != nil {
				return err
			}
			if err := tx.Bucket([]byte("meta")).Put([]byte("history_media_seeded"), []byte("1")); err != nil {
				return err
			}
		}
		if tx.Bucket([]byte("meta")).Get([]byte("history_seeded")) == nil {
			if err := each[model.Entry](tx, "entries", func(e model.Entry) error { return captureHistoryTx(tx, e.ID, nil) }); err != nil {
				return err
			}
			if err := tx.Bucket([]byte("meta")).Put([]byte("history_seeded"), []byte("1")); err != nil {
				return err
			}
		}
		return tx.Bucket([]byte("meta")).Put([]byte("schema_version"), []byte("2"))
	}); err != nil {
		db.Close()
		l.Unlock()
		return nil, err
	}
	if err = platform.SecurePrivate(filepath.Join(dir, "store.db"), false); err == nil {
		err = o.IO.SyncDir(dir)
	}
	if err == nil {
		for _, parent := range newParents {
			if err = o.IO.SyncDir(parent); err != nil {
				break
			}
		}
	}
	if err != nil {
		db.Close()
		l.Unlock()
		return nil, err
	}
	return &Store{db: db, dir: dir, lock: l, changed: make(chan struct{}), io: o.IO, space: o.SpacePolicy, leases: make(map[model.BlobID]int)}, nil
}
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	close(s.changed)
	err := s.db.Close()
	return errors.Join(err, s.lock.Unlock())
}
func canonicalUUID(v string) bool {
	u, err := uuid.Parse(v)
	return err == nil && u != uuid.Nil && u.String() == v
}
func (s *Store) BindScope(ctx context.Context, sc Scope) error {
	if !canonicalUUID(string(sc.MountID)) || !canonicalUUID(string(sc.ProfileID)) || !canonicalUUID(string(sc.RemoteID)) || sc.IdentityHash == "" || sc.FolderToken == "" {
		return errors.New("invalid scope")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.closed {
		return bolt.ErrDatabaseNotOpen
	}
	err := s.db.Update(func(tx *bolt.Tx) error {
		var old Scope
		err := get(tx, "meta", "scope", &old)
		if err == nil {
			if old != sc {
				return model.ErrIdentityChanged
			}
			if !s.bound {
				return recoverInterrupted(tx)
			}
			return nil
		}
		if !errors.Is(err, ErrNotFound) {
			return err
		}
		return put(tx, "meta", "scope", sc)
	})
	if err == nil {
		s.scope = sc
		s.bound = true
	}
	return err
}
func (s *Store) Scope(ctx context.Context) (Scope, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return Scope{}, err
	}
	if !s.bound {
		return Scope{}, ErrUnbound
	}
	return s.scope, nil
}
func (s *Store) update(ctx context.Context, fn func(*bolt.Tx) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if !s.bound {
		return ErrUnbound
	}
	if s.closed {
		return bolt.ErrDatabaseNotOpen
	}
	err := s.db.Update(func(tx *bolt.Tx) error {
		if err := fn(tx); err != nil {
			return err
		}
		return ctx.Err()
	})
	if err == nil {
		close(s.changed)
		s.changed = make(chan struct{})
	}
	return err
}
func (s *Store) view(ctx context.Context, fn func(*bolt.Tx) error) error {
	s.mu.Lock()
	bound, closed := s.bound, s.closed
	s.mu.Unlock()
	if !bound {
		return ErrUnbound
	}
	if closed {
		return bolt.ErrDatabaseNotOpen
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.db.View(fn)
}
func get(tx *bolt.Tx, b, k string, v any) error {
	x := tx.Bucket([]byte(b)).Get([]byte(k))
	if x == nil {
		return ErrNotFound
	}
	return json.Unmarshal(x, v)
}
func put(tx *bolt.Tx, b, k string, v any) error {
	x, err := json.Marshal(v)
	if err != nil {
		return err
	}
	if err := tx.Bucket([]byte(b)).Put([]byte(k), x); err != nil {
		return err
	}
	if b == "snapshots" {
		var r snapshotRecord
		if err := json.Unmarshal(x, &r); err != nil {
			return err
		}
		return indexHistoryMediaTx(tx, r.Snapshot)
	}
	if b == "entries" || b == "metadata" {
		return captureHistoryTx(tx, model.EntryID(k), nil)
	}
	if b == "conflicts" {
		var c model.Conflict
		if err := json.Unmarshal(x, &c); err != nil {
			return err
		}
		return captureHistoryTx(tx, c.EntryID, &c)
	}
	return nil
}

// Decode before callbacks so transactional journal updates cannot invalidate a bucket cursor.
func each[T any](tx *bolt.Tx, b string, fn func(T) error) error {
	var values []T
	if err := tx.Bucket([]byte(b)).ForEach(func(_, v []byte) error {
		var x T
		if err := json.Unmarshal(v, &x); err != nil {
			return err
		}
		values = append(values, x)
		return nil
	}); err != nil {
		return err
	}
	for _, x := range values {
		if err := fn(x); err != nil {
			return err
		}
	}
	return nil
}

func validRemote(p string, root bool) error {
	if p == "" && root {
		return nil
	}
	if p == "" || p == "." || strings.ContainsAny(p, "\\\x00") || strings.HasPrefix(p, "/") || path.Clean(p) != p {
		return errors.New("noncanonical remote path")
	}
	for _, v := range strings.Split(p, "/") {
		if v == ".." || v == "." || v == "" {
			return errors.New("unsafe remote path")
		}
	}
	return nil
}
func validKind(k model.Kind) bool {
	return k == model.KindFile || k == model.KindDocx || k == model.KindFolder
}
func entryTx(tx *bolt.Tx, id model.EntryID) (e model.Entry, err error) {
	err = get(tx, "entries", string(id), &e)
	return
}
func pathKey(p string) []byte {
	if p == "" {
		return []byte("/")
	}
	return []byte(p)
}
func byPath(tx *bolt.Tx, p string) (model.Entry, error) {
	b := tx.Bucket([]byte("paths")).Get(pathKey(p))
	if b == nil {
		return model.Entry{}, ErrNotFound
	}
	return entryTx(tx, model.EntryID(b))
}
func saveEntry(tx *bolt.Tx, e model.Entry) error { return put(tx, "entries", string(e.ID), e) }
func (s *Store) Entry(ctx context.Context, id model.EntryID) (e model.Entry, err error) {
	err = s.view(ctx, func(tx *bolt.Tx) error { e, err = entryTx(tx, id); return err })
	return
}
func (s *Store) EntryByRemote(ctx context.Context, p string) (e model.Entry, err error) {
	if err = validRemote(p, true); err != nil {
		return
	}
	err = s.view(ctx, func(tx *bolt.Tx) error { e, err = byPath(tx, p); return err })
	return
}
func (s *Store) Entries(ctx context.Context, parent string) (out []model.Entry, err error) {
	if err = validRemote(parent, true); err != nil {
		return
	}
	err = s.view(ctx, func(tx *bolt.Tx) error {
		return each[model.Entry](tx, "entries", func(e model.Entry) error {
			var m Metadata
			if err := get(tx, "metadata", string(e.ID), &m); err != nil {
				return err
			}
			p := path.Dir(e.Remote)
			if p == "." {
				p = ""
			}
			if !m.Tombstone && e.Remote != "" && p == parent {
				out = append(out, e)
			}
			return nil
		})
	})
	return
}
func (s *Store) EnsureEntry(ctx context.Context, p string, k model.Kind) (model.Entry, error) {
	return s.ObserveEntry(ctx, Observation{Remote: p, Kind: k})
}
func (s *Store) ObserveEntry(ctx context.Context, o Observation) (e model.Entry, err error) {
	if err = validRemote(o.Remote, true); err != nil {
		return
	}
	if !validKind(o.Kind) || (o.Remote == "" && o.Kind != model.KindFolder) {
		return e, errors.New("invalid entry kind")
	}
	// Virtual classifications only arrive with an observed provider token. They are
	// additive metadata, never a new writable kind or a local creation request.
	if o.VirtualType != "" && (o.Token == "" || o.Kind != model.KindFile || o.VirtualType == "file" || o.VirtualType == "docx" || o.VirtualType == "folder") {
		return e, model.ErrIdentityChanged
	}
	err = s.update(ctx, func(tx *bolt.Tx) error {
		var er error
		e, er = byPath(tx, o.Remote)
		if er != nil && !errors.Is(er, ErrNotFound) {
			return er
		}
		var m Metadata
		if er == nil {
			if e.Kind != o.Kind {
				return model.ErrIdentityChanged
			}
			if o.Token != "" && e.Token != "" && e.Token != o.Token {
				return model.ErrIdentityChanged
			}
			if o.Token != "" && e.Token == "" && (e.Local != "" || hasPending(tx, e.ID)) {
				return model.ErrIdentityChanged
			}
			if err := get(tx, "metadata", string(e.ID), &m); err != nil {
				return err
			}
			if o.Token != "" && m.VirtualType != o.VirtualType {
				return model.ErrIdentityChanged
			}
		} else {
			m.VirtualType = o.VirtualType
			if namespaceReserved(tx, o.Remote) {
				return ErrFrozen
			}
			e = model.Entry{ID: model.EntryID(uuid.NewString()), MountID: s.scope.MountID, ProfileID: s.scope.ProfileID, Remote: o.Remote, CloudName: path.Base(o.Remote), Kind: o.Kind, Status: model.StatusPaused}
			m.FirstObserved = time.Now().UTC()
			if err := tx.Bucket([]byte("paths")).Put(pathKey(o.Remote), []byte(e.ID)); err != nil {
				return err
			}
		}
		if o.Token != "" {
			e.Token = o.Token
		}
		if o.CloudName != "" {
			e.CloudName = o.CloudName
		}
		if o.SourceURL != "" || (o.Token != "" && m.VirtualType != "") {
			m.SourceURL = o.SourceURL
		}
		if !o.ModTime.IsZero() {
			m.ModTime = o.ModTime
		}
		if m.ModTime.IsZero() {
			m.ModTime = m.FirstObserved
		}
		if err := put(tx, "metadata", string(e.ID), m); err != nil {
			return err
		}
		return saveEntry(tx, e)
	})
	return
}
func (s *Store) Metadata(ctx context.Context, id model.EntryID) (m Metadata, err error) {
	err = s.view(ctx, func(tx *bolt.Tx) error { return get(tx, "metadata", string(id), &m) })
	return
}

// Backup writes a consistent database image. Blob files must also be copied for an offline backup.
func (s *Store) Backup(ctx context.Context, w io.Writer) error {
	return s.view(ctx, func(tx *bolt.Tx) error { _, err := tx.WriteTo(contextWriter{ctx, w}); return err })
}

type contextWriter struct {
	ctx context.Context
	w   io.Writer
}

func (w contextWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.w.Write(p)
}
