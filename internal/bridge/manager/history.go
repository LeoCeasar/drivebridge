package manager

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/rclone/rclone/internal/bridge/history"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/platform"
	"github.com/rclone/rclone/internal/bridge/store"
)

type HistoryQuery struct {
	ProfileID model.ProfileID `json:"profile_id,omitempty"`
	Cursor    string          `json:"cursor,omitempty"`
	history.Query
}
type HistoryWarning struct {
	ProfileID model.ProfileID `json:"profile_id"`
	Code      string          `json:"code"`
}
type HistoryPage struct {
	Versions []history.Version `json:"versions"`
	Cursor   string            `json:"cursor,omitempty"`
	Warnings []HistoryWarning  `json:"warnings,omitempty"`
}

type historyRepositoryCache struct {
	Dir, Identity, Git string
	Repo               *history.Repository
}

func (m *Manager) historyRepository(ctx context.Context, profile model.ProfileID) (*history.Repository, error) {
	c, err := m.LoadConfig(ctx)
	if err != nil {
		return nil, err
	}
	paths, err := m.paths(c)
	if err != nil {
		return nil, err
	}
	for _, p := range c.Profiles {
		if p.ID == profile {
			if p.IdentityHash == "" {
				return nil, model.ErrIdentityChanged
			}
			dir := filepath.Join(paths.StateDir, "history", string(p.ID))
			m.mu.Lock()
			cached, ok := m.historyRepositories[p.ID]
			m.mu.Unlock()
			if ok && cached.Dir == dir && cached.Identity == p.IdentityHash && cached.Git == m.options.GitExecutable {
				return cached.Repo, nil
			}
			repo, e := history.Open(ctx, history.Options{Dir: dir, ProfileID: p.ID, IdentityHash: p.IdentityHash, GitExecutable: m.options.GitExecutable})
			if e != nil {
				return nil, e
			}
			m.mu.Lock()
			if m.historyRepositories == nil {
				m.historyRepositories = map[model.ProfileID]historyRepositoryCache{}
			}
			m.historyRepositories[p.ID] = historyRepositoryCache{dir, p.IdentityHash, m.options.GitExecutable, repo}
			m.mu.Unlock()
			return repo, nil
		}
	}
	return nil, ErrConfig
}
func historyError(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, history.ErrGitUnavailable):
		return "git_unavailable"
	case errors.Is(err, history.ErrIntegrity):
		return "history_integrity"
	case errors.Is(err, history.ErrBodyUnavailable):
		return "body_not_cached"
	case errors.Is(err, model.ErrIdentityChanged):
		return "identity_changed"
	case errors.Is(err, context.DeadlineExceeded):
		return "history_timeout"
	default:
		return "history_unavailable"
	}
}
func (w *worker) runHistory(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		wake := w.services.store.HistoryWake()
		state, err := w.services.store.HistoryStatus(ctx)
		delay := 30 * time.Second
		if err == nil && state.Pending > 0 {
			var repo *history.Repository
			repo, err = w.manager.historyRepository(ctx, w.spec.Profile.ID)
			if err == nil {
				_, err = repo.Ingest(ctx, w.services.store)
			}
			if updated, e := w.services.store.HistoryStatus(ctx); e == nil {
				state = updated
			}
			if state.Pending > 0 {
				delay = 100 * time.Millisecond
			}
		}
		w.mu.Lock()
		previous := w.historyState
		w.mu.Unlock()
		state.Bytes, state.BudgetBytes, state.OverBudget, state.UsageAt = previous.Bytes, previous.BudgetBytes, previous.OverBudget, previous.UsageAt
		if err == nil && (previous.UsageAt.IsZero() || time.Since(previous.UsageAt) > time.Minute) {
			used, e := w.manager.historyUsage(ctx, w.spec.Profile.ID)
			if e == nil {
				policy, pErr := w.manager.HistoryPolicy(ctx, w.spec.Profile.ID)
				if pErr == nil {
					state.Bytes = used
					state.BudgetBytes = policy.MaxBytes
					state.OverBudget = used > policy.MaxBytes
					state.UsageAt = time.Now().UTC()
				}
			} else if state.Pending > 0 {
				err = e
			}
		}
		w.mu.Lock()
		ready := w.ready
		state.MaintenanceError = w.historyState.MaintenanceError
		w.mu.Unlock()
		if err == nil && state.Pending == 0 && ready {
			state.MaintenanceError = w.maintainHistory(ctx)
		}
		if err != nil {
			state.State = "error"
			state.LastError = historyError(err)
			delay = 5 * time.Second
		}
		w.mu.Lock()
		stateChanged := w.historyState != state
		w.historyState = state
		w.mu.Unlock()
		if ctx.Err() != nil {
			return
		}
		if stateChanged {
			_ = atomicJSON(filepath.Join(w.spec.Paths.Root, "history-state.json"), state)
		}
		timer := time.NewTimer(delay)
		if err != nil {
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		} else {
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-wake:
				timer.Stop()
			case <-timer.C:
			}
		}
	}
}
func (m *Manager) HistoryList(ctx context.Context, q HistoryQuery) (out HistoryPage, err error) {
	out.Versions = []history.Version{}
	if q.Limit <= 0 || q.Limit > 200 {
		q.Limit = 100
	}
	positions := map[string]uint64{}
	rawFilter, _ := json.Marshal(struct {
		Profile model.ProfileID
		Query   history.Query
	}{q.ProfileID, q.Query})
	filterHash := sha256.Sum256(rawFilter)
	signature := hex.EncodeToString(filterHash[:])
	type cursorData struct {
		Positions map[string]uint64 `json:"positions"`
		Filter    string            `json:"filter"`
	}

	if q.Cursor != "" {
		if len(q.Cursor) > 8192 {
			return out, ErrConfig
		}
		b, e := base64.RawURLEncoding.DecodeString(q.Cursor)
		var cursor cursorData
		if e != nil || json.Unmarshal(b, &cursor) != nil || cursor.Filter != signature || cursor.Positions == nil {
			return out, ErrConfig
		}
		positions = cursor.Positions
		for p := range positions {
			if !validID(p) {
				return out, ErrConfig
			}
		}
	}
	c, err := m.LoadConfig(ctx)
	if err != nil {
		return out, err
	}
	found := q.ProfileID == ""
	var all []history.Version
	for _, p := range c.Profiles {
		if q.ProfileID != "" && p.ID != q.ProfileID {
			continue
		}
		found = true
		if q.Cursor != "" {
			if _, seen := positions[string(p.ID)]; !seen {
				continue
			}
		}
		if p.IdentityHash == "" {
			continue
		}
		repo, e := m.historyRepository(ctx, p.ID)
		if e != nil {
			out.Warnings = append(out.Warnings, HistoryWarning{p.ID, historyError(e)})
			continue
		}
		query := q.Query
		query.Before = positions[string(p.ID)]
		page, e := repo.List(ctx, query)
		if e != nil {
			out.Warnings = append(out.Warnings, HistoryWarning{p.ID, historyError(e)})
			continue
		}
		if q.Cursor == "" {
			positions[string(p.ID)] = page.Upper + 1
		}
		all = append(all, page.Versions...)
	}
	if !found {
		return out, ErrConfig
	}
	sort.Slice(all, func(i, j int) bool {
		if all[i].RecordedAt.Equal(all[j].RecordedAt) {
			return all[i].ID > all[j].ID
		}
		return all[i].RecordedAt.After(all[j].RecordedAt)
	})
	more := len(all) >= q.Limit
	if len(all) > q.Limit {
		all = all[:q.Limit]
	}
	out.Versions = all
	for _, v := range all {
		positions[string(v.Event.ProfileID)] = v.Order
	}
	if more {
		b, _ := json.Marshal(cursorData{positions, signature})
		out.Cursor = base64.RawURLEncoding.EncodeToString(b)
	}
	return out, nil
}
func (m *Manager) HistoryBody(ctx context.Context, p model.ProfileID, id, role string) ([]byte, error) {
	if v, e := m.localCapturedVersion(ctx, p, id); e == nil && v.Pending {
		if body, e := m.localCapturedBody(ctx, p, id, role); e == nil || errors.Is(e, history.ErrLimit) {
			return body, e
		}
	}
	r, err := m.historyRepository(ctx, p)
	if err == nil {
		var b []byte
		b, err = r.Read(ctx, id, role)
		if err == nil {
			return b, nil
		}
	}
	if canReadLocalHistory(err) {
		if b, e := m.localCapturedBody(ctx, p, id, role); e == nil || errors.Is(e, history.ErrLimit) {
			return b, e
		}
	}
	return nil, err
}
func (m *Manager) HistoryVersion(ctx context.Context, p model.ProfileID, id string) (history.Version, error) {
	if v, e := m.localCapturedVersion(ctx, p, id); e == nil && v.Pending {
		return v, nil
	}
	r, err := m.historyRepository(ctx, p)
	if err == nil {
		v, e := r.Version(ctx, id)
		if e == nil {
			return v, nil
		}
		err = e
	}
	if canReadLocalHistory(err) {
		if v, e := m.localCapturedVersion(ctx, p, id); e == nil {
			return v, nil
		}
	}
	return history.Version{}, err
}
func (m *Manager) HistoryDiff(ctx context.Context, p model.ProfileID, left, right string) (history.Difference, error) {
	a, err := m.HistoryVersion(ctx, p, left)
	if err != nil {
		return history.Difference{}, err
	}
	b, err := m.HistoryVersion(ctx, p, right)
	if err != nil {
		return history.Difference{}, err
	}
	repo, gitErr := m.historyRepository(ctx, p)
	if gitErr == nil && !a.Pending && !b.Pending {
		return repo.Diff(ctx, left, right)
	}
	av, err := m.HistoryBody(ctx, p, left, "")
	if err != nil {
		return history.Difference{}, err
	}
	bv, err := m.HistoryBody(ctx, p, right, "")
	if err != nil {
		return history.Difference{}, err
	}
	if gitErr == nil {
		if diff, e := repo.DiffContent(ctx, av, bv); e == nil {
			return diff, nil
		}
	}
	return history.DiffBytes(av, bv)
}

