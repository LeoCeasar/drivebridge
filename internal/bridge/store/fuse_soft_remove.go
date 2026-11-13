package store

import (
	"path"
	"regexp"
	"time"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/model"
	bolt "go.etcd.io/bbolt"
)

const fuseSoftRemovesBucket = "fuse_soft_removes"

var fuseHiddenName = regexp.MustCompile(`^\.fuse_hidden[0-9a-fA-F]{16}$`)

// libfuse implements replacement of an open file by first moving the old
// inode to this reserved sibling name. The move alone can also be a real
// unlink, so it never grants permission to resurrect an unrelated tombstone.
func fuseSoftRemoveRename(src, dst string) bool {
	return path.Dir(src) == path.Dir(dst) && !fuseHiddenName.MatchString(path.Base(src)) && fuseHiddenName.MatchString(path.Base(dst))
}

func pendingFuseReplacementTx(tx *bolt.Tx, remote string, source model.Entry) (out model.Entry, operation string, err error) {
	bucket := tx.Bucket([]byte(fuseSoftRemovesBucket))
	if bucket == nil || source.Token != "" || source.RemoteCheckpoint != "" || source.Kind == model.KindFolder {
		return
	}
	id := bucket.Get([]byte(remote))
	if id == nil {
		return
	}
	var record namespaceRecord
	if err = get(tx, "namespace", string(id), &record); err != nil {
		return
	}
	if !record.FuseSoftRemove || record.Op.State != "finished" || record.Dispatched || record.Op.Src != remote {
		err = ErrIntegrity
		return
	}
	out, err = entryTx(tx, record.Op.SrcID)
	if err != nil {
		return
	}
	var metadata Metadata
	if err = get(tx, "metadata", string(out.ID), &metadata); err != nil {
		return
	}
	if metadata.Tombstone || metadata.FuseSoftRemove != record.Op.ID || out.Remote != record.Op.Dst || out.Token == "" {
		err = ErrIntegrity
		return
	}
	operation = record.Op.ID
	return
}

func markFuseSoftRemoveTx(tx *bolt.Tx, r namespaceRecord, e model.Entry) error {
	bucket, err := tx.CreateBucketIfNotExists([]byte(fuseSoftRemovesBucket))
	if err != nil {
		return err
	}
	if old := bucket.Get([]byte(r.Op.Src)); old != nil && string(old) != r.Op.ID {
		return ErrBusy
	}
	var metadata Metadata
	if err = get(tx, "metadata", string(e.ID), &metadata); err != nil {
		return err
	}
	metadata.FuseSoftRemove = r.Op.ID
	if err = put(tx, "metadata", string(e.ID), metadata); err != nil {
		return err
	}
	return bucket.Put([]byte(r.Op.Src), []byte(r.Op.ID))
}

func clearFuseSoftRemoveIndexTx(tx *bolt.Tx, operation string) error {
	if operation == "" {
		return nil
	}
	var record namespaceRecord
	if err := get(tx, "namespace", operation, &record); err != nil {
		return err
	}
	if bucket := tx.Bucket([]byte(fuseSoftRemovesBucket)); bucket != nil && string(bucket.Get([]byte(record.Op.Src))) == operation {
		return bucket.Delete([]byte(record.Op.Src))
	}
	return nil
}

