//go:build !desktop || (!windows && !cgo) || (!darwin && !windows && !linux)

package tray

import "context"

// Available reports native support in this build, not visibility in a session.
func Available() bool { return false }

func Run(ctx context.Context, initial View, updates <-chan View, actions chan<- Action) error {
	return RunWithShutdown(ctx, initial, updates, actions, nil)
}

// RunWithShutdown never invokes cleanup when no native event loop was started.
func RunWithShutdown(ctx context.Context, _ View, _ <-chan View, _ chan<- Action, _ func()) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return ErrUnavailable
}
