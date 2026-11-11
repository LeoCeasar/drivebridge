package integration

import (
	"testing"

	"github.com/rclone/rclone/internal/bridge/model"
)

const canonicalSizeInput = "# DriveBridge sync validation\n\nThis is synthetic test content only.\n\n- version: local-1\n- expected: upload and readback\n"

func TestManagedMarkdownPublicationUpdatesSizeAndContent(t *testing.T) {
	h := newHarness(t)
	const name = "sync-check.md"
	entry := h.save(name, canonicalSizeInput)
	if err := h.engine.SyncEntry(h.ctx, entry.ID); err != nil {
		t.Fatal(err)
	}
	entry = h.entry(name)
	if entry.Status != model.StatusSynced {
		t.Fatalf("not synced: %+v", entry)
	}
	canonical := h.blob(entry.Visible)
	if len(canonical) == len(canonicalSizeInput) {
		t.Fatal("fixture does not change projected size")
	}
	info, err := h.vfs.Stat(name)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != int64(len(canonical)) || h.read(name) != canonical {
		t.Fatalf("raw=%d canonical=%d stat=%d read=%q want=%q", len(canonicalSizeInput), len(canonical), info.Size(), h.read(name), canonical)
	}
	t.Logf("raw=%d canonical=%d stat=%d", len(canonicalSizeInput), len(canonical), info.Size())
}
