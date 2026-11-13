package model

import (
	"encoding/json"
	"time"
)

// ManagedCheckpoint describes an already durable WAL save. CacheInfo is opaque
// to the Store; namespace recovery uses it to restore the named cache binding
// without copying or hashing the saved body.
type ManagedCheckpoint struct {
	Journal   string          `json:"journal"`
	Sequence  uint64          `json:"sequence"`
	Remote    string          `json:"remote"`
	Size      int64           `json:"size"`
	ModTime   time.Time       `json:"mod_time"`
	CacheInfo json.RawMessage `json:"cache_info,omitempty"`
}
