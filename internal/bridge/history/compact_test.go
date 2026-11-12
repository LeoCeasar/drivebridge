package history_test

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/rclone/rclone/internal/bridge/history"
	"github.com/rclone/rclone/internal/bridge/model"
)

type replaySource struct{ Source history.Source }

func (s replaySource) HistoryCursor(ctx context.Context) (string, uint64, error) {
	id, _, err := s.Source.HistoryCursor(ctx)
	return id, 0, err
}
func (s replaySource) HistoryEvents(ctx context.Context, after uint64, limit int) ([]model.HistoryEvent, error) {
	return s.Source.HistoryEvents(ctx, after, limit)
}
func (s replaySource) ReadBlob(context.Context, model.BlobID) (io.ReadCloser, error) {
	return nil, errors.New("old source body already reclaimed")
}
func (s replaySource) AckHistory(ctx context.Context, seq uint64, id string) error {
	return s.Source.AckHistory(ctx, seq, id)
}
func TestCompactionPreservesPinsWatermarksAndOpenVersions(t *testing.T) {
	ctx := context.Background()
	s, o, e := fixture(t)
	for _, body := range []string{"one", "two", "three"} {
		if _, _, err := s.Stage(ctx, e.ID, nil, strings.NewReader(body)); err != nil {
			t.Fatal(err)
		}
	}
	r, err := history.Open(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = r.Ingest(ctx, s); err != nil {
		t.Fatal(err)
	}
	page, _ := r.List(ctx, history.Query{Kind: "local_saved"})
	old := page.Versions[2].ID
	middle := page.Versions[1].ID
	if err = r.Pin(ctx, old, true); err != nil {
		t.Fatal(err)
	}
	release, err := r.LeaseVersion(ctx, middle)
	if err != nil {
		t.Fatal(err)
	}
	source, ack, _ := s.HistoryCursor(ctx)
	policy := history.RetentionPolicy{Now: time.Now().Add(60 * 24 * time.Hour), Keep: 30 * 24 * time.Hour, MinimumVersions: 1, Acknowledged: map[string]uint64{source: ack}}
	if _, err = r.Compact(ctx, policy); err != nil {
		t.Fatal(err)
	}
	if b, err := r.Read(ctx, middle, "local"); err != nil || string(b) != "two" {
		t.Fatalf("lease lost %q %v", b, err)
	}
	release()
	result, err := r.Compact(ctx, policy)
	if err != nil || result.Expired == 0 {
		t.Fatalf("compaction %+v %v", result, err)
	}
	if b, err := r.Read(ctx, old, "local"); err != nil || string(b) != "one" {
		t.Fatalf("pin lost %q %v", b, err)
	}
	if _, err = r.Read(ctx, middle, "local"); !errors.Is(err, history.ErrBodyUnavailable) {
		t.Fatalf("expired still available %v", err)
	}
	replay, err := r.Ingest(ctx, replaySource{Source: s})
	if err != nil || replay.Archived != 0 {
		t.Fatalf("expired event replayed %+v %v", replay, err)
	}
	if _, err = r.Clean(ctx, policy.Now.Add(8*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	reopened, err := history.Open(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = reopened.Read(ctx, old, "local"); err != nil {
		t.Fatal(err)
	}
}
