package overlay

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/fs/object"
	"github.com/rclone/rclone/internal/bridge/discovery"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
	"github.com/rclone/rclone/lib/larkcli"
)

type accountTestSource struct {
	fakeSource
	tree    map[string][]larkcli.DriveItem
	parents []string
}

func (s *accountTestSource) List(_ context.Context, parent string) ([]larkcli.DriveItem, error) {
	s.parents = append(s.parents, parent)
	return append([]larkcli.DriveItem(nil), s.tree[parent]...), s.listErr
}
func accountFixture(t *testing.T, entries []discovery.Entry) (*Fs, *store.Store, *accountTestSource, *discovery.Snapshot, *error) {
	t.Helper()
	old, st, _ := fixture(t)
	source := &accountTestSource{tree: map[string][]larkcli.DriveItem{}}
	snapshot := &discovery.Snapshot{Entries: entries}
	var readErr error
	raw, err := New(Dependencies{Store: st, ProfileID: old.scope.ProfileID, MountID: old.scope.MountID, Source: source, Account: &AccountOptions{AccountToken: "root", PersonalToken: "personal", ReadSnapshot: func(context.Context) (discovery.Snapshot, error) { return *snapshot, readErr }}})
	if err != nil {
		t.Fatal(err)
	}
	return raw.(*Fs), st, source, snapshot, &readErr
}
func accountList(t *testing.T, f *Fs, remote string) map[string]fs.DirEntry {
	t.Helper()
	rows, err := f.List(ctx, remote)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]fs.DirEntry{}
	for _, row := range rows {
		out[row.Remote()] = row
	}
	return out
}
func TestAccountGroupsCanonicalRootsAndOfficialRootFiles(t *testing.T) {
	f, st, source, _, _ := accountFixture(t, []discovery.Entry{
		{Token: "both", Name: "Both", Memberships: []string{discovery.Personal, discovery.SharedVisible, discovery.Manual}},
		{Token: "shared", Name: "Shared", Memberships: []string{discovery.SharedHidden, discovery.Manual}},
		{Token: "official", Name: "Official", Placement: "personal", Memberships: []string{discovery.SharedVisible}},
		{Token: "manual", Name: "Manual", Placement: "shared", Memberships: []string{discovery.Manual}},
	})
	source.tree[""] = []larkcli.DriveItem{{Token: "rootfile", Name: "Root file", Type: "file"}, {Token: "both", Name: "Both", Type: "folder"}, {Token: "official", Name: "Official", Type: "folder"}}
	top := accountList(t, f, "")
	if len(top) != 2 || top["个人目录"] == nil || top["共享目录"] == nil || top["手动添加"] != nil {
		t.Fatal(top)
	}
	if err := f.RefreshDirectory(ctx, "个人目录"); err != nil {
		t.Fatal(err)
	}
	personal := accountList(t, f, "个人目录")
	if len(personal) != 3 || personal["个人目录/Root file"] == nil || personal["个人目录/Both"] == nil || personal["个人目录/Official"] == nil {
		t.Fatal(personal)
	}
	shared := accountList(t, f, "共享目录")
	if len(shared) != 2 || shared["共享目录/Shared"] == nil || shared["共享目录/Manual"] == nil {
		t.Fatal(shared)
	}
	for token, want := range map[string]string{"both": "个人目录/Both", "shared": "共享目录/Shared", "manual": "共享目录/Manual"} {
		entry, err := st.EntryByToken(ctx, token)
		if err != nil || entry.Remote != want {
			t.Fatal(entry, err)
		}
	}
	if source.observes != 0 || source.fetches != 0 {
		t.Fatal("metadata listing downloaded bodies")
	}
	for _, p := range source.parents {
		if p == "personal" || p == "root-shared" || p == "root-manual" || p == "root" {
			t.Fatal("synthetic token sent to official source", p)
		}
	}
}
func TestAccountMaterializesAllRootsBeforeNestedTraversalAndRescan(t *testing.T) {
	f, st, source, snapshot, readErr := accountFixture(t, []discovery.Entry{{Token: "a", Name: "A", Memberships: []string{discovery.SharedVisible}}, {Token: "b", Name: "B", Memberships: []string{discovery.SharedVisible}}})
	source.tree["a"] = []larkcli.DriveItem{{Token: "b", Name: "Nested B", Type: "folder", ParentToken: "a"}, {Token: "child", Name: "Child", Type: "file", ParentToken: "a"}}
	accountList(t, f, "")
	children := accountList(t, f, "共享目录/A")
	if len(children) != 1 || children["共享目录/A/Child"] == nil {
		t.Fatal(children)
	}
	b, err := st.EntryByToken(ctx, "b")
	if err != nil || b.Remote != "共享目录/B" {
		t.Fatal(b, err)
	}
	snapshot.Entries = append(snapshot.Entries, discovery.Entry{Token: "c", Name: "C", Memberships: []string{discovery.SharedVisible}})
	if err := f.RefreshCatalog(ctx); err != nil {
		t.Fatal(err)
	}
	source.tree["a"] = append(source.tree["a"], larkcli.DriveItem{Token: "c", Name: "Nested C", Type: "folder", ParentToken: "a"})
	if got := accountList(t, f, "共享目录/A"); len(got) != 1 {
		t.Fatal("new catalog root was claimed by nested traversal", got)
	}
	if got := accountList(t, f, "共享目录"); len(got) != 3 || got["共享目录/C"] == nil {
		t.Fatal(got)
	}
	snapshot.Entries = nil
	if got := accountList(t, f, "共享目录"); len(got) != 3 {
		t.Fatal("empty scan removed mappings", got)
	}
	*readErr = errors.New("catalog unavailable")
	if got := accountList(t, f, "共享目录"); len(got) != 3 {
		t.Fatal("warm snapshot failure hid entries", got)
	}
	if err := f.RefreshDirectory(ctx, "共享目录"); err == nil {
		t.Fatal("explicit metadata refresh did not report failure")
	}
	if e, err := st.EntryByToken(ctx, "c"); err != nil || e.Remote != "共享目录/C" {
		t.Fatal(e, err)
	}
	*readErr = nil
	source.listErr = errors.New("official metadata unavailable")
	if err := f.RefreshDirectory(ctx, "共享目录"); err != nil {
		t.Fatal("shared refresh depends on official personal root", err)
	}
	if err := f.RefreshDirectory(ctx, "个人目录"); err == nil {
		t.Fatal("missing official personal metadata failure")
	}
	if e, err := st.EntryByToken(ctx, "c"); err != nil || e.Remote != "共享目录/C" {
		t.Fatal(e, err)
	}
}
func TestAccountProtectsEntrancesAndSyntheticParentsButAllowsChildren(t *testing.T) {
	f, st, source, _, _ := accountFixture(t, []discovery.Entry{{Token: "shared", Name: "Shared", Memberships: []string{discovery.SharedVisible}}, {Token: "manual", Name: "Manual", Memberships: []string{discovery.Manual}}, {Token: "personal-folder", Name: "Personal", Memberships: []string{discovery.Personal}}})
	source.tree["shared"] = []larkcli.DriveItem{{Token: "child", Name: "Child", Type: "file", ParentToken: "shared"}}
	accountList(t, f, "")
	accountList(t, f, "共享目录/Shared")
	for _, remote := range []string{"个人目录", "共享目录", "手动添加", "共享目录/Shared", "手动添加/Manual", "个人目录/Personal"} {
		if err := f.Rmdir(ctx, remote); !errors.Is(err, fs.ErrorPermissionDenied) {
			t.Fatal(remote, err)
		}
		if _, err := f.BeginManagedRename(ctx, remote, "个人目录/Renamed"); !errors.Is(err, fs.ErrorPermissionDenied) {
			t.Fatal(remote, err)
		}
	}
	for _, parent := range []string{"", "共享目录", "手动添加"} {
		remote := "Forbidden"
		if parent != "" {
			remote = parent + "/Forbidden"
		}
		if _, err := f.Put(ctx, strings.NewReader("x"), object.NewStaticObjectInfo(remote, time.Now(), 1, true, nil, f)); !errors.Is(err, fs.ErrorPermissionDenied) {
			t.Fatal(remote, err)
		}
		if err := f.Mkdir(ctx, remote); !errors.Is(err, fs.ErrorPermissionDenied) {
			t.Fatal(remote, err)
		}
		if _, err := f.BeginManagedRename(ctx, "个人目录/Missing", remote); !errors.Is(err, fs.ErrorPermissionDenied) {
			t.Fatal(remote, err)
		}
		if _, err := st.EntryByRemote(ctx, remote); !errors.Is(err, store.ErrNotFound) {
			t.Fatal("denied admission created mapping", remote, err)
		}
	}
	if _, err := st.EntryByRemote(ctx, "个人目录/Missing"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("absent-source rename created mapping", err)
	}
	put(t, f, "个人目录/New", "new")
	put(t, f, "共享目录/Shared/New", "new")
	put(t, f, "共享目录/Shared/Child", "edit")
	if err := f.Mkdir(ctx, "手动添加/Manual/Child folder"); err != nil {
		t.Fatal(err)
	}
	id, err := f.BeginManagedRename(ctx, "共享目录/Shared/New", "共享目录/Shared/Renamed")
	if err != nil {
		t.Fatal(err)
	}
	if err := f.AbortManagedRename(ctx, id); err != nil {
		t.Fatal(err)
	}
}

