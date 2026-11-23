package manager

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestDirectoryRefreshDispatchValidatesRequest(t *testing.T) {
	calls := 0
	w := &worker{ready: true, services: &workerServices{refreshDirectories: func(context.Context) error { calls++; return nil }}}
	for _, tc := range []struct {
		method, body string
		valid        bool
	}{
		{"GET", "", false}, {"POST", `{"unknown":true}`, false}, {"POST", `{} {}`, false}, {"POST", `{}`, true},
	} {
		_, err := w.dispatch(httptest.NewRequest(tc.method, "/refresh-directories", strings.NewReader(tc.body)))
		if (err == nil) != tc.valid {
			t.Fatalf("%s %q: %v", tc.method, tc.body, err)
		}
	}
	if calls != 1 {
		t.Fatalf("invalid requests reached refresh: %d", calls)
	}
}

func TestDirectoryRefreshCancelledBeforeWorkerStops(t *testing.T) {
	entered := make(chan struct{})
	w := &worker{ready: true, services: &workerServices{refreshDirectories: func(ctx context.Context) error { close(entered); <-ctx.Done(); return ctx.Err() }}}
	done := make(chan error, 1)
	go func() {
		_, err := w.dispatch(httptest.NewRequest("POST", "/refresh-directories", strings.NewReader(`{}`)))
		done <- err
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("refresh never admitted")
	}
	w.closeControls()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("refresh blocked shutdown")
	}
	if _, err := w.dispatch(httptest.NewRequest("POST", "/refresh-directories", strings.NewReader(`{}`))); err == nil {
		t.Fatal("refresh admitted after shutdown")
	}
}

func TestScopedDirectoryRefreshDispatchAndShutdown(t *testing.T) {
	paths := []string{}
	w := &worker{ready: true, services: &workerServices{refreshDirectory: func(_ context.Context, remote string) error { paths = append(paths, remote); return nil }}}
	for _, tc := range []struct {
		method, body string
		valid        bool
	}{
		{"GET", "", false}, {"POST", `{}`, false}, {"POST", `{"remote":null}`, false}, {"POST", `{"remote":"../secret"}`, false}, {"POST", `{"remote":"/tmp"}`, false}, {"POST", `{"remote":"C:/Users"}`, false}, {"POST", `{"remote":"C:relative"}`, false}, {"POST", `{"remote":"a\\b"}`, false}, {"POST", `{"remote":"a//b"}`, false}, {"POST", `{"remote":"a","unknown":true}`, false}, {"POST", `{"remote":"a"} {}`, false}, {"POST", `{"remote":"个人目录/开发项目文档"}`, true}, {"POST", `{"remote":""}`, true},
	} {
		_, err := w.dispatch(httptest.NewRequest(tc.method, "/refresh-directory", strings.NewReader(tc.body)))
		if (err == nil) != tc.valid {
			t.Fatalf("%s %s: %v", tc.method, tc.body, err)
		}
	}
	if len(paths) != 2 || paths[0] != "个人目录/开发项目文档" || paths[1] != "" {
		t.Fatal("wrong scope", paths)
	}
	w.closeControls()
	if _, err := w.dispatch(httptest.NewRequest("POST", "/refresh-directory", strings.NewReader(`{"remote":""}`))); err == nil {
		t.Fatal("accepted request while stopping")
	}
}