// Reconcile only a live, recorded soft removal with a rename into its exact
// vacated name. Creating a file after a completed delete does not take this path.
// The hidden inode becomes a tokenless shadow before the stable identity is
// restored; libfuse's later unlink can therefore delete only the old generation.
func restoreFuseReplacementTx(tx *bolt.Tx, r namespaceRecord, destination model.Entry) (model.Entry, error) {
	var removal namespaceRecord
	if err := get(tx, "namespace", r.FuseSoftRestore, &removal); err != nil {
		return destination, err
	}
	if !removal.FuseSoftRemove || removal.Op.State != "finished" || removal.Op.Src != r.Op.Dst || removal.Op.SrcID != destination.ID || removal.Op.Dst != destination.Remote || removal.Dispatched {
		return destination, ErrInvalidState
	}
	var metadata Metadata
	if err := get(tx, "metadata", string(destination.ID), &metadata); err != nil {
		return destination, err
	}
	if metadata.FuseSoftRemove != removal.Op.ID || metadata.Tombstone {
		return destination, ErrInvalidState
	}
	shadow := destination
	shadow.ID = model.EntryID(uuid.NewString())
	shadow.Token = ""
	shadow.LocalCheckpoint = ""
	shadow.RemoteCheckpoint = ""
	shadow.Desired = ""
	shadow.Generation = 1
	shadow.FrozenBy = ""
	shadow.Status = model.StatusQueued
	shadow.CloudName = path.Base(shadow.Remote)
	now := time.Now().UTC()
	if err := put(tx, "metadata", string(shadow.ID), Metadata{FirstObserved: now, ModTime: metadata.ModTime, SafeSaveOriginal: destination.ID}); err != nil {
		return destination, err
	}
	if err := saveEntry(tx, shadow); err != nil {
		return destination, err
	}
	if err := tx.Bucket([]byte("paths")).Put([]byte(shadow.Remote), []byte(shadow.ID)); err != nil {
		return destination, err
	}
	// A background import already reading the old inode must remain history-only,
	// including when its hidden name is unlinked before the archive is released.
	refs, err := managedEntriesTx(tx)
	if err != nil {
		return destination, err
	}
	archive := destination
	archive.Remote = r.Op.Dst
	for key, id := range refs {
		if id != destination.ID {
			continue
		}
		route, err := managedRouteTx(tx, key)
		if err != nil {
			return destination, err
		}
		route.Entry = shadow
		route.ArchiveEntry = &archive
		route.HistoryOnly = true
		refs[key] = shadow.ID
		if err = saveManagedRouteTx(tx, key, route); err != nil {
			return destination, err
		}
	}
	if err = put(tx, "meta", "managed_entries", refs); err != nil {
		return destination, err
	}
	metadata.FuseSoftRemove = ""
	if err = put(tx, "metadata", string(destination.ID), metadata); err != nil {
		return destination, err
	}
	destination.Remote = r.Op.Dst
	if err = tx.Bucket([]byte("paths")).Put([]byte(destination.Remote), []byte(destination.ID)); err != nil {
		return destination, err
	}
	if err = clearFuseSoftRemoveIndexTx(tx, removal.Op.ID); err != nil {
		return destination, err
	}
	return destination, nil
}

func fuseSoftRemovalAtTx(tx *bolt.Tx, remote string) bool {
	b := tx.Bucket([]byte(fuseSoftRemovesBucket))
	return b != nil && b.Get([]byte(remote)) != nil
}

func logicalFuseHistoryEntryTx(tx *bolt.Tx, e model.Entry, metadata Metadata) (model.Entry, error) {
	var record namespaceRecord
	if err := get(tx, "namespace", metadata.FuseSoftRemove, &record); err != nil {
		return e, err
	}
	if !record.FuseSoftRemove || record.Op.SrcID != e.ID {
		return e, ErrIntegrity
	}
	e.Remote = record.Op.Src
	for _, before := range record.Before {
		if before.ID == e.ID {
			e.CloudName = before.CloudName
			break
		}
	}
	return e, nil
}

// A failed libfuse replacement may undo its temporary move. Any other explicit
// move of the hidden inode also ends the exact-vacated-path reconciliation.
func releaseMovedFuseSoftRemovalTx(tx *bolt.Tx, e model.Entry, destination string) (bool, error) {
	var metadata Metadata
	if err := get(tx, "metadata", string(e.ID), &metadata); err != nil {
		return false, err
	}
	if metadata.FuseSoftRemove == "" {
		return false, nil
	}
	var removal namespaceRecord
	if err := get(tx, "namespace", metadata.FuseSoftRemove, &removal); err != nil {
		return false, err
	}
	if !removal.FuseSoftRemove || removal.Op.SrcID != e.ID {
		return false, ErrIntegrity
	}
	if err := clearFuseSoftRemoveIndexTx(tx, removal.Op.ID); err != nil {
		return false, err
	}
	metadata.FuseSoftRemove = ""
	if err := put(tx, "metadata", string(e.ID), metadata); err != nil {
		return false, err
	}
	return destination == removal.Op.Src, nil
}
