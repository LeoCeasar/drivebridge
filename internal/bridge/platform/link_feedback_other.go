//go:build !windows

package platform

import "context"

func LinkFeedback(ctx context.Context, message string, failed bool) {
	_ = Notify(ctx, "DriveBridge", message)
}
