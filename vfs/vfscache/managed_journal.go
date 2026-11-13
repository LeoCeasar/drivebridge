package vfscache

// The cache data file is an immutable base while a journal is attached. Local
// writes append only the changed bytes; a checkpoint never scans that base.

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"os"
	"path/filepath"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/platform"
	"github.com/rclone/rclone/lib/ranges"
)

type managedSnapshotBackend interface {
	ManagedSnapshotBase(context.Context, string) (string, error)
	OpenManagedSnapshot(context.Context, string) (io.ReadCloser, error)
}

type managedSnapshotRetention interface {
	RetainManagedSnapshot(context.Context, string, string, string) error
	ReleaseManagedSnapshot(context.Context, string) error
	ReconcileManagedSnapshots(context.Context, map[string]bool) error
}

const (
	journalMagic            = 0x44424a31
	journalHeader           = 40
	journalBaseRecord       = 1
	journalWriteRecord      = 2
	journalTruncateRecord   = 3
	journalCheckpointRecord = 4
	journalAckRecord        = 5
	journalRetireRecord     = 6
	maxJournalPayload       = 64 << 20
)

type journalBase struct {
	Schema         int
	Name, Snapshot string
	Info           Info
}
type journalCheckpoint struct {
	info Info
	view []journalExtent
}
type managedJournal struct {
	file        *os.File
	base        journalBase
	baseLease   io.ReadCloser
	view        []journalExtent
	checkpoints []journalCheckpoint
	err         error
	retired     bool
	scanEnd     int64
	uncommitted bool
}

func journalRoot(meta string) string { return meta + ".journal" }
func journalPath(meta, id string) (string, error) {
	u, err := uuid.Parse(id)
	if err != nil || u == uuid.Nil || u.String() != id {
		return "", errors.New("invalid local-save journal identity")
	}
	p := filepath.Join(journalRoot(meta), id)
	return p, platform.CheckNoSymlinks(p)
}

func (item *Item) localJournalBackend() (managedSnapshotBackend, bool) {
	b, ok := item.c.fremote.(managedSnapshotBackend)
	return b, ok && item.c.managed.Load()
}

// Called with both snapshotMu and mu held. The first write publishes the
// journal identity durably; subsequent writes append without an fsync until save.
func (item *Item) startManagedJournal() error {
	if item.journal != nil {
		return item.journal.err
	}
	b, ok := item.localJournalBackend()
	if !ok {
		return errors.New("backend has no immutable local snapshots")
	}
	baseID, err := b.ManagedSnapshotBase(item.c.ctx, item.name)
	if err != nil {
		return err
	}
	id := uuid.NewString()
	if retain, ok := item.c.fremote.(managedSnapshotRetention); ok {
		if err := retain.RetainManagedSnapshot(item.c.ctx, id, item.name, baseID); err != nil {
			return err
		}
	}
	linked := false
	defer func() {
		if !linked {
			_ = item.releaseManagedBase(id)
		}
	}()
	base := journalBase{Schema: 1, Name: item.name, Snapshot: baseID, Info: item.info}
	j := &managedJournal{base: base, view: initialJournalView(base.Info.Size)}
	if baseID != "" {
		j.baseLease, err = b.OpenManagedSnapshot(item.c.ctx, baseID)
		if err != nil {
			return err
		}
	} else if base.Info.Size > 0 {
		// A migrated local-only base must be durable before we reference it.
		if err = item.c.managedIO.syncFile(item.fd); err != nil {
			return err
		}
	}
	if err = platform.EnsurePrivateDir(journalRoot(item.c.metaRoot)); err != nil {
		j.close()
		return err
	}
	p, err := journalPath(item.c.metaRoot, id)
	if err != nil {
		j.close()
		return err
	}
	j.file, err = os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		j.close()
		return err
	}
	data, err := json.Marshal(base)
	if err == nil {
		_, _, err = j.append(journalBaseRecord, base.Info.MutationSeq, 0, data)
	}
	if err == nil {
		err = item.c.managedIO.syncFile(j.file)
	}
	if err == nil {
		err = syncParents(journalRoot(item.c.metaRoot))
	}
	if err == nil {
		item.info.Journal = id
		err = item.saveManaged()
	}
	if err != nil {
		item.info.Journal = ""
		j.close()
		os.Remove(p)
		return err
	}
	item.journal = j
	linked = true
	return nil
}

func (item *Item) releaseManagedBase(id string) error {
	if retain, ok := item.c.fremote.(managedSnapshotRetention); ok {
		return retain.ReleaseManagedSnapshot(item.c.ctx, id)
	}
	return nil
}

func (j *managedJournal) close() {
	if j.file != nil {
		_ = j.file.Close()
		j.file = nil
	}
	if j.baseLease != nil {
		_ = j.baseLease.Close()
		j.baseLease = nil
	}
}
func (item *Item) closeManagedJournal() {
	item.mu.Lock()
	defer item.mu.Unlock()
	if item.journal != nil {
		item.journal.close()
		item.journal = nil
	}
}

