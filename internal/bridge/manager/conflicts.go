package manager

import (
	"bytes"
	"context"
	"errors"
	"io"
	"unicode/utf8"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
)

type ConflictView struct {
	Conflict                   model.Conflict `json:"conflict"`
	Entry                      model.Entry    `json:"entry"`
	Base, Local, Remote, Draft []byte
	Text                       bool   `json:"text"`
	SourceURL                  string `json:"source_url,omitempty"`
	Stale                      bool   `json:"stale"`
	RefreshWarning             string `json:"refresh_warning,omitempty"`
}

func blobText(ctx context.Context, s *store.Store, id model.BlobID) ([]byte, bool, error) {
	if id == "" {
		return nil, true, nil
	}
	r, err := s.ReadBlob(ctx, id)
	if err != nil {
		return nil, false, err
	}
	defer r.Close()
	b, err := io.ReadAll(io.LimitReader(r, (10<<20)+1))
	if err != nil {
		return nil, false, err
	}
	if len(b) > 10<<20 || !utf8.Valid(b) || bytes.IndexByte(b, 0) >= 0 {
		return nil, false, nil
	}
	return b, true, nil
}
func conflictSummaries(ctx context.Context, s *store.Store) ([]ConflictView, error) {
	list, err := s.Conflicts(ctx)
	if err != nil {
		return nil, err
	}
	out := []ConflictView{}
	for _, c := range list {
		entry, err := s.Entry(ctx, c.EntryID)
		if err != nil {
			return nil, err
		}
		metadata, err := s.Metadata(ctx, entry.ID)
		if err != nil {
			return nil, err
		}
		out = append(out, ConflictView{Conflict: c, Entry: entry, SourceURL: metadata.SourceURL})
	}
	return out, nil
}
func materializeConflict(ctx context.Context, s *store.Store, view ConflictView) (ConflictView, error) {
	view.Text = true
	parts := []*[]byte{&view.Base, &view.Local, &view.Remote, &view.Draft}
	for i, id := range []model.BlobID{view.Conflict.Base, view.Conflict.Local, view.Conflict.Remote, view.Conflict.Draft} {
		body, text, err := blobText(ctx, s, id)
		if err != nil {
			return view, err
		}
		*parts[i] = body
		view.Text = view.Text && text
	}
	if !view.Text {
		view.Base = nil
		view.Local = nil
		view.Remote = nil
		view.Draft = nil
	}
	return view, nil
}
func findSummary(ctx context.Context, s *store.Store, id model.ConflictID) (ConflictView, error) {
	list, err := conflictSummaries(ctx, s)
	if err != nil {
		return ConflictView{}, err
	}
	for _, v := range list {
		if v.Conflict.ID == id {
			return v, nil
		}
	}
	return ConflictView{}, store.ErrNotFound
}
func (w *worker) conflicts(ctx context.Context) ([]ConflictView, error) {
	return conflictSummaries(ctx, w.services.store)
}
func (w *worker) conflictDetail(ctx context.Context, id model.ConflictID) (ConflictView, error) {
	view, err := findSummary(ctx, w.services.store, id)
	if err != nil {
		return view, err
	}
	if w.services.refreshConflict != nil {
		fresh, refreshErr := w.services.refreshConflict(ctx, id)
		if fresh.ID != "" {
			view, err = findSummary(ctx, w.services.store, id)
			if err != nil {
				return view, err
			}
		}
		if refreshErr != nil {
			view.Stale = true
			view.RefreshWarning = errorCode(refreshErr)
		}
	}
	return materializeConflict(ctx, w.services.store, view)
}
func (m *Manager) Conflicts(ctx context.Context, id model.MountID) ([]ConflictView, error) {
	var out []ConflictView
	err := m.withStore(ctx, id, "/conflicts", nil, &out, func(s *store.Store) error { var err error; out, err = conflictSummaries(ctx, s); return err })
	if errors.Is(err, errNoState) {
		return []ConflictView{}, nil
	}
	return out, err
}
func (m *Manager) findConflict(ctx context.Context, id model.ConflictID) (ConflictView, error) {
	c, err := m.LoadConfig(ctx)
	if err != nil {
		return ConflictView{}, err
	}
	for _, mount := range c.Mounts {
		list, err := m.Conflicts(ctx, mount.ID)
		if err != nil {
			return ConflictView{}, err
		}
		for _, view := range list {
			if view.Conflict.ID == id {
				return view, nil
			}
		}
	}
	return ConflictView{}, store.ErrNotFound
}
func (m *Manager) Conflict(ctx context.Context, id model.ConflictID) (ConflictView, error) {
	summary, err := m.findConflict(ctx, id)
	if err != nil {
		return summary, err
	}
	var out ConflictView
	err = m.withStore(ctx, summary.Entry.MountID, "/conflict", struct {
		ID model.ConflictID `json:"id"`
	}{id}, &out, func(s *store.Store) error {
		view, err := findSummary(ctx, s, id)
		if err != nil {
			return err
		}
		view.Stale = true
		view.RefreshWarning = "offline"
		out, err = materializeConflict(ctx, s, view)
		return err
	})
	return out, err
}
func (m *Manager) SaveDraft(ctx context.Context, id model.ConflictID, content []byte) error {
	view, err := m.findConflict(ctx, id)
	if err != nil {
		return err
	}
	var out struct{}
	return m.withStore(ctx, view.Entry.MountID, "/draft", struct {
		ID      model.ConflictID `json:"id"`
		Content []byte           `json:"content"`
	}{id, content}, &out, func(s *store.Store) error { return s.SaveDraft(ctx, id, bytes.NewReader(content)) })
}
func (m *Manager) ResolveConflict(ctx context.Context, r model.ResolveRequest) error {
	view, err := m.findConflict(ctx, r.ConflictID)
	if err != nil {
		return err
	}
	if err = m.SaveDraft(ctx, r.ConflictID, r.Content); err != nil {
		return err
	}
	var out struct{}
	return m.call(ctx, view.Entry.MountID, "/resolve", r, &out)
}
func (m *Manager) ClaimConflictNotification(ctx context.Context, id model.ConflictID) (bool, error) {
	view, err := m.findConflict(ctx, id)
	if err != nil {
		return false, err
	}
	var out bool
	err = m.withStore(ctx, view.Entry.MountID, "/conflict/claim", struct {
		ID model.ConflictID `json:"id"`
	}{id}, &out, func(s *store.Store) error { var err error; out, err = s.ClaimConflictNotification(ctx, id); return err })
	return out, err
}

// ExportConflict returns human-readable private paths only for this explicit
// export action. Listing/status never discloses server filesystem paths.
func (m *Manager) ExportConflict(ctx context.Context, id model.ConflictID) (store.ConflictCopies, error) {
	view, err := m.findConflict(ctx, id)
	if err != nil {
		return store.ConflictCopies{}, err
	}
	var out store.ConflictCopies
	err = m.withStore(ctx, view.Entry.MountID, "/conflict/export", struct {
		ID model.ConflictID `json:"id"`
	}{id}, &out, func(s *store.Store) error { var err error; out, err = s.ExportConflictCopies(ctx, id); return err })
	return out, err
}
