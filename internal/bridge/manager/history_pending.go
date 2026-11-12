package manager

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"sort"

	"github.com/rclone/rclone/internal/bridge/history"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
)

type historyLocalRequest struct {
	ID      string        `json:"id"`
	Role    string        `json:"role,omitempty"`
	EntryID model.EntryID `json:"entry_id,omitempty"`
	Search  string        `json:"search,omitempty"`
	Limit   int           `json:"limit,omitempty"`
}

func localHistoryVersion(ctx context.Context, s *store.Store, id string) (out history.Version, err error) {
	ev, err := s.HistoryEvent(ctx, id)
	if err != nil {
		return out, err
	}
	out = history.Version{ID: ev.ID, RecordedAt: ev.At, Event: ev, Pending: !ev.Archived, Bodies: map[string]history.Ref{}, Media: map[string]history.Ref{}}
	for _, group := range []struct {
		In  map[string]model.BlobID
		Out map[string]history.Ref
	}{{ev.Bodies, out.Bodies}, {ev.Media, out.Media}} {
		for role, b := range group.In {
			size := int64(-1)
			f, e := s.ReadBlob(ctx, b)
			if e == nil {
				if info, ok := f.(interface{ Stat() (os.FileInfo, error) }); ok {
					if stat, e := info.Stat(); e == nil {
						size = stat.Size()
					}
				}
				f.Close()
			}
			group.Out[role] = history.Ref{SHA256: string(b), Size: size}
		}
	}
	return
}
func localHistoryBody(ctx context.Context, s *store.Store, id, role string) ([]byte, error) {
	f, err := s.ReadHistoryBody(ctx, id, role)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, (10<<20)+1))
	if err != nil {
		return nil, err
	}
	if len(b) > 10<<20 {
		return nil, history.ErrLimit
	}
	return b, nil
}
func localPendingHistory(ctx context.Context, s *store.Store, in historyLocalRequest) (out []history.Version, err error) {
	events, err := s.HistoryRecent(ctx, in.EntryID, in.Search, in.Limit)
	if err != nil {
		return nil, err
	}
	for _, ev := range events {
		v, e := localHistoryVersion(ctx, s, ev.ID)
		if e != nil {
			return nil, e
		}
		if v.Pending {
			out = append(out, v)
		}
	}
	return
}
func (m *Manager) profileMounts(ctx context.Context, profile model.ProfileID) (out []model.Mount, err error) {
	c, err := m.LoadConfig(ctx)
	if err != nil {
		return nil, err
	}
	remotes := map[model.RemoteID]model.ProfileID{}
	profiles := map[model.ProfileID]bool{}
	for _, p := range c.Profiles {
		profiles[p.ID] = true
	}
	if profile != "" && !profiles[profile] {
		return nil, ErrConfig
	}
	for _, r := range c.Remotes {
		remotes[r.ID] = r.ProfileID
	}
	for _, mount := range c.Mounts {
		if profile == "" || remotes[mount.RemoteID] == profile {
			out = append(out, mount)
		}
	}
	return
}
func (m *Manager) localCapturedVersion(ctx context.Context, profile model.ProfileID, id string) (out history.Version, err error) {
	mounts, err := m.profileMounts(ctx, profile)
	if err != nil {
		return out, err
	}
	for _, mount := range mounts {
		err = m.withStore(ctx, mount.ID, "/history/version-local", historyLocalRequest{ID: id}, &out, func(s *store.Store) error { var err error; out, err = localHistoryVersion(ctx, s, id); return err })
		if err == nil {
			if out.Event.ProfileID != profile {
				return out, model.ErrIdentityChanged
			}
			return out, nil
		}
		if !errors.Is(err, store.ErrNotFound) && !errors.Is(err, errNoState) {
			return out, err
		}
	}
	return out, history.ErrBodyUnavailable
}
func (m *Manager) localCapturedBody(ctx context.Context, profile model.ProfileID, id, role string) ([]byte, error) {
	v, err := m.localCapturedVersion(ctx, profile, id)
	if err != nil {
		return nil, err
	}
	var body []byte
	err = m.withStore(ctx, v.Event.MountID, "/history/body-local", historyLocalRequest{ID: id, Role: role}, &body, func(s *store.Store) error { var err error; body, err = localHistoryBody(ctx, s, id, role); return err })
	return body, err
}
func (m *Manager) HistoryPending(ctx context.Context, q HistoryQuery) (out HistoryPage, err error) {
	out.Versions = []history.Version{}
	mounts, err := m.profileMounts(ctx, q.ProfileID)
	if err != nil {
		return out, err
	}
	for _, mount := range mounts {
		var rows []history.Version
		in := historyLocalRequest{EntryID: q.EntryID, Search: q.Search, Limit: 50}
		err = m.withStore(ctx, mount.ID, "/history/pending", in, &rows, func(s *store.Store) error { var err error; rows, err = localPendingHistory(ctx, s, in); return err })
		if errors.Is(err, errNoState) {
			continue
		}
		if err != nil {
			out.Warnings = append(out.Warnings, HistoryWarning{Code: historyError(err)})
			continue
		}
		out.Versions = append(out.Versions, rows...)
	}
	sort.Slice(out.Versions, func(i, j int) bool { return out.Versions[i].Event.At.After(out.Versions[j].Event.At) })
	if len(out.Versions) > 50 {
		out.Versions = out.Versions[:50]
	}
	return out, nil
}
func (m *Manager) pendingRestore(ctx context.Context, s *store.Store, req model.HistoryRestoreRequest, publish func(context.Context, model.HistoryRestoreRequest, io.Reader, *store.HistoryRestoreBundle) (model.HistoryRestoreResult, error)) (model.HistoryRestoreResult, error) {
	release, err := s.LeaseHistoryEvent(ctx, req.VersionID)
	if err != nil {
		return model.HistoryRestoreResult{}, err
	}
	defer release()
	ev, err := s.HistoryEvent(ctx, req.VersionID)
	if err != nil {
		return model.HistoryRestoreResult{}, err
	}
	if ev.Entry.ID != req.EntryID || ev.ProfileID != req.ProfileID || ev.MountID != req.MountID {
		return model.HistoryRestoreResult{}, model.ErrIdentityChanged
	}
	body, err := s.ReadHistoryBody(ctx, req.VersionID, req.Role)
	if err != nil {
		return model.HistoryRestoreResult{}, err
	}
	defer body.Close()
	var bundle *store.HistoryRestoreBundle
	snapshot := ev.RemoteSnapshot
	if req.Role == "conflict_remote" && ev.ConflictRemoteSnapshot != nil {
		snapshot = ev.ConflictRemoteSnapshot
	}
	if ev.LocalSnapshot != nil && (req.Role == "" || req.Role == "local" || req.Role == "draft") {
		snapshot = ev.LocalSnapshot
	}
	if ev.Entry.Kind == model.KindDocx && snapshot != nil {
		bundle = &store.HistoryRestoreBundle{Snapshot: snapshot}
	}
	if publish != nil {
		return publish(ctx, req, body, bundle)
	}
	return s.RestoreHistory(ctx, req, body, bundle)
}
func canReadLocalHistory(err error) bool {
	return err != nil && !errors.Is(err, model.ErrIdentityChanged) && !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}
func pendingBodyReader(b []byte) io.ReadCloser { return io.NopCloser(bytes.NewReader(b)) }
