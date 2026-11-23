package store

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/model"
	bolt "go.etcd.io/bbolt"
)

func retainOnline(record *model.OnlineRecord, failure model.OnlineFailure) *model.OnlineRecord {
	if record == nil {
		record = &model.OnlineRecord{}
	}
	if record.First == nil && !record.FirstMissing {
		first := failure
		record.First = &first
	}
	changed := record.Last == nil || record.Last.Phase != failure.Phase || record.Last.Cause != failure.Cause || record.Last.Outcome != failure.Outcome || record.Last.Action != failure.Action
	if changed {
		record.History = append(record.History, failure)
		if len(record.History) > 8 {
			record.History = append([]model.OnlineFailure(nil), record.History[len(record.History)-8:]...)
		}
	}
	last := failure
	record.Last = &last
	record.Resolved = false
	return record
}

// RecordJobOnlineFailure adds evidence only; it never resumes or rephases work.
func (s *Store) RecordJobOnlineFailure(ctx context.Context, id model.JobID, failure model.OnlineFailure) error {
	return s.update(ctx, func(tx *bolt.Tx) error {
		j, err := jobTx(tx, id)
		if err != nil {
			return err
		}
		j.Online = retainOnline(j.Online, failure)
		j.Online.RequestFailures = nextOnlineCount(j.Online.RequestFailures)
		if failure.Cause == "canceled" && (failure.CancelOrigin == "user_stop" || failure.CancelOrigin == "unspecified_cancel" || failure.CancelOrigin == "") {
			j.Online.PausedReason = "cancel_" + failure.CancelOrigin
		}
		if retryableOnline(failure.Cause) {
			delay := 2 * time.Second
			for i := 1; i < j.Online.RequestFailures && delay < 5*time.Minute; i++ {
				delay *= 2
			}
			if delay > 5*time.Minute {
				delay = 5 * time.Minute
			}
			if server := onlineWait(failure.RetryAfterSeconds); server > delay {
				delay = server
			}
			next := failure.At.Add(retryJitter(delay, j.Online.RequestFailures))
			if j.Online.NotBefore.Before(next) {
				j.Online.NotBefore = next
			}
		}
		return saveJob(tx, j)
	})
}

func (s *Store) RecordNamespaceOnlineFailure(ctx context.Context, id string, failure model.OnlineFailure) error {
	return s.update(ctx, func(tx *bolt.Tx) error {
		var r namespaceRecord
		if err := get(tx, "namespace", id, &r); err != nil {
			return err
		}
		r.Op.Online = retainOnline(r.Op.Online, failure)
		pauseNamespaceCancellation(r.Op.Online, failure)
		return put(tx, "namespace", id, r)
	})
}

func protocolObservation(cause string) bool {
	return cause == "protocol_error" || cause == "api_error_unknown" || cause == "unclassified_error" || cause == "verification_unavailable"
}

// RecordCreateObservation keeps transport failures separate from valid cloud
// observations. The old RecordCreateCheck remains a legacy compatibility API.
func (s *Store) RecordCreateObservation(ctx context.Context, id, reason string, failure model.OnlineFailure, effective bool) (out model.CreateOp, err error) {
	err = s.update(ctx, func(tx *bolt.Tx) error {
		if er := get(tx, "createops", id, &out); er != nil {
			return er
		}
		if !pendingCreate(out) {
			return ErrInvalidState
		}
		j, er := jobTx(tx, out.JobID)
		if er != nil {
			return er
		}
		if j.Superseded || j.LocalReceipt {
			return ErrInvalidState
		}
		if out.RecoveryVersion == 0 {
			out.RecoveryVersion = 1
			out.LegacyCheckAttempts = out.CheckAttempts
			out.DiagnosticCycle = uuid.NewString()
		}
		if businessPause(out.CheckReason) && !businessPause(out.PausedReason) {
			out.PausedReason = out.CheckReason
		}
		out.CheckAttempts = nextOnlineCount(out.CheckAttempts)
		out.LastCheckedAt = failure.At.UTC()
		out.CheckReason = reason
		if effective {
			out.EffectiveChecks = nextOnlineCount(out.EffectiveChecks)
		} else {
			out.RequestFailures = nextOnlineCount(out.RequestFailures)
		}
		if protocolObservation(failure.Cause) {
			out.UnclassifiedProbes = nextOnlineCount(out.UnclassifiedProbes)
		}
		if reason == "multiple_matches" || reason == "content_mismatch" || reason == "location_changed" || failure.Cause == "identity_changed" || out.EffectiveChecks >= 3 || out.UnclassifiedProbes >= 3 {
			out.NeedsAttention = true
			if !businessPause(out.PausedReason) {
				out.PausedReason = reason
				if failure.Cause == "identity_changed" {
					out.PausedReason = "identity_changed"
				}
			}
		}
		// Once an operation needs a decision, background rounds cannot release it.
		out.ObservationReady = false
		delay := 2 * time.Second
		for i := 1; i < out.RequestFailures && delay < 5*time.Minute; i++ {
			delay *= 2
		}
		if delay > 5*time.Minute {
			delay = 5 * time.Minute
		}
		if server := onlineWait(failure.RetryAfterSeconds); server > delay {
			delay = server
		}
		next := failure.At.Add(retryJitter(delay, (out.RequestFailures%21+out.EffectiveChecks%21)%21)).UTC()
		if out.NextCheckAt.Before(next) {
			out.NextCheckAt = next
		}
		if out.FailureCode == "" {
			out.FailureCode = "unknown_result"
		}
		if er = put(tx, "createops", id, out); er != nil {
			return er
		}
		if j.Online == nil {
			j.Online = &model.OnlineRecord{FirstMissing: true}
			known := map[string]bool{"request_timeout": true, "permission_denied": true, "auth_required": true, "credential_unavailable": true, "identity_changed": true, "rate_limited": true, "offline": true, "canceled": true}
			if known[out.FailureCode] {
				first := model.OnlineFailure{SchemaVersion: 1, ID: uuid.NewString(), Operation: "create", Phase: "unknown", Cause: out.FailureCode, Outcome: "dispatched_unconfirmed", Action: "observe_only", At: out.FirstUncertainAt}
				j.Online.First = &first
				j.Online.FirstMissing = false
			}
		}
		j.Online = retainOnline(j.Online, failure)
		if failure.Cause == "canceled" && (failure.CancelOrigin == "" || failure.CancelOrigin == "unspecified_cancel" || failure.CancelOrigin == "user_stop") {
			j.Online.PausedReason = "cancel_" + failure.CancelOrigin
		}
		j.Online.RequestFailures = out.RequestFailures
		j.Online.EffectiveChecks = out.EffectiveChecks
		j.Online.LegacyAttempts = out.LegacyCheckAttempts
		j.Online.CycleID = out.DiagnosticCycle
		if !cancelPause(j.Online.PausedReason) {
			j.Online.PausedReason = out.PausedReason
		}
		j.Online.NotBefore = out.NextCheckAt
		if er = saveJob(tx, j); er != nil {
			return er
		}
		return saveCreateCheckState(tx, out, j)
	})
	return
}

