package overlay

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/object"
	"github.com/rclone/rclone/internal/bridge/cloud"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
	"github.com/rclone/rclone/lib/larkcli"
)

var ctx = context.Background()

type fakeSource struct {
	mu                               sync.Mutex
	items                            []larkcli.DriveItem
	bodies                           map[string]string
	docs                             map[string]string
	listErr, bodyErr                 error
	lists, observes, fetches, closed int
}

func (s *fakeSource) List(context.Context, string) ([]larkcli.DriveItem, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lists++
	return append([]larkcli.DriveItem(nil), s.items...), s.listErr
}
func (s *fakeSource) FetchDoc(_ context.Context, token, revision string) (cloud.Document, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fetches++
	return cloud.Document{Token: token, Revision: "revision", XML: []byte(s.docs[token]), SourceURL: "https://provided-source.example/doc"}, s.bodyErr
}
func (s *fakeSource) ObserveFile(_ context.Context, token string) (FileObservation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.observes++
	if s.bodyErr != nil {
		return FileObservation{}, s.bodyErr
	}
	body := s.bodies[token]
	h := sha256.Sum256([]byte(body))
	now := time.Now().UTC()
	return FileObservation{Token: token, Size: int64(len(body)), Identity: model.FileIdentity{Kind: model.FileIdentityObservation, ObservationID: uuid.NewString(), SHA256: hex.EncodeToString(h[:]), StartedAt: now, CompletedAt: now}, Open: func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(body)), nil }, Close: func() error { s.mu.Lock(); defer s.mu.Unlock(); s.closed++; return nil }}, nil
}
func fixture(t *testing.T) (*Fs, *store.Store, *fakeSource) {
	t.Helper()
	d, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	st, e := store.Open(d)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { st.Close() })
	sc := store.Scope{MountID: model.MountID(uuid.NewString()), ProfileID: model.ProfileID(uuid.NewString()), RemoteID: model.RemoteID(uuid.NewString()), IdentityHash: "identity", FolderToken: "root"}
	if e = st.BindScope(ctx, sc); e != nil {
		t.Fatal(e)
	}
	source := &fakeSource{bodies: map[string]string{}, docs: map[string]string{}}
	registered := len(fs.Registry)
	raw, e := New(Dependencies{Store: st, Source: source, ProfileID: sc.ProfileID, MountID: sc.MountID})
	if e != nil {
		t.Fatal(e)
	}
	if len(fs.Registry) != registered {
		t.Fatal("overlay registered backend")
	}
	t.Cleanup(func() { _ = raw.(*Fs).Shutdown(ctx) })
	return raw.(*Fs), st, source
}
func put(t *testing.T, f *Fs, name, body string) fs.Object {
	t.Helper()
	o, e := f.Put(ctx, strings.NewReader(body), object.NewStaticObjectInfo(name, time.Now(), int64(len(body)), true, nil, f))
	if e != nil {
		t.Fatal(e)
	}
	return o
}
func body(t *testing.T, o fs.Object, opts ...fs.OpenOption) string {
	t.Helper()
	r, e := o.Open(ctx, opts...)
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	b, e := io.ReadAll(r)
	if e != nil {
		t.Fatal(e)
	}
	return string(b)
}
func listed(t *testing.T, f *Fs) map[string]fs.DirEntry {
	t.Helper()
	es, e := f.List(ctx, "")
	if e != nil {
		t.Fatal(e)
	}
	m := map[string]fs.DirEntry{}
	for _, x := range es {
		m[x.Remote()] = x
	}
	return m
}
func TestUpdateOnlyDurablyQueues(t *testing.T) {
	f, st, source := fixture(t)
	o := put(t, f, "note.md", "hello\n")
	if source.observes != 0 || source.fetches != 0 || source.lists != 0 {
		t.Fatal("write used network")
	}
	jobs, e := st.Pending(ctx)
	if e != nil || len(jobs) != 1 || jobs[0].State != model.StatusQueued {
		t.Fatal(jobs, e)
	}
	entry, e := st.EntryByRemote(ctx, "note.md")
	if e != nil || entry.Status == model.StatusSynced || o.Size() != 6 || body(t, o) != "hello\n" {
		t.Fatal(entry, e, o.Size())
	}
	src := object.NewStaticObjectInfo(o.Remote(), o.ModTime(ctx), 6, true, nil, f)
	if e = o.Update(ctx, strings.NewReader("hello\n"), src); e != nil {
		t.Fatal(e)
	}
	again, _ := st.EntryByRemote(ctx, "note.md")
	if again.Generation != entry.Generation {
		t.Fatal("duplicate flush created generation")
	}
}
func TestNewMDKindAndExistingRawMD(t *testing.T) {
	f, st, source := fixture(t)
	source.items = []larkcli.DriveItem{{Token: "raw", Name: "existing.md", Type: "file"}}
	listed(t, f)
	put(t, f, "existing.md", "raw edits")
	put(t, f, "new.md", "new doc\n")
	raw, _ := st.EntryByRemote(ctx, "existing.md")
	doc, _ := st.EntryByRemote(ctx, "new.md")
	if raw.Kind != model.KindFile || raw.Token != "raw" || doc.Kind != model.KindDocx || doc.Token != "" {
		t.Fatal(raw, doc)
	}
}
func TestNamesOnlyAndMaterializeImmutableFile(t *testing.T) {
	f, st, s := fixture(t)
	s.items = []larkcli.DriveItem{{Token: "file", Name: "data", Type: "file"}, {Token: "doc", Name: "设计", Type: "docx"}}
	s.bodies["file"] = "abcdef"
	s.docs["doc"] = `<p id="p">中文</p>`
	names := listed(t, f)
	if len(names) != 2 || s.observes != 0 || s.fetches != 0 || names["data"].Size() != -1 || names["设计.md"].Size() != -1 {
		t.Fatal(names, s)
	}
	o, e := f.MaterializeManaged(ctx, "data")
	if e != nil || o.Size() != 6 {
		t.Fatal(o, e)
	}
	s.bodies["file"] = "changed remote"
	if got := body(t, o, &fs.RangeOption{Start: 1, End: 3}); got != "bcd" {
		t.Fatal(got)
	}
	if got := body(t, o, &fs.SeekOption{Offset: 4}); got != "ef" {
		t.Fatal(got)
	}
	if s.observes != 1 || s.closed != 1 {
		t.Fatal(s.observes, s.closed)
	}
	e1, _ := st.EntryByRemote(ctx, "data")
	sn, e := st.Snapshot(ctx, e1.RemoteCheckpoint)
	if e != nil || sn.FileData != e1.Visible || sn.FileIdentity == nil || sn.Revision != "" {
		t.Fatal(sn, e)
	}
	doc, e := f.MaterializeManaged(ctx, "设计.md")
	if e != nil || doc.Size() != int64(len("中文\n")) || body(t, doc) != "中文\n" {
		t.Fatal(doc, e)
	}
	if s.fetches != 1 {
		t.Fatal(s.fetches)
	}
	f.MaterializeManaged(ctx, "设计.md")
	if s.fetches != 1 {
		t.Fatal("materialized object refreshed outside publisher")
	}
}
func TestAtomicSaveKeepsDocumentToken(t *testing.T) {
	f, st, s := fixture(t)
	s.items = []larkcli.DriveItem{{Token: "doc-token", Name: "设计", Type: "docx"}}
	s.docs["doc-token"] = `<p id="p">old</p>`
	listed(t, f)
	if _, e := f.MaterializeManaged(ctx, "设计.md"); e != nil {
		t.Fatal(e)
	}
	before, _ := st.EntryByRemote(ctx, "设计.md")
	put(t, f, "editor.tmp", "new\n")
	id, e := f.BeginManagedRename(ctx, "editor.tmp", "设计.md")
	if e != nil {
		t.Fatal(e)
	}
	temp, e := f.NewObject(ctx, "editor.tmp")
	if e != nil {
		t.Fatal(e)
	}
	if e = temp.Update(ctx, strings.NewReader("flushed\n"), object.NewStaticObjectInfo("editor.tmp", time.Now(), 8, true, nil, f)); e != nil {
		t.Fatal(e)
	}
	moved, e := f.RenameManaged(ctx, id)
	if e != nil {
		t.Fatal(e)
	}
	after, _ := st.EntryByRemote(ctx, "设计.md")
	if after.ID != before.ID || after.Token != before.Token || after.Kind != model.KindDocx || body(t, moved) != "flushed\n" || after.FrozenBy == "" {
		t.Fatal(after)
	}
	if e = f.FinishManagedRename(ctx, id); e != nil {
		t.Fatal(e)
	}
	if _, e = f.NewObject(ctx, "editor.tmp"); !errors.Is(e, fs.ErrorObjectNotFound) {
		t.Fatal(e)
	}
}
func TestRenameMovesPendingGeneration(t *testing.T) {
	f, st, _ := fixture(t)
	put(t, f, "new.tmp", "pending\n")
	id, e := f.BeginManagedRename(ctx, "new.tmp", "final.md")
	if e != nil {
		t.Fatal(e)
	}
	moved, e := f.RenameManaged(ctx, id)
	if e != nil {
		t.Fatal(e)
	}
	if e = f.FinishManagedRename(ctx, id); e != nil {
		t.Fatal(e)
	}
	got, _ := st.EntryByRemote(ctx, "final.md")
	if got.Kind != model.KindDocx || body(t, moved) != "pending\n" {
		t.Fatal(got)
	}
	jobs, _ := st.Pending(ctx)
	if len(jobs) != 1 || jobs[0].EntryID != got.ID {
		t.Fatal(jobs)
	}
}
func TestCaseFoldAndReservedNameMapping(t *testing.T) {
	f, st, s := fixture(t)
	s.items = []larkcli.DriveItem{{Token: "a", Name: "A", Type: "file"}, {Token: "b", Name: "a", Type: "file"}, {Token: "reserved", Name: "CON.txt", Type: "file"}, {Token: "percent", Name: "100%/x. ", Type: "file"}, {Token: "nfc1", Name: "é", Type: "file"}, {Token: "nfc2", Name: "e\u0301", Type: "file"}}
	first := listed(t, f)
	if len(first) != 6 {
		t.Fatal(first)
	}
	folds := map[string]bool{}
	for name := range first {
		k := foldName(name)
		if folds[k] {
			t.Fatalf("fold collision %q", name)
		}
		folds[k] = true
		if strings.Contains(name, "/") {
			t.Fatal(name)
		}
	}
	if _, ok := first["%43ON.txt"]; !ok {
		t.Fatal(first)
	}
	if _, ok := first["100%25%2Fx%2E%20"]; !ok {
		t.Fatal(first)
	}
	original, e := st.EntryByToken(ctx, "b")
	if e != nil {
		t.Fatal(e)
	}
	s.items = []larkcli.DriveItem{{Token: "b", Name: "a", Type: "file"}}
	if err := f.RefreshDirectory(ctx, ""); err != nil {
		t.Fatal(err)
	}
	second := listed(t, f)
	if _, ok := second[original.Remote]; !ok {
		t.Fatal("mapping changed after collision disappears")
	}
}
func TestLocalDeleteSupersedesQueuedEdit(t *testing.T) {
	f, st, s := fixture(t)
	s.items = []larkcli.DriveItem{{Token: "t", Name: "file", Type: "file"}}
	s.bodies["t"] = "original"
	listed(t, f)
	put(t, f, "file", "edited")
	o, e := f.NewObject(ctx, "file")
	if e != nil {
		t.Fatal(e)
	}
	if e = o.Remove(ctx); e != nil {
		t.Fatal(e)
	}
	if _, e = st.EntryByRemote(ctx, "file"); !errors.Is(e, store.ErrNotFound) {
		t.Fatal(e)
	}
	cs, e := st.Conflicts(ctx)
	if e != nil || len(cs) != 0 {
		t.Fatal(cs, e)
	}
}
func TestRootRemoveDenied(t *testing.T) {
	f, _, _ := fixture(t)
	if e := f.Rmdir(ctx, ""); !errors.Is(e, fs.ErrorPermissionDenied) {
		t.Fatal(e)
	}
}
func TestDeleteTombstoneNotResurrectedByList(t *testing.T) {
	f, st, s := fixture(t)
	s.items = []larkcli.DriveItem{{Token: "t", Name: "file", Type: "file"}}
	s.bodies["t"] = "old"
	listed(t, f)
	o, e := f.MaterializeManaged(ctx, "file")
	if e != nil {
		t.Fatal(e)
	}
	if e = o.Remove(ctx); e != nil {
		t.Fatal(e)
	}
	names := listed(t, f)
	if _, ok := names["file"]; ok {
		t.Fatal("stale cloud list resurrected tombstone")
	}
	old, e := st.EntryByToken(ctx, "t")
	if e != nil {
		t.Fatal(e)
	}
	m, e := st.Metadata(ctx, old.ID)
	if e != nil || !m.Tombstone {
		t.Fatal(m, e)
	}
}
func TestTempFilterAndLiteralPercentCreation(t *testing.T) {
	f, st, _ := fixture(t)
	for _, name := range []string{".DS_Store", "Thumbs.db", "x.swp", "x.swo", ".~lock"} {
		o := put(t, f, name, "local")
		e, _ := st.EntryByRemote(ctx, o.Remote())
		ok, err := f.ShouldSync(ctx, e.ID)
		if err != nil || ok {
			t.Fatal(name, ok, err)
		}
	}
	put(t, f, "keep.tmp", "bytes")
	if f.LocalOnly("keep.tmp") {
		t.Fatal("blanket tmp exclusion")
	}
	put(t, f, "literal%2F.md", "body\n")
	e, err := st.EntryByRemote(ctx, "literal%2F.md")
	if err != nil || e.CloudName != "literal%2F.md" {
		t.Fatal(e, err)
	}
}
func TestListFailureKeepsAcceptedViewAndNoBodies(t *testing.T) {
	f, st, s := fixture(t)
	s.items = []larkcli.DriveItem{{Token: "t", Name: "old", Type: "file"}}
	listed(t, f)
	s.items = []larkcli.DriveItem{{Token: "other", Name: "partial", Type: "file"}}
	s.listErr = errors.New("later page failed")
	if _, e := f.List(ctx, ""); e != nil {
		t.Fatal("warm metadata disappeared offline", e)
	}
	if e := f.RefreshDirectory(ctx, ""); e == nil {
		t.Fatal("partial refresh succeeded")
	}
	if _, e := st.EntryByRemote(ctx, "old"); e != nil {
		t.Fatal(e)
	}
	if _, e := st.EntryByRemote(ctx, "partial"); !errors.Is(e, store.ErrNotFound) {
		t.Fatal(e)
	}
	if s.observes != 0 || s.fetches != 0 {
		t.Fatal("eager bodies")
	}
}
func TestBeginBeforeFlushWithoutSourceObject(t *testing.T) {
	f, st, _ := fixture(t)
	id, e := f.BeginManagedRename(ctx, "cache-only.tmp", "final.md")
	if e != nil {
		t.Fatal(e)
	}
	src, e := st.EntryByRemote(ctx, "cache-only.tmp")
	if e != nil || src.FrozenBy != id {
		t.Fatal(src, e)
	}
	put(t, f, "cache-only.tmp", "cache bytes\n")
	o, e := f.RenameManaged(ctx, id)
	if e != nil {
		t.Fatal(e)
	}
	if body(t, o) != "cache bytes\n" {
		t.Fatal("missing Flush bytes")
	}
	if e = f.FinishManagedRename(ctx, id); e != nil {
		t.Fatal(e)
	}
}
func TestCommittedReplayReconcilesBeforeFinish(t *testing.T) {
	f, st, _ := fixture(t)
	put(t, f, "src.tmp", "accepted\n")
	id, e := f.BeginManagedRename(ctx, "src.tmp", "dest.md")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = f.RenameManaged(ctx, id); e != nil {
		t.Fatal(e)
	}
	failed := errors.New("cache reconcile failed")
	if e = f.ReplayManagedNamespace(ctx, func(src, dst string) error { return failed }); !errors.Is(e, failed) {
		t.Fatal(e)
	}
	var committed interface{ Committed() bool }
	if !errors.As(e, &committed) || !committed.Committed() {
		t.Fatal("commit boundary lost", e)
	}
	pending, _ := st.PendingNamespace(ctx)
	if len(pending) != 1 || pending[0].State != "committed" {
		t.Fatal(pending)
	}
	called := false
	if e = f.ReplayManagedNamespace(ctx, func(src, dst string) error {
		called = true
		if src != "src.tmp" || dst != "dest.md" {
			t.Fatal(src, dst)
		}
		got, _ := st.EntryByRemote(ctx, dst)
		if got.FrozenBy != id {
			t.Fatal("finished before cache reconcile")
		}
		return nil
	}); e != nil || !called {
		t.Fatal(e)
	}
	pending, _ = st.PendingNamespace(ctx)
	if len(pending) != 0 {
		t.Fatal(pending)
	}
}
func TestNewVerifiesBoundScope(t *testing.T) {
	_, st, s := fixture(t)
	sc, _ := st.Scope(ctx)
	if _, e := New(Dependencies{Store: st, Source: s, ProfileID: sc.ProfileID, MountID: model.MountID(uuid.NewString())}); !errors.Is(e, model.ErrIdentityChanged) {
		t.Fatal(e)
	}
}

