package store

import (
	"context"
	"errors"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/model"
	bolt "go.etcd.io/bbolt"
)

type namespaceRecord struct {
	FuseSoftRemove     bool                      `json:"fuse_soft_remove,omitempty"`
	FuseSoftRestore    string                    `json:"fuse_soft_restore,omitempty"`
	ManagedCheckpoints []model.ManagedCheckpoint `json:"managed_checkpoints,omitempty"`
	SafeSaveCompleted  bool                      `json:"safe_save_completed,omitempty"`
	SafeSaveBackup     bool                      `json:"safe_save_backup,omitempty"`
	SafeSaveRestore    bool                      `json:"safe_save_restore,omitempty"`
	LocalReceipt       bool                      `json:"local_receipt,omitempty"`
	ResultLocalOnly    map[model.EntryID]bool    `json:"result_local_only,omitempty"`
	PromoteLocalOnly   map[model.EntryID]bool    `json:"promote_local_only,omitempty"`
	WaitFor            string                    `json:"wait_for,omitempty"`
	Ordinal            uint64                    `json:"ordinal,omitempty"`
	TemporaryName      string                    `json:"temporary_name,omitempty"`
	CloudStep          string                    `json:"cloud_step,omitempty"`
	Dispatched         bool                      `json:"dispatched,omitempty"`
	CompletedSteps     map[string]bool           `json:"completed_steps,omitempty"`
	Op                 model.NamespaceOp         `json:"op"`
	Before             []model.Entry             `json:"before"`
	Sealed             map[model.EntryID]uint64  `json:"sealed,omitempty"`
	LocalCancel        bool                      `json:"local_cancel,omitempty"`
	ActionPending      bool                      `json:"action_pending,omitempty"`
	ResultKind         model.Kind                `json:"result_kind,omitempty"`
}

func under(p, root string) bool { return p == root || (root != "" && strings.HasPrefix(p, root+"/")) }
func overlaps(a, b string) bool { return under(a, b) || under(b, a) }
func namespaceReserved(tx *bolt.Tx, p string) bool {
	yes := false
	_ = eachIndexedNamespaceTx(tx, namespaceActiveIndex, func(r namespaceRecord) error {
		if r.Op.State == "prepared" || r.Op.State == "sealed" || r.Op.State == "committed" {
			if (r.Op.Src != "" && under(p, r.Op.Src)) || (r.Op.Dst != "" && under(p, r.Op.Dst)) {
				yes = true
			}
		}
		return nil
	})
	return yes
}
func (s *Store) PrepareNamespace(ctx context.Context, op model.NamespaceOp) (model.NamespaceOp, error) {
	return s.prepareNamespace(ctx, op, false)
}

