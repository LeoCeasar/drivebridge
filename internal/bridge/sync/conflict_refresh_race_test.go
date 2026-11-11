package syncer

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rclone/rclone/internal/bridge/cloud"
	"github.com/rclone/rclone/internal/bridge/model"
)

type blockedConflictFetch struct {
	Source
	entered chan struct{}
	resume  chan struct{}
}

func (s blockedConflictFetch) FetchDoc(ctx context.Context, token, revision string) (cloud.Document, error) {
	close(s.entered)
	select {
	case <-s.resume:
		return s.Source.FetchDoc(ctx, token, revision)
	case <-ctx.Done():
		return cloud.Document{}, ctx.Err()
	}
}

func TestRefreshConflictKeepsDraftSavedDuringCloudFetch(t *testing.T) {
	s, e, source, entry := setup(t, model.KindDocx)
	entry = stageContent(t, s, entry, "alpha local\n\nbeta two\n")
	source.xml = strings.Replace(source.xml, "alpha one", "alpha remote", 1)
	source.revision++
	if err := e.SyncEntry(bg, entry.ID); err == nil {
		t.Fatal("expected overlapping edits")
	}
	list, err := s.Conflicts(bg)
	if err != nil || len(list) != 1 {
		t.Fatal(list, err)
	}
	captured := list[0]
	if err = s.SaveDraft(bg, captured.ID, strings.NewReader("old unfinished draft")); err != nil {
		t.Fatal(err)
	}
	source.xml = strings.Replace(source.xml, "alpha remote", "alpha remote newer", 1)
	source.revision++
	gate := blockedConflictFetch{Source: e.source, entered: make(chan struct{}), resume: make(chan struct{})}
	e.source = gate
	ctx, cancel := context.WithTimeout(bg, 5*time.Second)
	defer cancel()
	type result struct {
		conflict model.Conflict
		err      error
	}
	done := make(chan result, 1)
	go func() {
		c, err := e.RefreshConflict(ctx, captured.ID)
		done <- result{c, err}
	}()
	select {
	case <-gate.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	const latestDraft = "merged-B draft saved while refresh is fetching"
	if err = s.SaveDraft(ctx, captured.ID, strings.NewReader(latestDraft)); err != nil {
		t.Fatal(err)
	}
	close(gate.resume)
	refreshed := <-done
	if refreshed.err != nil {
		t.Fatal(refreshed.err)
	}
	list, err = s.Conflicts(ctx)
	if err != nil || len(list) != 1 {
		t.Fatal(list, err)
	}
	if got := blobText(t, s, list[0].Draft); got != latestDraft {
		t.Fatalf("refresh rolled back the saved draft: got %q want %q", got, latestDraft)
	}
	if got := blobText(t, s, refreshed.conflict.Draft); got != latestDraft {
		t.Fatalf("refresh returned a stale draft: got %q want %q", got, latestDraft)
	}
	if refreshed.conflict.RemoteRevision != "3" || !strings.Contains(blobText(t, s, refreshed.conflict.Remote), "remote newer") {
		t.Fatal("refresh lost current remote inputs", refreshed.conflict)
	}
}
