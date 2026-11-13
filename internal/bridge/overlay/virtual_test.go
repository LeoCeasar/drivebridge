package overlay

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/object"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
	"github.com/rclone/rclone/lib/larkcli"
)

func TestVirtualMixedListKeepsEveryResourceWithoutBodies(t *testing.T) {
	f, st, s := fixture(t)
	s.items = []larkcli.DriveItem{{Token: "sheet", Name: "sheet", Type: "sheet", URL: "https://provider.example/sheet/x"}, {Token: "doc", Name: "doc", Type: "docx"}, {Token: "binary", Name: "data.xlsx", Type: "file"}, {Token: "word-file", Name: "uploaded.docx", Type: "file"}, {Token: "unknown", Name: "future", Type: "future-kind"}}
	got := listed(t, f)
	for _, name := range []string{"sheet.url", "doc.md", "data.xlsx", "uploaded.docx", "future.unsupported.txt"} {
		if got[name] == nil {
			t.Fatalf("missing %q: %v", name, got)
		}
	}
	if s.observes+s.fetches != 0 {
		t.Fatal("metadata listing downloaded bodies")
	}
	link := got["sheet.url"].(fs.Object)
	want := "[InternetShortcut]\r\nURL=https://provider.example/sheet/x\r\n"
	if link.Size() != int64(len(want)) || body(t, link) != want || link.Storable() {
		t.Fatal("invalid readonly shortcut")
	}
	if body(t, link, &fs.RangeOption{Start: 1, End: 5}) != want[1:6] {
		t.Fatal("shortcut range")
	}
	o, err := f.MaterializeManaged(ctx, "sheet.url")
	if err != nil || body(t, o) != want {
		t.Fatal(err)
	}
	e, _ := st.EntryByRemote(ctx, "sheet.url")
	sync, err := f.ShouldSync(ctx, e.ID)
	if err != nil || sync || e.Generation != 0 || e.Visible != "" || e.Local != "" {
		t.Fatal("virtual resource became sync content", e, err)
	}
	if len(f.ActiveEntries()) != 0 || s.observes+s.fetches != 0 {
		t.Fatal("virtual resource polled/downloaded")
	}
	jobs, _ := st.Pending(ctx)
	if len(jobs) != 0 {
		t.Fatal(jobs)
	}
	if !strings.Contains(body(t, got["future.unsupported.txt"].(fs.Object)), "future-kind") {
		t.Fatal("missing unsupported explanation")
	}
	put(t, f, "data.xlsx", "true uploaded binary edit")
	put(t, f, "uploaded.docx", "uploaded document bytes")
	word, _ := st.EntryByRemote(ctx, "uploaded.docx")
	if word.Kind != model.KindFile || word.Generation != 1 {
		t.Fatal(word)
	}
	binary, _ := st.EntryByRemote(ctx, "data.xlsx")
	if binary.Kind != model.KindFile || binary.Generation != 1 {
		t.Fatal(binary)
	}
}

func TestVirtualAllMutationsDeniedIncludingAtomicTarget(t *testing.T) {
	f, st, s := fixture(t)
	s.items = []larkcli.DriveItem{{Token: "sheet", Name: "sheet", Type: "sheet", URL: "https://provider.example/sheet"}}
	listed(t, f)
	o, _ := f.NewObject(ctx, "sheet.url")
	src := object.NewStaticObjectInfo("sheet.url", time.Now(), 3, true, nil, f)
	temp := put(t, f, "save.tmp", "new")
	tests := map[string]func() error{
		"update":              func() error { return o.Update(ctx, strings.NewReader("new"), src) },
		"put":                 func() error { _, e := f.Put(ctx, strings.NewReader("new"), src); return e },
		"remove":              func() error { return o.Remove(ctx) },
		"mkdir":               func() error { return f.Mkdir(ctx, "sheet.url") },
		"unseen cache source": func() error { _, e := f.BeginManagedRename(ctx, "unseen.tmp", "sheet.url"); return e },
		"publication": func() error {
			entry, _ := st.EntryByRemote(ctx, "sheet.url")
			_, e := f.Publish(ctx, model.PublishCandidate{EntryID: entry.ID})
			return e
		},
		"mtime":         func() error { return o.SetModTime(ctx, time.Now()) },
		"rename source": func() error { _, e := f.BeginManagedRename(ctx, "sheet.url", "changed.url"); return e },
		"rename target": func() error { _, e := f.BeginManagedRename(ctx, "save.tmp", "sheet.url"); return e },
		"move source":   func() error { _, e := f.Move(ctx, o, "changed.url"); return e },
		"move target":   func() error { _, e := f.Move(ctx, temp, "sheet.url"); return e },
	}
	for name, fn := range tests {
		t.Run(name, func(t *testing.T) {
			if e := fn(); !errors.Is(e, fs.ErrorPermissionDenied) {
				t.Fatal(e)
			}
		})
	}
	ops, _ := st.PendingNamespace(ctx)
	if len(ops) != 0 {
		t.Fatal("denied operation froze namespace", ops)
	}
	if _, err := st.EntryByRemote(ctx, "unseen.tmp"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("denied replace created source", err)
	}
	e, _ := st.EntryByRemote(ctx, "sheet.url")
	if e.Generation != 0 || e.Local != "" || e.FrozenBy != "" {
		t.Fatal(e)
	}
}