// A failed append rolls back its own tail. A rollback failure poisons the
// journal, so neither a later save nor cleanup can mistake it for accepted data.
func (j *managedJournal) append(kind uint32, seq uint64, off int64, data []byte) (payload, end int64, err error) {
	if j.err != nil {
		return 0, 0, j.err
	}
	if len(data) > maxJournalPayload {
		return 0, 0, errors.New("local-save journal record too large")
	}
	start, err := j.file.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, 0, err
	}
	var h [journalHeader]byte
	binary.LittleEndian.PutUint32(h[0:4], journalMagic)
	binary.LittleEndian.PutUint32(h[4:8], kind)
	binary.LittleEndian.PutUint64(h[8:16], seq)
	binary.LittleEndian.PutUint64(h[16:24], uint64(off))
	binary.LittleEndian.PutUint64(h[24:32], uint64(len(data)))
	crc := crc32.Update(0, crc32.IEEETable, h[:32])
	binary.LittleEndian.PutUint32(h[32:36], crc32.Update(crc, crc32.IEEETable, data))
	_, err = j.file.Write(h[:])
	if err == nil {
		_, err = j.file.Write(data)
	}
	if err != nil {
		if e := j.file.Truncate(start); e != nil {
			j.err = errors.Join(err, e)
		}
		return 0, 0, err
	}
	return start + journalHeader, start + journalHeader + int64(len(data)), nil
}

func (item *Item) journalWrite(b []byte, off int64) (int, error) {
	if off < 0 || int64(len(b)) > math.MaxInt64-off {
		return 0, os.ErrInvalid
	}
	if len(b) == 0 {
		return 0, nil
	}
	if err := item.startManagedJournal(); err != nil {
		return 0, err
	}
	j := item.journal
	var n int
	for n < len(b) {
		count := min(len(b)-n, 1<<20)
		pos := off + int64(n)
		payload, _, err := j.append(journalWriteRecord, item.info.MutationSeq+1, pos, b[n:n+count])
		if err != nil {
			return n, err
		}
		j.view = writeJournalView(j.view, item.info.Size, pos, int64(count), payload)
		if pos > item.info.Size {
			item._written(item.info.Size, pos-item.info.Size)
		}
		item._written(pos, int64(count))
		item.info.Size = max(item.info.Size, pos+int64(count))
		item._dirty()
		n += count
	}
	return n, nil
}

func (item *Item) journalTruncate(size int64) error {
	if size < 0 {
		return os.ErrInvalid
	}
	if err := item.startManagedJournal(); err != nil {
		return err
	}
	if size == item.info.Size && item.o != nil {
		return nil
	}
	if _, _, err := item.journal.append(journalTruncateRecord, item.info.MutationSeq+1, size, nil); err != nil {
		return err
	}
	old := item.info.Size
	item.journal.view = truncateJournalView(item.journal.view, old, size)
	if size > old {
		item._written(old, size-old)
	} else {
		item.info.Rs = item.info.Rs.Intersection(ranges.Range{Pos: 0, Size: size})
	}
	item.info.Size = size
	item._dirty()
	return nil
}

// The WAL itself is the durability authority for changed bytes. There is no
// second full-body fsync, copy, hash, download or Git command on this path.
func (item *Item) checkpointManagedLocked() error {
	j := item.journal
	if j == nil || item.info.MutationSeq == item.info.DurableSeq {
		return nil
	}
	if j.err != nil {
		return j.err
	}
	info := item.info
	info.DurableSeq = info.MutationSeq
	info.JournalEnd = 0
	data, err := json.Marshal(info)
	if err != nil {
		return err
	}
	_, end, err := j.append(journalCheckpointRecord, info.MutationSeq, 0, data)
	if err == nil {
		err = item.c.managedIO.syncFile(j.file)
	}
	if err != nil {
		return err
	}
	info.JournalEnd = end
	previous := item.info
	item.info = info
	if err = item.saveManaged(); err != nil {
		item.info = previous
		return err
	}
	j.checkpoints = append(j.checkpoints, journalCheckpoint{info: info, view: append([]journalExtent(nil), j.view...)})
	return nil
}

func (item *Item) loadManagedJournal() error {
	p, err := journalPath(item.c.metaRoot, item.info.Journal)
	if err != nil {
		return err
	}
	j, recovered, err := readManagedJournal(p, item.info)
	if err != nil {
		return fmt.Errorf("local-save recovery: %w", err)
	}
	b, ok := item.localJournalBackend()
	if !ok {
		j.close()
		return errors.New("local-save journal needs a compatible backend")
	}
	if j.base.Snapshot != "" {
		j.baseLease, err = b.OpenManagedSnapshot(item.c.ctx, j.base.Snapshot)
		if err != nil {
			j.close()
			return err
		}
	}
	item.journal, item.info = j, recovered
	return nil
}
