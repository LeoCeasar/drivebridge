package syncer

import (
	"errors"
	"github.com/rclone/rclone/internal/bridge/cloud"
	"github.com/rclone/rclone/internal/bridge/history"
	"github.com/rclone/rclone/internal/bridge/model"
	"strings"
	"testing"
)

func TestGitCleanConcurrentTableChangesNeedNoHumanMerge(t *testing.T) {
	s, e, c, en := setup(t, model.KindDocx)
	c.xml = `<title id="doc">Title</title><h1 id="head">Heading</h1><table id="table"><thead><tr><th><p>Name</p></th></tr></thead><tbody><tr><td><p>first</p></td></tr><tr><td><p>middle</p></td></tr><tr><td><p>last</p></td></tr></tbody></table><p id="tail">Tail</p>`
	sn, _, err := e.saveDoc(bg, cloud.Document{Token: en.Token, Revision: "1", XML: []byte(c.xml)})
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := s.OfferRemote(bg, en.ID, en.Generation, sn, sn.Markdown)
	if err != nil {
		t.Fatal(err)
	}
	en, err = s.CommitPublication(bg, candidate)
	if err != nil {
		t.Fatal(err)
	}
	base := blobText(t, s, en.Local)
	en = stageContent(t, s, en, strings.Replace(base, "first", "local first", 1))
	c.xml = strings.Replace(c.xml, "last", "cloud last", 1)
	c.revision++
	if err = e.SyncEntry(bg, en.ID); err != nil {
		t.Fatal(err)
	}
	conflicts, _ := s.Conflicts(bg)
	current, _ := s.Entry(bg, en.ID)
	if len(conflicts) != 0 || current.Status != model.StatusSynced || !strings.Contains(c.xml, "local first") || !strings.Contains(c.xml, "cloud last") {
		t.Fatalf("clean table merge failed status=%s conflicts=%d", current.Status, len(conflicts))
	}
}

func TestGitCleanCapturedConflictRetriesWithoutManualAuthority(t *testing.T) {
	s, e, c, en, job, conflict := legacyFormatFixture(t, "alpha local\n\nbeta two\n")
	conflict.Reason = "ambiguous_mapping"
	if err := s.SaveConflict(bg, conflict); err != nil {
		t.Fatal(err)
	}
	if err := e.SyncEntry(bg, en.ID); err != nil {
		t.Fatal(err)
	}
	current, _ := s.Entry(bg, en.ID)
	conflicts, _ := s.Conflicts(bg)
	journal, _ := s.Journal(bg, job.ID)
	if current.Status != model.StatusSynced || len(conflicts) != 0 || c.writes == 0 || journal.HumanResolution {
		t.Fatalf("clean conflict not retried: %s conflicts=%d writes=%d", current.Status, len(conflicts), c.writes)
	}
}

func TestGitCleanWholePlainRewriteAutomaticallySyncs(t *testing.T) {
	s, e, c, en := setup(t, model.KindDocx)
	en = stageContent(t, s, en, "replacement\n")
	if err := e.SyncEntry(bg, en.ID); err != nil {
		t.Fatal(err)
	}
	current, _ := s.Entry(bg, en.ID)
	cs, _ := s.Conflicts(bg)
	if current.Status != model.StatusSynced || len(cs) != 0 || current.Token != en.Token || blobText(t, s, current.Visible) != "replacement\n" || c.writes == 0 {
		t.Fatal("rewrite not automatic", current, len(cs), c.writes)
	}
}

func TestGitFailureIsBlockedWithoutCreatingManualConflict(t *testing.T) {
	if got := classify(history.ErrGitUnavailable); got != "history_git_unavailable" {
		t.Fatalf("Git failure misclassified %s", got)
	}
	s, e, _, en := setup(t, model.KindDocx)
	en = stageContent(t, s, en, "local body\n")
	jobs, _ := s.Pending(bg)
	prepared, _, err := s.Prepare(bg, jobs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.pause(bg, prepared, history.ErrGitUnavailable); !errors.Is(err, history.ErrGitUnavailable) {
		t.Fatal(err)
	}
	current, _ := s.Entry(bg, en.ID)
	cs, _ := s.Conflicts(bg)
	if current.Status != model.StatusBlocked || len(cs) != 0 {
		t.Fatal("runtime failure became manual conflict", current, len(cs))
	}
}

func TestMergeTextNormalizesBreakBeforeComparingBothSides(t *testing.T) {
	_, e, _, _ := setup(t, model.KindDocx)
	got, reason, err := e.mergeText(bg, []byte("a<br>b\n"), []byte("a<br/>local\n"), []byte("cloud  \nb\n"))
	if err != nil || reason != "" || string(got) != "cloud  \nlocal\n" {
		t.Fatalf("format-only conflict %q %s %v", got, reason, err)
	}
}
func TestLegacyDraftDifferingOnlyInBreakSpellingAutomaticallyRetries(t *testing.T) {
	s, e, _, en, _, c := legacyFormatFixture(t, "alpha<br>local\n\nbeta two\n")
	if err := s.SaveDraft(bg, c.ID, strings.NewReader("alpha  \nlocal\n\nbeta two\n")); err != nil {
		t.Fatal(err)
	}
	if err := e.SyncEntry(bg, en.ID); err != nil {
		t.Fatal(err)
	}
	current, _ := s.Entry(bg, en.ID)
	cs, _ := s.Conflicts(bg)
	if current.Status != model.StatusSynced || len(cs) != 0 {
		t.Fatalf("format-only draft blocked %s conflicts=%d", current.Status, len(cs))
	}
}

func TestLegacyDraftConsecutiveBreakVariantsAutomaticallyRetry(t *testing.T) {
	s, e, source, en, job, c := legacyFormatFixture(t, "alpha<br><br>local\n\nbeta two\n")
	if err := s.SaveDraft(bg, c.ID, strings.NewReader("alpha<BR/><br />local\n\nbeta two\n")); err != nil {
		t.Fatal(err)
	}
	if err := e.SyncEntry(bg, en.ID); err != nil {
		journal, _ := s.Journal(bg, job.ID)
		t.Fatalf("%v cloud=%s journal=%+v", err, source.xml, journal)
	}
	current, _ := s.Entry(bg, en.ID)
	cs, _ := s.Conflicts(bg)
	if current.Status != model.StatusSynced || len(cs) != 0 {
		t.Fatalf("consecutive format-only draft blocked %s conflicts=%d", current.Status, len(cs))
	}
}
