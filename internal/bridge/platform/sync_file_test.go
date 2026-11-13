package platform

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestSyncFilePersistsAndReportsClosedHandles(t *testing.T) {
	p := filepath.Join(t.TempDir(), "durable")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteString("durable local bytes"); err != nil {
		t.Fatal(err)
	}
	if err = SyncFile(f); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	if err = SyncFile(f); !errors.Is(err, os.ErrClosed) {
		t.Fatal("closed handle sync must fail", err)
	}
	b, err := os.ReadFile(p)
	if err != nil || string(b) != "durable local bytes" {
		t.Fatal(string(b), err)
	}
}
