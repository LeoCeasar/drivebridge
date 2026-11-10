// Package auth coordinates the current OS user's CLI credentials and verified
// profile references. Credentials remain owned by the official CLI.
package auth

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"time"

	"github.com/gofrs/flock"
	"github.com/rclone/rclone/internal/bridge/platform"
)

// DefaultDomain is deliberately independent of Bridge config/StateDir overrides.
// All managed installations for this OS user use this same credential domain.
func DefaultDomain() (string, error) {
	p, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(p, "DriveBridge"), nil
}

type Coordinator struct{ root string }

func NewCoordinator() (*Coordinator, error) {
	root, err := DefaultDomain()
	if err != nil {
		return nil, err
	}
	return newCoordinatorAt(root)
}
func newCoordinatorAt(root string) (*Coordinator, error) {
	if !filepath.IsAbs(root) {
		return nil, ErrInvalid
	}
	root = filepath.Join(root, "auth", "locks")
	if err := platform.EnsurePrivateDir(root); err != nil {
		return nil, err
	}
	return &Coordinator{root: root}, nil
}
func key(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
func privateLock(path string) (*flock.Flock, error) {
	if err := platform.CheckNoSymlinks(path); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err = f.Close(); err != nil {
		return nil, err
	}
	if err = platform.SecurePrivate(path, false); err != nil {
		return nil, err
	}
	return flock.New(path), nil
}
func acquire(ctx context.Context, l *flock.Flock, shared bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var ok bool
	var err error
	if shared {
		ok, err = l.TryRLockContext(ctx, 5*time.Millisecond)
	} else {
		ok, err = l.TryLockContext(ctx, 5*time.Millisecond)
	}
	if err != nil {
		return err
	}
	if !ok {
		if err = ctx.Err(); err != nil {
			return err
		}
		return ErrBusy
	}
	return nil
}
func (c *Coordinator) lock(ctx context.Context, name string, shared bool, run func() error) error {
	if c == nil || run == nil {
		return ErrInvalid
	}
	l, err := privateLock(filepath.Join(c.root, name+".lock"))
	if err != nil {
		return err
	}
	if err = acquire(ctx, l, shared); err != nil {
		return err
	}
	defer func() { _ = l.Unlock() }()
	return run()
}
func (c *Coordinator) WithEnrollmentLock(ctx context.Context, run func() error) error {
	return c.lock(ctx, "enrollment", false, run)
}
func (c *Coordinator) WithCredentialLocks(ctx context.Context, appID, userOpenID string, run func() error) error {
	if appID == "" || run == nil {
		return ErrInvalid
	}
	return c.lock(ctx, "enrollment", true, func() error {
		return c.lock(ctx, "app-"+key(appID), false, func() error {
			if userOpenID == "" {
				return run()
			}
			return c.lock(ctx, "user-"+key(appID+"\x00"+userOpenID), false, run)
		})
	})
}
