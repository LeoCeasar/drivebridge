// Package overlay implements the internal filesystem used by a managed mount.
// Successful writes mean durable local acceptance; only the engine confirms cloud writes.
package overlay

import (
	"context"
	"errors"
	"io"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/hash"
	"github.com/rclone/rclone/internal/bridge/cloud"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
	"github.com/rclone/rclone/lib/larkcli"
)

var (
	ErrDeleteConflict   = errors.New("delete conflicts with pending local edits")
	ErrRecoveryRequired = errors.New("managed namespace cache recovery required")
)

// FileObservation owns one acquired immutable download until Close. Open never reacquires it.
type FileObservation struct {
	Token    string
	Identity model.FileIdentity
	Size     int64
	Open     func() (io.ReadCloser, error)
	Close    func() error
}

// Source is the read-only network seam. Mutation always belongs to the sync engine.
type Source interface {
	List(context.Context, string) ([]larkcli.DriveItem, error)
	FetchDoc(context.Context, string, string) (cloud.Document, error)
	ObserveFile(context.Context, string) (FileObservation, error)
}
type cloudSource struct{ c *cloud.Client }

func (s cloudSource) List(ctx context.Context, p string) ([]larkcli.DriveItem, error) {
	return s.c.List(ctx, p)
}
func (s cloudSource) FetchDoc(ctx context.Context, t, r string) (cloud.Document, error) {
	return s.c.FetchDoc(ctx, t, r)
}
func (s cloudSource) ObserveFile(ctx context.Context, t string) (FileObservation, error) {
	o, err := s.c.ObserveFile(ctx, t)
	if err != nil {
		return FileObservation{}, err
	}
	return FileObservation{Token: o.Token, Identity: o.Identity, Size: o.Size, Open: o.Open, Close: o.Close}, nil
}

type Dependencies struct {
	Store        *store.Store
	Cloud        *cloud.Client
	ProfileID    model.ProfileID
	MountID      model.MountID
	Source       Source
	TempPatterns []string
}
type Fs struct {
	store         *store.Store
	source        Source
	scope         store.Scope
	features      *fs.Features
	patterns      []string
	listMu        sync.Mutex
	activityMu    sync.Mutex
	active        map[model.EntryID]time.Time
	listed        map[model.EntryID]bool
	materialMu    sync.Mutex
	materializing map[model.EntryID]*materialization
}
type materialization struct {
	done   chan struct{}
	object fs.Object
	err    error
}

var defaultTemps = []string{".DS_Store", "Thumbs.db", "*.swp", "*.swo", ".~*", "._*"}

// NSDocument safe-save containers have a precise name; arbitrary .tmp or .sb
// user documents remain syncable. Only directory entries and their descendants
// use this rule, never a regular file with a similar name.
var safeSaveDirectory = regexp.MustCompile(`^.+\.sb-[0-9A-Fa-f]{8}-[A-Za-z0-9]{6}$`)

