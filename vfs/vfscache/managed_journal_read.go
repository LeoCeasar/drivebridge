package vfscache

import (
	"context"
	"errors"
	"github.com/rclone/rclone/lib/ranges"
	"io"
	"os"
)

const (
	journalBaseExtent = iota
	journalDataExtent
	journalZeroExtent
)

type journalExtent struct {
	start, end, payload int64
	kind                int
}

func initialJournalView(size int64) []journalExtent {
	if size <= 0 {
		return nil
	}
	return []journalExtent{{start: 0, end: size, kind: journalBaseExtent}}
}

func replaceJournalView(view []journalExtent, extent journalExtent) []journalExtent {
	out := make([]journalExtent, 0, len(view)+2)
	inserted := false
	for _, v := range view {
		if v.end <= extent.start {
			out = append(out, v)
			continue
		}
		if v.start >= extent.end {
			if !inserted {
				out = append(out, extent)
				inserted = true
			}
			out = append(out, v)
			continue
		}
		if v.start < extent.start {
			left := v
			left.end = extent.start
			out = append(out, left)
		}
		if !inserted {
			out = append(out, extent)
			inserted = true
		}
		if v.end > extent.end {
			right := v
			right.start = extent.end
			if right.kind == journalDataExtent {
				right.payload += right.start - v.start
			}
			out = append(out, right)
		}
	}
	if !inserted {
		out = append(out, extent)
	}
	return out
}
func writeJournalView(view []journalExtent, size, off, count, payload int64) []journalExtent {
	if off > size {
		view = append(view, journalExtent{start: size, end: off, kind: journalZeroExtent})
	}
	return replaceJournalView(view, journalExtent{start: off, end: off + count, payload: payload, kind: journalDataExtent})
}
func truncateJournalView(view []journalExtent, old, size int64) []journalExtent {
	if size > old {
		return append(view, journalExtent{start: old, end: size, kind: journalZeroExtent})
	}
	out := make([]journalExtent, 0, len(view))
	for _, v := range view {
		if v.start >= size {
			break
		}
		v.end = min(v.end, size)
		out = append(out, v)
	}
	return out
}

type journalReader struct {
	ctx       context.Context
	view      []journalExtent
	size, pos int64
	log       io.ReaderAt
	base      io.ReaderAt
	close     func() error
}

func (r *journalReader) ReadAt(p []byte, off int64) (n int, err error) {
	if err = r.ctx.Err(); err != nil {
		return 0, err
	}
	if off < 0 {
		return 0, os.ErrInvalid
	}
	if len(p) == 0 {
		return 0, nil
	}
	if off >= r.size {
		return 0, io.EOF
	}
	want := min(int64(len(p)), r.size-off)
	for _, v := range r.view {
		start, end := max(off, v.start), min(off+want, v.end)
		if end <= start {
			continue
		}
		if start != off+int64(n) {
			return n, errors.New("incomplete local-save extent map")
		}
		dst := p[n : n+int(end-start)]
		var count int
		switch v.kind {
		case journalZeroExtent:
			clear(dst)
			count = len(dst)
		case journalDataExtent:
			count, err = r.log.ReadAt(dst, v.payload+start-v.start)
		case journalBaseExtent:
			if r.base == nil {
				return n, errors.New("original cached content unavailable; local save retained")
			}
			count, err = r.base.ReadAt(dst, start)
		default:
			return n, errors.New("unknown local-save extent")
		}
		n += count
		if err != nil {
			return n, err
		}
	}
	if int64(n) != want {
		return n, io.ErrUnexpectedEOF
	}
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}
func (r *journalReader) Read(p []byte) (int, error) {
	n, err := r.ReadAt(p, r.pos)
	r.pos += int64(n)
	return n, err
}
func (r *journalReader) Close() error {
	if r.close == nil {
		return nil
	}
	fn := r.close
	r.close = nil
	return fn()
}

// Called under item.mu. Each reader owns its file handles and immutable view;
// background hashing never holds snapshotMu or prevents subsequent edits.
func (item *Item) managedJournalReader(ctx context.Context, cp journalCheckpoint) (*journalReader, error) {
	j := item.journal
	if j == nil || j.file == nil {
		return nil, errors.New("local-save journal unavailable")
	}
	log, err := os.Open(j.file.Name())
	if err != nil {
		return nil, err
	}
	r := &journalReader{ctx: ctx, view: cp.view, size: cp.info.Size, log: log}
	needBase := false
	for _, v := range cp.view {
		needBase = needBase || v.kind == journalBaseExtent
	}
	var base io.ReadCloser
	if needBase {
		if j.base.Snapshot != "" {
			b, _ := item.localJournalBackend()
			base, err = b.OpenManagedSnapshot(ctx, j.base.Snapshot)
		} else if j.base.Info.Rs.Present(ranges.Range{Pos: 0, Size: j.base.Info.Size}) {
			base, err = os.Open(item.c.toOSPath(item.name))
		} else {
			err = errors.New("original file was not cached; local save retained")
		}
		if err == nil {
			var ok bool
			r.base, ok = base.(io.ReaderAt)
			if !ok {
				err = errors.New("immutable local snapshot lacks random access")
			}
		}
		if err != nil {
			log.Close()
			if base != nil {
				base.Close()
			}
			return nil, err
		}
	}
	r.close = func() error {
		err := log.Close()
		if base != nil {
			err = errors.Join(err, base.Close())
		}
		return err
	}
	return r, nil
}
