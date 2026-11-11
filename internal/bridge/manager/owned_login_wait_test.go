package manager

import (
	"context"
	"testing"
	"time"
)

func TestLoginWaitRetriesUntilVerifiedThenStops(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	attempts := 0
	waitDiscoveryLogin(ctx, time.Millisecond, func(context.Context) bool { attempts++; return attempts == 2 })
	if attempts != 2 || ctx.Err() != nil {
		t.Fatalf("attempts=%d error=%v", attempts, ctx.Err())
	}
}
func TestLoginWaitStopsBeforeAnyRequestAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	waitDiscoveryLogin(ctx, time.Millisecond, func(context.Context) bool { t.Fatal("verification after shutdown"); return false })
}