func TestVirtualURLValidationAndCapturedBytes(t *testing.T) {
	for _, url := range []string{"", "http://provider.example/a", "https://user:pass@provider.example/a", "https://provider.example/a\nIconFile=evil", "https://provider.example/a%0d%0aIconFile=evil", "https://provider.example/a\\bad"} {
		t.Run(url, func(t *testing.T) {
			f, _, s := fixture(t)
			s.items = []larkcli.DriveItem{{Token: "x", Name: "item", Type: "wiki", URL: url}}
			o := listed(t, f)["item.unsupported.txt"].(fs.Object)
			b := body(t, o)
			if strings.Contains(b, "URL=") || strings.Contains(b, "user:pass") || o.Size() != int64(len(b)) {
				t.Fatal(b)
			}
		})
	}
	f, _, s := fixture(t)
	s.items = []larkcli.DriveItem{{Token: "x", Name: "item", Type: "bitable", URL: "https://provider.example/old"}}
	old := listed(t, f)["item.url"].(fs.Object)
	before := body(t, old)
	s.items[0].URL = "https://provider.example/new"
	if err := f.RefreshDirectory(ctx, ""); err != nil {
		t.Fatal(err)
	}
	fresh := listed(t, f)["item.url"].(fs.Object)
	if body(t, old) != before || body(t, fresh) == before {
		t.Fatal("virtual snapshot changed under an existing handle")
	}
	s.items[0].URL = "https://bad\nurl"
	if err := f.RefreshDirectory(ctx, ""); err != nil {
		t.Fatal(err)
	}
	listed(t, f)
	fresh, err := f.NewObject(ctx, "item.url")
	if err != nil || strings.Contains(body(t, fresh), "URL=") {
		t.Fatal("stale/unsafe URL retained", err)
	}
}

func TestVirtualFolderIsNotEmpty(t *testing.T) {
	f, st, s := fixture(t)
	if _, err := st.ObserveEntry(ctx, store.Observation{Remote: "folder", Kind: model.KindFolder, Token: "folder"}); err != nil {
		t.Fatal(err)
	}
	s.items = []larkcli.DriveItem{{Token: "future", Name: "unknown", Type: "future-kind", ParentToken: "folder"}}
	if err := f.Rmdir(ctx, "folder"); !errors.Is(err, fs.ErrorDirectoryNotEmpty) {
		t.Fatal(err)
	}
	ops, _ := st.PendingNamespace(ctx)
	if len(ops) != 0 {
		t.Fatal(ops)
	}
}

func TestVirtualMappingSurvivesReopenAndRawURLCollision(t *testing.T) {
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	sc := store.Scope{MountID: model.MountID(uuid.NewString()), ProfileID: model.ProfileID(uuid.NewString()), RemoteID: model.RemoteID(uuid.NewString()), IdentityHash: "identity", FolderToken: "root"}
	if err = st.BindScope(ctx, sc); err != nil {
		t.Fatal(err)
	}
	s := &fakeSource{items: []larkcli.DriveItem{{Token: "a-file", Name: "raw.url", Type: "file"}, {Token: "z-sheet", Name: "raw", Type: "sheet", URL: "https://provider.example/raw"}}}
	raw, err := New(Dependencies{Store: st, Source: s, ProfileID: sc.ProfileID, MountID: sc.MountID})
	if err != nil {
		t.Fatal(err)
	}
	got := listed(t, raw.(*Fs))
	if len(got) != 2 || got["raw.url"] == nil {
		t.Fatal(got)
	}
	e, err := st.EntryByToken(ctx, "z-sheet")
	if err != nil || e.Remote == "raw.url" || !strings.HasSuffix(e.Remote, ".url") {
		t.Fatal(e, err)
	}
	before := body(t, got[e.Remote].(fs.Object))
	if err = st.Close(); err != nil {
		t.Fatal(err)
	}
	st, err = store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err = st.BindScope(ctx, sc); err != nil {
		t.Fatal(err)
	}
	s.listErr = errors.New("offline")
	raw, err = New(Dependencies{Store: st, Source: s, ProfileID: sc.ProfileID, MountID: sc.MountID})
	if err != nil {
		t.Fatal(err)
	}
	o, err := raw.NewObject(ctx, e.Remote)
	if err != nil || body(t, o) != before || o.Size() != int64(len(before)) {
		t.Fatal(err)
	}
	generated, err := raw.(*Fs).MaterializeManaged(ctx, e.Remote)
	if err != nil || body(t, generated) != before {
		t.Fatal(err)
	}
	metadata, err := st.Metadata(ctx, e.ID)
	if err != nil || metadata.VirtualType != "sheet" {
		t.Fatal(metadata, err)
	}
	after, _ := st.EntryByRemote(ctx, e.Remote)
	if after.ID != e.ID || after.Token != e.Token {
		t.Fatal(after, e)
	}
	r, err := o.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.Copy(io.Discard, r)
	r.Close()
	if err != nil || s.observes+s.fetches != 0 {
		t.Fatal(err)
	}
}