func TestMaterializationFailureNeverPublishesEmptyOrFetchesEveryName(t *testing.T) {
	f, st, s := fixture(t)
	s.items = []larkcli.DriveItem{{Token: "t", Name: "target", Type: "file"}, {Token: "other", Name: "untouched", Type: "file"}}
	s.bodyErr = errors.New("download incomplete")
	listed(t, f)
	if _, e := f.MaterializeManaged(ctx, "target"); e == nil {
		t.Fatal("failed download adopted")
	}
	entry, e := st.EntryByRemote(ctx, "target")
	if e != nil || entry.Visible != "" || entry.RemoteCheckpoint != "" {
		t.Fatal(entry, e)
	}
	o, e := f.NewObject(ctx, "target")
	if e != nil || o.Size() != -1 {
		t.Fatal(o, e)
	}
	if s.observes != 1 {
		t.Fatal(s.observes)
	}
	s.bodyErr = nil
	s.bodies["t"] = ""
	o, e = f.MaterializeManaged(ctx, "target")
	if e != nil || o.Size() != 0 || body(t, o) != "" {
		t.Fatal(o, e)
	}
	if s.observes != 2 {
		t.Fatal(s.observes)
	}
}
func TestPublishIsLocalAndOldObjectKeepsSnapshot(t *testing.T) {
	f, st, s := fixture(t)
	s.items = []larkcli.DriveItem{{Token: "t", Name: "file", Type: "file"}}
	s.bodies["t"] = "old bytes"
	listed(t, f)
	old, e := f.MaterializeManaged(ctx, "file")
	if e != nil {
		t.Fatal(e)
	}
	entry, e := st.EntryByRemote(ctx, "file")
	if e != nil {
		t.Fatal(e)
	}
	now := time.Now().UTC()
	h := sha256.Sum256([]byte("new"))
	sn, e := st.SaveFileSnapshot(ctx, entry.Token, model.FileIdentity{Kind: model.FileIdentityObservation, ObservationID: uuid.NewString(), SHA256: hex.EncodeToString(h[:]), StartedAt: now, CompletedAt: now}, strings.NewReader("new"))
	if e != nil {
		t.Fatal(e)
	}
	candidate, e := st.OfferRemote(ctx, entry.ID, entry.Generation, sn, sn.FileData)
	if e != nil {
		t.Fatal(e)
	}
	beforeLists, beforeBodies := s.lists, s.observes
	fresh, e := f.Publish(ctx, candidate)
	if e != nil || fresh.Size() != 3 || body(t, fresh) != "new" {
		t.Fatal(fresh, e)
	}
	if body(t, old, &fs.RangeOption{Start: -1, End: 5}) != "bytes" || old.Size() != 9 {
		t.Fatal("old object mixed snapshot")
	}
	if beforeLists != s.lists || beforeBodies != s.observes {
		t.Fatal("Publish contacted cloud")
	}
}
func TestMkdirRmdirAndBoundKindRules(t *testing.T) {
	f, st, s := fixture(t)
	if e := f.Mkdir(ctx, "local"); e != nil {
		t.Fatal(e)
	}
	put(t, f, "local/child", "bytes")
	if e := f.Rmdir(ctx, "local"); !errors.Is(e, fs.ErrorDirectoryNotEmpty) {
		t.Fatal(e)
	}
	s.items = []larkcli.DriveItem{{Token: "raw-token", Name: "raw", Type: "file"}, {Token: "doc-token", Name: "native", Type: "docx"}}
	s.bodies["raw-token"] = "raw bytes"
	s.docs["doc-token"] = `<p id="p">doc</p>`
	if err := f.RefreshDirectory(ctx, ""); err != nil {
		t.Fatal(err)
	}
	listed(t, f)
	raw, e := f.MaterializeManaged(ctx, "raw")
	if e != nil {
		t.Fatal(e)
	}
	moved, e := f.Move(ctx, raw, "raw.md")
	if e != nil {
		t.Fatal(e)
	}
	entry, e := st.EntryByRemote(ctx, moved.Remote())
	if e != nil || entry.Kind != model.KindFile || entry.Token != "raw-token" {
		t.Fatal(entry, e)
	}
	op, e := f.BeginManagedRename(ctx, "native.md", "native.txt")
	if e != nil {
		t.Fatal("local rename must not depend on cloud document extension", e)
	}
	if e = f.AbortManagedRename(ctx, op); e != nil {
		t.Fatal(e)
	}
}
func TestUnsafePathsAndRangeOptions(t *testing.T) {
	f, _, _ := fixture(t)
	for _, p := range []string{"../x", "/x", "x//y", "x/../y", "a\\b", "."} {
		if _, e := f.Put(ctx, strings.NewReader("x"), object.NewStaticObjectInfo(p, time.Now(), 1, true, nil, f)); e == nil {
			t.Fatalf("accepted %q", p)
		}
	}
	o := put(t, f, "ranges", "abcdef")
	if body(t, o, &fs.RangeOption{Start: 4, End: 999}) != "ef" || body(t, o, &fs.RangeOption{Start: 99, End: 999}) != "" {
		t.Fatal("range cap")
	}
	if r, e := o.Open(ctx, &fs.SeekOption{Offset: -1}); e == nil {
		r.Close()
		t.Fatal("negative seek")
	}
	if r, e := o.Open(ctx, &fs.RangeOption{Start: 4, End: 1}); e == nil {
		r.Close()
		t.Fatal("backward range")
	}
}
func TestPreparedReplayAbortsAndRetainsFlush(t *testing.T) {
	f, st, _ := fixture(t)
	put(t, f, "src.tmp", "initial")
	id, e := f.BeginManagedRename(ctx, "src.tmp", "new.md")
	if e != nil {
		t.Fatal(e)
	}
	put(t, f, "src.tmp", "flushed")
	if e = f.ReplayManagedNamespace(ctx, func(string, string) error { t.Fatal("reconciled uncommitted namespace"); return nil }); e != nil {
		t.Fatal(e)
	}
	entry, e := st.EntryByRemote(ctx, "src.tmp")
	if e != nil || entry.FrozenBy != "" {
		t.Fatal(entry, e)
	}
	o, e := f.NewObject(ctx, "src.tmp")
	if e != nil || body(t, o) != "flushed" {
		t.Fatal(o, e)
	}
	if e = f.AbortManagedRename(ctx, id); e != nil {
		t.Fatal(e)
	}
}

