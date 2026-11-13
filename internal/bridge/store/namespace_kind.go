package store

import (
	"context"

	"github.com/rclone/rclone/internal/bridge/model"
	bolt "go.etcd.io/bbolt"
)

// ConfigureNamespaceKind changes only the committed result of an unbound local
// rename. The source remains its original type if preparation or Flush fails.
func (s *Store) ConfigureNamespaceKind(ctx context.Context, id string, kind model.Kind) error {
	if kind != model.KindFile && kind != model.KindDocx {
		return model.ErrUnsupportedContent
	}
	return s.update(ctx, func(tx *bolt.Tx) error {
		var r namespaceRecord
		if err := get(tx, "namespace", id, &r); err != nil {
			return err
		}
		if r.Op.State != "prepared" || r.Op.Kind != "rename" {
			return ErrInvalidState
		}
		e, err := entryTx(tx, r.Op.SrcID)
		if err != nil {
			return err
		}
		if e.FrozenBy != id {
			return ErrFrozen
		}
		if e.Kind == model.KindFolder || e.Token != "" || e.RemoteCheckpoint != "" {
			return model.ErrUnsupportedContent
		}
		if inflight(tx, e.ID, "") {
			return ErrBusy
		}
		if err = each[model.CreateOp](tx, "createops", func(op model.CreateOp) error {
			if op.EntryID == e.ID {
				return ErrBusy
			}
			return nil
		}); err != nil {
			return err
		}
		r.ResultKind = kind
		return put(tx, "namespace", id, r)
	})
}

// EntryByToken includes explicit local-delete tombstones so a stale listing
// cannot resurrect a deleted resource or allocate a second local identity after
// a rename. Remote-absence tombstones release their token binding instead.
func (s *Store) EntryByToken(ctx context.Context, token string) (out model.Entry, err error) {
	if token == "" {
		return out, ErrNotFound
	}
	found := false
	err = s.view(ctx, func(tx *bolt.Tx) error {
		return eachIndexedEntryTx(tx, entryTokens, token, func(e model.Entry) error {
			if found {
				return model.ErrIdentityChanged
			}
			found = true
			out = e
			return nil
		})
	})
	if err == nil && !found {
		err = ErrNotFound
	}
	return
}
