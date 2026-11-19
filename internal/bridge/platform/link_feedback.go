package platform

import (
	"context"
	"time"
)

func linkFeedback(ctx context.Context, message string, failed bool, show func(context.Context, string)) {
	if !failed || ctx.Err() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()
	show(ctx, message)
}
