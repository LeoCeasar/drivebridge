package syncer

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
)

func ordinaryMarkdownConflict(t *testing.T) (*store.Store, *Engine, *fixtureCloud, model.Entry, model.Conflict, model.Snapshot) {
	t.Helper()
	s, e, cloud, entry := setup(t, model.KindFile)
	entry = stageContent(t, s, entry, "local body\n")
	cloud.file = []byte("remote body\n")
	sn, err := e.observeFile(bg, entry.Token)
	if err != nil {
		t.Fatal(err)
	}
	c := model.Conflict{ID: model.ConflictID(uuid.NewString()), EntryID: entry.ID, Generation: entry.Generation, Base: entry.LocalCheckpoint, Local: entry.Local, Remote: sn.FileData, RemoteRevision: "sha256:" + string(sn.FileData), Draft: entry.Local, Reason: "overlapping_edits"}
	if err = s.SaveConflict(bg, c); err != nil {
		t.Fatal(err)
	}
	return s, e, cloud, entry, c, sn
}

func TestPlainMarkdownConflictCapturesFileContentIdentity(t *testing.T) {
	s, e, cloud, entry := setup(t, model.KindFile)
	entry = stageContent(t, s, entry, "local\n")
	cloud.file = []byte("remote\n")
	sn, err := e.observeFile(bg, entry.Token)
	if err != nil {
		t.Fatal(err)
	}
	jobs, _ := s.Pending(bg)
	if err = e.conflict(bg, jobs[0], sn, "overlapping_edits"); !errors.Is(err, model.ErrVersionChanged) {
		t.Fatal(err)
	}
	list, _ := s.Conflicts(bg)
	if len(list) != 1 || list[0].Remote != sn.FileData || list[0].RemoteRevision != "sha256:"+string(sn.FileData) || blobText(t, s, list[0].Base) != "old" {
		t.Fatal(list)
	}
}

func TestPlainMarkdownResolveRebasesAgainstFreshHash(t *testing.T) {
	s, e, cloud, entry, c, sn := ordinaryMarkdownConflict(t)
	resolved, job, err := e.Resolve(bg, model.ResolveRequest{ConflictID: c.ID, ExpectedGeneration: entry.Generation, ExpectedRemoteRevision: c.RemoteRevision, Content: []byte("merged human body\n")})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Generation != entry.Generation+1 || job.BaseLocal != sn.FileData || job.BaseRemote == "" || blobText(t, s, resolved.Local) != "merged human body\n" || cloud.writes != 0 {
		t.Fatal(resolved, job, cloud.writes)
	}
	baseline, err := s.Snapshot(bg, job.BaseRemote)
	if err != nil || baseline.FileData != sn.FileData || baseline.Revision != "" {
		t.Fatal(baseline, err)
	}
	journal, _ := s.Journal(bg, job.ID)
	if !journal.HumanResolution {
		t.Fatal(journal)
	}
	list, _ := s.Conflicts(bg)
	if len(list) != 0 {
		t.Fatal(list)
	}
}

func TestPlainMarkdownResolveRejectsDriftAndKeepsDraft(t *testing.T) {
	s, e, cloud, entry, c, _ := ordinaryMarkdownConflict(t)
	cloud.file = []byte("remote changed after review\n")
	_, _, err := e.Resolve(bg, model.ResolveRequest{ConflictID: c.ID, ExpectedGeneration: entry.Generation, ExpectedRemoteRevision: c.RemoteRevision, Content: []byte("my unsent draft\n")})
	if !errors.Is(err, model.ErrVersionChanged) {
		t.Fatal(err)
	}
	list, _ := s.Conflicts(bg)
	if len(list) != 1 || blobText(t, s, list[0].Draft) != "my unsent draft\n" || cloud.writes != 0 {
		t.Fatal(list, cloud.writes)
	}
	current, _ := s.Entry(bg, entry.ID)
	if current.Generation != entry.Generation || current.Local != entry.Local {
		t.Fatal(current)
	}
}

type plainResolutionSource struct {
	Source
	observeErr    error
	beforeObserve func()
}

func (s plainResolutionSource) ObserveFile(ctx context.Context, token string) (ObservedFile, error) {
	if s.beforeObserve != nil {
		s.beforeObserve()
	}
	if s.observeErr != nil {
		return ObservedFile{}, s.observeErr
	}
	return s.Source.ObserveFile(ctx, token)
}

