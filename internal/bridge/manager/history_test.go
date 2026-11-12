package manager

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rclone/rclone/internal/bridge/history"
	"github.com/rclone/rclone/internal/bridge/model"
)

func TestWorkerAutomaticallyArchivesLocalHistory(t *testing.T) {
	m, id, _, cancel := fixture(t, nil)
	defer cancel()
	ctx := context.Background()
	if _, err := m.Start(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	s := m.localWorker(id).services.store
	e, err := s.EnsureEntry(ctx, "notes.md", model.KindFile)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = s.Stage(ctx, e.ID, nil, strings.NewReader("saved without network"))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(8 * time.Second)
	for {
		_, ack, err := s.HistoryCursor(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if ack >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background history never archived")
		}
		time.Sleep(20 * time.Millisecond)
	}
	spec, err := m.spec(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	page, err := m.HistoryList(ctx, HistoryQuery{ProfileID: spec.Profile.ID, Query: history.Query{EntryID: e.ID, Kind: "local_saved"}})
	if err != nil || len(page.Versions) != 1 {
		t.Fatalf("history list %+v %v", page, err)
	}
	content, err := m.HistoryBody(ctx, spec.Profile.ID, page.Versions[0].ID, "local")
	if err != nil || string(content) != "saved without network" {
		t.Fatalf("body %q %v", content, err)
	}
}

func TestHistoryCursorRejectsChangedFilters(t *testing.T) {
	m, id, _, cancel := fixture(t, nil)
	defer cancel()
	ctx := context.Background()
	if _, err := m.Start(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	s := m.localWorker(id).services.store
	e, err := s.EnsureEntry(ctx, "notes.md", model.KindFile)
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"one", "two"} {
		if _, _, err = s.Stage(ctx, e.ID, nil, strings.NewReader(body)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = m.HistorySync(ctx, id); err != nil {
		t.Fatal(err)
	}
	page, err := m.HistoryList(ctx, HistoryQuery{Query: history.Query{Limit: 1}})
	if err != nil || page.Cursor == "" {
		t.Fatal(page, err)
	}
	if _, err = m.HistoryList(ctx, HistoryQuery{Cursor: page.Cursor, Query: history.Query{Limit: 1, Search: "changed"}}); !errors.Is(err, ErrConfig) {
		t.Fatalf("cursor reused across filters: %v", err)
	}
}

func TestPendingHistoryReadableWhenGitUnavailable(t *testing.T) {
	m, id, _, cancel := fixture(t, nil)
	defer cancel()
	m.options.GitExecutable = "/missing/drivebridge-git"
	ctx := context.Background()
	if _, err := m.Start(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	s := m.localWorker(id).services.store
	e, err := s.EnsureEntry(ctx, "pending.md", model.KindFile)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = s.Stage(ctx, e.ID, nil, strings.NewReader("saved before Git recovers"))
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := m.spec(ctx, id)
	page, err := m.HistoryPending(ctx, HistoryQuery{ProfileID: spec.Profile.ID, Query: history.Query{EntryID: e.ID}})
	if err != nil || len(page.Versions) != 1 {
		t.Fatalf("pending history %+v %v", page, err)
	}
	v := page.Versions[0]
	if !v.Pending {
		t.Fatal("pending version claimed archived")
	}
	body, err := m.HistoryBody(ctx, spec.Profile.ID, v.ID, "local")
	if err != nil || string(body) != "saved before Git recovers" {
		t.Fatalf("pending body %q %v", body, err)
	}
}
