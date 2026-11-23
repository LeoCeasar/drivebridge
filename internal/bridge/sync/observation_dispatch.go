package syncer

import (
	"context"
	"time"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/online"
)

func (d *backgroundDispatcher) admitDueObservations(now time.Time) {
	ops, err := d.e.store.PendingCreates(d.ctx)
	if err != nil {
		return
	}
	for _, op := range ops {
		if op.RecoveryVersion == 0 || op.NeedsAttention || op.ObservationReady || now.Before(op.NextCheckAt) {
			continue
		}
		if _, exists := d.entries[op.EntryID]; exists {
			continue
		}
		d.entries[op.EntryID] = &dispatchWork{cutoff: op.Generation, epoch: d.epoch, queued: true, observeOnly: true}
		d.queue = append(d.queue, op.EntryID)
	}
}

func (d *backgroundDispatcher) observePass(id model.EntryID) error {
	ctx, cancel := context.WithTimeout(d.ctx, d.e.opt.BackgroundTimeout)
	defer cancel()
	unlock, err := d.e.entryLock(ctx, id)
	if err != nil {
		return err
	}
	defer unlock()
	jobs, err := d.e.store.Pending(ctx)
	if err != nil {
		return err
	}
	for _, j := range jobs {
		if j.EntryID != id {
			continue
		}
		op, err := d.e.store.CreateForJob(ctx, j.ID)
		if err != nil {
			return err
		}
		if op.NeedsAttention || op.ObservationReady || d.e.now().Before(op.NextCheckAt) {
			return nil
		}
		if err = d.e.check(ctx); err != nil {
			return d.e.failedCreateCheck(ctx, op, creationCheckReason(err), online.AtPhase(err, "create", "identity_check"))
		}
		return d.e.observePendingCreation(ctx, j, op)
	}
	return nil
}