func TestPlainMarkdownRefreshKeepsDraftAndNewGeneration(t *testing.T) {
	s, e, cloud, entry, c, _ := ordinaryMarkdownConflict(t)
	entry = stageContent(t, s, entry, "local newer\n")
	cloud.file = []byte("remote newer\n")
	e.source = plainResolutionSource{Source: e.source, beforeObserve: func() {
		if err := s.SaveDraft(bg, c.ID, strings.NewReader("latest draft during fetch")); err != nil {
			t.Fatal(err)
		}
	}}
	fresh, err := e.RefreshConflict(bg, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Generation != entry.Generation || blobText(t, s, fresh.Local) != "local newer\n" || blobText(t, s, fresh.Remote) != "remote newer\n" || blobText(t, s, fresh.Draft) != "latest draft during fetch" || fresh.RemoteRevision == c.RemoteRevision {
		t.Fatal(fresh)
	}
}

func TestPlainMarkdownResolveReadFailureAndStaleGenerationPreserveDraft(t *testing.T) {
	for _, stale := range []bool{false, true} {
		t.Run(map[bool]string{false: "cloud unavailable", true: "generation changed"}[stale], func(t *testing.T) {
			s, e, cloud, entry, c, _ := ordinaryMarkdownConflict(t)
			want := errors.New("remote missing or inaccessible")
			if stale {
				entry = stageContent(t, s, entry, "newer edit\n")
				want = model.ErrGenerationChanged
			} else {
				e.source = plainResolutionSource{Source: e.source, observeErr: want}
			}
			_, _, err := e.Resolve(bg, model.ResolveRequest{ConflictID: c.ID, ExpectedGeneration: c.Generation, ExpectedRemoteRevision: c.RemoteRevision, Content: []byte("draft retained")})
			if !errors.Is(err, want) {
				t.Fatal(err)
			}
			list, _ := s.Conflicts(bg)
			if len(list) != 1 || blobText(t, s, list[0].Draft) != "draft retained" || cloud.writes != 0 {
				t.Fatal(list, cloud.writes)
			}
			got, _ := s.Entry(bg, entry.ID)
			if got.Local != entry.Local {
				t.Fatal(got)
			}
		})
	}
}

func TestPlainMarkdownResolveRejectsNonTextInputs(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content []byte
		remote  []byte
	}{
		{"invalid utf8", []byte{0xff}, nil},
		{"nul", []byte("text\x00binary"), nil},
		{"over limit", bytes.Repeat([]byte("x"), int(store.LargeSnapshotBytes)+1), nil},
		{"remote becomes binary", []byte("valid human text"), []byte("cloud\x00binary")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, e, cloud, entry, c, _ := ordinaryMarkdownConflict(t)
			if tc.remote != nil {
				cloud.file = tc.remote
				sn, err := e.observeFile(bg, entry.Token)
				if err != nil {
					t.Fatal(err)
				}
				c.Remote = sn.FileData
				c.RemoteRevision = "sha256:" + string(sn.FileData)
				if err = s.SaveConflict(bg, c); err != nil {
					t.Fatal(err)
				}
			}
			_, _, err := e.Resolve(bg, model.ResolveRequest{ConflictID: c.ID, ExpectedGeneration: c.Generation, ExpectedRemoteRevision: c.RemoteRevision, Content: tc.content})
			if !errors.Is(err, model.ErrUnsupportedContent) {
				t.Fatal(err)
			}
			list, _ := s.Conflicts(bg)
			if len(list) != 1 || cloud.writes != 0 {
				t.Fatal(list, cloud.writes)
			}
		})
	}
}

func TestPlainMarkdownResolveRejectsOtherFilesAndSaveDuringRecheck(t *testing.T) {
	for _, mode := range []string{"non markdown", "new save during fetch"} {
		t.Run(mode, func(t *testing.T) {
			s, e, cloud, entry, c, _ := ordinaryMarkdownConflict(t)
			want := model.ErrGenerationChanged
			if mode == "non markdown" {
				if err := s.RecordCloudName(bg, entry.ID, entry.Token, "note.txt"); err != nil {
					t.Fatal(err)
				}
				want = model.ErrUnsupportedContent
			} else {
				e.source = plainResolutionSource{Source: e.source, beforeObserve: func() { entry = stageContent(t, s, entry, "new save during remote check\n") }}
			}
			_, _, err := e.Resolve(bg, model.ResolveRequest{ConflictID: c.ID, ExpectedGeneration: c.Generation, ExpectedRemoteRevision: c.RemoteRevision, Content: []byte("human draft stays")})
			if !errors.Is(err, want) {
				t.Fatal(err)
			}
			list, _ := s.Conflicts(bg)
			if len(list) != 1 || blobText(t, s, list[0].Draft) != "human draft stays" || cloud.writes != 0 {
				t.Fatal(list, cloud.writes)
			}
			got, _ := s.Entry(bg, entry.ID)
			if got.Local != entry.Local {
				t.Fatal(got)
			}
		})
	}
}
