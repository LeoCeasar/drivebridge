//go:build !desktop || (!windows && !cgo) || (!darwin && !windows && !linux)

package tray

import (
	"context"
	"errors"
	"testing"
)

func TestStubReportsUnavailable(t *testing.T) {
	if Available() {
		t.Fatal("stub claims a native tray")
	}
	if err := Run(context.Background(), View{}, nil, nil); !errors.Is(err, ErrUnavailable) {
		t.Fatal(err)
	}
}

func TestStubHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Run(ctx, View{}, nil, nil); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
