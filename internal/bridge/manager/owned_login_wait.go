package manager

import (
	"context"
	"github.com/rclone/rclone/internal/bridge/model"
	"time"
)

// Verification runs only after an explicit login action, never on status polling.
func (m *Manager) watchDiscoveryLogin(id model.ProfileID) {
	m.discoveryMu.Lock()
	if m.discoveryClosing {
		m.discoveryMu.Unlock()
		return
	}
	if m.discoveryLogins == nil {
		m.discoveryLogins = map[model.ProfileID]context.CancelFunc{}
	}
	if old := m.discoveryLogins[id]; old != nil {
		old()
	}
	ctx, cancel := context.WithTimeout(m.ctx, 5*time.Minute)
	m.discoveryLogins[id] = cancel
	m.discoveryMu.Unlock()
	go func() {
		defer cancel()
		waitDiscoveryLogin(ctx, 3*time.Second, func(ctx context.Context) bool {
			view, _ := m.CompleteDiscoveryLogin(ctx, id)
			return view.Snapshot.Complete
		})
	}()
}

func waitDiscoveryLogin(ctx context.Context, interval time.Duration, verify func(context.Context) bool) {
	timer := time.NewTicker(interval)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			if ctx.Err() != nil {
				return
			}
			if verify(ctx) {
				return
			}
		}
	}
}