func (m *Manager) HistorySync(ctx context.Context, id model.MountID) (out history.IngestResult, err error) {
	release, e := m.startLease(ctx)
	if e != nil {
		return out, e
	}
	defer release()
	spec, err := m.spec(ctx, id)
	if err != nil {
		return out, err
	}
	err = m.withStore(ctx, id, "/history/sync", struct{}{}, &out, func(s *store.Store) error {
		r, e := m.historyRepository(ctx, spec.Profile.ID)
		if e != nil {
			return e
		}
		out, e = r.Ingest(ctx, s)
		return e
	})
	return
}
func (m *Manager) HistoryStates(ctx context.Context) (map[model.MountID]model.HistoryStatus, error) {
	c, err := m.LoadConfig(ctx)
	if err != nil {
		return nil, err
	}
	out := map[model.MountID]model.HistoryStatus{}
	for _, mount := range c.Mounts {
		var state model.HistoryStatus
		e := m.withStore(ctx, mount.ID, "/history/status", nil, &state, func(s *store.Store) error { var err error; state, err = s.HistoryStatus(ctx); return err })
		if errors.Is(e, errNoState) {
			continue
		}
		if e != nil {
			state.State = "error"
			state.LastError = historyError(e)
		}
		out[mount.ID] = state
	}
	return out, nil
}
func readHistoryState(root string, out *model.MountStatus) {
	p := filepath.Join(root, "history-state.json")
	if platform.CheckNoSymlinks(p) != nil {
		return
	}
	b, err := os.ReadFile(p)
	if err != nil || len(b) > 16384 {
		return
	}
	var state model.HistoryStatus
	if json.Unmarshal(b, &state) == nil {
		out.History = &state
	}
}

