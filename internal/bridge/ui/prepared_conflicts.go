package ui

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/rclone/rclone/internal/bridge/model"
)

const preparedMaxEntries = 32
const preparedMaxBytes = 32 << 20
const preparationWorkers = 2

// Fingerprint metadata identifies all four durable captures, including drafts.
// Bodies never enter status responses. The store's blobs are immutable.
func conflictRevision(in ConflictInputs) string {
	b, _ := json.Marshal(struct {
		Conflict  model.Conflict
		Entry     model.Entry
		SourceURL string
	}{in.Conflict, in.Entry, in.SourceURL})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func conflictSourceRevision(in ConflictInputs) string {
	in.Conflict.Draft = ""
	return conflictRevision(in)
}

type preparedConflict struct {
	revision string
	view     ConflictView
	bytes    int
	used     uint64
}
type preparedConflicts struct {
	mu      sync.Mutex
	entries map[model.ConflictID]preparedConflict
	desired map[model.ConflictID]string
	pending map[model.ConflictID]string
	active  map[model.ConflictID]string
	bytes   int
	clock   uint64
	epoch   uint64
	workers int
	ctx     context.Context
	cancel  context.CancelFunc
}

func (p *preparedConflicts) init() {
	if p.entries == nil {
		p.ctx, p.cancel = context.WithCancel(context.Background())
		p.entries = map[model.ConflictID]preparedConflict{}
		p.desired = map[model.ConflictID]string{}
		p.pending = map[model.ConflictID]string{}
		p.active = map[model.ConflictID]string{}
	}
}
func (p *preparedConflicts) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.init()
	p.cancel()
	p.pending = map[model.ConflictID]string{}
}

func (p *preparedConflicts) has(id model.ConflictID, revision string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	v, ok := p.entries[id]
	return ok && v.revision == revision
}
func (p *preparedConflicts) invalidate(id model.ConflictID) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.init()
	p.remove(id)
	p.epoch++
	delete(p.desired, id)
	delete(p.pending, id)
}
func (p *preparedConflicts) remove(id model.ConflictID) {
	p.bytes -= p.entries[id].bytes
	delete(p.entries, id)
}
func (p *preparedConflicts) get(id model.ConflictID) (ConflictView, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.init()
	v, ok := p.entries[id]
	if !ok || v.revision != p.desired[id] {
		return ConflictView{}, false
	}
	p.clock++
	v.used = p.clock
	p.entries[id] = v
	return v.view, true
}
func (p *preparedConflicts) put(id model.ConflictID, revision string, v ConflictView) {
	encoded, _ := json.Marshal(v)
	size := len(encoded)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.init()
	if p.desired[id] != revision || size > preparedMaxBytes {
		return
	}
	p.remove(id)
	for len(p.entries) >= preparedMaxEntries || p.bytes+size > preparedMaxBytes {
		var oldest model.ConflictID
		var age uint64 = ^uint64(0)
		for key, item := range p.entries {
			if item.used < age {
				oldest, age = key, item.used
			}
		}
		p.remove(oldest)
	}
	p.clock++
	p.entries[id] = preparedConflict{revision: revision, view: v, bytes: size, used: p.clock}
	p.bytes += size
}
func (h *Host) conflictPresentation(in ConflictInputs) ConflictView {
	v := ConflictView{Revision: conflictRevision(in), SourceRevision: conflictSourceRevision(in), Hunks: []DiffHunk{}, Stale: in.Stale, RefreshWarning: safeCode(in.RefreshWarning), HasDraft: in.Conflict.Draft != "" || in.Draft != nil, ID: in.Conflict.ID, EntryID: in.Entry.ID, Generation: in.Conflict.Generation, RemoteRevision: in.Conflict.RemoteRevision, Name: in.Entry.Remote, Text: in.Text, Reason: in.Conflict.Reason}
	v.RemoteDeleted = in.Conflict.Reason == model.ConflictRemoteDeleted
	v.LocalDeletePending = in.Conflict.Reason == model.ConflictLocalDeletePending
	_, v.CanExportCopies = h.backend.(ConflictExporter)
	if in.Text && utf8.Valid(in.Base) && utf8.Valid(in.Local) && utf8.Valid(in.Remote) && utf8.Valid(in.Draft) {
		v.Base = string(in.Base)
		v.Local = string(in.Local)
		v.Remote = string(in.Remote)
		v.Draft = string(in.Draft)
		if hunks := diffHunks(v.Base, v.Local, v.Remote); hunks != nil {
			v.Hunks = hunks
		}
	} else {
		v.Text = false
	}
	if safeHTTPS(in.SourceURL) {
		v.SourceURL = in.SourceURL
	}
	return v
}
func (h *Host) preparedConflict(ctx context.Context, id model.ConflictID, refresh bool) (ConflictView, error) {
	if !refresh {
		if v, ok := h.prepared.get(id); ok {
			return v, nil
		}
	}
	read := h.backend.Conflict
	if refresh {
		h.prepared.invalidate(id)
		if reader, ok := h.backend.(ConflictRefresher); ok {
			read = reader.RefreshConflict
		}
	}
	h.prepared.mu.Lock()
	h.prepared.init()
	epoch := h.prepared.epoch
	h.prepared.mu.Unlock()
	in, err := read(ctx, id)
	if err != nil {
		return ConflictView{}, err
	}
	if in.Conflict.ID != id || in.Conflict.EntryID != in.Entry.ID {
		return ConflictView{}, model.ErrVersionChanged
	}
	revision := conflictRevision(in)
	v := h.conflictPresentation(in)
	h.prepared.mu.Lock()
	h.prepared.init()
	if h.prepared.epoch == epoch {
		h.prepared.desired[id] = revision
	}
	h.prepared.mu.Unlock()
	h.prepared.put(id, revision, v)
	return v, nil
}

