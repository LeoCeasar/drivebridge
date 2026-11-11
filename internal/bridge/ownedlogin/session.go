// Package ownedlogin owns an app-native, per-account login session. Directory
// discovery stays inside the native WebView; browser credentials are never exported.
package ownedlogin

import (
	"context"
	"errors"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rclone/rclone/internal/bridge/discovery"
)

var (
	ErrUnavailable        = errors.New("app-owned login requires the platform native helper")
	ErrRuntimeUnavailable = errors.New("app-owned login requires Microsoft Edge WebView2 Runtime")
	ErrProtocol           = discovery.ErrProtocol
	ErrDisconnected       = discovery.ErrBrowserDisconnected
	ErrClosed             = errors.New("app-owned login session closed")
)

type Options struct{ HelperPath, ProfileID, Origin string }
type Session struct {
	process    *process
	ctx        context.Context
	cancel     context.CancelFunc
	closed     atomic.Bool
	closeOnce  sync.Once
	closeErr   error
	closedDone chan struct{}
}

// Start launches only the supplied app-owned helper. It starts hidden and uses
// ProfileID as its isolated persistent WebView store identifier.
func Start(ctx context.Context, o Options) (*Session, error) {
	if ctx == nil {
		return nil, ErrProtocol
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	origin, err := normalizeOrigin(o.Origin)
	if err != nil || !canonicalID(o.ProfileID) {
		return nil, ErrProtocol
	}
	if !supportedPlatform() {
		return nil, ErrUnavailable
	}
	p, err := startHelper(o.HelperPath, o.ProfileID, origin)
	if err != nil {
		return nil, err
	}
	readyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	select {
	case err = <-p.ready:
	case <-readyCtx.Done():
		err = readyCtx.Err()
	}
	if err != nil {
		stopCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
		_ = p.stop(stopCtx)
		stop()
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if errors.Is(err, ErrRuntimeUnavailable) {
			return nil, ErrRuntimeUnavailable
		}
		return nil, ErrUnavailable
	}
	life, stop := context.WithCancel(ctx)
	s := &Session{process: p, ctx: life, cancel: stop, closedDone: make(chan struct{})}
	go func() {
		select {
		case <-ctx.Done():
			_ = s.Close()
		case <-s.closedDone:
		}
	}()
	return s, nil
}
func (s *Session) Connected() bool {
	if s == nil || s.closed.Load() || s.ctx.Err() != nil {
		return false
	}
	s.process.mu.Lock()
	defer s.process.mu.Unlock()
	return !s.process.closed
}
func (s *Session) call(ctx context.Context, op, path, query string, timeout time.Duration) (response, error) {
	if s.closed.Load() {
		return response{}, ErrClosed
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	return s.process.call(ctx, op, path, query)
}

// Login shows the existing owned page without resetting an account or SSO form.
func (s *Session) Login(ctx context.Context) error {
	_, err := s.call(ctx, "show", "", "", 10*time.Second)
	return err
}
func (s *Session) Get(ctx context.Context, path string, q url.Values) ([]byte, error) {
	if !validRequest(path, q) {
		return nil, ErrProtocol
	}
	// A newly launched hidden WebView may still be restoring its persistent
	// tenant page. Keep one deadline for loading waits and the eventual fetch.
	ctx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	for {
		reply, err := s.call(ctx, "fetch", path, q.Encode(), 35*time.Second)
		if err != nil {
			return nil, err
		}
		if reply.Result == nil || reply.Result.Kind != "loading" {
			return validateResult(reply.Result)
		}
		timer := time.NewTimer(200 * time.Millisecond)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-s.ctx.Done():
			timer.Stop()
			return nil, s.ctx.Err()
		}
	}
}

// Complete is called only after the manager verifies account ownership and the
// directory scan. It shows a local completion page, preserving the hidden page.
func (s *Session) Complete(ctx context.Context) error {
	_, err := s.call(ctx, "complete", "", "", 10*time.Second)
	return err
}
func (s *Session) SealLogin(ctx context.Context) error { return s.Complete(ctx) }
func (s *Session) Close() error {
	if s == nil {
		return nil
	}
	s.closeOnce.Do(func() {
		s.closed.Store(true)
		s.cancel()
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		s.closeErr = s.process.stop(ctx)
		cancel()
		close(s.closedDone)
	})
	return s.closeErr
}