func TestVirtualClassificationCannotRebindAcceptedEntry(t *testing.T) {
	f, st, s := fixture(t)
	put(t, f, "local.url", "pending save")
	_, err := st.ObserveEntry(ctx, store.Observation{Remote: "local.url", Kind: model.KindFile, Token: "remote-sheet", VirtualType: "sheet"})
	if !errors.Is(err, model.ErrIdentityChanged) {
		t.Fatal(err)
	}
	e, _ := st.EntryByRemote(ctx, "local.url")
	m, _ := st.Metadata(ctx, e.ID)
	if e.Token != "" || m.VirtualType != "" || e.Generation != 1 {
		t.Fatal(e, m)
	}
	s.items = []larkcli.DriveItem{{Token: "sheet", Name: "sheet", Type: "sheet", URL: "https://provider.example/a"}}
	if err := f.RefreshDirectory(ctx, ""); err != nil {
		t.Fatal(err)
	}
	listed(t, f)
	e, _ = st.EntryByToken(ctx, "sheet")
	m, _ = st.Metadata(ctx, e.ID)
	if m.VirtualType != "sheet" {
		t.Fatal(m)
	}
	for _, typ := range []string{"file", "bitable", "docx"} {
		s.items[0].Type = typ
		if err = f.RefreshDirectory(ctx, ""); !errors.Is(err, model.ErrIdentityChanged) {
			t.Fatalf("%s: %v", typ, err)
		}
	}
	_, err = st.ObserveEntry(ctx, store.Observation{Remote: e.Remote, Kind: model.KindFile, Token: e.Token})
	if !errors.Is(err, model.ErrIdentityChanged) {
		t.Fatal("virtual flag cleared", err)
	}
	_, err = st.ObserveEntry(ctx, store.Observation{Remote: "new", Kind: model.KindFile, VirtualType: "sheet"})
	if !errors.Is(err, model.ErrIdentityChanged) {
		t.Fatal("local creation marked virtual", err)
	}
}

func TestVirtualRenameManagedRechecksPreparedTarget(t *testing.T) {
	f, st, s := fixture(t)
	s.items = []larkcli.DriveItem{{Token: "sheet", Name: "sheet", Type: "sheet", URL: "https://provider.example/a"}}
	listed(t, f)
	put(t, f, "temp", "data")
	op, err := st.PrepareNamespace(ctx, model.NamespaceOp{Kind: "replace", Src: "temp", Dst: "sheet.url"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.RenameManaged(ctx, op.ID); !errors.Is(err, fs.ErrorPermissionDenied) {
		t.Fatal(err)
	}
	if err = f.AbortManagedRename(ctx, op.ID); err != nil {
		t.Fatal(err)
	}
}

type blockedVirtualList struct {
	*fakeSource
	entered, release chan struct{}
}

func (s blockedVirtualList) List(context.Context, string) ([]larkcli.DriveItem, error) {
	close(s.entered)
	<-s.release
	return s.items, nil
}
func TestLocalNamespaceDoesNotWaitForRemoteClassification(t *testing.T) {
	f, st, s := fixture(t)
	put(t, f, "temp", "data")
	s.items = []larkcli.DriveItem{{Token: "sheet", Name: "sheet", Type: "sheet", URL: "https://provider.example/a"}}
	block := blockedVirtualList{s, make(chan struct{}), make(chan struct{})}
	f.source = block
	listing := make(chan error, 1)
	go func() { listing <- f.RefreshDirectory(ctx, "") }()
	<-block.entered
	type result struct {
		id  string
		err error
	}
	rename := make(chan result, 1)
	go func() { id, err := f.BeginManagedRename(ctx, "temp", "sheet.url"); rename <- result{id, err} }()
	select {
	case r := <-rename:
		if r.err != nil {
			close(block.release)
			<-listing
			t.Fatal(r.err)
		}
		if err := f.AbortManagedRename(ctx, r.id); err != nil {
			close(block.release)
			<-listing
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		close(block.release)
		<-listing
		t.Fatal("local namespace waited for cloud listing")
	}
	close(block.release)
	if err := <-listing; err != nil {
		t.Fatal(err)
	}
	if _, err := f.BeginManagedRename(ctx, "temp", "sheet.url"); !errors.Is(err, fs.ErrorPermissionDenied) {
		t.Fatal("known virtual target lost readonly protection", err)
	}
	ops, _ := st.PendingNamespace(ctx)
	if len(ops) != 0 {
		t.Fatal(ops)
	}
}