// Status and the tray's background poll both prepare durable local inputs. Two
// workers and a bounded pending window prevent one goroutine per conflict.
func (h *Host) prepareConflicts(list []ConflictSummary, partial bool) {
	p := &h.prepared
	p.mu.Lock()
	p.init()
	keep := map[model.ConflictID]bool{}
	for _, c := range list {
		keep[c.ID] = true
		if c.Revision == "" {
			continue
		}
		if p.desired[c.ID] != c.Revision {
			p.epoch++
		}
		p.desired[c.ID] = c.Revision
		if v, ok := p.entries[c.ID]; ok && v.revision != c.Revision {
			p.remove(c.ID)
		}
		if _, ok := p.entries[c.ID]; !ok && len(p.pending) < preparedMaxEntries {
			if p.active[c.ID] != c.Revision {
				p.pending[c.ID] = c.Revision
			}
		}
	}
	if !partial {
		for id := range p.desired {
			if !keep[id] {
				p.remove(id)
				p.epoch++
				delete(p.desired, id)
				delete(p.pending, id)
			}
		}
	}
	for p.workers < preparationWorkers && len(p.pending) > 0 {
		p.workers++
		go h.prepareWorker()
	}
	p.mu.Unlock()
}
func (h *Host) prepareWorker() {
	p := &h.prepared
	for {
		p.mu.Lock()
		if p.ctx.Err() != nil {
			p.workers--
			p.mu.Unlock()
			return
		}
		var id model.ConflictID
		var revision string
		for key, value := range p.pending {
			id, revision = key, value
			break
		}
		if id == "" {
			p.workers--
			p.mu.Unlock()
			return
		}
		delete(p.pending, id)
		p.active[id] = revision
		p.mu.Unlock()
		ctx, cancel := context.WithTimeout(p.ctx, 20*time.Second)
		in, err := h.backend.Conflict(ctx, id)
		cancel()
		if err == nil && in.Conflict.ID == id && in.Conflict.EntryID == in.Entry.ID && conflictRevision(in) == revision {
			p.put(id, revision, h.conflictPresentation(in))
		}
		p.mu.Lock()
		delete(p.active, id)
		p.mu.Unlock()
	}
}
