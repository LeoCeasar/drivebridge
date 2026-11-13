package history

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/platform"
	bolt "go.etcd.io/bbolt"
)

type RetentionPolicy struct {
	Protected       map[string]bool
	Now             time.Time
	Keep            time.Duration
	MinimumVersions int
	Acknowledged    map[string]uint64
}
type CompactResult struct {
	Retained int    `json:"retained"`
	Expired  int    `json:"expired"`
	Epoch    string `json:"epoch"`
}
type retiredEpoch struct {
	Git   string    `json:"git"`
	After time.Time `json:"after"`
}
type catalogue struct {
	Schema   int            `json:"schema"`
	Active   string         `json:"active"`
	Previous []retiredEpoch `json:"previous,omitempty"`
}

func validEpoch(name string) bool {
	if name == "history.git" {
		return true
	}
	parts := strings.Split(filepath.ToSlash(name), "/")
	return len(parts) == 3 && parts[0] == "epochs" && validUUID(parts[1]) && parts[2] == "history.git"
}
func (r *Repository) loadCatalogue() (c catalogue, err error) {
	p := filepath.Join(r.dir, "active.json")
	if err = platform.CheckNoSymlinks(p); err != nil {
		return c, err
	}
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return catalogue{Schema: 1, Active: "history.git"}, nil
	}
	if err != nil {
		return c, err
	}
	if len(b) > 1<<20 || json.Unmarshal(b, &c) != nil || c.Schema != 1 || !validEpoch(c.Active) {
		return c, ErrIntegrity
	}
	for _, v := range c.Previous {
		if !validEpoch(v.Git) || v.Git == c.Active {
			return c, ErrIntegrity
		}
	}
	return
}
func (r *Repository) refreshEpoch() error {
	c, err := r.loadCatalogue()
	if err != nil {
		return err
	}
	p := filepath.Join(r.dir, filepath.FromSlash(c.Active))
	if err = platform.CheckNoSymlinks(p); err != nil {
		return err
	}
	if c.Active != "history.git" {
		if _, err = os.Stat(p); err != nil {
			return ErrIntegrity
		}
	}
	r.gitDir = p
	return nil
}
func (r *Repository) readPins() (map[string]bool, error) {
	p := filepath.Join(r.dir, "pins.json")
	if err := platform.CheckNoSymlinks(p); err != nil {
		return nil, err
	}
	b, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return map[string]bool{}, nil
	}
	if err != nil {
		return nil, err
	}
	pins := map[string]bool{}
	if len(b) > 4<<20 || json.Unmarshal(b, &pins) != nil {
		return nil, ErrIntegrity
	}
	for id := range pins {
		if !validVersion(id) {
			return nil, ErrIntegrity
		}
	}
	return pins, nil
}
func (r *Repository) Pin(ctx context.Context, id string, pin bool) error {
	return r.locked(ctx, func() error {
		if err := r.syncIndex(ctx); err != nil {
			return err
		}
		if _, err := r.version(ctx, id); err != nil {
			return err
		}
		pins, err := r.readPins()
		if err != nil {
			return err
		}
		if pin {
			pins[id] = true
		} else {
			delete(pins, id)
		}
		b, _ := json.Marshal(pins)
		return writePrivate(filepath.Join(r.dir, "pins.json"), b)
	})
}
func leaseName(id string) string {
	h := sha256.Sum256([]byte(id))
	return hex.EncodeToString(h[:]) + ".lock"
}
func (r *Repository) LeaseVersion(ctx context.Context, id string) (func(), error) {
	if !validVersion(id) {
		return nil, ErrBodyUnavailable
	}
	p := filepath.Join(r.dir, "leases", leaseName(id))
	if err := platform.CheckNoSymlinks(p); err != nil {
		return nil, err
	}
	l := flock.New(p)
	ok, err := l.TryRLockContext(ctx, 10*time.Millisecond)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, context.Canceled
	}
	if _, err = r.Version(ctx, id); err != nil {
		l.Unlock()
		return nil, err
	}
	var once sync.Once
	return func() { once.Do(func() { l.Unlock() }) }, nil
}
func (r *Repository) activeLeases(versions []Version) (map[string]bool, error) {
	out := map[string]bool{}
	entries, err := os.ReadDir(filepath.Join(r.dir, "leases"))
	if err != nil {
		return nil, err
	}
	names := map[string]string{}
	for _, v := range versions {
		names[leaseName(v.ID)] = v.ID
	}
	for _, entry := range entries {
		if entry.IsDir() {
			return nil, ErrIntegrity
		}
		if id, ok := names[entry.Name()]; ok {
			p := filepath.Join(r.dir, "leases", entry.Name())
			if err := platform.CheckNoSymlinks(p); err != nil {
				return nil, err
			}
			l := flock.New(p)
			locked, err := l.TryLock()
			if err != nil {
				return nil, err
			}
			if locked {
				l.Unlock()
			} else {
				out[id] = true
			}
		}
	}
	return out, nil
}
func (r *Repository) allVersions(ctx context.Context) (out []Version, err error) {
	err = r.index(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("order")).ForEach(func(_, id []byte) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			var v Version
			if err := json.Unmarshal(tx.Bucket([]byte("versions")).Get(id), &v); err != nil {
				return err
			}
			out = append(out, v)
			return nil
		})
	})
	return
}
func cloneRepository(r *Repository) *Repository {
	return &Repository{dir: r.dir, gitDir: r.gitDir, executable: r.executable, profile: r.profile, identity: r.identity}
}
func retainVersions(versions []Version, p RetentionPolicy, pins, leases map[string]bool) []Version {
	keep := map[string]bool{}
	last := map[string]bool{}
	counts := map[string]int{}
	seen := map[string]map[string]bool{}
	for i := len(versions) - 1; i >= 0; i-- {
		v := versions[i]
		group := string(v.Event.Entry.ID) + ":" + string(v.Event.MountID)
		if !last[group] {
			keep[v.ID] = true
			last[group] = true
		}
		known, ok := p.Acknowledged[v.Event.Source]
		if !ok || v.Event.Sequence > known || pins[v.ID] || leases[v.ID] || p.Protected[v.ID] || p.Now.Sub(v.RecordedAt) <= p.Keep {
			keep[v.ID] = true
		}
		if len(v.Bodies) > 0 {
			raw, _ := json.Marshal(struct{ Bodies, Media map[string]Ref }{v.Bodies, v.Media})
			signature := string(raw)
			if seen[group] == nil {
				seen[group] = map[string]bool{}
			}
			if !seen[group][signature] {
				seen[group][signature] = true
				if counts[group] < p.MinimumVersions {
					keep[v.ID] = true
					counts[group]++
				}
			}
		}
	}
	out := []Version{}
	for _, v := range versions {
		if keep[v.ID] {
			out = append(out, v)
		}
	}
	return out
}
func (r *Repository) importVersion(ctx context.Context, old *Repository, v Version) error {
	seen := map[string]bool{}
	for _, refs := range []map[string]Ref{v.Bodies, v.Media} {
		for _, ref := range refs {
			key := ref.OID + ":" + ref.Asset + ":" + ref.SHA256
			if seen[key] {
				continue
			}
			seen[key] = true
			if ref.OID != "" {
				in, err := old.openGitBody(ctx, ref)
				if err != nil {
					return err
				}
				if r.gitDir != old.gitDir {
					oid, e := r.blob(ctx, in)
					err = e
					if err == nil && oid != ref.OID {
						err = ErrIntegrity
					}
				} else {
					_, err = io.Copy(io.Discard, in)
				}
				err = errors.Join(err, in.Close())
				if err != nil {
					return err
				}
			}
			if ref.Asset != "" {
				if !validHash(ref.Asset) || ref.Asset != ref.SHA256 {
					return ErrIntegrity
				}
				p := filepath.Join(r.dir, "objects", ref.Asset)
				if err := platform.CheckNoSymlinks(p); err != nil {
					return err
				}
				f, err := os.Open(p)
				if err != nil {
					return err
				}
				h := sha256.New()
				n, err := io.Copy(h, contextReader{ctx, f})
				f.Close()
				if err != nil {
					return err
				}
				if n != ref.Size || hex.EncodeToString(h.Sum(nil)) != ref.SHA256 {
					return ErrIntegrity
				}
			}
		}
	}
	return nil
}
func (r *Repository) Compact(ctx context.Context, p RetentionPolicy) (result CompactResult, err error) {
	if p.Now.IsZero() {
		p.Now = time.Now().UTC()
	}
	if p.Keep == 0 {
		p.Keep = 30 * 24 * time.Hour
	}
	if p.MinimumVersions == 0 {
		p.MinimumVersions = 10
	}
	if p.Keep < 0 || p.MinimumVersions < 1 {
		return result, ErrIntegrity
	}
	// Only one rebuild per account. Normal ingest keeps using its separate lock.
	if err := platform.CheckNoSymlinks(filepath.Join(r.dir, "compact.lock")); err != nil {
		return result, err
	}
	compactLock := flock.New(filepath.Join(r.dir, "compact.lock"))
	ok, err := compactLock.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil {
		return result, err
	}
	if !ok {
		return result, context.Canceled
	}
	defer compactLock.Unlock()
	var old *Repository
	var base string
	var l ledger
	var versions, retained []Version
	err = r.locked(ctx, func() error {
		if e := r.syncIndex(ctx); e != nil {
			return e
		}
		var e error
		old = cloneRepository(r)
		base, e = r.head(ctx)
		if e != nil {
			return e
		}
		if base == "" {
			return nil
		}
		l, e = r.loadLedger(ctx, base)
		if e != nil {
			return e
		}
		versions, e = r.allVersions(ctx)
		if e != nil {
			return e
		}
		pins, e := r.readPins()
		if e != nil {
			return e
		}
		leases, e := r.activeLeases(versions)
		if e != nil {
			return e
		}
		retained = retainVersions(versions, p, pins, leases)
		return nil
	})
	if err != nil || base == "" {
		return result, err
	}
	result.Retained = len(retained)
	result.Expired = len(versions) - len(retained)
	if result.Expired == 0 {
		return result, nil
	}
	relative := "epochs/" + uuid.NewString() + "/history.git"
	build := cloneRepository(old)
	build.gitDir = filepath.Join(r.dir, filepath.FromSlash(relative))
	if err = platform.EnsurePrivateDir(filepath.Dir(build.gitDir)); err != nil {
		return result, err
	}
	published := false
	defer func() {
		if !published {
			os.RemoveAll(filepath.Dir(build.gitDir))
		}
	}()
	if _, err = build.git(ctx, nil, "init", "--bare", "--initial-branch=history"); err != nil {
		return result, err
	}
	head := ""
	retainedIDs := map[string]bool{}
	for _, v := range retained {
		if err = ctx.Err(); err != nil {
			return result, err
		}
		if err = build.importVersion(ctx, old, v); err != nil {
			return result, err
		}
		head, err = build.commitVersion(ctx, head, l, v)
		if err != nil {
			return result, err
		}
		retainedIDs[v.ID] = true
	}
	if err = build.syncIndex(ctx); err != nil {
		return result, err
	}
	// Catch up before the short publication window. Abort if the account is too
	// busy or its active epoch changed; old history remains authoritative.
	tailBase := base
	for attempt := 0; attempt < 4; attempt++ {
		var tail []Version
		var tip string
		var nextLedger ledger
		err = r.locked(ctx, func() error {
			if r.gitDir != old.gitDir {
				return ErrIntegrity
			}
			var e error
			tip, e = r.head(ctx)
			if e != nil {
				return e
			}
			nextLedger, e = r.loadLedger(ctx, tip)
			if e != nil {
				return e
			}
			if tip == tailBase {
				return nil
			}
			rows, e := old.git(ctx, nil, "rev-list", "--reverse", tip, "^"+tailBase)
			if e != nil {
				return e
			}
			for _, id := range strings.Fields(string(rows)) {
				raw, e := old.git(ctx, nil, "cat-file", "blob", id+":last-event.json")
				if e != nil {
					return e
				}
				var v Version
				if json.Unmarshal(raw, &v) != nil {
					return ErrIntegrity
				}
				tail = append(tail, v)
			}
			return nil
		})
		if err != nil {
			return result, err
		}
		for _, v := range tail {
			if err = build.importVersion(ctx, old, v); err != nil {
				return result, err
			}
			head, err = build.commitVersion(ctx, head, nextLedger, v)
			if err != nil {
				return result, err
			}
			retainedIDs[v.ID] = true
		}
		if len(tail) > 0 {
			if err = build.syncIndex(ctx); err != nil {
				return result, err
			}
		}
		tailBase = tip
		switched := false
		err = r.locked(ctx, func() error {
			if r.gitDir != old.gitDir {
				return ErrIntegrity
			}
			current, e := r.head(ctx)
			if e != nil {
				return e
			}
			if current != tailBase {
				return nil
			}
			all, e := r.allVersions(ctx)
			if e != nil {
				return e
			}
			pins, e := r.readPins()
			if e != nil {
				return e
			}
			leases, e := r.activeLeases(all)
			if e != nil {
				return e
			}
			for id := range pins {
				if !retainedIDs[id] {
					return errors.New("history protection changed; rebuild deferred")
				}
			}
			for id := range leases {
				if !retainedIDs[id] {
					return errors.New("history reader changed; rebuild deferred")
				}
			}
			if _, e = build.loadLedger(ctx, head); e != nil {
				return e
			}
			if _, e = build.git(ctx, nil, "fsck", "--no-reflogs", "--full"); e != nil {
				return e
			}
			c, e := r.loadCatalogue()
			if e != nil {
				return e
			}
			oldRelative, e := filepath.Rel(r.dir, old.gitDir)
			if e != nil {
				return e
			}
			c.Previous = append(c.Previous, retiredEpoch{filepath.ToSlash(oldRelative), p.Now.Add(7 * 24 * time.Hour)})
			c.Active = relative
			c.Schema = 1
			data, _ := json.Marshal(c)
			if e = writePrivate(filepath.Join(r.dir, "active.json"), data); e != nil {
				return e
			}
			r.gitDir = build.gitDir
			switched = true
			return nil
		})
		if err != nil {
			return result, err
		}
		if switched {
			published = true
			result.Epoch = relative
			result.Retained = len(retainedIDs)
			return result, nil
		}
	}
	return result, errors.New("history busy; rebuild deferred")
}

