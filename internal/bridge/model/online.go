package model

import "time"

// OnlineFailure contains only safe diagnostic facts. It is not authorization to
// dispatch a mutation; the operation journal remains the authority for that.
type OnlineFailure struct {
	SchemaVersion     int       `json:"schema_version"`
	ID                string    `json:"diagnostic_id"`
	Operation         string    `json:"operation"`
	Phase             string    `json:"phase"`
	Cause             string    `json:"cause"`
	Outcome           string    `json:"outcome"`
	Action            string    `json:"action"`
	APICode           string    `json:"api_code,omitempty"`
	At                time.Time `json:"at"`
	RetryAfterSeconds int64     `json:"retry_after_seconds,omitempty"`
	CancelOrigin      string    `json:"cancel_origin,omitempty"`
}

// OnlineRecord supplements the existing operation history. LegacyAttempts must
// never be reconstructed as effective observations or failed requests.
type OnlineRecord struct {
	FirstMissing    bool            `json:"first_missing,omitempty"`
	First           *OnlineFailure  `json:"first,omitempty"`
	Last            *OnlineFailure  `json:"last,omitempty"`
	History         []OnlineFailure `json:"history,omitempty"`
	RequestFailures int             `json:"request_failures,omitempty"`
	EffectiveChecks int             `json:"effective_checks,omitempty"`
	LegacyAttempts  int             `json:"legacy_attempts,omitempty"`
	Resolved        bool            `json:"resolved,omitempty"`
	CycleID         string          `json:"cycle_id,omitempty"`
	PausedReason    string          `json:"paused_reason,omitempty"`
	NotBefore       time.Time       `json:"not_before,omitempty"`
}
