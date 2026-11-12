package manager

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"unicode/utf8"

	"github.com/rclone/rclone/internal/bridge/auth"
	"github.com/rclone/rclone/internal/bridge/cloud"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/platform"
	"github.com/rclone/rclone/internal/bridge/store"
	syncer "github.com/rclone/rclone/internal/bridge/sync"
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
func (w *worker) conflictDetail(ctx context.Context, id model.ConflictID, refresh bool) (ConflictView, error) {
	view, err := findSummary(ctx, w.services.store, id)
	if err != nil {
		return view, err
	}
	if refresh && w.services.refreshConflict != nil {
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
	var failures []error
	for _, mount := range c.Mounts {
		if err := ctx.Err(); err != nil {
			return ConflictView{}, err
		}
		list, err := m.Conflicts(ctx, mount.ID)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		for _, view := range list {
			if view.Conflict.ID == id {
				return view, nil
			}
		}
	}
	if len(failures) > 0 {
		return ConflictView{}, errors.Join(failures...)
	}
	return ConflictView{}, store.ErrNotFound
}
func (m *Manager) Conflict(ctx context.Context, id model.ConflictID) (ConflictView, error) {
	return m.conflictView(ctx, id, true)
}

// ConflictForResolve returns the locally stored versions without re-reading the
// cloud. Accepting a merge is rechecked against the cloud exactly once, inside
// the worker's resolve step, so a pre-read here would only repeat that round trip
// (two more serialized lark-cli processes per file) while holding the file's
// entry lock.
func (m *Manager) ConflictForResolve(ctx context.Context, id model.ConflictID) (ConflictView, error) {
	return m.conflictView(ctx, id, false)
}
func (m *Manager) conflictView(ctx context.Context, id model.ConflictID, refresh bool) (ConflictView, error) {
	summary, err := m.findConflict(ctx, id)
	if err != nil {
		return summary, err
	}
	var out ConflictView
	err = m.withStore(ctx, summary.Entry.MountID, "/conflict", struct {
		ID        model.ConflictID `json:"id"`
		NoRefresh bool             `json:"no_refresh,omitempty"`
	}{id, !refresh}, &out, func(s *store.Store) error {
		out, err = m.stoppedConflict(ctx, s, id, m.buildConflictEngine)
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
	if r.Action != "" && r.Action != model.ResolveRestoreLocal && r.Action != model.ResolveAcceptDelete {
		return model.ErrUnsupportedContent
	}
	view, err := m.findConflict(ctx, r.ConflictID)
	if err != nil {
		return err
	}
	if r.Action != model.ResolveAcceptDelete && (r.Action != model.ResolveRestoreLocal || r.Content != nil) {
		if err = m.SaveDraft(ctx, r.ConflictID, r.Content); err != nil {
			return err
		}
	}
	var out struct{}
	return m.withStore(ctx, view.Entry.MountID, "/resolve", r, &out, func(s *store.Store) error {
		return m.stoppedResolve(ctx, s, r, m.buildConflictEngine)
	})
}

type conflictEngine interface {
	RefreshConflict(context.Context, model.ConflictID) (model.Conflict, error)
	Resolve(context.Context, model.ResolveRequest) (model.Entry, model.Job, error)
}

type conflictEngineBuilder func(context.Context, mountSpec, *store.Store) (conflictEngine, error)

// buildConflictEngine permits explicit review and local resolution while no
// runtime owns the store. It never starts a worker, publishes, or writes cloud
// content, including when the mount configuration is archived.
func (m *Manager) buildConflictEngine(ctx context.Context, spec mountSpec, s *store.Store) (conflictEngine, error) {
	scope, err := s.Scope(ctx)
	if err != nil {
		return nil, err
	}
	if scope != ownershipFor(m.options.ConfigFile, spec).Scope {
		return nil, model.ErrIdentityChanged
	}
	a, err := m.Auth(ctx)
	if err != nil {
		return nil, err
	}
	runner, err := a.ManagedRunner(ctx, spec.Profile.ID)
	if err != nil {
		return nil, err
	}
	verifyIdentity := func(ctx context.Context) error {
		identity, err := a.Status(ctx, spec.Profile.ID)
		if err != nil {
			return err
		}
		if auth.IdentityHash(identity.Brand, identity.AppID, identity.UserOpenID) != spec.Profile.IdentityHash {
			return model.ErrIdentityChanged
		}
		return nil
	}
	if err = verifyIdentity(ctx); err != nil {
		return nil, err
	}
	transportDir := filepath.Join(spec.Paths.Root, "transport")
	if err = platform.EnsurePrivateDir(transportDir); err != nil {
		return nil, err
	}
	client, err := cloud.New(runner, cloud.Options{CheckIdentity: verifyIdentity, CheckMutation: func(context.Context) error {
		return ErrUnavailable
	}, TempDir: transportDir})
	if err != nil {
		return nil, err
	}
	return syncer.New(syncer.Dependencies{CheckIdentity: verifyIdentity, Store: s, Cloud: client, Publisher: syncer.PublisherFunc(func(context.Context, model.PublishCandidate) error {
		return ErrUnavailable
	})}, syncer.Options{})
}

func (m *Manager) stoppedConflict(ctx context.Context, s *store.Store, id model.ConflictID, build conflictEngineBuilder) (ConflictView, error) {
	view, err := findSummary(ctx, s, id)
	if err != nil {
		return view, err
	}
	// A never-bound local unlink has no cloud identity to check. The store
	// validates its journals before exposing the explicit local choices.
	if view.Entry.Token == "" && (view.Conflict.Reason == "delete_vs_pending_edit" || view.Conflict.Reason == model.ConflictLocalDeletePending) {
		_, refreshErr := s.ObserveUnsentDeletion(ctx, view.Entry)
		view, err = findSummary(ctx, s, id)
		if err != nil {
			return view, err
		}
		if refreshErr != nil {
			view.Stale = true
			view.RefreshWarning = errorCode(refreshErr)
		}
		return materializeConflict(ctx, s, view)
	}
	spec, refreshErr := m.spec(ctx, view.Entry.MountID)
	if refreshErr == nil {
		var engine conflictEngine
		engine, refreshErr = build(ctx, spec, s)
		if refreshErr == nil {
			_, refreshErr = engine.RefreshConflict(ctx, id)
			// Refresh may have saved a newer observation even if it also reports
			// a cloud error. Always materialize the durable capture.
			view, err = findSummary(ctx, s, id)
			if err != nil {
				return view, err
			}
		}
	}
	if refreshErr != nil {
		view.Stale = true
		view.RefreshWarning = errorCode(refreshErr)
	}
	return materializeConflict(ctx, s, view)
}

func (m *Manager) stoppedResolve(ctx context.Context, s *store.Store, r model.ResolveRequest, build conflictEngineBuilder) error {
	view, err := findSummary(ctx, s, r.ConflictID)
	if err != nil {
		return err
	}
	if view.Conflict.Reason == model.ConflictLocalDeletePending {
		_, _, err = s.ResolveRemoteDeletion(ctx, r)
		return err
	}
	spec, err := m.spec(ctx, view.Entry.MountID)
	if err != nil {
		return err
	}
	engine, err := build(ctx, spec, s)
	if err != nil {
		return err
	}
	_, _, err = engine.Resolve(ctx, r)
	return err
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
