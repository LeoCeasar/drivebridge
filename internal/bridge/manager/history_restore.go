package manager

import (
	"context"
	"errors"
	"io"
	"path"

	"github.com/rclone/rclone/internal/bridge/history"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
)

type HistoryCurrent struct {
	Entry   model.Entry `json:"entry"`
	Parent  model.Entry `json:"parent"`
	Deleted bool        `json:"deleted"`
}
type HistoryDetail struct {
	Version          history.Version `json:"version"`
	Current          *HistoryCurrent `json:"current,omitempty"`
	RestoreAvailable bool            `json:"restore_available"`
	Warning          string          `json:"warning,omitempty"`
}

func historyCurrent(ctx context.Context, s *store.Store, id model.EntryID) (out HistoryCurrent, err error) {
	out.Entry, err = s.Entry(ctx, id)
	if err != nil {
		return out, err
	}
	m, err := s.Metadata(ctx, id)
	if err != nil {
		return out, err
	}
	out.Deleted = m.Tombstone
	parent := path.Dir(out.Entry.Remote)
	if parent == "." {
		parent = ""
	}
	out.Parent, err = s.EntryByRemote(ctx, parent)
	return
}
func (m *Manager) HistoryDetail(ctx context.Context, profile model.ProfileID, id string) (out HistoryDetail, err error) {
	out.Version, err = m.HistoryVersion(ctx, profile, id)
	if err != nil {
		return out, err
	}
	mount := out.Version.Event.MountID
	var current HistoryCurrent
	err = m.withStore(ctx, mount, "/history/current", struct {
		ID model.EntryID `json:"id"`
	}{out.Version.Event.Entry.ID}, &current, func(s *store.Store) error {
		var err error
		current, err = historyCurrent(ctx, s, out.Version.Event.Entry.ID)
		return err
	})
	if err != nil {
		out.Warning = "current_unavailable"
		return out, nil
	}
	out.Current = &current
	spec, err := m.spec(ctx, mount)
	if err != nil {
		return out, err
	}
	out.RestoreAvailable = !spec.Mount.Archived && current.Entry.Kind != model.KindFolder
	if spec.Mount.Archived {
		out.Warning = "mount_archived"
	}
	return out, nil
}
func validRestoreRole(role string) bool {
	switch role {
	case "", "local", "remote", "base_local", "conflict_base", "conflict_local", "conflict_remote", "draft":
		return true
	}
	return false
}
func (m *Manager) restoreHistoryStore(ctx context.Context, s *store.Store, req model.HistoryRestoreRequest, publish func(context.Context, model.HistoryRestoreRequest, io.Reader, *store.HistoryRestoreBundle) (model.HistoryRestoreResult, error)) (model.HistoryRestoreResult, error) {
	if !validRestoreRole(req.Role) {
		return model.HistoryRestoreResult{}, ErrConfig
	}
	if saved, found, err := s.HistoryRestoreReceipt(ctx, req); err != nil || found {
		return saved, err
	}
	if ev, e := s.HistoryEvent(ctx, req.VersionID); e == nil && !ev.Archived {
		return m.pendingRestore(ctx, s, req, publish)
	}
	repo, err := m.historyRepository(ctx, req.ProfileID)
	if err != nil {
		return model.HistoryRestoreResult{}, err
	}
	release, e := repo.LeaseVersion(ctx, req.VersionID)
	if e != nil {
		return model.HistoryRestoreResult{}, e
	}
	defer release()
	v, err := repo.Version(ctx, req.VersionID)
	if err != nil {
		return model.HistoryRestoreResult{}, err
	}
	if v.Event.Entry.ID != req.EntryID || v.Event.MountID != req.MountID || v.Event.ProfileID != req.ProfileID {
		return model.HistoryRestoreResult{}, model.ErrIdentityChanged
	}
	body, err := repo.OpenBody(ctx, req.VersionID, req.Role)
	if err != nil {
		return model.HistoryRestoreResult{}, err
	}
	defer body.Close()
	role := req.Role
	if role == "" {
		role = "local"
		if _, ok := v.Bodies[role]; !ok {
			role = "remote"
		}
	}
	var bundle *store.HistoryRestoreBundle
	snapshot := v.Event.RemoteSnapshot
	if role == "conflict_remote" && v.Event.ConflictRemoteSnapshot != nil {
		snapshot = v.Event.ConflictRemoteSnapshot
	}
	if (role == "local" || role == "draft" || role == "conflict_local") && v.Event.LocalSnapshot != nil {
		snapshot = v.Event.LocalSnapshot
	}
	if v.Event.Entry.Kind == model.KindDocx && snapshot != nil {
		bundle = &store.HistoryRestoreBundle{Snapshot: snapshot, Blobs: map[model.BlobID]io.Reader{}}
		var opened []io.Closer
		defer func() {
			for _, f := range opened {
				f.Close()
			}
		}()
		for _, hash := range snapshot.Blobs() {
			if hash == "" {
				continue
			}
			if _, ok := bundle.Blobs[hash]; ok {
				continue
			}
			sourceRole := ""
			for k, ref := range v.Bodies {
				if ref.SHA256 == string(hash) {
					sourceRole = k
					break
				}
			}
			if sourceRole == "" {
				for k, ref := range v.Media {
					if ref.SHA256 == string(hash) {
						sourceRole = "media:" + k
						break
					}
				}
			}
			if sourceRole == "" {
				return model.HistoryRestoreResult{}, history.ErrBodyUnavailable
			}
			role := sourceRole
			f := &historyResourceReader{open: func() (io.ReadCloser, error) { return repo.OpenBody(ctx, req.VersionID, role) }}
			opened = append(opened, f)
			bundle.Blobs[hash] = f
		}
	}
	if publish != nil {
		return publish(ctx, req, body, bundle)
	}
	return s.RestoreHistory(ctx, req, body, bundle)
}
func (m *Manager) HistoryRestore(ctx context.Context, req model.HistoryRestoreRequest) (out model.HistoryRestoreResult, err error) {
	spec, err := m.spec(ctx, req.MountID)
	if err != nil {
		return out, err
	}
	if spec.Profile.ID != req.ProfileID {
		return out, model.ErrIdentityChanged
	}
	if spec.Mount.Archived {
		return out, ErrArchived
	}
	err = m.withStore(ctx, req.MountID, "/history/restore", req, &out, func(s *store.Store) error {
		pending, e := offlineCachePending(ownershipFor(m.options.ConfigFile, spec))
		if e != nil {
			return e
		}
		if pending {
			return ErrPending
		}
		out, e = m.restoreHistoryStore(ctx, s, req, nil)
		return e
	})
	return
}
func (w *worker) restoreHistory(ctx context.Context, req model.HistoryRestoreRequest) (model.HistoryRestoreResult, error) {
	if req.MountID != w.spec.Mount.ID || req.ProfileID != w.spec.Profile.ID {
		return model.HistoryRestoreResult{}, model.ErrIdentityChanged
	}
	current, err := w.manager.ResolveMount(ctx, string(req.MountID))
	if err != nil {
		return model.HistoryRestoreResult{}, err
	}
	if current.Archived {
		return model.HistoryRestoreResult{}, ErrArchived
	}
	if w.services.restoreHistory == nil {
		return model.HistoryRestoreResult{}, errors.Join(ErrDriver, errors.New("mounted history publication unavailable"))
	}
	return w.manager.restoreHistoryStore(ctx, w.services.store, req, w.services.restoreHistory)
}

// Open one resource at a time while the enclosing version lease prevents expiry.
type historyResourceReader struct {
	open   func() (io.ReadCloser, error)
	reader io.ReadCloser
	done   bool
}

func (r *historyResourceReader) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	if r.reader == nil {
		f, err := r.open()
		if err != nil {
			return 0, err
		}
		r.reader = f
	}
	n, err := r.reader.Read(p)
	if err == io.EOF {
		closeErr := r.Close()
		if closeErr != nil {
			return n, closeErr
		}
	}
	return n, err
}
func (r *historyResourceReader) Close() error {
	if r.done {
		return nil
	}
	r.done = true
	if r.reader != nil {
		return r.reader.Close()
	}
	return nil
}
