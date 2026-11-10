package manager

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"time"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/platform"
)

// DesktopMount joins the configured mount with its durable recovery intent.
// A running mount is not necessarily desired: workers outlive the desktop shell.
type DesktopMount struct {
	Mount   model.Mount
	Desired bool
}

type desktopState struct {
	SchemaVersion int                    `json:"schema_version"`
	ConfigFile    string                 `json:"config_file"`
	Desired       map[model.MountID]bool `json:"desired"`
	Legacy        []model.MountID        `json:"legacy_startup_pending,omitempty"`
}

func (m *Manager) readDesktopState() (*desktopState, error) {
	path := m.ConfigFile() + ".desktop.json"
	if err := platform.CheckNoSymlinks(path); err != nil {
		return nil, err
	}
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var state desktopState
	if len(raw) > 1<<20 || json.Unmarshal(raw, &state) != nil || state.SchemaVersion != 1 || state.ConfigFile != m.ConfigFile() || state.Desired == nil {
		return nil, ErrConfig
	}
	for id := range state.Desired {
		if !validID(string(id)) {
			return nil, ErrConfig
		}
	}
	for _, id := range state.Legacy {
		if !validID(string(id)) {
			return nil, ErrConfig
		}
	}
	return &state, nil
}
func (m *Manager) writeDesktopState(state *desktopState) error {
	return atomicJSON(m.ConfigFile()+".desktop.json", state)
}
func (m *Manager) desktopIntentLock(ctx context.Context) (func(), error) {
	lock, err := privateLock(m.ConfigFile() + ".desktop.lock")
	if err != nil {
		return nil, err
	}
	ok, err := lock.TryLockContext(ctx, 5*time.Millisecond)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrPending
	}
	return func() { _ = lock.Unlock() }, nil
}

func (m *Manager) DesktopMounts(ctx context.Context) ([]DesktopMount, error) {
	c, err := m.LoadConfig(ctx)
	if err != nil {
		return nil, err
	}
	state, err := m.readDesktopState()
	if err != nil {
		return nil, err
	}
	out := make([]DesktopMount, 0, len(c.Mounts))
	for _, mount := range c.Mounts {
		desired := mount.Autostart
		if state != nil {
			desired = state.Desired[mount.ID]
		}
		out = append(out, DesktopMount{Mount: mount, Desired: desired})
	}
	return out, nil
}

// EnableDesktop migrates the existing per-mount login intent exactly once. The
// durable pending list lets a failed registration removal be retried safely.
func (m *Manager) EnableDesktop(ctx context.Context) error {
	return m.enableDesktop(ctx, platform.SetStartup)
}
func (m *Manager) enableDesktop(ctx context.Context, startup func(context.Context, platform.StartupSpec, bool) error) error {
	release, err := m.startLease(ctx)
	if err != nil {
		return err
	}
	defer release()
	unlock, err := m.desktopIntentLock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	c, err := m.LoadConfig(ctx)
	if err != nil {
		return err
	}
	state, err := m.readDesktopState()
	if err != nil {
		return err
	}
	if state == nil {
		state = &desktopState{SchemaVersion: 1, ConfigFile: m.ConfigFile(), Desired: map[model.MountID]bool{}}
		for _, mount := range c.Mounts {
			state.Desired[mount.ID] = mount.Autostart
			if mount.Autostart {
				state.Legacy = append(state.Legacy, mount.ID)
			}
		}
		// Preserve the old intent before making any OS registration changes.
		if err = m.writeDesktopState(state); err != nil {
			return err
		}
	}
	for len(state.Legacy) > 0 {
		id := state.Legacy[0]
		if err = startup(ctx, platform.StartupSpec{ID: string(id), Executable: m.options.Executable}, false); err != nil {
			return err
		}
		state.Legacy = state.Legacy[1:]
		// Save each successful removal before moving to the next registration;
		// retrying a later failure must not repeat already completed OS changes.
		if err = m.writeDesktopState(state); err != nil {
			return err
		}
	}
	return nil
}

// desktopIntent is called while holding the operation lock, always after the
// lifecycle admission lease. Keeping that lock through Start/Stop makes a stop
// win over any automatic start that had already read the old desired value.
func (m *Manager) desktopIntent(ctx context.Context, id model.MountID, desired *bool, required bool) (bool, error) {
	if _, err := m.ResolveMount(ctx, string(id)); err != nil {
		return false, err
	}
	state, err := m.readDesktopState()
	if err != nil {
		return false, err
	}
	if state == nil {
		if required {
			return false, ErrConfig
		}
		return true, nil // Pure CLI mode keeps its existing behavior.
	}
	if desired != nil {
		state.Desired[id] = *desired
		if err = m.writeDesktopState(state); err != nil {
			return false, err
		}
	}
	return state.Desired[id], nil
}
func (m *Manager) SetDesktopDesired(ctx context.Context, id model.MountID, enabled bool) error {
	release, err := m.startLease(ctx)
	if err != nil {
		return err
	}
	defer release()
	unlock, err := m.desktopIntentLock(ctx)
	if err != nil {
		return err
	}
	defer unlock()
	_, err = m.desktopIntent(ctx, id, &enabled, true)
	return err
}

func (m *Manager) StartDesktopMount(ctx context.Context, id model.MountID) (model.MountStatus, error) {
	return m.startWithDesktopIntent(ctx, id, true)
}
func (m *Manager) startWithDesktopIntent(ctx context.Context, id model.MountID, automatic bool) (model.MountStatus, error) {
	readyCtx, cancel := context.WithTimeout(ctx, m.options.ReadyTimeout)
	defer cancel()
	release, err := m.startLease(readyCtx)
	if err != nil {
		return model.MountStatus{}, err
	}
	defer release()
	unlock, err := m.desktopIntentLock(readyCtx)
	if err != nil {
		return model.MountStatus{}, err
	}
	defer unlock()
	var desired *bool
	if !automatic {
		enabled := true
		desired = &enabled
	}
	enabled, err := m.desktopIntent(readyCtx, id, desired, automatic)
	if err != nil {
		return model.MountStatus{}, err
	}
	if !enabled {
		return m.Status(readyCtx, id)
	}
	state, err := m.startAdmitted(readyCtx, id)
	if errors.Is(err, context.DeadlineExceeded) && ctx.Err() == nil {
		err = ErrReadyTimeout
	}
	return state, err
}
