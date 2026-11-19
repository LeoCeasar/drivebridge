package platform

import (
	"context"
	"testing"
	"time"
)

func TestLinkFeedbackSilentSuccessAndBoundedFailure(t *testing.T) {
	for _, failed := range []bool{false, true} {
		calls := 0
		linkFeedback(context.Background(), "error text", failed, func(ctx context.Context, message string) {
			calls++
			deadline, ok := ctx.Deadline()
			if message != "error text" || !ok || time.Until(deadline) > 6*time.Second {
				t.Fatal(message, deadline)
			}
		})
		if (calls == 1) != failed {
			t.Fatalf("failed=%v calls=%d", failed, calls)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	linkFeedback(ctx, "cancelled", true, func(context.Context, string) { t.Fatal("showed cancelled feedback") })
}
