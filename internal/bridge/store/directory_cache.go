package store

import (
	"context"
	"path"
	"sort"
	"time"

	"github.com/rclone/rclone/internal/bridge/model"
	bolt "go.etcd.io/bbolt"
)

// DirectoryEntries reads the durable folder index, including the mount root.
// It does not acquire file blobs or infer deletions from missing cloud rows.
func (s *Store) DirectoryEntries(ctx context.Context) (entries []model.Entry, err error) {
	err = s.view(ctx, func(tx *bolt.Tx) error {
		return each[model.Entry](tx, "entries", func(e model.Entry) error {
			if e.Kind != model.KindFolder {
				return nil
			}
			var metadata Metadata
			if err := get(tx, "metadata", string(e.ID), &metadata); err != nil {
				return err
			}
			if !metadata.Tombstone {
				entries = append(entries, e)
			}
			return nil
		})
	})
	sort.Slice(entries, func(i, j int) bool { return entries[i].Remote < entries[j].Remote })
	return
}

// MarkDirectoryCached records local metadata availability even for empty folders.
// The identity/path check prevents a response from marking a renamed directory.
func (s *Store) MarkDirectoryCached(ctx context.Context, expected model.Entry) error {
	return s.update(ctx, func(tx *bolt.Tx) error {
		entry, err := entryTx(tx, expected.ID)
		if err != nil {
			return err
		}
		if entry.Kind != model.KindFolder || entry.Remote != expected.Remote || entry.Token != expected.Token {
			return model.ErrIdentityChanged
		}
		var metadata Metadata
		if err := get(tx, "metadata", string(entry.ID), &metadata); err != nil {
			return err
		}
		if metadata.Tombstone {
			return ErrNotFound
		}
		metadata.DirectoryCachedAt = time.Now().UTC()
		return put(tx, "metadata", string(entry.ID), metadata)
	})
}

// SeedDirectoryCache upgrades pre-marker stores in one metadata-only pass. A
// persisted nonempty directory is already renderable without a remote request.
func (s *Store) SeedDirectoryCache(ctx context.Context) error {
	return s.update(ctx, func(tx *bolt.Tx) error {
		type folder struct {
			entry    model.Entry
			metadata Metadata
		}
		var folders []folder
		children := map[string]bool{}
		if err := each[model.Entry](tx, "entries", func(e model.Entry) error {
			var metadata Metadata
			if err := get(tx, "metadata", string(e.ID), &metadata); err != nil {
				return err
			}
			if metadata.Tombstone {
				return nil
			}
			if e.Remote != "" {
				parent := path.Dir(e.Remote)
				if parent == "." {
					parent = ""
				}
				children[parent] = true
			}
			if e.Kind == model.KindFolder && metadata.DirectoryCachedAt.IsZero() {
				folders = append(folders, folder{e, metadata})
			}
			return nil
		}); err != nil {
			return err
		}
		now := time.Now().UTC()
		for _, folder := range folders {
			if !children[folder.entry.Remote] {
				continue
			}
			folder.metadata.DirectoryCachedAt = now
			if err := put(tx, "metadata", string(folder.entry.ID), folder.metadata); err != nil {
				return err
			}
		}
		return nil
	})
}

// RecordDirectoryOnline retains only safe diagnostic facts, with the same
// stable identity check used by directory publication. nil resolves history.
func (s *Store) RecordDirectoryOnline(ctx context.Context, expected model.Entry, failure *model.OnlineFailure) error {
	return s.update(ctx, func(tx *bolt.Tx) error {
		en, err := entryTx(tx, expected.ID)
		if err != nil {
			return err
		}
		if en.Kind != model.KindFolder || en.Token != expected.Token || en.Remote != expected.Remote {
			return model.ErrIdentityChanged
		}
		var m Metadata
		if err = get(tx, "metadata", string(en.ID), &m); err != nil {
			return err
		}
		if m.Tombstone {
			return ErrNotFound
		}
		if failure != nil {
			// Canceling a read did not observe an online failure. Allow the next
			// worker lifetime to resume without a synthetic waiting penalty.
			if failure.Cause == "canceled" {
				return nil
			}
			m.DirectoryOnline = retainOnline(m.DirectoryOnline, *failure)
			r := m.DirectoryOnline
			r.RequestFailures = nextOnlineCount(r.RequestFailures)
			delay := 30 * time.Second
			for i := 1; i < r.RequestFailures && delay < 5*time.Minute; i++ {
				delay *= 2
			}
			if delay > 5*time.Minute {
				delay = 5 * time.Minute
			}
			if server := onlineWait(failure.RetryAfterSeconds); server > delay {
				delay = server
			}
			next := failure.At.Add(retryJitter(delay, r.RequestFailures)).UTC()
			if r.NotBefore.Before(next) {
				r.NotBefore = next
			}
		} else if m.DirectoryOnline != nil {
			m.DirectoryOnline.Resolved = true
			m.DirectoryOnline.RequestFailures = 0
			m.DirectoryOnline.NotBefore = time.Time{}
		}
		return put(tx, "metadata", string(en.ID), m)
	})
}

// AdoptDirectory atomically binds a complete listing to the current live route.
// An old response cannot insert even one child after delete/recreate or rename.
func (s *Store) AdoptDirectory(ctx context.Context, expected model.Entry, rows []Observation) error {
	return s.update(ctx, func(tx *bolt.Tx) error {
		en, err := byPath(tx, expected.Remote)
		if err != nil {
			return err
		}
		if en.ID != expected.ID || en.Kind != model.KindFolder || en.Token != expected.Token {
			return model.ErrIdentityChanged
		}
		var m Metadata
		if err = get(tx, "metadata", string(en.ID), &m); err != nil {
			return err
		}
		if m.Tombstone {
			return ErrNotFound
		}
		if en.FrozenBy != "" {
			return ErrFrozen
		}
		for _, o := range rows {
			parent := path.Dir(o.Remote)
			if parent == "." {
				parent = ""
			}
			if parent != en.Remote || o.Remote == en.Remote {
				return model.ErrIdentityChanged
			}
			if _, err = s.observeEntryTx(tx, o); err != nil {
				return err
			}
		}
		now := time.Now().UTC()
		m.DirectoryCachedAt = now
		m.DirectoryVerifiedAt = now
		return put(tx, "metadata", string(en.ID), m)
	})
}
