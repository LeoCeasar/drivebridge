package syncer

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
	bolt "go.etcd.io/bbolt"
)

func TestWriteBackDefersQueuedDispatchUntilPersistentDeadline(t *testing.T) {
	for _, delay := range []time.Duration{2 * time.Second, 10 * time.Second} {
		t.Run(delay.String(), func(t *testing.T) {
			s, e, source, entry := setup(t, model.KindFile)
			entry = stageContent(t, s, entry, "complete local payload")
			jobs, _ := s.Pending(bg)
			journal, err := s.Journal(bg, jobs[0].ID)
			if err != nil || journal.StagedAt.IsZero() {
				t.Fatal("missing staging clock", journal, err)
			}
			now := journal.StagedAt.Add(delay - time.Nanosecond)
			e.opt.WriteBack = delay
			e.opt.Now = func() time.Time { return now }
			if err := e.SyncEntry(bg, entry.ID); err != nil {
				t.Fatal(err)
			}
			jobs, err = s.Pending(bg)
			if err != nil || source.writes != 0 || len(jobs) != 1 || jobs[0].State != model.StatusQueued {
				t.Fatalf("dispatched early: writes=%d jobs=%+v err=%v", source.writes, jobs, err)
			}
			scope, err := s.Scope(bg)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := store.Open(source.directory)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if err = reopened.BindScope(bg, scope); err != nil {
				t.Fatal(err)
			}
			recovered, err := reopened.Journal(bg, jobs[0].ID)
			if err != nil || !recovered.StagedAt.Equal(journal.StagedAt) {
				t.Fatal("staging clock changed on restart", recovered, err)
			}
			e, err = New(Dependencies{Store: reopened, Source: source, Publisher: PublisherFunc(func(ctx context.Context, c model.PublishCandidate) error {
				_, err := reopened.CommitPublication(ctx, c)
				return err
			})}, Options{WriteBack: delay, Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			if err = e.SyncEntry(bg, entry.ID); err != nil || source.writes != 0 {
				t.Fatal("restart skipped delay", source.writes, err)
			}
			now = journal.StagedAt.Add(delay)
			if err = e.SyncEntry(bg, entry.ID); err != nil || source.writes != 1 {
				t.Fatal("deadline did not dispatch", source.writes, err)
			}
		})
	}
}

func TestNewSaveResetsWriteBackDeadline(t *testing.T) {
	s, e, source, entry := setup(t, model.KindFile)
	entry = stageContent(t, s, entry, "first save")
	jobs, _ := s.Pending(bg)
	first, _ := s.Journal(bg, jobs[0].ID)
	entry = stageContent(t, s, entry, "latest save")
	jobs, _ = s.Pending(bg)
	latest, _ := s.Journal(bg, jobs[0].ID)
	now := first.StagedAt.Add(2 * time.Second)
	e.opt.WriteBack = 2 * time.Second
	e.opt.Now = func() time.Time { return now }
	if err := e.SyncEntry(bg, entry.ID); err != nil || source.writes != 0 {
		t.Fatal("new save inherited old deadline", source.writes, err)
	}
	now = latest.StagedAt.Add(2 * time.Second)
	if err := e.SyncEntry(bg, entry.ID); err != nil || source.writes != 1 || string(source.file) != "latest save" {
		t.Fatal(source.writes, string(source.file), err)
	}
}

func TestWriteBackLeavesPlannedAndUnknownReconciliationImmediate(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		s, e, source, entry := setup(t, model.KindFile)
		entry = stageContent(t, s, entry, "already dispatched bytes")
		jobs, _ := s.Pending(bg)
		job := jobs[0]
		journal, _ := s.Journal(bg, job.ID)
		if _, _, err := s.Prepare(bg, job.ID); err != nil {
			t.Fatal(err)
		}
		if err := s.RecordPlan(bg, job.ID, nil, job.Local); err != nil {
			t.Fatal(err)
		}
		if unknown {
			if err := s.MarkUnknown(bg, job.ID); err != nil {
				t.Fatal(err)
			}
		}
		source.file = []byte("already dispatched bytes")
		e.opt.WriteBack = 24 * time.Hour
		e.opt.Now = func() time.Time { return journal.StagedAt }
		if err := e.SyncEntry(bg, entry.ID); err != nil {
			t.Fatal(err)
		}
		current, _ := s.Entry(bg, entry.ID)
		if current.Status != model.StatusSynced || source.writes != 0 {
			t.Fatal("reconciliation delayed or redispatched", current, source.writes)
		}
	}
}

func TestLegacyQueuedJobWithoutTimestampDispatchesImmediately(t *testing.T) {
	s, e, source, entry := setup(t, model.KindFile)
	entry = stageContent(t, s, entry, "legacy payload")
	jobs, _ := s.Pending(bg)
	scope, _ := s.Scope(bg)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	database, err := bolt.Open(filepath.Join(source.directory, "store.db"), 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = database.Update(func(tx *bolt.Tx) error {
		bucket := tx.Bucket([]byte("jobs"))
		key := []byte(jobs[0].ID)
		var record map[string]json.RawMessage
		if err := json.Unmarshal(bucket.Get(key), &record); err != nil {
			return err
		}
		delete(record, "staged_at")
		raw, err := json.Marshal(record)
		if err != nil {
			return err
		}
		return bucket.Put(key, raw)
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = database.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(source.directory)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err = reopened.BindScope(bg, scope); err != nil {
		t.Fatal(err)
	}
	e, err = New(Dependencies{Store: reopened, Source: source, Publisher: PublisherFunc(func(ctx context.Context, c model.PublishCandidate) error {
		_, err := reopened.CommitPublication(ctx, c)
		return err
	})}, Options{WriteBack: 24 * time.Hour, Now: func() time.Time { return time.Unix(0, 0) }})
	if err != nil {
		t.Fatal(err)
	}
	if err = e.SyncEntry(bg, entry.ID); err != nil || source.writes != 1 {
		t.Fatal("legacy work delayed", source.writes, err)
	}
}

func TestWriteBackKeepsPlaceholderCancellableAndEmptyUploadBlocked(t *testing.T) {
	s, e, source, _ := setup(t, model.KindFile)
	entry, err := s.EnsureEntry(bg, "placeholder.txt", model.KindFile)
	if err != nil {
		t.Fatal(err)
	}
	entry = stageContent(t, s, entry, "")
	jobs, _ := s.Pending(bg)
	journal, _ := s.Journal(bg, jobs[0].ID)
	now := journal.StagedAt.Add(100 * time.Millisecond)
	e.opt.WriteBack = 2 * time.Second
	e.opt.Now = func() time.Time { return now }
	if err = e.SyncEntry(bg, entry.ID); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.CancelUnsentLocalFile(bg, entry.ID, entry.Generation); err != nil || !ok {
		t.Fatal("engine claimed placeholder before delay", ok, err)
	}
	entry, err = s.EnsureEntry(bg, "placeholder.txt", model.KindFile)
	if err != nil {
		t.Fatal(err)
	}
	entry = stageContent(t, s, entry, "")
	jobs, _ = s.Pending(bg)
	journal, _ = s.Journal(bg, jobs[0].ID)
	now = journal.StagedAt.Add(2 * time.Second)
	if err = e.SyncEntry(bg, entry.ID); !errors.Is(err, model.ErrUnsupportedContent) || source.writes != 0 {
		t.Fatal("empty body dispatched", source.writes, err)
	}
}
