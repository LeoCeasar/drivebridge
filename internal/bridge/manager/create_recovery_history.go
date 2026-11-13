package manager

import (
	"context"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
)

// Archive the finite history prefix that includes the reviewed local input.
// Later saves are protected by the recovery transaction's generation/hash guard.
func (m *Manager) archiveCreationRecovery(ctx context.Context, profile model.ProfileID, s *store.Store, en model.Entry) error {
	current, err := s.Entry(ctx, en.ID)
	if err != nil {
		return err
	}
	if current.Generation != en.Generation || current.Local != en.Local || current.Remote != en.Remote {
		return model.ErrVersionChanged
	}
	state, err := s.HistoryStatus(ctx)
	if err != nil {
		return err
	}
	target := state.Archived + state.Pending
	repo, err := m.historyRepository(ctx, profile)
	if err != nil {
		return err
	}
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		result, err := repo.Ingest(ctx, s)
		if err != nil {
			return err
		}
		state, err = s.HistoryStatus(ctx)
		if err != nil {
			return err
		}
		if state.Archived >= target {
			return nil
		}
		if result.Acknowledged == 0 {
			return ErrHistoryPending
		}
	}
}
