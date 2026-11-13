package store

import (
	"bytes"
	"sort"

	"github.com/rclone/rclone/internal/bridge/model"
	bolt "go.etcd.io/bbolt"
)

// paths already indexes the live namespace. Materialize the affected identities
// before writes so renaming a subtree cannot invalidate the traversal cursor.
func namespaceEntriesTx(tx *bolt.Tx, roots ...model.Entry) ([]model.Entry, error) {
	entries := map[model.EntryID]model.Entry{}
	for _, root := range roots {
		if root.ID == "" {
			continue
		}
		entries[root.ID] = root
		if root.Kind != model.KindFolder {
			continue
		}
		prefix := []byte(root.Remote + "/")
		cursor := tx.Bucket([]byte("paths")).Cursor()
		for key, value := cursor.Seek(prefix); key != nil && bytes.HasPrefix(key, prefix); key, value = cursor.Next() {
			e, err := entryTx(tx, model.EntryID(value))
			if err != nil {
				return nil, err
			}
			entries[e.ID] = e
		}
	}
	out := make([]model.Entry, 0, len(entries))
	for _, e := range entries {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (s *Store) blockOversizeNamespaceDocumentTx(tx *bolt.Tx, e *model.Entry) error {
	if e.Kind != model.KindDocx || e.Local == "" {
		return nil
	}
	size, err := s.blobSize(e.Local)
	if err != nil {
		return err
	}
	if size <= LargeSnapshotBytes {
		return nil
	}
	if err = each[jobRecord](tx, "jobs", func(r jobRecord) error {
		if r.Job.EntryID != e.ID || r.Job.Generation != e.Generation || !unsentJob(r) {
			return nil
		}
		r.Job.State, r.ErrorCode, r.ResumeState = model.StatusBlocked, "unsupported_content", model.StatusMerging
		return saveJob(tx, r)
	}); err != nil {
		return err
	}
	e.Status = model.StatusBlocked
	return saveEntry(tx, *e)
}