func TestActivityAndDirectoryRefreshRemainLazy(t *testing.T) {
	f, st, s := fixture(t)
	s.items = []larkcli.DriveItem{{Token: "t", Name: "file", Type: "file"}}
	s.bodies["t"] = "bytes"
	listed(t, f)
	if ids := f.ActiveEntries(); len(ids) != 0 {
		t.Fatal("names list activated bodies", ids)
	}
	o, e := f.MaterializeManaged(ctx, "file")
	if e != nil {
		t.Fatal(e)
	}
	o.Size()
	if ids := f.ActiveEntries(); len(ids) != 0 {
		t.Fatal("stat activated body retention", ids)
	}
	if got := body(t, o); got != "bytes" {
		t.Fatal(got)
	}
	entry, _ := st.EntryByRemote(ctx, "file")
	ids := f.ActiveEntries()
	if len(ids) != 1 || ids[0] != entry.ID {
		t.Fatal(ids)
	}
	beforeLists, beforeBodies := s.lists, s.observes
	if e = f.RefreshDirectories(ctx); e != nil {
		t.Fatal(e)
	}
	if s.lists != beforeLists+1 || s.observes != beforeBodies {
		t.Fatal("refresh eagerly acquired bodies")
	}
}

type beforeObservation struct {
	*fakeSource
	before func()
}

