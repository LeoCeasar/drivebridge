package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/model"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func copyConflict(t *testing.T, s *Store) model.Conflict {
	t.Helper()
	e, err := s.EnsureEntry(ctx, "notes.md", model.KindDocx)
	if err != nil {
		t.Fatal(err)
	}
	e, _, err = s.Stage(ctx, e.ID, nil, strings.NewReader("本地😀\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	base, err := s.PutBlob(ctx, strings.NewReader("原版\n"))
	if err != nil {
		t.Fatal(err)
	}
	remote, err := s.PutBlob(ctx, strings.NewReader("云端\n"))
	if err != nil {
		t.Fatal(err)
	}
	c := model.Conflict{ID: model.ConflictID(uuid.NewString()), EntryID: e.ID, Generation: e.Generation, Base: base, Local: e.Local, Remote: remote, RemoteRevision: "observed-revision", Reason: "fixture"}
	if err = s.SaveConflict(ctx, c); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveDraft(ctx, c.ID, strings.NewReader("草稿\n")); err != nil {
		t.Fatal(err)
	}
	return c
}
func TestExportConflictCopiesExactBytesAndReopen(t *testing.T) {
	s, d, sc := openTest(t)
	c := copyConflict(t, s)
	copies, err := s.ExportConflictCopies(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	for p, want := range map[string]string{copies.Base: "原版\n", copies.Local: "本地😀\r\n", copies.Remote: "云端\n", copies.Draft: "草稿\n"} {
		got, err := os.ReadFile(p)
		if err != nil || string(got) != want {
			t.Fatal(p, string(got), err)
		}
		info, err := os.Stat(p)
		if err != nil || runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
			t.Fatal(info, err)
		}
	}
	raw, err := os.ReadFile(copies.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("http")) {
		t.Fatal("unexpected URL")
	}
	var m ConflictCopyManifest
	if err = json.Unmarshal(raw, &m); err != nil || len(m.Files) != 4 {
		t.Fatal(m, err)
	}
	s.Close()
	s, err = Open(d)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.BindScope(ctx, sc); err != nil {
		t.Fatal(err)
	}
	again, err := s.ExportConflictCopies(ctx, c.ID)
	if err != nil || again != copies {
		t.Fatal(again, copies, err)
	}
}
func TestConflictCopyNeverOverwritesUserEdits(t *testing.T) {
	s, _, _ := openTest(t)
	c := copyConflict(t, s)
	copies, err := s.ExportConflictCopies(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(copies.Local, []byte("user preserved edit"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ExportConflictCopies(ctx, c.ID); !errors.Is(err, ErrConflictCopyChanged) {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(copies.Local)
	if string(got) != "user preserved edit" {
		t.Fatal(string(got))
	}
	if read(t, s, c.Local) != "本地😀\r\n" {
		t.Fatal("source lost")
	}
}
func TestConflictCopySymlinkAndTraversalRefused(t *testing.T) {
	s, d, _ := openTest(t)
	if _, err := s.ExportConflictCopies(ctx, "../escape"); err == nil {
		t.Fatal("traversal ID accepted")
	}
	c := copyConflict(t, s)
	copies, err := s.ExportConflictCopies(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(d, "outside")
	if err = os.WriteFile(outside, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = os.Remove(copies.Remote); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(outside, copies.Remote); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ExportConflictCopies(ctx, c.ID); err == nil {
		t.Fatal("symlink accepted")
	}
	got, _ := os.ReadFile(outside)
	if string(got) != "outside" {
		t.Fatal(string(got))
	}
}
func TestConflictCopyFailureRetainsQueueAndBlobs(t *testing.T) {
	for _, point := range []string{"fsync", "rename", "dirsync"} {
		t.Run(point, func(t *testing.T) {
			s, _, _ := openTest(t)
			c := copyConflict(t, s)
			s.io = failingIO{point}
			if _, err := s.ExportConflictCopies(ctx, c.ID); err == nil {
				t.Fatal("injected copy failure accepted")
			}
			if read(t, s, c.Local) != "本地😀\r\n" {
				t.Fatal("failed export damaged source")
			}
			jobs, err := s.Pending(ctx)
			if err != nil || len(jobs) != 1 {
				t.Fatal(jobs, err)
			}
			s.io = nativeIO{}
			if _, err = s.ExportConflictCopies(ctx, c.ID); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestDraftChangesCreateNewImmutableCopyVersion(t *testing.T) {
	s, _, _ := openTest(t)
	c := copyConflict(t, s)
	first, err := s.ExportConflictCopies(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.SaveDraft(ctx, c.ID, strings.NewReader("新草稿")); err != nil {
		t.Fatal(err)
	}
	second, err := s.ExportConflictCopies(ctx, c.ID)
	if err != nil || second.Directory == first.Directory {
		t.Fatal(second, err)
	}
	old, _ := os.ReadFile(first.Draft)
	newDraft, _ := os.ReadFile(second.Draft)
	if string(old) != "草稿\n" || string(newDraft) != "新草稿" {
		t.Fatal(string(old), string(newDraft))
	}
}
func TestUnobservedCopiesStayUnavailableAndExecutableExtensionIsInert(t *testing.T) {
	s, _, _ := openTest(t)
	e, err := s.EnsureEntry(ctx, "untrusted.exe", model.KindFile)
	if err != nil {
		t.Fatal(err)
	}
	e, _, err = s.Stage(ctx, e.ID, nil, strings.NewReader(""))
	if err != nil {
		t.Fatal(err)
	}
	c := model.Conflict{ID: model.ConflictID(uuid.NewString()), EntryID: e.ID, Generation: e.Generation, Local: e.Local}
	if err = s.SaveConflict(ctx, c); err != nil {
		t.Fatal(err)
	}
	copies, err := s.ExportConflictCopies(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if copies.Base != "" || copies.Remote != "" || copies.Draft != "" || !strings.HasSuffix(copies.Local, ".bin") {
		t.Fatal(copies)
	}
	info, err := os.Stat(copies.Local)
	if err != nil || info.Size() != 0 {
		t.Fatal(info, err)
	}
	source, err := os.Stat(s.blobPath(e.Local))
	if err != nil || os.SameFile(source, info) {
		t.Fatal("copy aliases protected blob", err)
	}
}
