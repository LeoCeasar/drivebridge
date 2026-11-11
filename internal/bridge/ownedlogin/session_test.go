package ownedlogin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/discovery"
)

func fakeSession(t *testing.T, reply func(command) []any) *Session {
	t.Helper()
	commands, stdin := io.Pipe()
	stdout, replies := io.Pipe()
	p := &process{stdin: stdin, pending: map[string]chan delivery{}, done: make(chan struct{}), ready: make(chan error, 1)}
	go p.read(stdout)
	go func() {
		decoder := json.NewDecoder(commands)
		for {
			var request command
			if decoder.Decode(&request) != nil {
				return
			}
			for _, item := range reply(request) {
				if json.NewEncoder(replies).Encode(item) != nil {
					return
				}
			}
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	s := &Session{process: p, ctx: ctx, cancel: cancel, closedDone: make(chan struct{})}
	t.Cleanup(func() {
		cancel()
		_ = stdin.Close()
		_ = commands.Close()
		_ = replies.Close()
		_ = stdout.Close()
		p.disconnected()
	})
	return s
}
func TestOwnedSessionFetchAndLocalCompletion(t *testing.T) {
	ok := true
	var ops []string
	s := fakeSession(t, func(request command) []any {
		ops = append(ops, request.Op)
		if request.Op == "fetch" {
			if request.Path != "/space/api/explorer/v3/my_space/folder/" || request.Query != "length=50" {
				t.Error("invalid fetch request")
			}
			return []any{map[string]any{"event": "ready", "private": "ignored"}, response{ID: uuid.NewString(), OK: &ok}, response{ID: request.ID, OK: &ok, Result: &helperResult{Status: 200, ContentType: "application/json", Body: `{"code":0,"data":{}}`}}}
		}
		return []any{response{ID: request.ID, OK: &ok}}
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.Login(ctx); err != nil {
		t.Fatal(err)
	}
	body, err := s.Get(ctx, "/space/api/explorer/v3/my_space/folder/", url.Values{"length": {"50"}})
	if err != nil || string(body) != `{"code":0,"data":{}}` {
		t.Fatalf("%q %v", body, err)
	}
	if err = s.SealLogin(ctx); err != nil {
		t.Fatal(err)
	}
	if !s.Connected() {
		t.Fatal("completion closed owned session")
	}
	if strings.Join(ops, ",") != "show,fetch,complete" {
		t.Fatal(ops)
	}
}
func TestOwnedSessionRejectsRequestsAndRedactsErrors(t *testing.T) {
	ok := false
	s := fakeSession(t, func(request command) []any {
		return []any{response{ID: request.ID, OK: &ok, Error: "private credential"}}
	})
	ctx := context.Background()
	if _, err := s.Get(ctx, "https://evil.test", nil); !errors.Is(err, ErrProtocol) {
		t.Fatal(err)
	}
	if _, err := s.Get(ctx, "/space/api/explorer/v3/my_space/folder/", url.Values{"script": {"private"}}); !errors.Is(err, ErrProtocol) {
		t.Fatal(err)
	}
	if err := s.Login(ctx); !errors.Is(err, ErrProtocol) || strings.Contains(err.Error(), "credential") {
		t.Fatal(err)
	}
	for _, raw := range []string{"https://team.feishu.cn.evil.test", "https://user@team.feishu.cn", "https://team.feishu.cn:443", "https://team.feishu.cn/?x=y"} {
		if _, err := normalizeOrigin(raw); err == nil {
			t.Fatal(raw)
		}
	}
}
func TestOwnedSessionLoginAndTransientClassification(t *testing.T) {
	for _, test := range []struct {
		result helperResult
		want   error
	}{{helperResult{Kind: "login"}, discovery.ErrNeedsLogin}, {helperResult{Status: 401}, discovery.ErrNeedsLogin}, {helperResult{Status: 200, ContentType: "application/json", Body: `{"code":399,"secret":"private"}`}, discovery.ErrNeedsLogin}, {helperResult{Status: 200, ContentType: "application/json", Body: `{"code":null}`}, ErrProtocol}} {
		if _, err := validateResult(&test.result); !errors.Is(err, test.want) {
			t.Fatal(err)
		}
	}
	_, err := validateResult(&helperResult{Status: 503})
	var status interface{ StatusCode() int }
	if !errors.As(err, &status) || status.StatusCode() != 503 {
		t.Fatal(err)
	}
}

func TestUnavailableHelperAndIdempotentClose(t *testing.T) {
	_, err := Start(context.Background(), Options{HelperPath: "/missing/drivebridge-session-host", ProfileID: uuid.NewString(), Origin: "https://team.feishu.cn"})
	if !errors.Is(err, ErrUnavailable) {
		t.Fatal("missing helper not classified unavailable", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	close(done)
	s := &Session{process: &process{done: done}, ctx: ctx, cancel: cancel, closedDone: make(chan struct{})}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if s.Connected() {
		t.Fatal("closed session connected")
	}
	if err = s.Login(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatal(err)
	}
}

func TestLoadingSessionRetriesUntilInitialTenantPageIsReady(t *testing.T) {
	ok := true
	var calls atomic.Int32
	s := fakeSession(t, func(request command) []any {
		calls.Add(1)
		result := &helperResult{Kind: "loading"}
		if calls.Load() >= 2 {
			result = &helperResult{Status: 200, ContentType: "application/json", Body: `{"code":0,"data":{}}`}
		}
		return []any{response{ID: request.ID, OK: &ok, Result: result}}
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	body, err := s.Get(ctx, "/space/api/explorer/v3/my_space/folder/", nil)
	if err != nil || string(body) != `{"code":0,"data":{}}` || calls.Load() != 2 {
		t.Fatalf("initial loading was treated as expired login: calls=%d body=%q err=%v", calls.Load(), body, err)
	}
}
func TestLoadingSessionCancellationStopsRetryAndAuthDoesNotRetry(t *testing.T) {
	ok := true
	t.Run("cancel loading", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var calls atomic.Int32
		s := fakeSession(t, func(request command) []any {
			calls.Add(1)
			time.AfterFunc(20*time.Millisecond, cancel)
			return []any{response{ID: request.ID, OK: &ok, Result: &helperResult{Kind: "loading"}}}
		})
		_, err := s.Get(ctx, "/space/api/explorer/v3/my_space/folder/", nil)
		if !errors.Is(err, context.Canceled) || calls.Load() != 1 {
			t.Fatalf("loading ignored cancellation: calls=%d err=%v", calls.Load(), err)
		}
	})
	t.Run("settled authentication page", func(t *testing.T) {
		var calls atomic.Int32
		s := fakeSession(t, func(request command) []any {
			calls.Add(1)
			return []any{response{ID: request.ID, OK: &ok, Result: &helperResult{Kind: "login"}}}
		})
		_, err := s.Get(context.Background(), "/space/api/explorer/v3/my_space/folder/", nil)
		if !errors.Is(err, discovery.ErrNeedsLogin) || calls.Load() != 1 {
			t.Fatalf("authentication page was retried: calls=%d err=%v", calls.Load(), err)
		}
	})
}

func TestLoadingSessionRespectsExistingRequestDeadline(t *testing.T) {
	ok := true
	var calls atomic.Int32
	s := fakeSession(t, func(request command) []any {
		calls.Add(1)
		return []any{response{ID: request.ID, OK: &ok, Result: &helperResult{Kind: "loading"}}}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	_, err := s.Get(ctx, "/space/api/explorer/v3/my_space/folder/", nil)
	if !errors.Is(err, context.DeadlineExceeded) || calls.Load() > 1 {
		t.Fatalf("loading extended request deadline: calls=%d err=%v", calls.Load(), err)
	}
}
