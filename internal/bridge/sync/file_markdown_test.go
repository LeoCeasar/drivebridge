package syncer

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strconv"
	"testing"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
	"github.com/rclone/rclone/lib/larkcli"
)

type markdownFileCloud struct {
	*fixtureCloud
	observations  int
	beforeObserve func(int)
	afterUpload   func()
	failReadback  bool
}

func (c *markdownFileCloud) ObserveFile(ctx context.Context, token string) (ObservedFile, error) {
	c.observations++
	if c.beforeObserve != nil {
		c.beforeObserve(c.observations)
	}
	if c.failReadback && c.writes > 0 {
		return ObservedFile{}, context.DeadlineExceeded
	}
	return c.fixtureCloud.ObserveFile(ctx, token)
}
func (c *markdownFileCloud) UploadFile(ctx context.Context, parent, token, name string, r io.Reader) (larkcli.UploadResult, error) {
	v, err := c.fixtureCloud.UploadFile(ctx, parent, token, name, r)
	if c.afterUpload != nil {
		f := c.afterUpload
		c.afterUpload = nil
		f()
	}
	return v, err
}
func plainFileSetup(t *testing.T, name, base string) (*store.Store, *Engine, *markdownFileCloud, model.Entry) {
	t.Helper()
	s, e, c, en := setup(t, model.KindFile)
	if name != "note.md" {
		var err error
		en, err = s.ObserveEntry(bg, store.Observation{Remote: name, CloudName: name, Kind: model.KindFile, Token: "plain-resource"})
		if err != nil {
			t.Fatal(err)
		}
	}
	c.file = []byte(base)
	sn, err := e.observeFile(bg, en.Token)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := s.OfferRemote(bg, en.ID, en.Generation, sn, sn.FileData)
	if err != nil {
		t.Fatal(err)
	}
	en, err = s.CommitPublication(bg, candidate)
	if err != nil {
		t.Fatal(err)
	}
	source := &markdownFileCloud{fixtureCloud: c}
	e.source = source
	return s, e, source, en
}
func TestPlainMarkdownThreeWaySync(t *testing.T) {
	const base = "alpha one\nbeta two\n"
	for _, tc := range []struct {
		name, filename, local, remote, want string
		writes                              int
		conflict                            bool
	}{
		{"local_only", "note.md", "alpha local\nbeta two\n", base, "alpha local\nbeta two\n", 1, false},
		{"remote_only", "note.md", base, "alpha one\nbeta remote\n", "alpha one\nbeta remote\n", 0, false},
		{"same_changes", "note.md", "same\n", "same\n", "same\n", 0, false},
		{"independent", "note.md", "alpha local\nbeta two\n", "alpha one\nbeta remote\n", "alpha local\nbeta remote\n", 1, false},
		{"uppercase", "NOTE.MD", "alpha local\nbeta two\n", "alpha one\nbeta remote\n", "alpha local\nbeta remote\n", 1, false},
		{"overlap", "note.md", "alpha local\nbeta two\n", "alpha cloud\nbeta two\n", "alpha cloud\nbeta two\n", 0, true},
		{"other_file_overwrites", "data.txt", "alpha local\nbeta two\n", "alpha cloud\nbeta two\n", "alpha local\nbeta two\n", 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, e, c, en := plainFileSetup(t, tc.filename, base)
			en = stageContent(t, s, en, tc.local)
			c.file = []byte(tc.remote)
			err := e.SyncEntry(bg, en.ID)
			conflicts, _ := s.Conflicts(bg)
			if tc.conflict {
				if !errors.Is(err, model.ErrVersionChanged) || len(conflicts) != 1 {
					t.Fatalf("expected conflict: %v %+v", err, conflicts)
				}
				if blobText(t, s, conflicts[0].Base) != base || blobText(t, s, conflicts[0].Local) != tc.local || blobText(t, s, conflicts[0].Remote) != tc.remote {
					t.Fatal("conflict lost inputs")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				fresh, _ := s.Entry(bg, en.ID)
				if fresh.Status != model.StatusSynced || blobText(t, s, fresh.Visible) != tc.want {
					t.Fatalf("not published %+v", fresh)
				}
			}
			if c.writes != tc.writes || string(c.file) != tc.want {
				t.Fatalf("writes=%d cloud=%q want=%q", c.writes, c.file, tc.want)
			}
		})
	}
}
func TestPlainMarkdownRechecksRemoteBeforeDispatch(t *testing.T) {
	s, e, c, en := plainFileSetup(t, "note.md", "alpha one\nbeta two\n")
	en = stageContent(t, s, en, "alpha local\nbeta two\n")
	c.beforeObserve = func(n int) {
		if n == 2 {
			c.file = []byte("alpha one\nbeta later\n")
		}
	}
	if err := e.SyncEntry(bg, en.ID); err != nil {
		t.Fatal(err)
	}
	if string(c.file) != "alpha local\nbeta later\n" || c.writes != 1 {
		t.Fatalf("overwrote recheck %q writes %d", c.file, c.writes)
	}
}
func TestPlainMarkdownRepeatedRemoteDriftDoesNotUpload(t *testing.T) {
	s, e, c, en := plainFileSetup(t, "note.md", "alpha one\nbeta two\n")
	en = stageContent(t, s, en, "alpha local\nbeta two\n")
	c.beforeObserve = func(n int) { c.file = []byte("alpha one\nbeta drift " + strconv.Itoa(n) + "\n") }
	if err := e.SyncEntry(bg, en.ID); err == nil {
		t.Fatal("drift not reported")
	}
	conflicts, _ := s.Conflicts(bg)
	jobs, _ := s.Pending(bg)
	if c.writes != 0 || len(conflicts) != 0 || len(jobs) != 1 || jobs[0].State != model.StatusPaused {
		t.Fatalf("clean remote drift became human conflict: writes=%d conflicts=%d jobs=%+v", c.writes, len(conflicts), jobs)
	}
	journal, _ := s.Journal(bg, jobs[0].ID)
	if journal.ErrorCode != "remote_drift" || !transient(journal.ErrorCode) {
		t.Fatalf("drift is not retryable %s", journal.ErrorCode)
	}
}
func TestPlainMarkdownMergedUploadRecoveryUsesDesired(t *testing.T) {
	s, e, c, en := plainFileSetup(t, "note.md", "alpha one\nbeta two\n")
	en = stageContent(t, s, en, "alpha local\nbeta two\n")
	c.file = []byte("alpha one\nbeta remote\n")
	c.failReadback = true
	if err := e.SyncEntry(bg, en.ID); err == nil {
		t.Fatal("readback outage falsely confirmed")
	}
	c.failReadback = false
	scope, err := s.Scope(bg)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(c.directory)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err = reopened.BindScope(bg, scope); err != nil {
		t.Fatal(err)
	}
	resumed, err := New(Dependencies{Store: reopened, Source: c, Publisher: PublisherFunc(func(ctx context.Context, p model.PublishCandidate) error {
		_, err := reopened.CommitPublication(ctx, p)
		return err
	})}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err = resumed.SyncEntry(bg, en.ID); err != nil {
		t.Fatal(err)
	}
	fresh, _ := reopened.Entry(bg, en.ID)
	if c.writes != 1 || blobText(t, reopened, fresh.Visible) != "alpha local\nbeta remote\n" || fresh.Status != model.StatusSynced {
		t.Fatalf("recovery %+v writes=%d cloud=%q", fresh, c.writes, c.file)
	}
}
func TestPlainMarkdownOlderUploadPreservesNewerSave(t *testing.T) {
	s, e, c, en := plainFileSetup(t, "note.md", "alpha one\nbeta two\n")
	en = stageContent(t, s, en, "alpha local\nbeta two\n")
	c.file = []byte("alpha one\nbeta remote\n")
	c.afterUpload = func() { stageContent(t, s, en, "alpha newest\nbeta two\n") }
	if err := e.SyncEntry(bg, en.ID); err != nil && !errors.Is(err, model.ErrGenerationChanged) {
		t.Fatal(err)
	}
	latest, _ := s.Entry(bg, en.ID)
	if blobText(t, s, latest.Local) != "alpha newest\nbeta two\n" {
		t.Fatal("newer save overwritten")
	}
	if err := e.SyncEntry(bg, en.ID); err != nil {
		t.Fatal(err)
	}
	latest, _ = s.Entry(bg, en.ID)
	if !bytes.Equal(c.file, []byte("alpha newest\nbeta remote\n")) || latest.Status != model.StatusSynced {
		t.Fatalf("newer merge %q %+v", c.file, latest)
	}
}

func TestPlainMarkdownUnknownMergedPlanDoesNotConfirmOriginalLocal(t *testing.T) {
	s, e, c, en := plainFileSetup(t, "note.md", "alpha one\nbeta two\n")
	en = stageContent(t, s, en, "alpha local\nbeta two\n")
	jobs, _ := s.Pending(bg)
	if _, _, err := s.Prepare(bg, jobs[0].ID); err != nil {
		t.Fatal(err)
	}
	desired, err := s.PutBlob(bg, bytes.NewBufferString("alpha local\nbeta remote\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RecordPlan(bg, jobs[0].ID, nil, desired); err != nil {
		t.Fatal(err)
	}
	c.file = []byte("alpha local\nbeta two\n")
	if err = e.SyncEntry(bg, en.ID); !errors.Is(err, model.ErrUnknownResult) {
		t.Fatalf("lost merged intent: %v", err)
	}
	fresh, _ := s.Entry(bg, en.ID)
	if fresh.Status == model.StatusSynced || c.writes != 0 {
		t.Fatal("unknown mutation replayed or falsely confirmed", fresh, c.writes)
	}
	journal, err := s.Journal(bg, jobs[0].ID)
	if err != nil || journal.Desired != desired {
		t.Fatal("merged plan not retained", journal, err)
	}
}
func TestPlainMarkdownUnknownBaseAndBinaryNeverOverwrite(t *testing.T) {
	for _, tc := range []string{"missing_base", "unsupported_content"} {
		t.Run(tc, func(t *testing.T) {
			s, e, c, en := plainFileSetup(t, "note.md", "base\n")
			if tc == "missing_base" {
				var err error
				en, err = s.ObserveEntry(bg, store.Observation{Remote: "unbased.md", Kind: model.KindFile, Token: "unbased"})
				if err != nil {
					t.Fatal(err)
				}
			} else {
				c.file = []byte{0xff, 0, 1}
			}
			original := bytes.Clone(c.file)
			en = stageContent(t, s, en, "local\n")
			if err := e.SyncEntry(bg, en.ID); !errors.Is(err, model.ErrVersionChanged) {
				t.Fatal(err)
			}
			conflicts, _ := s.Conflicts(bg)
			if len(conflicts) != 1 || conflicts[0].Reason != tc || c.writes != 0 || !bytes.Equal(c.file, original) {
				t.Fatal("unsafe overwrite", conflicts, c.writes)
			}
		})
	}
}