// PrepareEditorBackup never queues a cloud rename. Existing upload journals are
// retained while the editor temporarily parks the stable document identity.
func (s *Store) PrepareEditorBackup(ctx context.Context, src, dst string) (model.NamespaceOp, error) {
	if !model.EditorTildeBackup(src, dst) {
		return model.NamespaceOp{}, ErrInvalidState
	}
	return s.prepareNamespace(ctx, model.NamespaceOp{Kind: "rename", Src: src, Dst: dst}, true)
}
func (s *Store) prepareNamespace(ctx context.Context, op model.NamespaceOp, editorBackup bool) (out model.NamespaceOp, err error) {
	err = s.update(ctx, func(tx *bolt.Tx) error {
		var er error
		out, er = s.prepareNamespaceTx(tx, op, editorBackup)
		return er
	})
	return
}
func (s *Store) prepareNamespaceTx(tx *bolt.Tx, op model.NamespaceOp, editorBackup bool) (out model.NamespaceOp, err error) {
	if op.Kind != "rename" && op.Kind != "replace" && op.Kind != "delete" && op.Kind != "mkdir" {
		return out, errors.New("unsupported namespace operation")
	}
	if op.Kind != "mkdir" {
		if err = validRemote(op.Src, false); err != nil {
			return
		}
	}
	if op.Kind != "delete" {
		if err = validRemote(op.Dst, false); err != nil {
			return
		}
	}
	if op.Src != "" && op.Dst != "" && (under(op.Dst, op.Src) || under(op.Src, op.Dst)) {
		return out, errors.New("overlapping namespace paths")
	}
	err = func() error {
		if op.ID != "" {
			var old namespaceRecord
			if er := get(tx, "namespace", op.ID, &old); er == nil {
				if old.Op.Kind != op.Kind || old.Op.Src != op.Src || old.Op.Dst != op.Dst {
					return ErrInvalidState
				}
				out = old.Op
				return nil
			} else if !errors.Is(er, ErrNotFound) {
				return er
			}
		} else {
			op.ID = uuid.NewString()
		}
		if namespaceReserved(tx, op.Src) || namespaceReserved(tx, op.Dst) {
			return ErrFrozen
		}
		restoreParked := false
		softRestore := ""
		var src, dst model.Entry
		var er error
		if op.Kind != "mkdir" {
			src, er = byPath(tx, op.Src)
			if er != nil {
				return er
			}
			if editorBackup {
				if err := completeManagedEditorCycleTx(tx, src); err != nil {
					return err
				}
				safe, err := editorBackupSource(tx, src, "")
				if err != nil {
					return err
				}
				if !safe {
					return ErrBusy
				}
			}
			op.SrcID = src.ID
		}
		if op.Kind != "delete" {
			dst, er = byPath(tx, op.Dst)
			if errors.Is(er, ErrNotFound) && op.Kind == "rename" && src.Token == "" && src.Kind != model.KindFolder {
				var candidate model.Entry
				candidate, softRestore, er = pendingFuseReplacementTx(tx, op.Dst, src)
				if er != nil {
					return er
				}
				if softRestore != "" {
					dst = candidate
					op.Kind = "replace"
				} else {
					er = ErrNotFound
				}
			}
			if errors.Is(er, ErrNotFound) && op.Kind == "rename" && src.Token == "" && src.Kind != model.KindFolder {
				var found bool
				dst, found, er = parkedSafeSave(tx, op.Dst)
				if er != nil {
					return er
				}
				if found {
					restoreParked = true
					op.Kind = "replace"
				} else {
					er = ErrNotFound
				}
			}
			if er == nil {
				op.DstExists = true
				op.DstID = dst.ID
			} else if !errors.Is(er, ErrNotFound) {
				return er
			}
			if editorBackup && op.DstExists {
				var metadata Metadata
				if err := get(tx, "metadata", string(dst.ID), &metadata); err != nil {
					return err
				}
				if dst.Token != "" || metadata.SafeSaveOriginal != src.ID {
					return ErrBusy
				}
			}
			if op.DstExists && (op.Kind != "replace") && !editorBackup {
				return errors.New("namespace destination exists")
			}
			if op.Kind == "replace" && !op.DstExists {
				return ErrNotFound
			}
			if op.Kind == "replace" && (src.Kind == model.KindFolder || dst.Kind == model.KindFolder || (src.Kind != dst.Kind && (src.Token != "" || src.RemoteCheckpoint != ""))) {
				return model.ErrUnsupportedContent
			}
			parent := path.Dir(op.Dst)
			if parent != "." {
				p, er := byPath(tx, parent)
				if er != nil {
					return er
				}
				if p.Kind != model.KindFolder {
					return model.ErrUnsupportedContent
				}
			}
		}
		sequence, er := tx.Bucket([]byte("namespace")).NextSequence()
		if er != nil {
			return er
		}
		r := namespaceRecord{Op: op, Ordinal: sequence, SafeSaveRestore: restoreParked, SafeSaveBackup: editorBackup, FuseSoftRestore: softRestore}
		r.FuseSoftRemove = op.Kind == "rename" && src.Token != "" && src.Kind != model.KindFolder && fuseSoftRemoveRename(op.Src, op.Dst)
		if op.Kind == "mkdir" {
			r.TemporaryName = "DriveBridge-pending-" + op.ID
		}
		affected, er := namespaceEntriesTx(tx, src, dst)
		if er != nil {
			return er
		}
		for _, e := range affected {
			var m Metadata
			if er := get(tx, "metadata", string(e.ID), &m); er != nil {
				return er
			}
			if m.Tombstone && !(restoreParked && e.ID == dst.ID) {
				continue
			}
			if e.FrozenBy != "" {
				return ErrFrozen
			}
			r.Before = append(r.Before, e)
			e.FrozenBy = op.ID
			if er = saveEntry(tx, e); er != nil {
				return er
			}
		}
		r.Op.State = "prepared"
		out = r.Op
		return put(tx, "namespace", op.ID, r)
	}()
	return
}
func (s *Store) SealNamespace(ctx context.Context, id string) (out model.NamespaceOp, err error) {
	err = s.update(ctx, func(tx *bolt.Tx) error {
		var er error
		out, er = sealNamespaceTx(tx, id)
		return er
	})
	return
}
func sealNamespaceTx(tx *bolt.Tx, id string) (out model.NamespaceOp, err error) {
	err = func() error {
		var r namespaceRecord
		if er := get(tx, "namespace", id, &r); er != nil {
			return er
		}
		if r.Op.State == "sealed" || r.Op.State == "committed" || r.Op.State == "finished" {
			out = r.Op
			return nil
		}
		if r.Op.State != "prepared" {
			return ErrInvalidState
		}
		r.Sealed = make(map[model.EntryID]uint64)
		for _, old := range r.Before {
			e, er := entryTx(tx, old.ID)
			if er != nil {
				return er
			}
			if e.FrozenBy != id {
				return ErrFrozen
			}
			r.Sealed[e.ID] = e.Generation
			if e.ID == r.Op.SrcID {
				r.Op.ExpectedSrc = e.Generation
				r.Op.Input = e.Local
			}
			if e.ID == r.Op.DstID {
				r.Op.ExpectedDst = e.Generation
				r.Op.Previous = e.Local
			}
		}
		r.Op.State = "sealed"
		out = r.Op
		return put(tx, "namespace", id, r)
	}()
	return
}
func tombstone(tx *bolt.Tx, e model.Entry) error {
	var m Metadata
	if er := get(tx, "metadata", string(e.ID), &m); er != nil {
		return er
	}
	m.Tombstone = true
	if er := put(tx, "metadata", string(e.ID), m); er != nil {
		return er
	}
	if er := tx.Bucket([]byte("paths")).Delete([]byte(e.Remote)); er != nil {
		return er
	}
	return saveEntry(tx, e)
}
func supersedeQueued(tx *bolt.Tx, id model.EntryID) error {
	return each[jobRecord](tx, "jobs", func(j jobRecord) error {
		if j.Job.EntryID == id && !active(j.Job.State) && pending(j.Job.State) {
			j.Superseded = true
			if !j.LocalReceipt {
				j.Job.State = model.StatusBlocked
			}
			return saveJob(tx, j)
		}
		return nil
	})
}
func supersedeNamespaceQueuedTx(tx *bolt.Tx, id model.EntryID) error {
	creates := map[model.JobID]bool{}
	if err := each[model.CreateOp](tx, "createops", func(op model.CreateOp) error { creates[op.JobID] = true; return nil }); err != nil {
		return err
	}
	return each[jobRecord](tx, "jobs", func(r jobRecord) error {
		if r.Job.EntryID != id || r.Superseded || active(r.Job.State) || !pending(r.Job.State) || !deletionJobUndispatched(r) || creates[r.Job.ID] {
			return nil
		}
		r.Superseded = true
		return saveJob(tx, r)
	})
}