// Clean retires only validated, inactive epochs and assets not referenced by any
// retained epoch. Preparation grace protects an archive being assembled.
func (r *Repository) Clean(ctx context.Context, now time.Time) (removed int, err error) {
	if now.IsZero() {
		now = time.Now().UTC()
	}
	err = r.locked(ctx, func() error {
		c, e := r.loadCatalogue()
		if e != nil {
			return e
		}
		var remaining []retiredEpoch
		for _, epoch := range c.Previous {
			if now.Before(epoch.After) {
				remaining = append(remaining, epoch)
				continue
			}
			p := filepath.Join(r.dir, filepath.FromSlash(epoch.Git))
			if e = platform.CheckNoSymlinks(p); e != nil {
				return e
			}
			if e = os.RemoveAll(p); e != nil {
				return e
			}
			index := filepath.Join(filepath.Dir(p), "index.db")
			if e = platform.CheckNoSymlinks(index); e != nil {
				return e
			}
			if e = os.Remove(index); e != nil && !os.IsNotExist(e) {
				return e
			}
			removed++
		}
		c.Previous = remaining
		data, _ := json.Marshal(c)
		if e = writePrivate(filepath.Join(r.dir, "active.json"), data); e != nil {
			return e
		}
		refs := map[string]bool{}
		epochs := append([]retiredEpoch{{Git: c.Active}}, c.Previous...)
		for _, epoch := range epochs {
			other := cloneRepository(r)
			other.gitDir = filepath.Join(r.dir, filepath.FromSlash(epoch.Git))
			if e = other.syncIndex(ctx); e != nil {
				return e
			}
			versions, e := other.allVersions(ctx)
			if e != nil {
				return e
			}
			for _, v := range versions {
				for _, group := range []map[string]Ref{v.Bodies, v.Media} {
					for _, ref := range group {
						if ref.Asset != "" {
							refs[ref.Asset] = true
						}
					}
				}
			}
		}
		files, e := os.ReadDir(filepath.Join(r.dir, "objects"))
		if e != nil {
			return e
		}
		sort.Slice(files, func(i, j int) bool { return files[i].Name() < files[j].Name() })
		for _, f := range files {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if !validHash(f.Name()) || refs[f.Name()] {
				continue
			}
			info, e := f.Info()
			if e != nil {
				return e
			}
			if !info.Mode().IsRegular() || now.Sub(info.ModTime()) < 24*time.Hour {
				continue
			}
			p := filepath.Join(r.dir, "objects", f.Name())
			if e = platform.CheckNoSymlinks(p); e != nil {
				return e
			}
			if e = os.Remove(p); e != nil {
				return e
			}
			removed++
		}
		if _, e := r.git(ctx, nil, "prune", "--expire=24.hours.ago"); e != nil {
			return e
		}
		return platform.SyncDir(r.dir)
	})
	return
}

func (r *Repository) Usage(ctx context.Context) (bytes int64, err error) {
	err = filepath.WalkDir(r.dir, func(p string, d os.DirEntry, e error) error {
		if os.IsNotExist(e) {
			return nil
		}
		if e != nil {
			return e
		}
		if e = ctx.Err(); e != nil {
			return e
		}
		if d.Type()&os.ModeSymlink != 0 {
			return ErrIntegrity
		}
		if d.IsDir() {
			return nil
		}
		info, e := d.Info()
		if os.IsNotExist(e) {
			return nil
		}
		if e != nil {
			return e
		}
		if !info.Mode().IsRegular() {
			return ErrIntegrity
		}
		bytes += info.Size()
		return nil
	})
	return
}