func (m *Manager) HistoryOpenBody(ctx context.Context, p model.ProfileID, id, role string) (io.ReadCloser, error) {
	if v, e := m.localCapturedVersion(ctx, p, id); e == nil && v.Pending {
		b, e := m.localCapturedBody(ctx, p, id, role)
		if e == nil || errors.Is(e, history.ErrLimit) {
			return pendingBodyReader(b), e
		}
	}
	r, err := m.historyRepository(ctx, p)
	if err == nil {
		var stream io.ReadCloser
		stream, err = r.OpenBody(ctx, id, role)
		if err == nil {
			return stream, nil
		}
	}
	if canReadLocalHistory(err) {
		b, e := m.localCapturedBody(ctx, p, id, role)
		if e == nil {
			return pendingBodyReader(b), nil
		}
	}
	return nil, err
}

func (w *worker) historyStatus(ctx context.Context) (model.HistoryStatus, error) {
	state, err := w.services.store.HistoryStatus(ctx)
	if err != nil {
		return state, err
	}
	w.mu.Lock()
	cached := w.historyState
	w.mu.Unlock()
	state.Bytes, state.BudgetBytes, state.OverBudget, state.UsageAt = cached.Bytes, cached.BudgetBytes, cached.OverBudget, cached.UsageAt
	state.MaintenanceError = cached.MaintenanceError
	if cached.LastError != "" {
		state.LastError = cached.LastError
		state.State = "error"
	}
	return state, nil
}
