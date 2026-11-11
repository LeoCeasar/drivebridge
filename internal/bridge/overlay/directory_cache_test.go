package overlay

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/internal/bridge/discovery"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
	"github.com/rclone/rclone/lib/larkcli"
)

type metadataTreeSource struct {
	fakeSource
	metadataMu       sync.Mutex
	tree             map[string][]larkcli.DriveItem
	calls            map[string]int
	fail             map[string]error
	blocked          string
	entered, release chan struct{}
}

func (s *metadataTreeSource) List(ctx context.Context, token string) ([]larkcli.DriveItem, error) {
	s.metadataMu.Lock()
	if s.calls == nil {
		s.calls = map[string]int{}
	}
	s.calls[token]++
	items := append([]larkcli.DriveItem(nil), s.tree[token]...)
	err := s.fail[token]
	blocked, entered, release := token == s.blocked && s.entered != nil, s.entered, s.release
	s.metadataMu.Unlock()
	if blocked {
		select {
		case entered <- struct{}{}:
		default:
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-release:
		}
	}
	return items, err
}
func (s *metadataTreeSource) count(token string) int {
	s.metadataMu.Lock()
	defer s.metadataMu.Unlock()
	return s.calls[token]
}
func cacheFixture(t *testing.T, source Source) (*Fs, *store.Store, string, store.Scope) {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	scope := store.Scope{MountID: model.MountID(uuid.NewString()), ProfileID: model.ProfileID(uuid.NewString()), RemoteID: model.RemoteID(uuid.NewString()), IdentityHash: "identity", FolderToken: "root"}
	if err := st.BindScope(ctx, scope); err != nil {
		t.Fatal(err)
	}
	raw, err := New(Dependencies{Store: st, Source: source, MountID: scope.MountID, ProfileID: scope.ProfileID})
	if err != nil {
		t.Fatal(err)
	}
	return raw.(*Fs), st, dir, scope
}
func TestDurableDirectoryMetadataIsLocalAcrossRepeatRestartAndOffline(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(map[bool]string{false: "placeholder", true: "empty directory"}[empty], func(t *testing.T) {
			source := &fakeSource{bodies: map[string]string{"file": "lazy body"}}
			if !empty {
				source.items = []larkcli.DriveItem{{Token: "file", Name: "File", Type: "file"}}
			}
			f, st, dir, scope := cacheFixture(t, source)
			first, err := f.List(ctx, "")
			if err != nil {
				t.Fatal(err)
			}
			if source.lists != 1 || source.observes != 0 || source.fetches != 0 {
				t.Fatal(source.lists, source.observes, source.fetches)
			}
			source.listErr = errors.New("offline")
			if _, err := f.List(ctx, ""); err != nil || source.lists != 1 {
				t.Fatal("warm read fetched metadata", err, source.lists)
			}
			if err := st.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := store.Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if err := reopened.BindScope(ctx, scope); err != nil {
				t.Fatal(err)
			}
			raw, err := New(Dependencies{Store: reopened, Source: source, MountID: scope.MountID, ProfileID: scope.ProfileID})
			if err != nil {
				t.Fatal(err)
			}
			f = raw.(*Fs)
			rows, err := f.List(ctx, "")
			if err != nil || len(rows) != len(first) || source.lists != 1 {
				t.Fatal("offline restart lost metadata", rows, err, source.lists)
			}
			if !empty {
				o := rows[0].(fs.Object)
				if o.Size() != -1 || source.observes != 0 {
					t.Fatal("placeholder acquired body", o.Size(), source.observes)
				}
				if got := body(t, o); got != "lazy body" || source.observes != 1 {
					t.Fatal(got, source.observes)
				}
				if got := body(t, o); got != "lazy body" || source.observes != 1 {
					t.Fatal("reopened object fetched twice", got, source.observes)
				}
			}
			status, err := f.DirectoryStatus(ctx)
			if err != nil || status.Known != 1 || status.Cached != 1 {
				t.Fatal(status, err)
			}
		})
	}
}
func TestWarmDirectoryReadsAndColdChildDoNotWaitForPersonalRefresh(t *testing.T) {
	source := &metadataTreeSource{tree: map[string][]larkcli.DriveItem{
		"shared": {{Token: "nested", Name: "Nested", Type: "folder", ParentToken: "shared"}, {Token: "file", Name: "File", Type: "file", ParentToken: "shared"}},
		"nested": {{Token: "inner", Name: "Inner", Type: "file", ParentToken: "nested"}},
	}, entered: make(chan struct{}, 1), release: make(chan struct{})}
	old, st, _, scope := cacheFixture(t, source)
	raw, err := New(Dependencies{Store: st, Source: source, MountID: scope.MountID, ProfileID: scope.ProfileID, Account: &AccountOptions{AccountToken: "root", PersonalToken: "personal", ReadSnapshot: func(context.Context) (discovery.Snapshot, error) {
		return discovery.Snapshot{Entries: []discovery.Entry{{Token: "shared", Name: "Shared", Memberships: []string{discovery.SharedVisible}}}}, nil
	}}})
	if err != nil {
		t.Fatal(err)
	}
	_ = old
	f := raw.(*Fs)
	accountList(t, f, "")
	accountList(t, f, "共享目录/Shared")
	if source.count("") != 0 {
		t.Fatal("account catalog fetched official root")
	}
	source.metadataMu.Lock()
	source.blocked = ""
	source.metadataMu.Unlock()
	refresh := make(chan error, 1)
	go func() { refresh <- f.RefreshDirectory(ctx, "个人目录") }()
	select {
	case <-source.entered:
	case <-time.After(time.Second):
		t.Fatal("personal refresh did not enter")
	}
	read := make(chan error, 1)
	go func() {
		_, err := f.List(ctx, "共享目录/Shared")
		if err == nil {
			_, err = f.List(ctx, "共享目录/Shared/Nested")
		}
		read <- err
	}()
	select {
	case err := <-read:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		close(source.release)
		t.Fatal("cached/shared child blocked on personal root")
	}
	status, err := f.DirectoryStatus(ctx)
	if err != nil || status.Refreshing != 1 {
		t.Fatal(status, err)
	}
	close(source.release)
	if err := <-refresh; err != nil {
		t.Fatal(err)
	}
	if source.count("shared") != 1 || source.count("nested") != 1 || source.observes != 0 || source.fetches != 0 {
		t.Fatal(source.calls)
	}
}
func TestWarmMetadataBFSContinuesAcrossBatchesWithoutDownloadingBodies(t *testing.T) {
	source := &metadataTreeSource{tree: map[string][]larkcli.DriveItem{
		"root":  {{Token: "one", Name: "One", Type: "folder", ParentToken: "root"}},
		"one":   {{Token: "two", Name: "Two", Type: "folder", ParentToken: "one"}},
		"two":   {{Token: "three", Name: "Three", Type: "folder", ParentToken: "two"}},
		"three": {{Token: "payload", Name: "Payload", Type: "file", ParentToken: "three"}},
	}}
	f, st, _, _ := cacheFixture(t, source)
	for i := 0; i < 4; i++ {
		if err := f.warmDirectoryBatch(ctx, 1); err != nil {
			t.Fatal(err)
		}
	}
	if entry, err := st.EntryByToken(ctx, "payload"); err != nil || entry.Remote != "One/Two/Three/Payload" {
		t.Fatal(entry, err)
	}
	for _, token := range []string{"root", "one", "two", "three"} {
		if source.count(token) != 1 {
			t.Fatal("batch restarted at first directory", source.calls)
		}
	}
	if source.observes != 0 || source.fetches != 0 {
		t.Fatal("metadata warming downloaded a file")
	}
	status, err := f.DirectoryStatus(ctx)
	if err != nil || status.Known != 4 || status.Cached != 4 {
		t.Fatal(status, err)
	}
}
func TestAccountWarmMetadataContinuesAfterRootFailureAndKeepsPriorRows(t *testing.T) {
	source := &metadataTreeSource{tree: map[string][]larkcli.DriveItem{"shared": {{Token: "nested", Name: "Nested", Type: "folder", ParentToken: "shared"}}, "nested": {{Token: "payload", Name: "Payload", Type: "file", ParentToken: "nested"}}}, fail: map[string]error{"": errors.New("personal root offline")}}
	_, st, _, scope := cacheFixture(t, source)
	raw, err := New(Dependencies{Store: st, Source: source, MountID: scope.MountID, ProfileID: scope.ProfileID, Account: &AccountOptions{AccountToken: "root", PersonalToken: "personal", ReadSnapshot: func(context.Context) (discovery.Snapshot, error) {
		return discovery.Snapshot{Entries: []discovery.Entry{{Token: "shared", Name: "Shared", Memberships: []string{discovery.SharedVisible}}}}, nil
	}}})
	if err != nil {
		t.Fatal(err)
	}
	f := raw.(*Fs)
	if err := f.WarmDirectories(ctx); err == nil {
		t.Fatal("failed personal refresh was not reported")
	}
	if entry, err := st.EntryByToken(ctx, "payload"); err != nil || entry.Remote != "共享目录/Shared/Nested/Payload" {
		t.Fatal(entry, err)
	}
	source.metadataMu.Lock()
	source.fail["nested"] = errors.New("later page failed")
	source.tree["nested"] = []larkcli.DriveItem{{Token: "partial", Name: "Partial", Type: "file", ParentToken: "nested"}}
	source.metadataMu.Unlock()
	if err := f.WarmDirectories(ctx); err == nil {
		t.Fatal("failed refresh not reported")
	}
	rows := accountList(t, f, "共享目录/Shared/Nested")
	if rows["共享目录/Shared/Nested/Payload"] == nil || len(rows) != 1 {
		t.Fatal("failed scan changed local placeholder view", rows)
	}
	if _, err := st.EntryByToken(ctx, "partial"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
	status, err := f.DirectoryStatus(ctx)
	if err != nil || status.Failed < 2 || status.Cached < 4 {
		t.Fatal(status, err)
	}
}

func TestMetadataWarmCancellationReleasesRefreshAndResumesWork(t *testing.T) {
	source := &metadataTreeSource{tree: map[string][]larkcli.DriveItem{"root": {{Token: "folder", Name: "Folder", Type: "folder", ParentToken: "root"}}}, blocked: "root", entered: make(chan struct{}, 1), release: make(chan struct{})}
	f, st, _, _ := cacheFixture(t, source)
	cancelCtx, cancel := context.WithCancel(ctx)
	done := make(chan error, 1)
	go func() { done <- f.WarmDirectories(cancelCtx) }()
	select {
	case <-source.entered:
	case <-time.After(time.Second):
		t.Fatal("metadata request did not start")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("warm request survived cancellation")
	}
	status, err := f.DirectoryStatus(ctx)
	if err != nil || status.Refreshing != 0 {
		t.Fatal(status, err)
	}
	close(source.release)
	if err := f.WarmDirectories(ctx); err != nil {
		t.Fatal(err)
	}
	if e, err := st.EntryByToken(ctx, "folder"); err != nil || e.Remote != "Folder" {
		t.Fatal(e, err)
	}
	if source.observes != 0 || source.fetches != 0 {
		t.Fatal("canceled/resumed metadata acquired a body")
	}
}
func TestRefreshCatalogPublishesNewRootsWithoutAnyNetwork(t *testing.T) {
	f, st, source, snapshot, _ := accountFixture(t, []discovery.Entry{{Token: "first", Name: "First", Memberships: []string{discovery.SharedVisible}}})
	accountList(t, f, "")
	accountList(t, f, "共享目录")
	before := len(source.parents)
	source.listErr = errors.New("official metadata blocked")
	snapshot.Entries = append(snapshot.Entries, discovery.Entry{Token: "second", Name: "Second", Memberships: []string{discovery.Manual}, Manual: true, Placement: "shared"})
	if err := f.RefreshCatalog(ctx); err != nil {
		t.Fatal(err)
	}
	if len(source.parents) != before {
		t.Fatal("catalog performed network I/O")
	}
	if e, err := st.EntryByToken(ctx, "second"); err != nil || e.Remote != "共享目录/Second" {
		t.Fatal(e, err)
	}
	rows := accountList(t, f, "共享目录")
	if rows["共享目录/Second"] == nil || len(source.parents) != before {
		t.Fatal(rows, source.parents)
	}
}

func TestKnownPersonalPlaceholdersAreVisibleDuringFirstRootRefresh(t *testing.T) {
	source := &metadataTreeSource{tree: map[string][]larkcli.DriveItem{}, blocked: "", entered: make(chan struct{}, 1), release: make(chan struct{})}
	_, st, _, scope := cacheFixture(t, source)
	raw, err := New(Dependencies{Store: st, Source: source, MountID: scope.MountID, ProfileID: scope.ProfileID, Account: &AccountOptions{AccountToken: "root", PersonalToken: "personal", ReadSnapshot: func(context.Context) (discovery.Snapshot, error) {
		return discovery.Snapshot{Entries: []discovery.Entry{{Token: "known", Name: "Known", Memberships: []string{discovery.Personal}}}}, nil
	}}})
	if err != nil {
		t.Fatal(err)
	}
	f := raw.(*Fs)
	if err := f.RefreshCatalog(ctx); err != nil {
		t.Fatal(err)
	}
	refresh := make(chan error, 1)
	go func() { refresh <- f.RefreshDirectory(ctx, "个人目录") }()
	select {
	case <-source.entered:
	case <-time.After(time.Second):
		t.Fatal("root refresh not started")
	}
	read := make(chan error, 1)
	go func() {
		rows, err := f.List(ctx, "个人目录")
		if err == nil && len(rows) != 1 {
			err = errors.New("known placeholder missing")
		}
		read <- err
	}()
	select {
	case err := <-read:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		close(source.release)
		t.Fatal("known personal placeholders waited for first complete root listing")
	}
	close(source.release)
	if err := <-refresh; err != nil {
		t.Fatal(err)
	}
	if source.observes != 0 || source.fetches != 0 {
		t.Fatal("placeholder acquired body")
	}
}