func (s beforeObservation) ObserveFile(ctx context.Context, token string) (FileObservation, error) {
	s.before()
	return s.fakeSource.ObserveFile(ctx, token)
}
func TestEditDuringDeleteMaterializationBecomesConflict(t *testing.T) {
	f, st, s := fixture(t)
	s.items = []larkcli.DriveItem{{Token: "t", Name: "file", Type: "file"}}
	s.bodies["t"] = "old"
	listed(t, f)
	f.source = beforeObservation{fakeSource: s, before: func() { put(t, f, "file", "new local edit") }}
	o, e := f.NewObject(ctx, "file")
	if e != nil {
		t.Fatal(e)
	}
	if e = o.Remove(ctx); !errors.Is(e, ErrDeleteConflict) {
		t.Fatalf("concurrent edit was deleted: %v", e)
	}
	entry, e := st.EntryByRemote(ctx, "file")
	if e != nil || entry.Status != model.StatusConflict {
		t.Fatal(entry, e)
	}
}

type gatedReader struct {
	start  chan struct{}
	resume chan struct{}
	once   sync.Once
	r      io.Reader
}

func (r *gatedReader) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.start); <-r.resume })
	return r.r.Read(p)
}
func TestSlowStaleUpdateCannotOverwriteNewAcceptedGeneration(t *testing.T) {
	f, st, _ := fixture(t)
	put(t, f, "file", "initial")
	slow, e := f.NewObject(ctx, "file")
	if e != nil {
		t.Fatal(e)
	}
	fast, e := f.NewObject(ctx, "file")
	if e != nil {
		t.Fatal(e)
	}
	r := &gatedReader{start: make(chan struct{}), resume: make(chan struct{}), r: strings.NewReader("stale")}
	done := make(chan error, 1)
	go func() { done <- slow.Update(ctx, r, object.NewStaticObjectInfo("file", time.Now(), 5, true, nil, f)) }()
	<-r.start
	if e = fast.Update(ctx, strings.NewReader("newer"), object.NewStaticObjectInfo("file", time.Now(), 5, true, nil, f)); e != nil {
		t.Fatal(e)
	}
	close(r.resume)
	if e = <-done; !errors.Is(e, model.ErrGenerationChanged) {
		t.Fatalf("stale update accepted: %v", e)
	}
	entry, _ := st.EntryByRemote(ctx, "file")
	if entry.Generation != 2 || body(t, fast) != "newer" {
		t.Fatal(entry)
	}
}
func TestDeleteUnknownPreservesReconciliationJournal(t *testing.T) {
	f, st, _ := fixture(t)
	o := put(t, f, "file", "unknown")
	jobs, e := st.Pending(ctx)
	if e != nil || len(jobs) != 1 {
		t.Fatal(jobs, e)
	}
	if _, _, e = st.Prepare(ctx, jobs[0].ID); e != nil {
		t.Fatal(e)
	}
	if e = st.MarkUnknown(ctx, jobs[0].ID); e != nil {
		t.Fatal(e)
	}
	if e = o.Remove(ctx); !errors.Is(e, ErrDeleteConflict) || !errors.Is(e, store.ErrBusy) {
		t.Fatal(e)
	}
	j, e := st.Job(ctx, jobs[0].ID)
	if e != nil || j.State != model.StatusAwaitingVerification {
		t.Fatal(j, e)
	}
}
