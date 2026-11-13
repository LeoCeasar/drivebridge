package store

import (
	"context"
	"strings"

	"github.com/rclone/rclone/internal/bridge/model"
	bolt "go.etcd.io/bbolt"
)

// PrepareManagedRename persists reservation, result kind and path classification
// together. Callbacks are pure path policies; they must not call Store or cloud.
func (s *Store) PrepareManagedRename(ctx context.Context, op model.NamespaceOp, editorBackup bool, resultKind model.Kind, classify func(string, model.Kind) bool, safeSibling func(string, string) bool) (out model.NamespaceOp, err error) {
	err = s.update(ctx, func(tx *bolt.Tx) error {
		if op.Kind != "rename" && op.Kind != "replace" {
			return ErrInvalidState
		}
		if editorBackup && !model.EditorTildeBackup(op.Src, op.Dst) {
			return ErrInvalidState
		}
		var er error
		out, er = s.prepareNamespaceTx(tx, op, editorBackup)
		if er != nil {
			return er
		}
		if out.State != "prepared" {
			return nil
		}
		var r namespaceRecord
		if er = get(tx, "namespace", out.ID, &r); er != nil {
			return er
		}
		src, er := entryTx(tx, r.Op.SrcID)
		if er != nil {
			return er
		}
		if r.Op.Kind == "rename" && src.Token == "" && src.Kind != model.KindFolder && resultKind != "" {
			if resultKind != model.KindFile && resultKind != model.KindDocx {
				return model.ErrUnsupportedContent
			}
			// Admission binds a file/document protocol before its cloud token may be
			// known. Keep that protocol and still accept the new local name. Background
			// namespace dispatch waits for the predecessor instead of rejecting save.
			if src.Kind != resultKind {
				bound, er := managedProtocolBoundTx(tx, src)
				if er != nil {
					return er
				}
				if bound {
					resultKind = src.Kind
				}
			}
			r.ResultKind = resultKind
		}
		r.ResultLocalOnly = map[model.EntryID]bool{}
		r.PromoteLocalOnly = map[model.EntryID]bool{}
		for _, before := range r.Before {
			destination, kind := before.Remote, before.Kind
			if r.FuseSoftRestore != "" && before.ID == r.Op.DstID {
				destination = r.Op.Dst
			}
			if under(before.Remote, r.Op.Src) {
				destination = r.Op.Dst + strings.TrimPrefix(before.Remote, r.Op.Src)
				if before.ID == src.ID && src.Token == "" && kind != model.KindFolder && resultKind != "" {
					kind = resultKind
				}
			}
			local := false
			if classify != nil {
				local = classify(destination, kind)
			}
			var metadata Metadata
			if er = get(tx, "metadata", string(before.ID), &metadata); er != nil {
				return er
			}
			if metadata.SafeSaveOriginal != "" && safeSibling != nil {
				original, er := entryTx(tx, metadata.SafeSaveOriginal)
				if er != nil {
					return er
				}
				local = local || safeSibling(original.Remote, destination)
			}
			r.ResultLocalOnly[before.ID] = local
			r.PromoteLocalOnly[before.ID] = classify != nil && classify(before.Remote, before.Kind) && !local
		}
		return put(tx, "namespace", r.Op.ID, r)
	})
	return
}

func managedProtocolBoundTx(tx *bolt.Tx, e model.Entry) (bool, error) {
	bound := e.Token != "" || e.RemoteCheckpoint != "" || inflight(tx, e.ID, "")
	if err := each[model.CreateOp](tx, "createops", func(c model.CreateOp) error {
		if c.EntryID == e.ID {
			bound = true
		}
		return nil
	}); err != nil {
		return false, err
	}
	if bound {
		return true, nil
	}
	if err := each[jobRecord](tx, "jobs", func(r jobRecord) error {
		if r.Job.EntryID == e.ID && (r.Job.Plan != "" || len(r.Job.OperationIDs) > 0 || len(r.Operations) > 0 || len(r.Results) > 0 || r.Desired != "") {
			bound = true
		}
		return nil
	}); err != nil {
		return false, err
	}
	return bound, nil
}
