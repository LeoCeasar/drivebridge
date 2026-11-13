// Package history archives durable mount events in private, account-bound Git
// repositories. It never traverses a mountpoint or performs cloud operations.
package history

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"
	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/platform"
	bolt "go.etcd.io/bbolt"
)

type Options struct {
	Dir, GitExecutable string
	ProfileID          model.ProfileID
	IdentityHash       string
}
type Repository struct {
	dir, gitDir, executable string
	profile                 model.ProfileID
	identity                string
	mu                      sync.Mutex
}
type Source interface {
	HistoryCursor(context.Context) (string, uint64, error)
	HistoryEvents(context.Context, uint64, int) ([]model.HistoryEvent, error)
	ReadBlob(context.Context, model.BlobID) (io.ReadCloser, error)
	AckHistory(context.Context, uint64, string) error
}
type Ref struct {
	OID    string `json:"oid,omitempty"`
	Asset  string `json:"asset,omitempty"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}
type Version struct {
	Pending    bool               `json:"pending,omitempty"`
	Pinned     bool               `json:"pinned,omitempty"`
	ID         string             `json:"id"`
	Order      uint64             `json:"order"`
	RecordedAt time.Time          `json:"recorded_at"`
	Event      model.HistoryEvent `json:"event"`
	Bodies     map[string]Ref     `json:"bodies"`
	Media      map[string]Ref     `json:"media,omitempty"`
	Commit     string             `json:"commit,omitempty"`
}
type ledger struct {
	LastRecordedAt time.Time         `json:"last_recorded_at,omitempty"`
	Schema         int               `json:"schema"`
	Profile        model.ProfileID   `json:"profile"`
	Identity       string            `json:"identity"`
	Order          uint64            `json:"order"`
	Sources        map[string]uint64 `json:"sources"`
}
type IngestResult struct {
	Archived     int `json:"archived"`
	Acknowledged int `json:"acknowledged"`
}

func validUUID(s string) bool {
	u, err := uuid.Parse(s)
	return err == nil && u != uuid.Nil && u.String() == s
}
func validHash(s string) bool {
	b, err := hex.DecodeString(s)
	return err == nil && len(b) == 32 && s == strings.ToLower(s)
}
func validVersion(s string) bool {
	a, b, ok := strings.Cut(s, ":")
	if !ok || !validUUID(a) || b == "" {
		return false
	}
	for _, c := range b {
		if c < '0' || c > '9' {
			return false
		}
	}
	return b != "0"
}
func Open(ctx context.Context, o Options) (*Repository, error) {
	if !filepath.IsAbs(o.Dir) || !validUUID(string(o.ProfileID)) || o.IdentityHash == "" {
		return nil, model.ErrIdentityChanged
	}
	executable, err := resolveGit(ctx, o.GitExecutable)
	if err != nil {
		return nil, err
	}
	r := &Repository{dir: o.Dir, gitDir: filepath.Join(o.Dir, "history.git"), executable: executable, profile: o.ProfileID, identity: o.IdentityHash}
	for _, dir := range []string{r.dir, filepath.Join(r.dir, "objects"), filepath.Join(r.dir, "leases")} {
		if err = platform.EnsurePrivateDir(dir); err != nil {
			return nil, err
		}
	}
	err = r.locked(ctx, func() error {
		account := filepath.Join(r.dir, "account.json")
		if err := platform.CheckNoSymlinks(account); err != nil {
			return err
		}
		raw, readErr := os.ReadFile(account)
		if readErr == nil {
			var x ledger
			if json.Unmarshal(raw, &x) != nil || x.Schema != 1 || x.Profile != r.profile || x.Identity != r.identity {
				return model.ErrIdentityChanged
			}
		} else if !os.IsNotExist(readErr) {
			return readErr
		} else {
			b, _ := json.Marshal(ledger{Schema: 1, Profile: r.profile, Identity: r.identity})
			if err := writePrivate(account, b); err != nil {
				return err
			}
		}
		if err := platform.CheckNoSymlinks(r.gitDir); err != nil {
			return err
		}
		if _, err := os.Stat(r.gitDir); os.IsNotExist(err) {
			if _, err = r.git(ctx, nil, "init", "--bare", "--initial-branch=history"); err != nil {
				return errors.Join(ErrGitUnavailable, err)
			}
		} else if err != nil {
			return err
		}
		return r.syncIndex(ctx)
	})
	return r, err
}
func (r *Repository) locked(ctx context.Context, fn func() error) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	p := filepath.Join(r.dir, "history.lock")
	if err := platform.CheckNoSymlinks(p); err != nil {
		return err
	}
	l := flock.New(p)
	ok, err := l.TryLockContext(ctx, 10*time.Millisecond)
	if err != nil {
		return err
	}
	if !ok {
		return context.Canceled
	}
	defer l.Unlock()
	if err = platform.SecurePrivate(p, false); err != nil {
		return err
	}
	if err := r.refreshEpoch(); err != nil {
		return err
	}
	return fn()
}
func writePrivate(name string, data []byte) error {
	if err := platform.CheckNoSymlinks(name); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(name), ".history-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = platform.ReplaceFile(f.Name(), name); err != nil {
		return err
	}
	return platform.SyncDir(filepath.Dir(name))
}
func (r *Repository) head(ctx context.Context) (string, error) {
	b, err := r.git(ctx, nil, "rev-parse", "--verify", "refs/heads/history")
	if err == nil {
		return strings.TrimSpace(string(b)), nil
	}
	_, probe := r.git(ctx, nil, "show-ref", "--verify", "--quiet", "refs/heads/history")
	var exit *exec.ExitError
	if errors.As(probe, &exit) && exit.ExitCode() == 1 {
		return "", nil
	}
	return "", err
}

func (r *Repository) loadLedger(ctx context.Context, head string) (ledger, error) {
	l := ledger{Schema: 1, Profile: r.profile, Identity: r.identity, Sources: map[string]uint64{}}
	if head == "" {
		return l, nil
	}
	raw, err := r.git(ctx, nil, "cat-file", "blob", head+":source-watermarks.json")
	if err != nil {
		return l, err
	}
	if json.Unmarshal(raw, &l) != nil || l.Schema != 1 || l.Profile != r.profile || l.Identity != r.identity || l.Sources == nil {
		return l, ErrIntegrity
	}
	return l, nil
}

// prepare runs under the repository lock: objects cannot be collected between
// import and the commit that makes them reachable. The Source retains ownership
// until the archive receipt is durable. No second raw-body archive is created.
func (r *Repository) prepare(ctx context.Context, s Source, ev model.HistoryEvent) (map[model.BlobID]Ref, error) {
	result := map[model.BlobID]Ref{}
	for _, b := range ev.Blobs() {
		if !validHash(string(b)) {
			return nil, ErrIntegrity
		}
		var cached Ref
		if err := r.index(func(tx *bolt.Tx) error {
			if raw := tx.Bucket([]byte("blob_refs")).Get([]byte(b)); raw != nil {
				return json.Unmarshal(raw, &cached)
			}
			return nil
		}); err != nil {
			return nil, err
		}
		if cached.OID != "" {
			if cached.SHA256 != string(b) || cached.Size < 0 {
				return nil, ErrIntegrity
			}
			raw, err := r.git(ctx, nil, "cat-file", "-s", cached.OID)
			if err != nil {
				return nil, errors.Join(ErrIntegrity, err)
			}
			size, err := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
			if err != nil || size != cached.Size {
				return nil, ErrIntegrity
			}
			result[b] = cached
			continue
		}
		in, err := s.ReadBlob(ctx, b)
		if err != nil {
			return nil, err
		}
		digest := sha256.New()
		count := &countWriter{}
		oid, err := r.blob(ctx, io.TeeReader(contextReader{ctx, in}, io.MultiWriter(digest, count)))
		err = errors.Join(err, in.Close())
		if err != nil {
			return nil, err
		}
		if hex.EncodeToString(digest.Sum(nil)) != string(b) {
			return nil, ErrIntegrity
		}
		result[b] = Ref{OID: oid, SHA256: string(b), Size: count.n}
	}
	return result, nil
}

type countWriter struct{ n int64 }

func (w *countWriter) Write(p []byte) (int, error) { w.n += int64(len(p)); return len(p), nil }

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
func (r *Repository) Ingest(ctx context.Context, s Source) (result IngestResult, err error) {
	source, ack, err := s.HistoryCursor(ctx)
	if err != nil {
		return result, err
	}
	if !validUUID(source) {
		return result, ErrIntegrity
	}
	events, err := s.HistoryEvents(ctx, ack, 50)
	if err != nil {
		return result, err
	}
	for _, ev := range events {
		if ev.Source != source || ev.ID != fmt.Sprintf("%s:%d", source, ev.Sequence) || ev.ProfileID != r.profile || ev.IdentityHash != r.identity || ev.Entry.ProfileID != r.profile || ev.Entry.MountID != ev.MountID || !validUUID(string(ev.Entry.ID)) {
			return result, model.ErrIdentityChanged
		}
		already := false
		if err := r.locked(ctx, func() error {
			if err := r.syncIndex(ctx); err != nil {
				return err
			}
			head, err := r.head(ctx)
			if err != nil {
				return err
			}
			l, err := r.loadLedger(ctx, head)
			if err != nil {
				return err
			}
			already = ev.Sequence <= l.Sources[source]
			return nil
		}); err != nil {
			return result, err
		}
		if already {
			if err := s.AckHistory(ctx, ev.Sequence, ev.ID); err != nil {
				return result, err
			}
			result.Acknowledged++
			continue
		}
		err = r.locked(ctx, func() error {
			head, err := r.head(ctx)
			if err != nil {
				return err
			}
			l, err := r.loadLedger(ctx, head)
			if err != nil {
				return err
			}
			if ev.Sequence <= l.Sources[source] {
				return r.syncIndex(ctx)
			}
			if ev.Sequence != l.Sources[source]+1 {
				return ErrIntegrity
			}
			prepared, err := r.prepare(ctx, s, ev)
			if err != nil {
				return err
			}
			v := Version{ID: ev.ID, Order: l.Order + 1, RecordedAt: time.Now().UTC(), Event: ev, Bodies: map[string]Ref{}, Media: map[string]Ref{}}
			if !v.RecordedAt.After(l.LastRecordedAt) {
				v.RecordedAt = l.LastRecordedAt.Add(time.Nanosecond)
			}
			l.LastRecordedAt = v.RecordedAt
			v.Event.Archived = true
			for role, b := range ev.Bodies {
				v.Bodies[role] = prepared[b]
			}
			for role, b := range ev.Media {
				v.Media[role] = prepared[b]
			}

			l.Order = v.Order
			l.Sources[source] = ev.Sequence
			if _, err = r.commitVersion(ctx, head, l, v); err != nil {
				return err
			}
			if err = r.syncIndex(ctx); err != nil {
				return err
			}
			result.Archived++
			return nil
		})
		if err != nil {
			return result, err
		}
		if err = s.AckHistory(ctx, ev.Sequence, ev.ID); err != nil {
			return result, err
		}
		result.Acknowledged++
	}
	return result, nil
}
func (r *Repository) index(fn func(*bolt.Tx) error) error {
	p := filepath.Join(filepath.Dir(r.gitDir), "index.db")
	if err := platform.CheckNoSymlinks(p); err != nil {
		return err
	}
	db, err := bolt.Open(p, 0600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return err
	}
	defer db.Close()
	return db.Update(func(tx *bolt.Tx) error {
		for _, n := range []string{"meta", "versions", "order", "blob_refs"} {
			if _, err := tx.CreateBucketIfNotExists([]byte(n)); err != nil {
				return err
			}
		}
		return fn(tx)
	})
}
func (r *Repository) syncIndex(ctx context.Context) error {
	head, err := r.head(ctx)
	if err != nil {
		return err
	}
	if head == "" {
		return r.index(func(tx *bolt.Tx) error {
			if tx.Bucket([]byte("meta")).Get([]byte("head")) != nil {
				return ErrIntegrity
			}
			return nil
		})
	}
	return r.index(func(tx *bolt.Tx) error {
		old := string(tx.Bucket([]byte("meta")).Get([]byte("head")))
		if old == head {
			return nil
		}
		args := []string{"rev-list", "--reverse", head}
		if old != "" {
			if _, e := r.git(ctx, nil, "merge-base", "--is-ancestor", old, head); e != nil {
				return ErrIntegrity
			}
			args = append(args, "^"+old)
		}
		commits, err := r.git(ctx, nil, args...)
		if err != nil {
			return err
		}
		for _, commit := range strings.Fields(string(commits)) {
			raw, err := r.git(ctx, nil, "cat-file", "blob", commit+":last-event.json")
			if err != nil {
				return err
			}
			var v Version
			if json.Unmarshal(raw, &v) != nil || !validVersion(v.ID) || v.Event.ProfileID != r.profile || v.Event.IdentityHash != r.identity {
				return ErrIntegrity
			}
			for _, refs := range []map[string]Ref{v.Bodies, v.Media} {
				for _, ref := range refs {
					if ref.OID != "" {
						raw, _ := json.Marshal(ref)
						if err := tx.Bucket([]byte("blob_refs")).Put([]byte(ref.SHA256), raw); err != nil {
							return err
						}
					}
				}
			}
			v.Commit = commit
			encoded, _ := json.Marshal(v)
			if err = tx.Bucket([]byte("versions")).Put([]byte(v.ID), encoded); err != nil {
				return err
			}
			if err = tx.Bucket([]byte("order")).Put([]byte(fmt.Sprintf("%020d", v.Order)), []byte(v.ID)); err != nil {
				return err
			}
		}
		return tx.Bucket([]byte("meta")).Put([]byte("head"), []byte(head))
	})
}
