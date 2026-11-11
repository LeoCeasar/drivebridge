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