func (s *Store) CommitNamespace(ctx context.Context, id string) (out model.Entry, err error) {
	err = s.update(ctx, func(tx *bolt.Tx) error {
		var er error
		out, er = s.commitNamespaceTx(tx, id)
		return er
	})
	return
}
func (s *Store) commitNamespaceTx(tx *bolt.Tx, id string) (out model.Entry, err error) {
	err = func() error {
		var r namespaceRecord
		if er := get(tx, "namespace", id, &r); er != nil {
			return er
		}
		if r.Op.State == "committed" || r.Op.State == "finished" {
			if r.Op.ResultID == "" {
				return nil
			}
			var er error
			out, er = entryTx(tx, r.Op.ResultID)
			return er
		}
		if r.Op.State != "sealed" {
			return ErrInvalidState
		}
		for key, generation := range r.Sealed {
			e, er := entryTx(tx, key)
			if er != nil {
				return er
			}
			if e.Generation != generation {
				return model.ErrGenerationChanged
			}
			if e.FrozenBy != id {
				return ErrFrozen
			}
		}
		if er := put(tx, "recovery", "namespace:"+id, recoveryRecord{NamespaceID: id, Entries: r.Before}); er != nil {
			return er
		}
		if r.SafeSaveBackup {
			shadow, err := commitSafeSaveBackup(tx, r)
			if err != nil {
				return err
			}
			if err := routeManagedNamespaceTx(tx, r, shadow); err != nil {
				return err
			}
			out = shadow
			r.Op.ResultID = shadow.ID
			r.Op.State = "committed"
			return put(tx, "namespace", id, r)
		}
		switch r.Op.Kind {
		case "replace":
			src, er := entryTx(tx, r.Op.SrcID)
			if er != nil {
				return er
			}
			dst, er := entryTx(tx, r.Op.DstID)
			if er != nil {
				return er
			}
			if r.FuseSoftRestore != "" {
				dst, er = restoreFuseReplacementTx(tx, r, dst)
				if er != nil {
					return er
				}
			}
			pendingInput, er := pendingManagedInputTx(tx, r, src.ID)
			if er != nil {
				return er
			}
			if src.Local == "" && !pendingInput {
				return errors.New("replacement source is not materialized")
			}

			if er = supersedeNamespaceQueuedTx(tx, src.ID); er != nil {
				return er
			}
			if er = supersedeNamespaceQueuedTx(tx, dst.ID); er != nil {
				return er
			}
			if er = tombstone(tx, src); er != nil {
				return er
			}
			dst.Generation++
			if !pendingInput {
				dst.Local = r.Op.Input
				dst.Visible = r.Op.Input
				dst.Status = model.StatusQueued
				j := model.Job{ID: model.JobID(uuid.NewString()), EntryID: dst.ID, Generation: dst.Generation, Local: dst.Local, BaseLocal: dst.LocalCheckpoint, BaseRemote: dst.RemoteCheckpoint, State: model.StatusQueued}
				if er = saveJob(tx, jobRecord{Job: j, StagedAt: time.Now().UTC()}); er != nil {
					return er
				}
				if r.ResultLocalOnly[dst.ID] {
					if safe, err := eligibleLocalReceipt(tx, dst, r.Op.ID); err != nil {
						return err
					} else if safe {
						if err := recordLocalReceipt(tx, j.ID); err != nil {
							return err
						}
					}
				}
			} else {
				dst.Status = model.StatusQueued
			}
			if r.SafeSaveRestore {
				if err := restoreSafeSaveMapping(tx, dst); err != nil {
					return err
				}
			}
			if er = saveEntry(tx, dst); er != nil {
				return er
			}
			if er = completeSafeSave(tx, dst); er != nil {
				return er
			}
			out = dst
			r.Op.ResultID = dst.ID
		case "rename":
			for _, old := range r.Before {
				e, er := entryTx(tx, old.ID)
				if er != nil {
					return er
				}
				if !under(e.Remote, r.Op.Src) {
					continue
				}
				if er = tx.Bucket([]byte("paths")).Delete([]byte(e.Remote)); er != nil {
					return er
				}
				destination := r.Op.Dst + strings.TrimPrefix(e.Remote, r.Op.Src)
				undoSoftRemove, er := releaseMovedFuseSoftRemovalTx(tx, e, destination)
				if er != nil {
					return er
				}
				if undoSoftRemove && e.ID == r.Op.SrcID {
					r.LocalReceipt = true
				}
				e.Remote = destination
				if e.ID == r.Op.SrcID {
					if !r.FuseSoftRemove && !undoSoftRemove {
						e.CloudName = path.Base(r.Op.Dst)
					}
					if r.ResultKind != "" {

						e.Kind = r.ResultKind
					}
				}
				if localOnly, configured := r.ResultLocalOnly[e.ID]; configured && e.Kind != model.KindFolder {
					if localOnly {
						if safe, err := eligibleLocalReceipt(tx, e, r.Op.ID); err != nil {
							return err
						} else if safe {
							if err := receiptLatestLocalJob(tx, e); err != nil {
								return err
							}
						}
					} else {
						var metadata Metadata
						if err := get(tx, "metadata", string(e.ID), &metadata); err != nil {
							return err
						}
						if metadata.SafeSaveOriginal != "" {
							metadata.SafeSaveOriginal = ""
							if err := put(tx, "metadata", string(e.ID), metadata); err != nil {
								return err
							}
						}
						if err := promoteLocalReceipt(tx, &e); err != nil {
							return err
						}
					}
				}
				if er = tx.Bucket([]byte("paths")).Put([]byte(e.Remote), []byte(e.ID)); er != nil {
					return er
				}
				if r.FuseSoftRemove && e.ID == r.Op.SrcID {
					if er = markFuseSoftRemoveTx(tx, r, e); er != nil {
						return er
					}
				}
				if er = saveEntry(tx, e); er != nil {
					return er
				}
				if e.ID == r.Op.SrcID {
					out = e
					r.Op.ResultID = e.ID
				}
			}
			if err := queuePromotedFolders(tx, r); err != nil {
				return err
			}
		case "delete":
			for _, old := range r.Before {
				e, er := entryTx(tx, old.ID)
				if er != nil {
					return er
				}
				if er = supersedeNamespaceQueuedTx(tx, e.ID); er != nil {
					return er
				}
				if er = resolveEntryConflicts(tx, e.ID); er != nil {
					return er
				}
				var metadata Metadata
				if er = get(tx, "metadata", string(e.ID), &metadata); er != nil {
					return er
				}
				if er = clearFuseSoftRemoveIndexTx(tx, metadata.FuseSoftRemove); er != nil {
					return er
				}
				if er = tombstone(tx, e); er != nil {
					return er
				}
				if e.ID == r.Op.SrcID {
					out = e
					r.Op.ResultID = e.ID
				}
			}
		case "mkdir":
			if _, er := byPath(tx, r.Op.Dst); er == nil {
				return errors.New("mkdir path occupied")
			} else if !errors.Is(er, ErrNotFound) {
				return er
			}
			out = model.Entry{ID: model.EntryID(uuid.NewString()), MountID: s.scope.MountID, ProfileID: s.scope.ProfileID, Remote: r.Op.Dst, CloudName: path.Base(r.Op.Dst), Kind: model.KindFolder, Generation: 1, Status: model.StatusQueued, FrozenBy: id}
			now := time.Now().UTC()
			if er := put(tx, "metadata", string(out.ID), Metadata{FirstObserved: now, ModTime: now}); er != nil {
				return er
			}
			if er := saveEntry(tx, out); er != nil {
				return er
			}
			if er := tx.Bucket([]byte("paths")).Put([]byte(out.Remote), []byte(out.ID)); er != nil {
				return er
			}
			r.Op.ResultID = out.ID
		}
		// A nonmatching successful occupant ends an old soft removal's claim on
		// this name; a later unrelated local file must receive its own identity.
		if r.Op.Kind == "rename" || r.Op.Kind == "mkdir" {
			if bucket := tx.Bucket([]byte(fuseSoftRemovesBucket)); bucket != nil {
				if er := bucket.Delete([]byte(r.Op.Dst)); er != nil {
					return er
				}
			}
		}
		if er := s.blockOversizeNamespaceDocumentTx(tx, &out); er != nil {
			return er
		}
		if er := routeManagedNamespaceTx(tx, r, out); er != nil {
			return er
		}
		r.Op.State = "committed"
		return put(tx, "namespace", id, r)
	}()
	return
}
func (s *Store) FinishNamespace(ctx context.Context, id string) error {
	return s.update(ctx, func(tx *bolt.Tx) error {
		var r namespaceRecord
		if er := get(tx, "namespace", id, &r); er != nil {
			return er
		}
		if r.Op.State == "finished" {
			return nil
		}
		if r.Op.State != "committed" {
			return ErrInvalidState
		}
		ids := map[model.EntryID]bool{}
		for _, old := range r.Before {
			ids[old.ID] = true
		}
		if r.Op.ResultID != "" {
			ids[r.Op.ResultID] = true
		}
		for id := range ids {
			e, err := entryTx(tx, id)
			if err != nil {
				return err
			}
			if e.FrozenBy == r.Op.ID {
				e.FrozenBy = ""
				if err := saveEntry(tx, e); err != nil {
					return err
				}
			}
		}
		r.Op.State = "finished"
		r.ActionPending = !r.LocalReceipt && !r.FuseSoftRemove
		return put(tx, "namespace", id, r)
	})
}
func (s *Store) AbortNamespace(ctx context.Context, id string) error {
	return s.update(ctx, func(tx *bolt.Tx) error {
		var r namespaceRecord
		if er := get(tx, "namespace", id, &r); er != nil {
			return er
		}
		if r.Op.State == "aborted" {
			return nil
		}
		if r.Op.State != "prepared" && r.Op.State != "sealed" {
			return ErrInvalidState
		}
		for _, old := range r.Before {
			e, er := entryTx(tx, old.ID)
			if er != nil {
				return er
			}
			if e.FrozenBy == id {
				e.FrozenBy = ""
				if er = saveEntry(tx, e); er != nil {
					return er
				}
			}
		}
		r.Op.State = "aborted"
		return put(tx, "namespace", id, r)
	})
}
func (s *Store) PendingNamespace(ctx context.Context) (out []model.NamespaceOp, err error) {
	err = s.view(ctx, func(tx *bolt.Tx) error {
		return eachIndexedNamespaceTx(tx, namespaceActiveIndex, func(r namespaceRecord) error {
			if r.Op.State != "finished" && r.Op.State != "aborted" {
				out = append(out, r.Op)
			}
			return nil
		})
	})
	return
}

