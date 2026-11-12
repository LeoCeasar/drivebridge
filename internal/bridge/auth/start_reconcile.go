package auth

import (
	"context"
	"errors"
	"fmt"

	"github.com/rclone/rclone/internal/bridge/model"
)

// ErrReconcileFailed marks a failure of the reconcile step that StartOrReconcile
// runs when an interrupted authorization (for example an expired QR code or a
// restarted desktop) is still recorded. The wrapped cause stays inspectable with
// errors.Is. Nothing was created or discarded when it is returned: the existing
// application configuration, identity binding and pending data are untouched.
var ErrReconcileFailed = errors.New("authentication reconcile failed")

// maxReconcileAttempts bounds Start -> Reconcile cycles. A healthy interrupted
// state needs exactly one reconcile; more only happens when the official CLI
// keeps failing right after reconcile declared the application ready.
const maxReconcileAttempts = 3

// FlowStarter is the subset of Service used by StartOrReconcile.
type FlowStarter interface {
	Start(context.Context, model.ProfileID) (AuthFlow, error)
	StartRecovery(context.Context, model.ProfileID) (AuthFlow, error)
	Reconcile(context.Context, model.ProfileID) (Identity, error)
}

// StartResult is either a fresh authorization Flow to wait on, or, when
// Reconciled is true, the Identity that an earlier interrupted authorization had
// in fact already completed (no new authorization is needed).
type StartResult struct {
	Flow       AuthFlow
	Identity   Identity
	Reconciled bool
}

// StartOrReconcile starts (or, with recovery, restarts same-account) user
// authorization like Start/StartRecovery, but handles ErrNeedsReconcile the way
// the CLI login command does: the retained enrollment is reconciled against the
// on-disk application configuration and credentials, never replaced.
//
//   - reconcile verifies an existing user: the earlier authorization completed;
//     the verified identity is returned with Reconciled=true.
//   - reconcile reports ErrUserMissing: the application stays configured, a fresh
//     authorization is started for the same application and identity.
//   - any other reconcile failure is returned wrapped in ErrReconcileFailed.
//
// Other Start errors (busy, identity changed, ...) are returned unchanged.
func StartOrReconcile(ctx context.Context, s FlowStarter, id model.ProfileID, recovery bool) (StartResult, error) {
	for attempt := 0; attempt < maxReconcileAttempts; attempt++ {
		var flow AuthFlow
		var err error
		if recovery {
			flow, err = s.StartRecovery(ctx, id)
		} else {
			flow, err = s.Start(ctx, id)
		}
		if !errors.Is(err, ErrNeedsReconcile) {
			return StartResult{Flow: flow}, err
		}
		identity, reconcileErr := s.Reconcile(ctx, id)
		switch {
		case reconcileErr == nil:
			return StartResult{Identity: identity, Reconciled: true}, nil
		case errors.Is(reconcileErr, ErrUserMissing):
			continue
		default:
			return StartResult{}, fmt.Errorf("%w: %w", ErrReconcileFailed, reconcileErr)
		}
	}
	return StartResult{}, fmt.Errorf("%w: %w", ErrReconcileFailed, ErrNeedsReconcile)
}

var _ FlowStarter = (*Service)(nil)
