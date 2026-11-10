//go:build !desktop || (!windows && !cgo) || (!darwin && !windows && !linux)

package tray

import "context"

// Available reports native support in this build, not visibility in a session.
func Available() bool { return false }

func Run(ctx context.Context, _ View, _ <-chan View, _ chan<- Action) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return ErrUnavailable
}
