package auth

import (
	"context"
	"path/filepath"
	"sync"
)

// AcquireWorkerStart prevents credential lifecycle changes while a manager or
// hidden worker establishes mount ownership and readiness. Parent and child may
// both hold shared leases. Release only after readiness or complete cleanup.
func (c *Coordinator) AcquireWorkerStart(ctx context.Context) (func(), error) {
	if c == nil {
		return nil, ErrInvalid
	}
	l, err := privateLock(filepath.Join(c.root, "lifecycle.lock"))
	if err != nil {
		return nil, err
	}
	if err = acquire(ctx, l, true); err != nil {
		return nil, err
	}
	var once sync.Once
	return func() { once.Do(func() { _ = l.Unlock() }) }, nil
}

// AcquireLifecycle excludes all new managed worker starts and reference changes
// in this OS-user domain. Callers must stop/recheck existing workers separately;
// the lease is not evidence that active workers have drained.
func (c *Coordinator) AcquireLifecycle(ctx context.Context) (func(), error) {
	if c == nil {
		return nil, ErrInvalid
	}
	l, err := privateLock(filepath.Join(c.root, "lifecycle.lock"))
	if err != nil {
		return nil, err
	}
	if err = acquire(ctx, l, false); err != nil {
		return nil, err
	}
	var once sync.Once
	return func() { once.Do(func() { _ = l.Unlock() }) }, nil
}
