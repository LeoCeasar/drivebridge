package syncer

import (
	"errors"
	"github.com/rclone/rclone/internal/bridge/markdown"
	"os"
	"strings"
	"testing"

	"github.com/rclone/rclone/internal/bridge/model"
)

func TestPausePersistsSafeFailureTypeWithoutInventingSource(t *testing.T) {
	s, e, _, en := setup(t, model.KindDocx)
	en = stageContent(t, s, en, "local text\n")
	jobs, err := s.Pending(bg)
	if err != nil || len(jobs) != 1 {
		t.Fatal(jobs, err)
	}
	job, _, err := s.Prepare(bg, jobs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	sensitive := errors.Join(os.ErrPermission, errors.New("CLI --token secret-not-for-status"))
	if err := e.pause(bg, job, sensitive); !errors.Is(err, os.ErrPermission) {
		t.Fatal(err)
	}
	journal, err := s.Journal(bg, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if journal.Diagnostic == nil || journal.Diagnostic.Kind != "permission_denied" || journal.Diagnostic.Message == "" {
		t.Fatalf("missing diagnostic: %+v", journal)
	}
	if journal.Diagnostic.Line != 0 || journal.Diagnostic.Text != "" || journal.Diagnostic.Side != "" {
		t.Fatalf("invented source: %+v", journal.Diagnostic)
	}
	if strings.Contains(journal.Diagnostic.Message, "secret-not-for-status") || strings.Contains(journal.Diagnostic.Message, "--token") {
		t.Fatal("raw transport error exposed")
	}
}

func TestPauseDiagnosticUsesImmutableJobInsteadOfNewerSave(t *testing.T) {
	s, e, _, en := setup(t, model.KindDocx)
	captured := "alpha one\n\n<span>captured unsupported block</span>\n\nbeta two\n"
	en = stageContent(t, s, en, captured)
	jobs, err := s.Pending(bg)
	if err != nil || len(jobs) != 1 {
		t.Fatal(jobs, err)
	}
	job, _, err := s.Prepare(bg, jobs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	_, contentErr := markdown.Encode([]byte(captured))
	if contentErr == nil {
		t.Fatal("fixture must be unsupported content")
	}
	newer := stageContent(t, s, en, "newer safe content\n")
	if err := e.pause(bg, job, contentErr); !errors.Is(err, contentErr) {
		t.Fatal(err)
	}
	journal, err := s.Journal(bg, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	d := journal.Diagnostic
	if d == nil || d.Kind == "" || d.Message == "" || d.Line != 3 || !strings.Contains(d.Text, "captured unsupported block") || d.Side != "local" {
		t.Fatalf("missing captured source: %+v", d)
	}
	current, err := s.Entry(bg, en.ID)
	if err != nil || current.Local != newer.Local || current.Status == model.StatusBlocked {
		t.Fatalf("old failure blocked newer save: %+v %v", current, err)
	}
}

func TestPauseMappingDiagnosticKeepsCloudSnapshotLocations(t *testing.T) {
	s, e, _, en := setup(t, model.KindDocx)
	en = stageContent(t, s, en, "captured job\n")
	jobs, err := s.Pending(bg)
	if err != nil || len(jobs) != 1 {
		t.Fatal(jobs, err)
	}
	job, _, err := s.Prepare(bg, jobs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	current, err := markdown.Decode([]byte(`<title id="doc">Example</title><p id="p1">repeated cloud text</p><p id="p2">middle</p><p id="p3">repeated cloud text</p>`), "1")
	if err != nil {
		t.Fatal(err)
	}
	failure := markdown.ExplainPlanFailure([]byte("# Example\n\nrepeated cloud text\n\nupdated\n\nrepeated cloud text\n"), current, "ambiguous_mapping")
	newer := stageContent(t, s, en, "newer editor save at different lines\n")
	_ = e.pause(bg, job, failure)
	journal, err := s.Journal(bg, job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if journal.Diagnostic == nil || journal.Diagnostic.LocationCount != 2 || len(journal.Diagnostic.Locations) != 2 {
		t.Fatal(journal.Diagnostic)
	}
	for _, loc := range journal.Diagnostic.Locations {
		if loc.Side != "remote" || loc.Text != "repeated cloud text" {
			t.Fatal(loc)
		}
	}
	latest, err := s.Entry(bg, en.ID)
	if err != nil || latest.Local != newer.Local || latest.Status == model.StatusBlocked {
		t.Fatal("diagnosis changed newer save", latest, err)
	}
}