// RecordCreationObserved records a candidate only. Normal execution still
// re-verifies content and identity before binding or dispatching any rename.
func (s *Store) RecordCreationObserved(ctx context.Context, id, token, revision string) error {
	if token == "" {
		return ErrInvalidState
	}
	return s.update(ctx, func(tx *bolt.Tx) error {
		var op model.CreateOp
		if err := get(tx, "createops", id, &op); err != nil {
			return err
		}
		if !pendingCreate(op) || op.NeedsAttention {
			return ErrInvalidState
		}
		if op.Token != "" && op.Token != token || op.CandidateToken != "" && op.CandidateToken != token {
			return model.ErrIdentityChanged
		}
		j, err := jobTx(tx, op.JobID)
		if err != nil {
			return err
		}
		if j.Superseded || j.LocalReceipt {
			return ErrInvalidState
		}
		op.CandidateToken = token
		op.CandidateRevision = revision
		op.ObservationOnly = false
		op.ObservationReady = true
		businessPaused := businessPause(op.PausedReason)
		if businessPaused {
			op.NeedsAttention = true
			op.ObservationReady = false
		}
		op.NextCheckAt = time.Time{}
		if err = put(tx, "createops", id, op); err != nil {
			return err
		}
		if j.Online != nil {
			j.Online.NotBefore = time.Time{}
			if !cancelPause(j.Online.PausedReason) && !businessPaused {
				j.Online.PausedReason = ""
			}
		}
		return saveCreateCheckState(tx, op, j)
	})
}

// JobOnlineDue is independent of the in-memory retry map and survives restart.
func (s *Store) JobOnlineDue(ctx context.Context, id model.JobID, now time.Time) (bool, error) {
	j, err := s.Journal(ctx, id)
	if err != nil {
		return false, err
	}
	if j.Online == nil {
		return true, nil
	}
	return !cancelPause(j.Online.PausedReason) && !now.Before(j.Online.NotBefore), nil
}

func cancelPause(reason string) bool {
	return reason == "cancel_" || reason == "cancel_user_stop" || reason == "cancel_unspecified_cancel"
}

func retryableOnline(cause string) bool {
	switch cause {
	case "request_timeout", "dns_error", "connection_error", "transport_error", "rate_limited", "service_unavailable", "parent_pending":
		return true
	}
	return false
}

// ClearJobOnlinePause is called only by an explicit continuation action. It
// cannot release ambiguity, protocol pauses, or service waiting bounds.
func (s *Store) ClearJobOnlinePause(ctx context.Context, id model.JobID) error {
	// Ordinary retry admission does not need a write transaction/fsync when
	// no cancellation restriction exists. Recheck under the write lock below.
	snapshot, err := s.Journal(ctx, id)
	if err != nil {
		return err
	}
	if snapshot.Online == nil || !cancelPause(snapshot.Online.PausedReason) {
		return nil
	}
	return s.update(ctx, func(tx *bolt.Tx) error {
		j, err := jobTx(tx, id)
		if err != nil {
			return err
		}
		if j.Online == nil || !cancelPause(j.Online.PausedReason) {
			return nil
		}
		j.Online.PausedReason = ""
		return saveJob(tx, j)
	})
}

func businessPause(reason string) bool {
	return reason == "multiple_matches" || reason == "content_mismatch" || reason == "location_changed" || reason == "identity_changed"
}

func onlineWait(seconds int64) time.Duration {
	if seconds <= 0 {
		return 0
	}
	if seconds > int64((1<<63-1)/int64(time.Second)) {
		return time.Duration(1<<63 - 1)
	}
	return time.Duration(seconds) * time.Second
}
func retryJitter(delay time.Duration, n int) time.Duration {
	jitter := delay / 100 * time.Duration(n%21)
	if time.Duration(1<<63-1)-delay < jitter {
		return time.Duration(1<<63 - 1)
	}
	return delay + jitter
}

func nextOnlineCount(n int) int {
	if n < 0 {
		return 1
	}
	if n < int(^uint(0)>>1) {
		return n + 1
	}
	return n
}