func New(d Dependencies) (fs.Fs, error) {
	if d.Store == nil {
		return nil, errors.New("missing durable store")
	}
	sc, err := d.Store.Scope(context.Background())
	if err != nil {
		return nil, err
	}
	if sc.ProfileID != d.ProfileID || sc.MountID != d.MountID {
		return nil, model.ErrIdentityChanged
	}
	source := d.Source
	if source == nil {
		if d.Cloud == nil {
			return nil, errors.New("missing cloud source")
		}
		source = cloudSource{d.Cloud}
	}
	patterns := d.TempPatterns
	if patterns == nil {
		patterns = defaultTemps
	}
	patterns = append([]string(nil), patterns...)
	for _, p := range patterns {
		if _, err := path.Match(p, "probe"); err != nil {
			return nil, err
		}
	}
	f := &Fs{store: d.Store, source: source, scope: sc, patterns: patterns, materializing: make(map[model.EntryID]*materialization), active: make(map[model.EntryID]time.Time), listed: make(map[model.EntryID]bool)}
	f.features = &fs.Features{CanHaveEmptyDirectories: true, Move: f.Move}
	if _, err = d.Store.ObserveEntry(context.Background(), store.Observation{Remote: "", Kind: model.KindFolder, Token: sc.FolderToken}); err != nil {
		return nil, err
	}
	return f, nil
}
func (f *Fs) Name() string             { return "drivebridge-" + string(f.scope.MountID) }
func (f *Fs) Root() string             { return "" }
func (f *Fs) String() string           { return "DriveBridge managed mount " + string(f.scope.MountID) }
func (f *Fs) Precision() time.Duration { return time.Nanosecond }
func (f *Fs) Hashes() hash.Set         { return hash.NewHashSet(hash.SHA256) }
func (f *Fs) Features() *fs.Features   { return f.features }
func (f *Fs) LocalOnly(remote string) bool {
	parts := strings.Split(remote, "/")
	for i, component := range parts {
		if !safeSaveDirectory.MatchString(component) {
			continue
		}
		if i < len(parts)-1 {
			return true
		}
		entry, err := f.store.EntryByRemote(context.Background(), remote)
		if err == nil && entry.Kind == model.KindFolder {
			return true
		}
	}
	base := path.Base(remote)
	for _, p := range f.patterns {
		if ok, _ := path.Match(p, base); ok {
			return true
		}
	}
	return false
}
func (f *Fs) ShouldSync(ctx context.Context, id model.EntryID) (bool, error) {
	e, err := f.store.Entry(ctx, id)
	if err != nil {
		return false, err
	}
	m, err := f.store.Metadata(ctx, id)
	if err != nil {
		return false, err
	}
	return m.VirtualType == "" && !m.Tombstone && !f.LocalOnly(e.Remote) && e.FrozenBy == "", nil
}
func kindForItem(t string) (model.Kind, error) {
	switch t {
	case "file":
		return model.KindFile, nil
	case "docx":
		return model.KindDocx, nil
	case "folder":
		return model.KindFolder, nil
	}
	if t == "" {
		return "", errors.New("missing remote resource type")
	}
	return model.KindFile, nil
}
func modified(s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil || v < 0 {
		return time.Time{}, errors.New("invalid remote modification time")
	}
	return time.Unix(v, 0).UTC(), nil
}
func (f *Fs) directory(ctx context.Context, p string) (model.Entry, error) {
	e, err := f.store.EntryByRemote(ctx, p)
	if errors.Is(err, store.ErrNotFound) {
		return e, fs.ErrorDirNotFound
	}
	if err != nil {
		return e, err
	}
	if e.Kind != model.KindFolder {
		return e, fs.ErrorIsFile
	}
	return e, nil
}

