package store

import (
	"context"

	"github.com/rclone/rclone/internal/bridge/model"
	bolt "go.etcd.io/bbolt"
)

// NamespaceDeleteParent is the immutable provider binding at local deletion.
type NamespaceDeleteParent struct {
	ID    model.EntryID `json:"id,omitempty"`
	Token string        `json:"token"`
}

// namespaceDeleteParentTx upgrades legacy journals only from the durable deletion
// event tied to this exact operation, never from today's occupant of its path.
func (s *Store) namespaceDeleteParentTx(tx *bolt.Tx, r *namespaceRecord) error {
	if r.DeleteParent != nil {
		return nil
	}
	parentRemote := entryParent(model.Entry{Remote: r.Op.Src})
	var binding *NamespaceDeleteParent
	parents := map[model.EntryID]model.HistoryEvent{}
	found := false
	err := each[model.HistoryEvent](tx, "history_events", func(ev model.HistoryEvent) error {
		if found {
			return nil
		}
		if ev.Entry.Kind == model.KindFolder {
			parents[ev.Entry.ID] = ev
		}
		if ev.Deleted && ev.Entry.ID == r.Op.SrcID && ev.Entry.FrozenBy == r.Op.ID {
			original := false
			for _, before := range r.Before {
				if before.ID == ev.Entry.ID && before.Token == ev.Entry.Token && before.Kind == ev.Entry.Kind && before.Remote == ev.Entry.Remote {
					original = true
				}
			}
			if !original {
				return model.ErrIdentityChanged
			}
			found = true
			if parentRemote == "" {
				binding = &NamespaceDeleteParent{Token: s.scope.FolderToken}
				return nil
			}
			for _, p := range parents {
				if !p.Deleted && p.Entry.Remote == parentRemote && p.Entry.Token != "" {
					if binding != nil {
						return model.ErrIdentityChanged
					}
					binding = &NamespaceDeleteParent{ID: p.Entry.ID, Token: p.Entry.Token}
				}
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !found || binding == nil || binding.Token == "" {
		return model.ErrUnknownResult
	}
	r.DeleteParent = binding
	return put(tx, "namespace", r.Op.ID, *r)
}

// NamespaceDeleteProof captures the original parent by ID, including tombstones.
func (s *Store) NamespaceDeleteProof(ctx context.Context, id string) (proof DeleteAbsenceProof, err error) {
	err = s.update(ctx, func(tx *bolt.Tx) error {
		var r namespaceRecord
		if er := get(tx, "namespace", id, &r); er != nil {
			return er
		}
		if r.Op.Kind != "delete" || r.Op.State != "finished" {
			return ErrInvalidState
		}
		if er := s.namespaceDeleteParentTx(tx, &r); er != nil {
			return er
		}
		proof.NamespaceID = id
		var er error
		proof.Entry, er = entryTx(tx, r.Op.SrcID)
		if er != nil {
			return er
		}
		proof.ParentToken = r.DeleteParent.Token
		if proof.ParentToken == "" {
			return ErrBusy
		}
		if r.DeleteParent.ID != "" {
			proof.Parent, er = entryTx(tx, r.DeleteParent.ID)
			if er != nil {
				return er
			}
			if proof.Parent.Kind != model.KindFolder || proof.Parent.Token != proof.ParentToken {
				return model.ErrIdentityChanged
			}
		} else if proof.ParentToken != s.scope.FolderToken {
			return model.ErrIdentityChanged
		}
		return nil
	})
	return
}

// DeleteAbsenceProof is captured before a successful complete provider listing.
// Only the sync engine constructs it; it is never accepted from UI/CLI input.
type DeleteAbsenceProof struct {
	NamespaceID string
	Entry       model.Entry
	Parent      model.Entry
	ParentToken string
}

// ConfirmNamespaceDeleteAbsence fulfills removal from the original synchronized
// directory, not proof of global/permanent deletion. No cloud mutation is replayed.
// Retain the journal and recovery snapshots while atomically retiring its action.
func (s *Store) ConfirmNamespaceDeleteAbsence(ctx context.Context, proof DeleteAbsenceProof) error {
	return s.update(ctx, func(tx *bolt.Tx) error {
		var r namespaceRecord
		if err := get(tx, "namespace", proof.NamespaceID, &r); err != nil {
			return err
		}
		if r.Op.Kind != "delete" || r.Op.State != "finished" || r.Op.SrcID != proof.Entry.ID || r.CloudStep != "" && r.CloudStep != "delete" {
			return ErrInvalidState
		}
		current, err := entryTx(tx, proof.Entry.ID)
		if err != nil {
			return err
		}
		if current != proof.Entry || current.Token == "" {
			return model.ErrIdentityChanged
		}
		var metadata Metadata
		if err = get(tx, "metadata", string(current.ID), &metadata); err != nil {
			return err
		}
		if !metadata.Tombstone {
			return model.ErrIdentityChanged
		}
		bound := false
		for _, before := range r.Before {
			if before.ID == current.ID && before.Token == current.Token && before.Kind == current.Kind && before.Remote == current.Remote && r.Op.Src == before.Remote {
				bound = true
				break
			}
		}
		if !bound {
			return model.ErrIdentityChanged
		}
		if err := s.namespaceDeleteParentTx(tx, &r); err != nil {
			return err
		}
		if r.DeleteParent.Token == "" || r.DeleteParent.Token != proof.ParentToken || r.DeleteParent.ID != proof.Parent.ID {
			return model.ErrIdentityChanged
		}
		if r.DeleteParent.ID == "" {
			if proof.ParentToken != s.scope.FolderToken {
				return model.ErrIdentityChanged
			}
		} else {
			parent, err := entryTx(tx, r.DeleteParent.ID)
			if err != nil {
				return err
			}
			if parent != proof.Parent || parent.Kind != model.KindFolder || parent.Token != proof.ParentToken {
				return model.ErrIdentityChanged
			}
		}
		r.Dispatched = false
		r.ActionPending = false
		if r.CompletedSteps == nil {
			r.CompletedSteps = map[string]bool{}
		}
		r.CompletedSteps["delete"] = true
		return put(tx, "namespace", r.Op.ID, r)
	})
}
