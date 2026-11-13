package syncer

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
)

type heldPendingFile struct {
	Source
	token   string
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *heldPendingFile) ObserveFile(ctx context.Context, token string) (ObservedFile, error) {
	if token != s.token {
		return s.Source.ObserveFile(ctx, token)
	}
	s.once.Do(func() { close(s.started) })
	select {
	case <-s.release:
		return ObservedFile{}, model.ErrVersionChanged
	case <-ctx.Done():
		return ObservedFile{}, ctx.Err()
	}
}

// A regression to serial upload work, or returning from the entire cycle for a
// blocked/conflicted job, must not prevent another entry's durable publication.
func TestBackgroundCycleIsolatesBlockedConflictedAndSlowFiles(t *testing.T) {
	for _, state := range []string{"blocked", "conflict", "slow"} {
		t.Run(state, func(t *testing.T) {
			s, engine, source, other := setup(t, model.KindFile)
			healthy, err := s.ObserveEntry(bg, store.Observation{Remote: "healthy.bin", Kind: model.KindFile, Token: "healthy"})
			if err != nil {
				t.Fatal(err)
			}
			sn, err := engine.observeFile(bg, healthy.Token)
			if err != nil {
				t.Fatal(err)
			}
			healthy, err = s.AdoptFileSnapshot(bg, healthy.ID, healthy.Generation, sn.ID)
			if err != nil {
				t.Fatal(err)
			}
			other = stageContent(t, s, other, "other local edit")
			jobs, err := s.Pending(bg)
			if err != nil {
				t.Fatal(err)
			}
			switch state {
			case "blocked":
				if _, _, err = s.Prepare(bg, jobs[0].ID); err != nil {
					t.Fatal(err)
				}
				if err = s.PauseJob(bg, jobs[0].ID, model.StatusBlocked, "permission_denied"); err != nil {
					t.Fatal(err)
				}
			case "conflict":
				if err = s.SaveConflict(bg, model.Conflict{ID: model.ConflictID(uuid.NewString()), EntryID: other.ID, Generation: other.Generation, Base: other.LocalCheckpoint, Local: other.Local, Remote: sn.FileData, RemoteRevision: "reviewed", Reason: "overlapping_edits"}); err != nil {
					t.Fatal(err)
				}
			}
			held := &heldPendingFile{Source: source, token: other.Token, started: make(chan struct{}), release: make(chan struct{})}
			if state == "slow" {
				engine.source = held
			}
			var release sync.Once
			defer release.Do(func() { close(held.release) })
			stageContent(t, s, healthy, "healthy upload")
			published := make(chan struct{}, 1)
			engine.publisher = PublisherFunc(func(ctx context.Context, c model.PublishCandidate) error {
				_, err := s.CommitPublication(ctx, c)
				if err == nil && c.EntryID == healthy.ID {
					published <- struct{}{}
				}
				return err
			})
			now := time.Now()
			engine.opt.Now = func() time.Time { return now }
			if err = engine.backgroundCycle(bg); err != nil {
				t.Fatal(err)
			}
			if source.writes != 0 {
				t.Fatal("background upload bypassed five-minute schedule")
			}
			now = now.Add(5 * time.Minute)
			ctx, cancel := context.WithCancel(bg)
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- engine.backgroundCycle(ctx) }()
			if state == "slow" {
				select {
				case <-held.started:
				case <-time.After(time.Second):
					t.Fatal("slow entry never reached cloud observation")
				}
			}
			select {
			case <-published:
			case <-time.After(time.Second):
				cancel()
				release.Do(func() { close(held.release) })
				<-done
				t.Fatal("another file prevented healthy upload")
			}
			current, err := s.Entry(bg, healthy.ID)
			if err != nil || current.Status != model.StatusSynced || blobText(t, s, current.Visible) != "healthy upload" {
				t.Fatalf("healthy publication missing: %+v %v", current, err)
			}
			if state == "slow" {
				select {
				case <-done:
					t.Fatal("cycle returned while slow file still pending")
				default:
				}
			}
			release.Do(func() { close(held.release) })
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if state == "blocked" || state == "conflict" {
				current, err = s.Entry(bg, other.ID)
				want := model.StatusBlocked
				if state == "conflict" {
					want = model.StatusConflict
				}
				if err != nil || current.Status != want || blobText(t, s, current.Local) != "other local edit" {
					t.Fatalf("unrelated file was changed: %+v %v", current, err)
				}
			}
		})
	}
}
