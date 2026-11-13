package vfscache

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"math"
	"os"

	"github.com/rclone/rclone/lib/ranges"
)

// Reading and validation have no destructive side effects. A torn, uncommitted
// tail can be discarded only after all metadata-referenced bytes were verified.
func scanManagedJournal(p string, persisted Info) (*managedJournal, Info, int64, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, persisted, 0, err
	}
	defer f.Close()
	j := &managedJournal{}
	stat, err := f.Stat()
	if err != nil {
		return nil, persisted, 0, err
	}
	if !stat.Mode().IsRegular() || stat.Size() < persisted.JournalEnd {
		return nil, persisted, 0, errors.New("incomplete committed local-save journal")
	}
	var pos, committedEnd int64
	var ack uint64
	current := persisted
	var latest *journalCheckpoint
	for pos < stat.Size() {
		var h [journalHeader]byte
		_, e := f.ReadAt(h[:], pos)
		if e != nil {
			if pos < persisted.JournalEnd {
				return nil, persisted, 0, e
			}
			break
		}
		kind := binary.LittleEndian.Uint32(h[4:8])
		seq := binary.LittleEndian.Uint64(h[8:16])
		off := int64(binary.LittleEndian.Uint64(h[16:24]))
		size := binary.LittleEndian.Uint64(h[24:32])
		if binary.LittleEndian.Uint32(h[:4]) != journalMagic || size > maxJournalPayload || off < 0 || int64(size) > math.MaxInt64-pos-journalHeader || pos+journalHeader+int64(size) > stat.Size() {
			if pos < persisted.JournalEnd {
				return nil, persisted, 0, errors.New("invalid committed local-save record")
			}
			break
		}
		data := make([]byte, int(size))
		if _, e = f.ReadAt(data, pos+journalHeader); e != nil {
			return nil, persisted, 0, e
		}
		crc := crc32.Update(0, crc32.IEEETable, h[:32])
		if crc32.Update(crc, crc32.IEEETable, data) != binary.LittleEndian.Uint32(h[32:36]) {
			if pos < persisted.JournalEnd {
				return nil, persisted, 0, errors.New("committed local-save checksum mismatch")
			}
			break
		}
		end := pos + journalHeader + int64(size)
		switch kind {
		case journalBaseRecord:
			if pos != 0 || json.Unmarshal(data, &j.base) != nil || j.base.Schema != 1 || j.base.Info.Size < 0 {
				return nil, persisted, 0, errors.New("invalid local-save base")
			}
			current = j.base.Info
			current.Rs = append(ranges.Ranges(nil), current.Rs...)
			j.view = initialJournalView(current.Size)
			committedEnd = end
		case journalWriteRecord:
			j.uncommitted = true
			j.retired = false
			if pos == 0 || seq <= current.DurableSeq || seq < current.MutationSeq || int64(size) > math.MaxInt64-off {
				return nil, persisted, 0, errors.New("invalid local-save write sequence")
			}
			j.view = writeJournalView(j.view, current.Size, off, int64(size), pos+journalHeader)
			if off > current.Size {
				current.Rs.Insert(ranges.Range{Pos: current.Size, Size: off - current.Size})
			}
			current.Rs.Insert(ranges.Range{Pos: off, Size: int64(size)})
			current.Size = max(current.Size, off+int64(size))
			current.MutationSeq = seq
		case journalTruncateRecord:
			j.uncommitted = true
			j.retired = false
			if pos == 0 || seq <= current.DurableSeq || seq < current.MutationSeq || size != 0 {
				return nil, persisted, 0, errors.New("invalid local-save truncate sequence")
			}
			j.view = truncateJournalView(j.view, current.Size, off)
			if off > current.Size {
				current.Rs.Insert(ranges.Range{Pos: current.Size, Size: off - current.Size})
			} else {
				current.Rs = current.Rs.Intersection(ranges.Range{Pos: 0, Size: off})
			}
			current.Size = off
			current.MutationSeq = seq
		case journalCheckpointRecord:
			j.uncommitted = false
			j.retired = false
			var info Info
			if json.Unmarshal(data, &info) != nil || info.Size != current.Size || info.MutationSeq != seq || info.DurableSeq != seq || seq < current.MutationSeq || info.Journal != persisted.Journal {
				return nil, persisted, 0, errors.New("invalid local-save checkpoint")
			}
			info.JournalEnd = end
			cp := journalCheckpoint{info: info, view: append([]journalExtent(nil), j.view...)}
			if latest != nil && latest.info.MutationSeq == seq {
				j.checkpoints[len(j.checkpoints)-1] = cp
			} else {
				j.checkpoints = append(j.checkpoints, cp)
			}
			latest = &j.checkpoints[len(j.checkpoints)-1]
			current = info
			committedEnd = end
		case journalAckRecord:
			if latest == nil || seq > latest.info.DurableSeq || size != 0 {
				return nil, persisted, 0, errors.New("invalid local-save archive acknowledgement")
			}
			ack = max(ack, seq)
		case journalRetireRecord:
			if latest == nil || seq != latest.info.DurableSeq || seq > ack || size != 0 {
				return nil, persisted, 0, errors.New("invalid local-save retirement")
			}
			j.retired = true
		default:
			return nil, persisted, 0, fmt.Errorf("unsupported local-save record %d", kind)
		}
		pos = end
		j.scanEnd = pos
	}
	if committedEnd == 0 || persisted.JournalEnd > committedEnd {
		return nil, persisted, 0, errors.New("missing committed local-save checkpoint")
	}
	info := j.base.Info
	info.Journal = persisted.Journal
	if latest != nil {
		info = latest.info
		j.view = latest.view
	} else {
		j.view = initialJournalView(info.Size)
	}
	info.StagedSeq = max(info.StagedSeq, persisted.StagedSeq, ack)
	if info.StagedSeq > info.MutationSeq || info.DurableSeq > info.MutationSeq {
		return nil, persisted, 0, errors.New("invalid local-save acknowledgement sequence")
	}
	info.Dirty = info.MutationSeq > info.StagedSeq
	return j, info, committedEnd, nil
}

func readManagedJournal(p string, persisted Info) (*managedJournal, Info, error) {
	j, info, end, err := scanManagedJournal(p, persisted)
	if err != nil {
		return nil, persisted, err
	}
	j.file, err = os.OpenFile(p, os.O_RDWR, 0600)
	if err != nil {
		return nil, persisted, err
	}
	// Remove uncommitted writes before accepting new ones. Re-append the durable
	// acknowledgement if it followed that tail; old accepted saves cannot replay
	// backwards after a crash between Git/archive and cache compaction.
	if err = j.file.Truncate(end); err == nil && info.StagedSeq > j.base.Info.StagedSeq {
		_, _, err = j.append(journalAckRecord, info.StagedSeq, 0, nil)
	}
	if err == nil {
		err = j.file.Sync()
	}
	if err != nil {
		j.close()
		return nil, persisted, err
	}
	j.uncommitted, j.retired = false, false
	return j, info, nil
}
