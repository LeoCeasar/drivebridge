package overlay

import (
	"context"
	"github.com/rclone/rclone/internal/bridge/model"
	"sort"
	"time"
)

const ActivityWindow = 5 * time.Minute

func (f *Fs) touch(e model.Entry) {
	if e.Kind == model.KindFolder || e.Visible == "" {
		return
	}
	f.activityMu.Lock()
	f.active[e.ID] = time.Now()
	f.activityMu.Unlock()
}

// ActiveEntries is a bounded recent-access set; it never materializes content.
func (f *Fs) ActiveEntries() []model.EntryID {
	now := time.Now()
	f.activityMu.Lock()
	var ids []model.EntryID
	for id, at := range f.active {
		if now.Sub(at) > ActivityWindow {
			delete(f.active, id)
		} else {
			ids = append(ids, id)
		}
	}
	f.activityMu.Unlock()
	var result []model.EntryID
	for _, id := range ids {
		e, err := f.store.Entry(context.Background(), id)
		if err != nil || e.Visible == "" || e.Kind == model.KindFolder {
			continue
		}
		m, err := f.store.Metadata(context.Background(), id)
		if err == nil && !m.Tombstone && m.VirtualType == "" {
			result = append(result, id)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result
}