// List acquires only complete metadata, validates it before binding any entries,
// and merges persisted local names without inferring deletion from missing rows.
func (f *Fs) List(ctx context.Context, dir string) (fs.DirEntries, error) {
	if err := validPath(dir, true); err != nil {
		return nil, err
	}
	parent, err := f.directory(ctx, dir)
	if err != nil {
		return nil, err
	}
	f.listMu.Lock()
	defer f.listMu.Unlock()
	existing, err := f.store.Entries(ctx, dir)
	if err != nil {
		return nil, err
	}
	var items []larkcli.DriveItem
	if parent.Token != "" {
		items, err = f.source.List(ctx, parent.Token)
		if err != nil {
			return nil, err
		}
	}
	type observed struct {
		item        larkcli.DriveItem
		kind        model.Kind
		mtime       time.Time
		remote      string
		skip        bool
		virtualType string
	}
	var rows []observed
	tokens := map[string]bool{}
	sort.Slice(items, func(i, j int) bool { return items[i].Token < items[j].Token })
	used := map[string]bool{}
	for _, e := range existing {
		used[foldName(path.Base(e.Remote))] = true
	}
	for _, item := range items {
		if item.Token == "" || item.Name == "" || tokens[item.Token] || (item.ParentToken != "" && item.ParentToken != parent.Token) {
			return nil, errors.New("invalid complete directory metadata")
		}
		tokens[item.Token] = true
		k, err := kindForItem(item.Type)
		if err != nil {
			return nil, err
		}
		mt, err := modified(item.ModifiedTime)
		if err != nil {
			return nil, err
		}
		r := observed{item: item, kind: k, mtime: mt}
		if item.Type != "file" && item.Type != "docx" && item.Type != "folder" {
			r.virtualType = item.Type
			r.item.URL = safeSourceURL(item.URL)
		}
		prior, err := f.store.EntryByToken(ctx, item.Token)
		if err == nil {
			m, err := f.store.Metadata(ctx, prior.ID)
			if err != nil {
				return nil, err
			}
			if m.Tombstone || parentPath(prior.Remote) != dir {
				r.skip = true
			} else {
				if prior.Kind != k || m.VirtualType != r.virtualType {
					return nil, model.ErrIdentityChanged
				}
				r.remote = prior.Remote
			}
		} else if !errors.Is(err, store.ErrNotFound) {
			return nil, err
		} else {
			name := encodeName(item.Name)
			if k == model.KindDocx {
				name += ".md"
			} else if r.virtualType != "" {
				if r.item.URL != "" {
					name += ".url"
				} else {
					name += ".unsupported.txt"
				}
			}
			name, err = chooseName(name, item.Token, used)
			if err != nil {
				return nil, err
			}
			used[foldName(name)] = true
			r.remote = path.Join(dir, name)
		}
		rows = append(rows, r)
	}
	for _, r := range rows {
		if r.skip {
			continue
		}
		if _, err = f.store.ObserveEntry(ctx, store.Observation{Remote: r.remote, Kind: r.kind, Token: r.item.Token, CloudName: r.item.Name, SourceURL: r.item.URL, ModTime: r.mtime, VirtualType: r.virtualType}); err != nil {
			return nil, err
		}
	}
	entries, err := f.store.Entries(ctx, dir)
	if err != nil {
		return nil, err
	}
	var result fs.DirEntries
	for _, e := range entries {
		m, err := f.store.Metadata(ctx, e.ID)
		if err != nil {
			return nil, err
		}
		if e.Kind == model.KindFolder {
			result = append(result, &directory{f: f, e: e, m: m})
		} else {
			o, err := f.object(ctx, e)
			if err != nil {
				return nil, err
			}
			result = append(result, o)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Remote() < result[j].Remote() })
	f.activityMu.Lock()
	f.listed[parent.ID] = true
	f.activityMu.Unlock()
	return result, nil
}
func (f *Fs) find(ctx context.Context, remote string) (model.Entry, error) {
	if err := validPath(remote, false); err != nil {
		return model.Entry{}, err
	}
	e, err := f.store.EntryByRemote(ctx, remote)
	if !errors.Is(err, store.ErrNotFound) {
		return e, err
	}
	if _, err = f.List(ctx, parentPath(remote)); err != nil {
		return model.Entry{}, err
	}
	return f.store.EntryByRemote(ctx, remote)
}
func (f *Fs) NewObject(ctx context.Context, remote string) (fs.Object, error) {
	e, err := f.find(ctx, remote)
	if errors.Is(err, store.ErrNotFound) {
		return nil, fs.ErrorObjectNotFound
	}
	if err != nil {
		return nil, err
	}
	if e.Kind == model.KindFolder {
		return nil, fs.ErrorIsDir
	}
	return f.object(ctx, e)
}
func newKind(remote string) model.Kind {
	if strings.EqualFold(path.Ext(remote), ".md") {
		return model.KindDocx
	}
	return model.KindFile
}
func (f *Fs) Put(ctx context.Context, in io.Reader, src fs.ObjectInfo, options ...fs.OpenOption) (fs.Object, error) {
	if src == nil {
		return nil, errors.New("missing object metadata")
	}
	if err := validPath(src.Remote(), false); err != nil {
		return nil, err
	}
	if err := checkOptions(options, false); err != nil {
		return nil, err
	}
	// Existing mappings are authoritative. Unknown local paths are creations,
	// not a reason to perform network I/O on a durable local write.
	e, err := f.store.EntryByRemote(ctx, src.Remote())
	if errors.Is(err, store.ErrNotFound) {
		if _, err = f.directory(ctx, parentPath(src.Remote())); err != nil {
			return nil, err
		}
		existing, err := f.store.Entries(ctx, parentPath(src.Remote()))
		if err != nil {
			return nil, err
		}
		for _, x := range existing {
			if foldName(x.Remote) == foldName(src.Remote()) {
				return nil, errors.New("local name collides after normalization")
			}
		}
		e, err = f.store.EnsureEntry(ctx, src.Remote(), newKind(src.Remote()))
	}
	if err != nil {
		return nil, err
	}
	if e.Kind == model.KindFolder {
		return nil, fs.ErrorIsDir
	}
	o, err := f.object(ctx, e)
	if err != nil {
		return nil, err
	}
	if err = o.Update(ctx, in, src, options...); err != nil {
		return o, err
	}
	return o, nil
}

// Publish is local only and must be called inside the VFS external-update reservation.
func (f *Fs) Publish(ctx context.Context, c model.PublishCandidate) (fs.Object, error) {
	current, err := f.store.Entry(ctx, c.EntryID)
	if err != nil {
		return nil, err
	}
	if err = f.requireWritable(ctx, current); err != nil {
		return nil, err
	}
	e, err := f.store.CommitPublication(ctx, c)
	if err != nil {
		return nil, err
	}
	o, err := f.object(ctx, e)
	if err != nil {
		return nil, &CommittedError{Err: err}
	}
	return o, nil
}

type directory struct {
	f *Fs
	e model.Entry
	m store.Metadata
}

func (d *directory) Fs() fs.Info                       { return d.f }
func (d *directory) String() string                    { return d.e.Remote }
func (d *directory) Remote() string                    { return d.e.Remote }
func (d *directory) ModTime(context.Context) time.Time { return d.m.ModTime }
func (d *directory) Size() int64                       { return -1 }
func (d *directory) Items() int64                      { return -1 }
func (d *directory) ID() string                        { return string(d.e.ID) }

var _ fs.Fs = (*Fs)(nil)
var _ fs.Directory = (*directory)(nil)
