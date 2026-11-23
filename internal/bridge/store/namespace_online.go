package store

import (
	"context"
	"errors"

	"github.com/rclone/rclone/internal/bridge/model"
	bolt "go.etcd.io/bbolt"
)

// ErrNamespaceWritePaused leaves the business journal intact. Read-only
// reconciliation remains possible, but a remaining mutation needs continuation.
var ErrNamespaceWritePaused = errors.New("namespace write awaits explicit continuation")

func pauseNamespaceCancellation(record *model.OnlineRecord, failure model.OnlineFailure) {
	if record == nil || failure.Cause != "canceled" || record.PausedReason != "" && !cancelPause(record.PausedReason) {
		return
	}
	switch failure.CancelOrigin {
	case "user_stop", "unspecified_cancel", "":
		record.PausedReason = "cancel_" + failure.CancelOrigin
	}
}
func namespaceWritePaused(record *model.OnlineRecord) bool {
	return record != nil && cancelPause(record.PausedReason)
}

// ClearNamespaceOnlinePauses is called by an explicit mount retry. It releases
// only cancellation write restrictions, retaining diagnostics, service bounds,
// business pauses, the original operation and its dispatch/confirmation facts.
func (s *Store) ClearNamespaceOnlinePauses(ctx context.Context) error {
	return s.update(ctx, func(tx *bolt.Tx) error {
		return eachIndexedNamespaceTx(tx, namespaceActionIndex, func(r namespaceRecord) error {
			if !r.ActionPending || !namespaceWritePaused(r.Op.Online) {
				return nil
			}
			r.Op.Online.PausedReason = ""
			return put(tx, "namespace", r.Op.ID, r)
		})
	})
}

// ResumeStoppedNamespaces is used by explicit Start. Unspecified cancellation,
// business restrictions and service waiting bounds remain in force.
func (s *Store) ResumeStoppedNamespaces(ctx context.Context) error {
	return s.update(ctx, func(tx *bolt.Tx) error {
		return eachIndexedNamespaceTx(tx, namespaceActionIndex, func(r namespaceRecord) error {
			if !r.ActionPending || r.Op.Online == nil || r.Op.Online.PausedReason != "cancel_user_stop" {
				return nil
			}
			r.Op.Online.PausedReason = ""
			return put(tx, "namespace", r.Op.ID, r)
		})
	})
}
