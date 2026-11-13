package vfscache

import (
	"context"
	"errors"
	"fmt"
	"sort"
)

// ManagedLocalSaveFailures reports failed WAL confirmations separately from
// immutable archive preparation. These bytes have not been acknowledged durable.
func (c *Cache) ManagedLocalSaveFailures() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	var failed []string
	for name, item := range c.item {
		item.mu.Lock()
		if item.managedCheckpointError != nil {
			failed = append(failed, name)
		}
		item.mu.Unlock()
	}
	sort.Strings(failed)
	return failed
}

// RetryManagedLocalSaves confirms only failed saves whose handles have closed.
// It neither checkpoints active/never-saved writes nor imports/uploads bodies.
func (c *Cache) RetryManagedLocalSaves(ctx context.Context) error {
	c.archiveMu.RLock()
	defer c.archiveMu.RUnlock()
	c.mu.Lock()
	items := make([]*Item, 0, len(c.item))
	for _, item := range c.item {
		items = append(items, item)
	}
	c.mu.Unlock()
	var failures error
	changed := false
	for _, item := range items {
		accepted, err := item.retryManagedLocalSave(ctx)
		changed = changed || accepted
		if err != nil {
			failures = errors.Join(failures, fmt.Errorf("local save %s: %w", item.GetName(), err))
		}
	}
	if changed {
		c.wakeManagedWorker()
	}
	return failures
}

func (item *Item) retryManagedLocalSave(ctx context.Context) (bool, error) {
	// Match the foreground mutation order; never acquire stageMu here.
	item.snapshotMu.Lock()
	defer item.snapshotMu.Unlock()
	item.mu.Lock()
	defer item.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if item.managedCheckpointError == nil || item.journal == nil || item.opens != 0 || item.pendingAccesses != 0 || item.namespacePins != 0 || item.closing != nil || item.beingReset {
		return false, nil
	}
	previous := item.info.DurableSeq
	err := item.checkpointManagedLocked()
	return item.info.DurableSeq > previous, err
}