// NamespaceActions contains committed local intents eligible for later engine dispatch after Finish.
func (s *Store) NamespaceActions(ctx context.Context) (out []model.NamespaceOp, err error) {
	var records []namespaceRecord
	err = s.view(ctx, func(tx *bolt.Tx) error {
		return eachIndexedNamespaceTx(tx, namespaceActionIndex, func(r namespaceRecord) error {
			if r.ActionPending {
				blocked, err := managedNamespaceBlockedTx(tx, r)
				if err != nil {
					return err
				}
				if blocked {
					return nil
				}
				if r.WaitFor != "" {
					var parent namespaceRecord
					if err := get(tx, "namespace", r.WaitFor, &parent); err != nil {
						return err
					}
					if parent.Op.State != "finished" {
						return nil
					}
				}
				records = append(records, r)
			}
			return nil
		})
	})
	sort.SliceStable(records, func(i, j int) bool { return records[i].Ordinal < records[j].Ordinal })
	for _, r := range records {
		out = append(out, r.Op)
	}
	return
}
func (s *Store) AcknowledgeNamespaceAction(ctx context.Context, id string) error {
	return s.update(ctx, func(tx *bolt.Tx) error {
		var r namespaceRecord
		if er := get(tx, "namespace", id, &r); er != nil {
			return er
		}
		if r.Op.State != "finished" {
			return ErrInvalidState
		}
		if r.Dispatched {
			return model.ErrUnknownResult
		}
		if !r.ActionPending {
			return nil
		}
		blocked, err := managedNamespaceBlockedTx(tx, r)
		if err != nil {
			return err
		}
		if blocked {
			return ErrBusy
		}
		if r.Op.Kind == "mkdir" {
			en, err := entryTx(tx, r.Op.ResultID)
			if err != nil {
				return err
			}
			if en.Token == "" {
				return ErrInvalidState
			}
			en.Status = model.StatusSynced
			if err = saveEntry(tx, en); err != nil {
				return err
			}
		}
		r.ActionPending = false
		return put(tx, "namespace", id, r)
	})
}
