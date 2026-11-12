package cloud

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/rclone/rclone/internal/bridge/model"
)

func TestOpenDocMediaIdentityPreflightAndCanonicalErrors(t *testing.T) {
	c, err := New(managed(t, "must-not-run"), Options{CheckIdentity: func(context.Context) error { return model.ErrIdentityChanged }})
	if err != nil {
		t.Fatal(err)
	}
	if rc, err := c.OpenDocMedia(context.Background(), "imageToken"); rc != nil || !errors.Is(err, ErrNotDispatched) || !errors.Is(err, model.ErrIdentityChanged) {
		t.Fatal("identity preflight missing", err)
	}
	script := filepath.Join(t.TempDir(), "cli")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s' '{\"ok\":true,\"identity\":\"tenant\",\"data\":{}}'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	temp := t.TempDir()
	c, err = New(managed(t, script), Options{TempDir: temp})
	if err != nil {
		t.Fatal(err)
	}
	if rc, err := c.OpenDocMedia(context.Background(), "imageToken"); rc != nil || !errors.Is(err, model.ErrIdentityChanged) {
		t.Fatal("canonical identity lost", err)
	}
	entries, _ := os.ReadDir(temp)
	if len(entries) != 0 {
		t.Fatal("failure leaked files")
	}
}
func TestOpenDocMediaReadRemainsAvailableWithMutationDisabled(t *testing.T) {
	script := filepath.Join(t.TempDir(), "cli")
	body := "#!/bin/sh\nprintf '\211PNG\r\n\032\nimage' > media\nprintf '%s' '{\"ok\":true,\"identity\":\"user\",\"data\":{\"saved_path\":\"media\",\"size_bytes\":13,\"content_type\":\"image/png\"}}'\n"
	if err := os.WriteFile(script, []byte(body), 0700); err != nil {
		t.Fatal(err)
	}
	temp := t.TempDir()
	checks := 0
	c, err := New(managed(t, script), Options{TempDir: temp, CheckIdentity: func(context.Context) error { checks++; return nil }, CheckMutation: func(context.Context) error { t.Fatal("read used mutation guard"); return model.ErrUnsupportedContent }})
	if err != nil {
		t.Fatal(err)
	}
	rc, err := c.OpenDocMedia(context.Background(), "imageToken")
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(rc)
	if err != nil || len(b) != 13 || checks != 1 {
		t.Fatal("read failed", err)
	}
	if err := rc.Close(); err != nil {
		t.Fatal(err)
	}
	entries, _ := os.ReadDir(temp)
	if len(entries) != 0 {
		t.Fatal("close leaked files")
	}
}
