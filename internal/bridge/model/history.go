package model

import "time"

// HistoryEvent is an immutable, source-ordered capture. Bodies reference durable
// Store blobs until an archive receipt has been committed by their owning Store.
type HistoryEvent struct {
	ConflictRemoteSnapshot *Snapshot         `json:"conflict_remote_snapshot,omitempty"`
	RestoredFrom           string            `json:"restored_from,omitempty"`
	RemoteSnapshot         *Snapshot         `json:"remote_snapshot,omitempty"`
	LocalSnapshot          *Snapshot         `json:"local_snapshot,omitempty"`
	ID                     string            `json:"id"`
	Source                 string            `json:"source"`
	Sequence               uint64            `json:"sequence"`
	ProfileID              ProfileID         `json:"profile_id"`
	MountID                MountID           `json:"mount_id"`
	IdentityHash           string            `json:"identity_hash"`
	Entry                  Entry             `json:"entry"`
	Kind                   string            `json:"kind"`
	At                     time.Time         `json:"at"`
	Deleted                bool              `json:"deleted"`
	Bodies                 map[string]BlobID `json:"bodies"`
	Media                  map[string]BlobID `json:"media,omitempty"`
	Conflict               *Conflict         `json:"conflict,omitempty"`
	Archived               bool              `json:"archived"`
}

func (e HistoryEvent) Blobs() []BlobID {
	seen := map[BlobID]bool{}
	var out []BlobID
	for _, refs := range []map[string]BlobID{e.Bodies, e.Media} {
		for _, b := range refs {
			if b != "" && !seen[b] {
				seen[b] = true
				out = append(out, b)
			}
		}
	}
	return out
}

type HistoryStatus struct {
	MaintenanceError string    `json:"maintenance_error,omitempty"`
	Bytes            int64     `json:"bytes"`
	BudgetBytes      int64     `json:"budget_bytes"`
	OverBudget       bool      `json:"over_budget"`
	UsageAt          time.Time `json:"usage_at,omitempty"`
	State            string    `json:"state"`
	Pending          uint64    `json:"pending"`
	Archived         uint64    `json:"archived"`
	OldestAt         time.Time `json:"oldest_at,omitempty"`
	LastError        string    `json:"last_error,omitempty"`
}

type HistoryRestoreRequest struct {
	ProfileID          ProfileID `json:"profile_id"`
	MountID            MountID   `json:"mount_id"`
	EntryID            EntryID   `json:"entry_id"`
	VersionID          string    `json:"version_id"`
	Role               string    `json:"role,omitempty"`
	RequestID          string    `json:"request_id"`
	ExpectedGeneration uint64    `json:"expected_generation"`
	ExpectedPath       string    `json:"expected_path"`
	ExpectedDeleted    bool      `json:"expected_deleted"`
	ParentID           EntryID   `json:"parent_id"`
	ParentGeneration   uint64    `json:"parent_generation"`
}
type HistoryRestoreResult struct {
	Entry      Entry `json:"entry"`
	Job        Job   `json:"job"`
	LocalSaved bool  `json:"local_saved"`
}

type HistoryPolicy struct {
	KeepDays        int   `json:"keep_days"`
	MinimumVersions int   `json:"minimum_versions"`
	MaxBytes        int64 `json:"max_bytes"`
}

func DefaultHistoryPolicy() HistoryPolicy {
	return HistoryPolicy{KeepDays: 30, MinimumVersions: 10, MaxBytes: 5 << 30}
}
func (p HistoryPolicy) Valid() bool {
	return p.KeepDays >= 1 && p.KeepDays <= 36500 && p.MinimumVersions >= 1 && p.MinimumVersions <= 100000 && p.MaxBytes >= 1<<20
}

type HistoryProtection struct {
	Source       string   `json:"source"`
	Acknowledged uint64   `json:"acknowledged"`
	Versions     []string `json:"versions"`
}
