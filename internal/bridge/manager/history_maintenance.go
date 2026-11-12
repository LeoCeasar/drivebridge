package manager

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/rclone/rclone/internal/bridge/config"
	"github.com/rclone/rclone/internal/bridge/history"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/platform"
	"github.com/rclone/rclone/internal/bridge/store"
)

func (m *Manager) HistoryPolicy(ctx context.Context, id model.ProfileID) (model.HistoryPolicy, error) {
	c, err := m.LoadConfig(ctx)
	if err != nil {
		return model.HistoryPolicy{}, err
	}
	for _, p := range c.Profiles {
		if p.ID == id {
			if p.History != nil {
				return *p.History, nil
			}
			return model.DefaultHistoryPolicy(), nil
		}
	}
	return model.HistoryPolicy{}, ErrConfig
}
func (m *Manager) SetHistoryPolicy(ctx context.Context, id model.ProfileID, p model.HistoryPolicy) error {
	if !p.Valid() {
		return ErrConfig
	}
	return m.admittedConfigUpdate(ctx, func(c *config.Config) error {
		for i := range c.Profiles {
			if c.Profiles[i].ID == id {
				c.Profiles[i].History = &p
				return nil
			}
		}
		return ErrConfig
	})
}
func (m *Manager) HistoryPin(ctx context.Context, profile model.ProfileID, id string, pin bool) error {
	release, err := m.startLease(ctx)
	if err != nil {
		return err
	}
	defer release()
	r, err := m.historyRepository(ctx, profile)
	if err != nil {
		return err
	}
	return r.Pin(ctx, id, pin)
}
func (m *Manager) historyProtections(ctx context.Context, profile model.ProfileID) (map[string]uint64, map[string]bool, error) {
	c, err := m.LoadConfig(ctx)
	if err != nil {
		return nil, nil, err
	}
	acks := map[string]uint64{}
	versions := map[string]bool{}
	remotes := map[model.RemoteID]model.ProfileID{}
	for _, r := range c.Remotes {
		remotes[r.ID] = r.ProfileID
	}
	for _, mount := range c.Mounts {
		if remotes[mount.RemoteID] != profile {
			continue
		}
		var p model.HistoryProtection
		err = m.withStore(ctx, mount.ID, "/history/protection", nil, &p, func(s *store.Store) error { var e error; p, e = s.HistoryProtection(ctx); return e })
		if errors.Is(err, errNoState) {
			continue
		}
		if err != nil {
			return nil, nil, err
		}
		acks[p.Source] = p.Acknowledged
		for _, id := range p.Versions {
			versions[id] = true
		}
	}
	return acks, versions, nil
}
func (m *Manager) HistoryCompact(ctx context.Context, profile model.ProfileID) (history.CompactResult, error) {
	release, err := m.startLease(ctx)
	if err != nil {
		return history.CompactResult{}, err
	}
	defer release()
	policy, err := m.HistoryPolicy(ctx, profile)
	if err != nil {
		return history.CompactResult{}, err
	}
	acks, protected, err := m.historyProtections(ctx, profile)
	if err != nil {
		return history.CompactResult{}, err
	}
	r, err := m.historyRepository(ctx, profile)
	if err != nil {
		return history.CompactResult{}, err
	}
	result, err := r.Compact(ctx, history.RetentionPolicy{Now: time.Now().UTC(), Keep: time.Duration(policy.KeepDays) * 24 * time.Hour, MinimumVersions: policy.MinimumVersions, Acknowledged: acks, Protected: protected})
	if err == nil {
		_, err = r.Clean(ctx, time.Now().UTC())
	}
	return result, err
}
func (w *worker) maintainHistory(ctx context.Context) string {
	root := filepath.Join(filepath.Dir(filepath.Dir(w.spec.Paths.Root)), "history", string(w.spec.Profile.ID))
	name := filepath.Join(root, "maintenance.json")
	if platform.CheckNoSymlinks(name) != nil {
		return "history_integrity"
	}
	policy, err := w.manager.HistoryPolicy(ctx, w.spec.Profile.ID)
	if err != nil {
		return historyError(err)
	}
	var last struct {
		At     time.Time           `json:"at"`
		Policy model.HistoryPolicy `json:"policy"`
		Error  string              `json:"error,omitempty"`
	}
	if b, e := os.ReadFile(name); e == nil && len(b) < 16384 {
		if json.Unmarshal(b, &last) != nil {
			return "history_integrity"
		}
		interval := 24 * time.Hour
		if last.Error != "" {
			interval = time.Hour
		}
		if time.Since(last.At) >= 0 && time.Since(last.At) < interval && last.Policy == policy {
			return last.Error
		}
	}
	_, err = w.manager.HistoryCompact(ctx, w.spec.Profile.ID)
	if ctx.Err() != nil {
		return ""
	}
	last.At = time.Now().UTC()
	last.Policy = policy
	last.Error = historyError(err)
	if e := atomicJSON(name, last); e != nil {
		return historyError(e)
	}
	return last.Error
}

func (m *Manager) historyUsage(ctx context.Context, profile model.ProfileID) (bytes int64, err error) {
	r, err := m.historyRepository(ctx, profile)
	if err != nil {
		return 0, err
	}
	bytes, err = r.Usage(ctx)
	if err != nil {
		return
	}
	c, err := m.LoadConfig(ctx)
	if err != nil {
		return
	}
	remotes := map[model.RemoteID]model.ProfileID{}
	for _, r := range c.Remotes {
		remotes[r.ID] = r.ProfileID
	}
	for _, mount := range c.Mounts {
		if remotes[mount.RemoteID] != profile {
			continue
		}
		var pending int64
		var e error
		if local := m.localWorker(mount.ID); local != nil {
			local.mu.Lock()
			service := local.services
			local.mu.Unlock()
			if service != nil {
				pending, e = service.store.PendingHistoryBytes(ctx)
			}
		} else {
			e = m.withStore(ctx, mount.ID, "/history/pending-bytes", nil, &pending, func(s *store.Store) error { var e error; pending, e = s.PendingHistoryBytes(ctx); return e })
		}
		if errors.Is(e, errNoState) {
			continue
		}
		if e != nil {
			return bytes, e
		}
		bytes += pending
	}
	return
}

// RunHistoryMaintenance also consumes outboxes of stopped/archived mappings.
// It does not mount anything or acquire cloud credentials.
func (m *Manager) RunHistoryMaintenance(ctx context.Context) {
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		m.retryRemovals(ctx)
		c, err := m.LoadConfig(ctx)
		if err == nil {
			for _, mount := range c.Mounts {
				if ctx.Err() != nil {
					return
				}
				var state model.MountStatus
				probe := m.call(ctx, mount.ID, "/status", nil, &state)
				if probe == nil {
					continue
				}
				if !os.IsNotExist(probe) {
					continue
				}
				batchCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
				_, _ = m.HistorySync(batchCtx, mount.ID)
				cancel()
			}
		}
		timer.Reset(30 * time.Second)
	}
}
