package store

import (
	"io"
	"os"
	"strings"
	"testing"
	"time"
)

func TestPutBlobIdenticalContentWhileReaderOpen(t *testing.T) {
	s, _, _ := openTest(t)
	const body = "identical content-addressed bytes"
	id, err := s.PutBlob(ctx, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	// Age the blob so a refreshed grace timestamp is observable.
	old := time.Now().Add(-48 * time.Hour)
	if err = os.Chtimes(s.blobPath(id), old, old); err != nil {
		t.Fatal(err)
	}
	reader, err := s.ReadBlob(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()

	again, err := s.PutBlob(ctx, strings.NewReader(body))
	if err != nil {
		t.Fatalf("re-putting identical content while a reader holds the blob open: %v", err)
	}
	if again != id {
		t.Fatalf("blob id changed: %s != %s", again, id)
	}
	// The reader obtained before the re-put is still valid.
	got, err := io.ReadAll(reader)
	if err != nil || string(got) != body {
		t.Fatalf("reader after re-put: %q %v", got, err)
	}
	// A fresh reader sees the same bytes.
	fresh, err := s.ReadBlob(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	got, _ = io.ReadAll(fresh)
	fresh.Close()
	if string(got) != body {
		t.Fatalf("fresh reader: %q", got)
	}
	// The grace timestamp was refreshed, so an immediate GC keeps the blob that
	// an accepting transaction may still be about to reference.
	info, err := os.Stat(s.blobPath(id))
	if err != nil {
		t.Fatal(err)
	}
	if time.Since(info.ModTime()) > time.Hour {
		t.Fatalf("grace timestamp not refreshed: %v", info.ModTime())
	}
	if _, err = s.GC(ctx, GCOptions{Now: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(s.blobPath(id)); err != nil {
		t.Fatalf("freshly re-put blob was collected inside its grace window: %v", err)
	}
	// No prepared temp files are left behind.
	entries, err := os.ReadDir(s.dir + string(os.PathSeparator) + "blobs")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".prepared-") {
			t.Fatalf("leftover temp file %s", e.Name())
		}
	}
}

func TestPutBlobSameContentTwice(t *testing.T) {
	s, _, _ := openTest(t)
	first, err := s.PutBlob(ctx, strings.NewReader("two files, one body"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.PutBlob(ctx, strings.NewReader("two files, one body"))
	if err != nil || first != second {
		t.Fatal(first, second, err)
	}
}

// Failures that are not "destination in use" must never be swallowed, even if
// an equal blob exists.
func TestPutBlobRenameFailureNotMaskedByExistingBlob(t *testing.T) {
	s, _, _ := openTest(t)
	if _, err := s.PutBlob(ctx, strings.NewReader("present")); err != nil {
		t.Fatal(err)
	}
	s.io = failingIO{"rename"}
	if _, err := s.PutBlob(ctx, strings.NewReader("present")); err == nil || !strings.Contains(err.Error(), "injected rename") {
		t.Fatalf("injected rename failure was masked: %v", err)
	}
}
