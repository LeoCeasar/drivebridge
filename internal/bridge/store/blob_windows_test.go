//go:build windows

package store

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/rclone/rclone/internal/bridge/platform"
)

// Documents the platform behaviour the PutBlob fallback exists for.
func TestWindowsReplaceOfOpenFileIsDenied(t *testing.T) {
	dir := t.TempDir()
	target, source := filepath.Join(dir, "target"), filepath.Join(dir, "source")
	if err := os.WriteFile(target, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	held, err := os.Open(target)
	if err != nil {
		t.Fatal(err)
	}
	defer held.Close()
	err = platform.ReplaceFile(source, target)
	if err == nil {
		t.Skip("this Windows configuration allows replacing an open file; the PutBlob fallback is not exercised")
	}
	if !replaceDenied(err) {
		t.Fatalf("replace failure %v (%T) is not classified as destination-in-use", err, err)
	}
	// Attribute updates must still work while the file is open elsewhere.
	now := time.Now().UTC()
	if err = os.Chtimes(target, now, now); err != nil {
		t.Fatalf("Chtimes on an open file: %v", err)
	}
}

func TestPutBlobWindowsRealOpenReaderDeniesRename(t *testing.T) {
	s, _, _ := openTest(t) // real nativeIO: platform.ReplaceFile
	const body = "retry of a flush whose blob is being uploaded right now"
	id, err := s.PutBlob(ctx, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-72 * time.Hour)
	if err = os.Chtimes(s.blobPath(id), old, old); err != nil {
		t.Fatal(err)
	}
	// Several concurrent readers, as with an in-flight upload plus a verify.
	var readers []io.ReadCloser
	for i := 0; i < 3; i++ {
		r, e := s.ReadBlob(ctx, id)
		if e != nil {
			t.Fatal(e)
		}
		readers = append(readers, r)
	}
	defer func() {
		for _, r := range readers {
			r.Close()
		}
	}()
	for i := 0; i < 3; i++ {
		got, e := s.PutBlob(ctx, strings.NewReader(body))
		if e != nil || got != id {
			t.Fatalf("attempt %d: %v %s", i, e, got)
		}
	}
	for _, r := range readers {
		b, e := io.ReadAll(r)
		if e != nil || string(b) != body {
			t.Fatalf("reader invalidated: %q %v", b, e)
		}
	}
	if info, e := os.Stat(s.blobPath(id)); e != nil || time.Since(info.ModTime()) > time.Hour {
		t.Fatalf("grace timestamp not refreshed: %v %v", info, e)
	}
	// Leases keep the blob through GC even far in the future, and once the
	// readers are gone the refreshed timestamp still protects it today.
	if _, e := s.GC(ctx, GCOptions{Now: time.Now().Add(90 * 24 * time.Hour)}); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(s.blobPath(id)); e != nil {
		t.Fatalf("leased blob collected: %v", e)
	}
	for _, r := range readers {
		r.Close()
	}
	if _, e := s.GC(ctx, GCOptions{Now: time.Now()}); e != nil {
		t.Fatal(e)
	}
	if _, e := os.Stat(s.blobPath(id)); e != nil {
		t.Fatalf("blob inside its grace window collected: %v", e)
	}
}

// A blob of a different length must not be accepted as "the same" blob.
func TestPutBlobWindowsDeniedReplaceWithDifferentSizeFails(t *testing.T) {
	s, _, _ := openTest(t)
	id, err := s.PutBlob(ctx, strings.NewReader("short"))
	if err != nil {
		t.Fatal(err)
	}
	// Corrupt the stored blob (truncate) so that the length no longer matches.
	if err = os.WriteFile(s.blobPath(id), []byte("sh"), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := os.Open(s.blobPath(id)) // raw handle without FILE_SHARE_DELETE
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err = s.PutBlob(ctx, strings.NewReader("short")); err == nil {
		t.Fatal("a truncated blob must not be treated as the identical blob")
	}
}

// Only "destination in use" errnos may trigger the fallback.
func TestPutBlobWindowsSyscallDeniedClassification(t *testing.T) {
	for _, errno := range []syscall.Errno{5, 32, 33} {
		if !replaceDenied(errno) {
			t.Fatalf("errno %d should be classified as destination-in-use", errno)
		}
	}
	if replaceDenied(syscall.Errno(2)) || replaceDenied(io.ErrUnexpectedEOF) || replaceDenied(nil) {
		t.Fatal("unrelated errors classified as destination-in-use")
	}
}
