package larkcli

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestRequestHookRejectionIsProvablyUndispatched(t *testing.T) {
	wait := errors.New("cooldown")
	after := false
	r := NewDirect("must-not-start").WithRequestHooks(func(context.Context) error { return wait }, func(context.Context, error) error { after = true; return nil })
	_, err := r.Run(context.Background(), Request{Args: []string{"drive", "files", "list"}})
	if !errors.Is(err, ErrNotDispatched) || !errors.Is(err, wait) || after {
		t.Fatal(err, after)
	}
}

type hookLock struct{ mu sync.Mutex }

func (g *hookLock) WithCredentialLocks(_ context.Context, _, _ string, fn func() error) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return fn()
}
func (g *hookLock) WithEnrollmentLock(ctx context.Context, fn func() error) error {
	return g.WithCredentialLocks(ctx, "", "", fn)
}
func TestResponseHookPersistsBeforeReleasingCredentialGate(t *testing.T) {
	g := &hookLock{}
	r, err := NewManaged(ManagedOptions{Executable: testExecutable(t, "stderr"), AuthRef: "ref", AuthRoot: t.TempDir(), Identity: Identity{Brand: "feishu", AppID: "app", UserOpenID: "user"}, Coordinator: g})
	if err != nil {
		t.Fatal(err)
	}
	r = r.WithRequestHooks(nil, func(context.Context, error) error {
		if g.mu.TryLock() {
			g.mu.Unlock()
			t.Error("next credential request could bypass unpersisted cooldown")
		}
		return nil
	})
	_, _ = r.Run(context.Background(), Request{Args: []string{"drive", "files", "list"}})
}

func TestRequestHookRecordsActualResponseWithoutMasking(t *testing.T) {
	called := false
	r := NewDirect(testExecutable(t, "stderr")).WithRequestHooks(nil, func(_ context.Context, err error) error {
		var api *Error
		if !errors.As(err, &api) || api.RetryAfter != 3*time.Second {
			t.Fatalf("lost response: %v", err)
		}
		called = true
		return errors.New("record unavailable")
	})
	_, err := r.Run(context.Background(), Request{Args: []string{"drive", "files", "list"}})
	var api *Error
	if !called || !errors.As(err, &api) || api.Code != "429" {
		t.Fatal(err, called)
	}
}

func TestRequestDeadlineCarriesActualOrigin(t *testing.T) {
	_, err := NewDirect(testExecutable(t, "cancel")).Run(context.Background(), Request{Timeout: 20 * time.Millisecond})
	var origin interface{ CancellationOrigin() string }
	if !errors.Is(err, context.DeadlineExceeded) || !errors.As(err, &origin) || origin.CancellationOrigin() != "phase_deadline" {
		t.Fatal(err)
	}
}

func TestRetryAfterWireOverflowIsProtocolFailure(t *testing.T) {
	for _, seconds := range []int64{-1, 9223372036854775807, 9223372037} {
		e := (&wireError{Type: "api", Subtype: "rate_limit", Code: json.RawMessage(`99991400`), RetryAfter: seconds}).safe()
		if !errors.Is(e, ErrProtocol) || e.RetryAfter != 0 {
			t.Fatalf("unsafe seconds %d: %+v", seconds, e)
		}
	}
}
