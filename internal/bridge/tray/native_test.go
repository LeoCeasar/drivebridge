//go:build desktop && (windows || (cgo && (darwin || linux)))

package tray

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestNativeCancellationBeforeInitialization(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Run(ctx, View{}, nil, make(chan Action)); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if nativeStarted.Load() {
		t.Fatal("canceled run initialized native UI")
	}
}

func TestNativeClickRelayCancelsWithBlockedController(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	source := make(chan struct{}, 1)
	source <- struct{}{}
	done := make(chan struct{})
	go func() { watchClicks(ctx, source, "open", make(chan string)); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("click relay did not cancel")
	}
}
