package ui

import (
	"context"
	"sync"
	"time"

	"github.com/rclone/rclone/internal/bridge/manager"
	"github.com/rclone/rclone/internal/bridge/model"
)

type statusReader interface {
	Status(context.Context, model.MountID) (model.MountStatus, error)
	Conflicts(context.Context, model.MountID) ([]manager.ConflictView, error)
}
type mountSnapshot struct {
	state     model.MountStatus
	conflicts []ConflictSummary
	updatedAt string
}

// Status is an observation, not a cloud verification. Offline stores require an
// ownership lock even for inspection. Keep the last complete per-mount sample
// when another reader owns that lock; never report a synthetic clean result.
// Concurrent tabs share the current observation instead of multiplying reads.
type statusSnapshots struct {
	refresh sync.Mutex
	mu      sync.Mutex
	mounts  map[model.MountID]mountSnapshot
}

func (s *statusSnapshots) read(ctx context.Context, mounts []MountView, reader statusReader) StatusView {
	refresh := s.refresh.TryLock()
	if refresh {
		defer s.refresh.Unlock()
	}
	result := StatusView{SchemaVersion: 1, Mounts: append([]MountView{}, mounts...), Conflicts: []ConflictSummary{}}
	for i, mount := range result.Mounts {
		var next mountSnapshot
		warning := "pending"
		if refresh {
			state, err := reader.Status(ctx, mount.ID)
			if err == nil {
				var conflicts []manager.ConflictView
				conflicts, err = reader.Conflicts(ctx, mount.ID)
				if err == nil {
					next = mountSnapshot{state: state, updatedAt: time.Now().UTC().Format(time.RFC3339), conflicts: []ConflictSummary{}}
					for _, c := range conflicts {
						next.conflicts = append(next.conflicts, ConflictSummary{SourceRevision: conflictSourceRevision(ConflictInputs{Conflict: c.Conflict, Entry: c.Entry, SourceURL: c.SourceURL}), Revision: conflictRevision(ConflictInputs{Conflict: c.Conflict, Entry: c.Entry, SourceURL: c.SourceURL}), ID: c.Conflict.ID, MountID: mount.ID, Name: c.Entry.Remote})
					}
				}
			}
			if err == nil {
				warning = ""
			} else {
				warning = statusWarningCode(err)
			}
		}
		s.mu.Lock()
		if s.mounts == nil {
			s.mounts = map[model.MountID]mountSnapshot{}
		}
		if warning == "" {
			s.mounts[mount.ID] = next
		} else {
			var found bool
			next, found = s.mounts[mount.ID]
			if !found {
				next.state = model.MountStatus{SchemaVersion: 1, MountID: mount.ID, CloudState: "unknown"}
			}
		}
		s.mu.Unlock()
		result.Mounts[i].State = next.state
		result.Mounts[i].StatusUpdatedAt = next.updatedAt
		result.Mounts[i].StatusWarning = warning
		result.Partial = result.Partial || warning != ""
		result.Conflicts = append(result.Conflicts, next.conflicts...)
	}
	if refresh {
		keep := map[model.MountID]bool{}
		for _, m := range mounts {
			keep[m.ID] = true
		}
		s.mu.Lock()
		for id := range s.mounts {
			if !keep[id] {
				delete(s.mounts, id)
			}
		}
		s.mu.Unlock()
	}
	return result
}

// Keep actionable account failures distinct from temporary local observation work.
func statusWarningCode(err error) string {
	switch code := manager.ErrorInfo(err).Code; code {
	case "unauthorized":
		return "auth_required"
	case "network":
		return "worker_unavailable"
	default:
		return safeCode(code)
	}
}
