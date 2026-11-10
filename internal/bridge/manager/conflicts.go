package manager

import (
	"bytes"
	"context"
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
func conflictViews(ctx context.Context, s *store.Store) ([]ConflictView, error) {
	list, err := s.Conflicts(ctx)
	if err != nil {
		return nil, err
	}
	out := []ConflictView{}
	for _, c := range list {
		e, err := s.Entry(ctx, c.EntryID)
		if err != nil {
			return nil, err
		}
		view := ConflictView{Conflict: c, Entry: e, Text: true}
		m, err := s.Metadata(ctx, e.ID)
		if err != nil {
			return nil, err
		}
		view.SourceURL = m.SourceURL
		parts := []*[]byte{&view.Base, &view.Local, &view.Remote, &view.Draft}
		for i, id := range []model.BlobID{c.Base, c.Local, c.Remote, c.Draft} {
			b, text, err := blobText(ctx, s, id)
			if err != nil {
				return nil, err
			}
			*parts[i] = b
			view.Text = view.Text && text
		}
		if !view.Text {
			view.Base = nil
			view.Local = nil
			view.Remote = nil
			view.Draft = nil
		}
		out = append(out, view)
	}
	return out, nil
}
func (w *worker) conflicts(ctx context.Context) ([]ConflictView, error) {
	return conflictViews(ctx, w.services.store)
}
func (m *Manager) Conflicts(ctx context.Context, id model.MountID) ([]ConflictView, error) {
	var out []ConflictView
	err := m.withStore(ctx, id, "/conflicts", nil, &out, func(s *store.Store) error { var err error; out, err = conflictViews(ctx, s); return err })
	return out, err
}
func (m *Manager) Conflict(ctx context.Context, id model.ConflictID) (ConflictView, error) {
	c, err := m.LoadConfig(ctx)
	if err != nil {
		return ConflictView{}, err
	}
	for _, mount := range c.Mounts {
		views, err := m.Conflicts(ctx, mount.ID)
		if err != nil {
			return ConflictView{}, err
		}
		for _, view := range views {
			if view.Conflict.ID == id {
				return view, nil
			}
		}
	}
	return ConflictView{}, ErrConfig
}
func (m *Manager) SaveDraft(ctx context.Context, id model.ConflictID, content []byte) error {
	view, err := m.Conflict(ctx, id)
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
	view, err := m.Conflict(ctx, r.ConflictID)
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
	view, err := m.Conflict(ctx, id)
	if err != nil {
		return false, err
	}
	var out bool
	err = m.withStore(ctx, view.Entry.MountID, "/conflict/claim", struct {
		ID model.ConflictID `json:"id"`
	}{id}, &out, func(s *store.Store) error { var err error; out, err = s.ClaimConflictNotification(ctx, id); return err })
	return out, err
}