func TestAccountManualPlacementDoesNotCreateThirdCategoryOrNestedAlias(t *testing.T) {
	f, st, source, _, _ := accountFixture(t, []discovery.Entry{
		{Token: "personal-manual", Name: "Personal manual", Memberships: []string{discovery.Manual}, Manual: true, Placement: "personal"},
		{Token: "shared-manual", Name: "Shared manual", Memberships: []string{discovery.Manual}, Manual: true, Placement: "shared"},
		{Token: "nested", Name: "Nested", Memberships: []string{discovery.Manual}, Manual: true, Placement: "shared", CanonicalRootToken: "shared-manual", RelativePath: []string{"Nested"}},
	})
	source.tree["shared-manual"] = []larkcli.DriveItem{{Token: "nested", Name: "Nested", Type: "folder", ParentToken: "shared-manual"}}
	if rows := accountList(t, f, ""); len(rows) != 2 || rows["手动添加"] != nil {
		t.Fatal(rows)
	}
	if e, err := st.EntryByToken(ctx, "personal-manual"); err != nil || e.Remote != "个人目录/Personal manual" {
		t.Fatal(e, err)
	}
	if e, err := st.EntryByToken(ctx, "shared-manual"); err != nil || e.Remote != "共享目录/Shared manual" {
		t.Fatal(e, err)
	}
	if _, err := st.EntryByToken(ctx, "nested"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("nested supplemental entry became a root", err)
	}
	accountList(t, f, "共享目录/Shared manual")
	nested, err := st.EntryByToken(ctx, "nested")
	if err != nil || nested.Remote != "共享目录/Shared manual/Nested" {
		t.Fatal(nested, err)
	}
	if err := f.requireWritable(ctx, nested); err != nil {
		t.Fatal("nested supplemental folder protected as root", err)
	}
}
func TestAccountLegacyManualCanonicalPathsAndPendingEditsArePreserved(t *testing.T) {
	f, st, source, _, _ := accountFixture(t, []discovery.Entry{{Token: "legacy", Name: "Legacy", Memberships: []string{discovery.Manual}, Manual: true}})
	accountList(t, f, "")
	put(t, f, "手动添加/Legacy/Unsynced", "accepted bytes")
	before, err := st.EntryByToken(ctx, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	pending, err := st.Pending(ctx)
	if err != nil || len(pending) != 1 {
		t.Fatal(pending, err)
	}
	raw, err := New(Dependencies{Store: st, Source: source, MountID: f.scope.MountID, ProfileID: f.scope.ProfileID, Account: &AccountOptions{AccountToken: "root", PersonalToken: "personal", ReadSnapshot: func(context.Context) (discovery.Snapshot, error) {
		return discovery.Snapshot{Entries: []discovery.Entry{{Token: "legacy", Name: "Legacy", Memberships: []string{discovery.Manual}, Manual: true, Placement: "shared"}}}, nil
	}}})
	if err != nil {
		t.Fatal(err)
	}
	f = raw.(*Fs)
	if err := f.RefreshDirectory(ctx, ""); err != nil {
		t.Fatal(err)
	}
	rows := accountList(t, f, "")
	if rows["手动添加"] == nil {
		t.Fatal("legacy subtree hidden", rows)
	}
	after, err := st.EntryByToken(ctx, "legacy")
	if err != nil || after.ID != before.ID || after.Remote != before.Remote {
		t.Fatal(before, after, err)
	}
	if _, err := st.EntryByRemote(ctx, "共享目录/Legacy"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("legacy token duplicated", err)
	}
	o, err := f.NewObject(ctx, "手动添加/Legacy/Unsynced")
	if err != nil || body(t, o) != "accepted bytes" {
		t.Fatal(o, err)
	}
	jobs, err := st.Pending(ctx)
	if err != nil || len(jobs) != 1 || jobs[0].ID != pending[0].ID {
		t.Fatal(jobs, err)
	}
}
func TestAccountEmptyLegacyManualCategoryIsHiddenWithoutDeletingStoreRows(t *testing.T) {
	f, st, _, _, _ := accountFixture(t, nil)
	if _, err := st.ObserveEntry(ctx, store.Observation{Remote: "手动添加", Kind: model.KindFolder, Token: "root-manual"}); err != nil {
		t.Fatal(err)
	}
	rows := accountList(t, f, "")
	if len(rows) != 2 || rows["手动添加"] != nil {
		t.Fatal(rows)
	}
	if e, err := st.EntryByRemote(ctx, "手动添加"); err != nil || e.Token != "root-manual" {
		t.Fatal("legacy row deleted", e, err)
	}
}

func TestPersonalRootAnchoredSupplementIsNotProtectedAsAnEntrance(t *testing.T) {
	f, st, source, _, _ := accountFixture(t, []discovery.Entry{{Token: "direct", Name: "Direct", Placement: "personal", CanonicalRootToken: "personal", RelativePath: []string{"Direct"}, Memberships: []string{discovery.Manual}, Manual: true}})
	source.tree[""] = []larkcli.DriveItem{{Token: "direct", Name: "Direct", Type: "folder"}}
	accountList(t, f, "")
	accountList(t, f, "个人目录")
	entry, err := st.EntryByToken(ctx, "direct")
	if err != nil || entry.Remote != "个人目录/Direct" {
		t.Fatal(entry, err)
	}
	if err := f.requireWritable(ctx, entry); err != nil {
		t.Fatal("ordinary personal child classified as protected entrance", err)
	}
}
