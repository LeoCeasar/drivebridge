package overlay

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
	"github.com/rclone/rclone/lib/larkcli"
)

func TestStaleOpenedDirectoryDiscoversTwelfthChildBeforeBulkScan(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		old := make([]larkcli.DriveItem, 11)
		for i := range old {
			old[i] = larkcli.DriveItem{Token: fmt.Sprintf("old-%d", i), Name: fmt.Sprintf("old-%d", i), Type: "folder", ParentToken: "target"}
		}
		source := &metadataTreeSource{tree: map[string][]larkcli.DriveItem{"root": {{Token: "bulk", Name: "Bulk", Type: "folder", ParentToken: "root"}, {Token: "target", Name: "Target", Type: "folder", ParentToken: "root"}}, "target": old}}
		f, s, _, _ := cacheFixture(t, source)
		if _, err := f.List(ctx, ""); err != nil {
			t.Fatal(err)
		}
		if rows, err := f.List(ctx, "Target"); err != nil || len(rows) != 11 {
			t.Fatal(len(rows), err)
		}
		// Root was scanned; Bulk is ahead of Target in the existing queue.
		if err := f.warmDirectoryBatch(ctx, 1); err != nil {
			t.Fatal(err)
		}
		source.metadataMu.Lock()
		source.tree["target"] = append(old, larkcli.DriveItem{Token: "new-folder", Name: "Open平台渠道协议与分销功能", Type: "folder", ParentToken: "target"})
		source.metadataMu.Unlock()
		time.Sleep(31 * time.Second)
		rows, err := f.List(ctx, "Target")
		if err != nil || len(rows) != 11 {
			t.Fatal("stale read must return cached rows immediately", len(rows), err)
		}
		if source.count("target") != 1 {
			t.Fatal("file-manager read waited for a network refresh")
		}
		if err := f.warmDirectoryBatch(ctx, 1); err != nil {
			t.Fatal(err)
		}
		children, err := s.Entries(ctx, "Target")
		if err != nil || len(children) != 12 {
			t.Fatalf("opened stale parent waited behind bulk scan: got %d children, want 12; err=%v", len(children), err)
		}
		if source.count("bulk") != 0 {
			t.Fatal("background bulk directory ran before opened parent")
		}
		if source.observes != 0 || source.fetches != 0 {
			t.Fatal("directory discovery downloaded file bodies")
		}
	})
}

func TestManualDirectoryRefreshMergesRequestsAndNotifiesParent(t *testing.T) {
	source := &metadataTreeSource{tree: map[string][]larkcli.DriveItem{"root": {{Token: "existing", Name: "Existing", Type: "folder", ParentToken: "root"}}}}
	f, s, _, _ := cacheFixture(t, source)
	if _, err := f.List(ctx, ""); err != nil {
		t.Fatal(err)
	}
	source.metadataMu.Lock()
	source.tree["root"] = append(source.tree["root"], larkcli.DriveItem{Token: "added", Name: "Added", Type: "folder", ParentToken: "root"})
	source.metadataMu.Unlock()
	for range 20 {
		if err := f.RequestDirectoryRefresh(ctx, ""); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-f.DirectoryRefreshWake():
	default:
		t.Fatal("request did not wake metadata worker")
	}
	notices := []string{}
	if err := f.warmDirectoryBatch(ctx, 1, func(remote string) { notices = append(notices, remote) }); err != nil {
		t.Fatal(err)
	}
	rows, err := s.Entries(ctx, "")
	if err != nil || len(rows) != 2 {
		t.Fatal(rows, err)
	}
	if source.count("root") != 2 || len(notices) != 1 || notices[0] != "" {
		t.Fatal("duplicate reads or missing parent notification", source.count("root"), notices)
	}
}

func TestDirectoryRefreshDeadlineDoesNotBlockFollowingDirectory(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		source := &metadataTreeSource{tree: map[string][]larkcli.DriveItem{"root": {{Token: "slow", Name: "Slow", Type: "folder", ParentToken: "root"}, {Token: "healthy", Name: "Healthy", Type: "folder", ParentToken: "root"}}}, blocked: "slow", entered: make(chan struct{}, 1), release: make(chan struct{})}
		f, _, _, _ := cacheFixture(t, source)
		if _, err := f.List(ctx, ""); err != nil {
			t.Fatal(err)
		}
		if err := f.RequestDirectoryRefresh(ctx, "Slow"); err != nil {
			t.Fatal(err)
		}
		if err := f.RequestDirectoryRefresh(ctx, "Healthy"); err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		err := f.warmDirectoryBatch(ctx, 2)
		if err == nil || time.Since(started) != 30*time.Second || source.count("healthy") != 1 {
			t.Fatal("slow read consumed the following directory's opportunity", time.Since(started), err, source.count("healthy"))
		}
	})
}

func TestDirectoryRefreshShutdownCancelsReadAndRefusesNewWork(t *testing.T) {
	source := &metadataTreeSource{tree: map[string][]larkcli.DriveItem{"root": {}}, blocked: "root", entered: make(chan struct{}, 1), release: make(chan struct{})}
	f, _, _, _ := cacheFixture(t, source)
	if err := f.RequestDirectoryRefresh(ctx, ""); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- f.warmDirectoryBatch(ctx, 1) }()
	select {
	case <-source.entered:
	case <-time.After(time.Second):
		t.Fatal("read did not start")
	}
	stop, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	if err := f.Shutdown(stop); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-stop.Done():
		t.Fatal("read survived shutdown")
	}
	if err := f.RequestDirectoryRefresh(ctx, ""); !errors.Is(err, context.Canceled) {
		t.Fatal("work accepted after close", err)
	}
}

func TestDirectoryRequestInterruptsAnExistingBatchAndCoalescesInflight(t *testing.T) {
	source := &metadataTreeSource{tree: map[string][]larkcli.DriveItem{"root": {{Token: "bulk", Name: "Bulk", Type: "folder"}, {Token: "target", Name: "Target", Type: "folder"}}}, blocked: "bulk", entered: make(chan struct{}, 1), release: make(chan struct{})}
	f, _, _, _ := cacheFixture(t, source)
	if _, err := f.List(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if err := f.warmDirectoryBatch(ctx, 1); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- f.warmDirectoryBatch(ctx, 2) }()
	<-source.entered
	for range 20 {
		if err := f.RequestDirectoryRefresh(ctx, "Target"); err != nil {
			t.Fatal(err)
		}
	}
	close(source.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if source.count("target") != 1 {
		t.Fatal("batch did not accept priority work", source.count("target"))
	}
	// Start a request and request a single trailing observation many times.
	source.metadataMu.Lock()
	source.blocked = "target"
	source.release = make(chan struct{})
	release := source.release
	source.metadataMu.Unlock()
	if err := f.RequestDirectoryRefresh(ctx, "Target"); err != nil {
		t.Fatal(err)
	}
	go func() { done <- f.warmDirectoryBatch(ctx, 1) }()
	<-source.entered
	for range 20 {
		if err := f.RequestDirectoryRefresh(ctx, "Target"); err != nil {
			t.Fatal(err)
		}
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := f.warmDirectoryBatch(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if source.count("target") != 3 {
		t.Fatal("inflight clicks did not merge into one trailing read", source.count("target"))
	}
}

func TestDirectoryPriorityBoundFairnessAndIdentity(t *testing.T) {
	f, _, _, _ := cacheFixture(t, &metadataTreeSource{})
	for i := range maxDirectoryRequests {
		en := model.Entry{ID: model.EntryID(fmt.Sprint(i)), Remote: fmt.Sprint(i), Token: fmt.Sprint(i)}
		if err := f.queueDirectoryRefresh(en, store.Metadata{}, true); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.queueDirectoryRefresh(model.Entry{ID: "overflow"}, store.Metadata{}, true); !errors.Is(err, store.ErrBusy) {
		t.Fatal("unbounded queue", err)
	}
	for range 3 {
		if f.takeDirectoryRequest(true) == nil {
			t.Fatal("missing priority turn")
		}
	}
	if f.takeDirectoryRequest(true) != nil {
		t.Fatal("priority work starved normal scan")
	}
	f.normalDirectoryTurn()
	if f.takeDirectoryRequest(true) == nil {
		t.Fatal("normal turn failed to release next priority work")
	}
	source := &metadataTreeSource{tree: map[string][]larkcli.DriveItem{"root": {{Token: "old", Name: "Target", Type: "folder"}}}}
	other, s, _, _ := cacheFixture(t, source)
	if _, err := other.List(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if err := other.RequestDirectoryRefresh(ctx, "Target"); err != nil {
		t.Fatal(err)
	}
	// Rename after admission: stable ID now has another route.
	op, err := s.PrepareNamespace(ctx, model.NamespaceOp{Kind: "rename", Src: "Target", Dst: "Moved"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SealNamespace(ctx, op.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CommitNamespace(ctx, op.ID); err != nil {
		t.Fatal(err)
	}

	if err := other.warmDirectoryBatch(ctx, 1); !errors.Is(err, model.ErrIdentityChanged) {
		t.Fatal("old request adopted replacement", err)
	}
	if source.count("old") != 0 || source.count("replacement") != 0 {
		t.Fatal("stale request dispatched network")
	}
}

func TestDirectoryFailureRetainsCacheAndPersistsBackoff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		source := &metadataTreeSource{tree: map[string][]larkcli.DriveItem{"root": {{Token: "old", Name: "Old", Type: "folder"}}}}
		f, s, _, _ := cacheFixture(t, source)
		if _, err := f.List(ctx, ""); err != nil {
			t.Fatal(err)
		}
		source.metadataMu.Lock()
		source.fail = map[string]error{"root": context.DeadlineExceeded}
		source.metadataMu.Unlock()
		if err := f.RequestDirectoryRefresh(ctx, ""); err != nil {
			t.Fatal(err)
		}
		if err := f.warmDirectoryBatch(ctx, 1); err == nil {
			t.Fatal("expected failure")
		}
		root, err := f.directory(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		m, err := s.Metadata(ctx, root.ID)
		if err != nil {
			t.Fatal(err)
		}
		if m.DirectoryOnline == nil || m.DirectoryOnline.RequestFailures != 1 || !m.DirectoryOnline.NotBefore.After(time.Now()) {
			t.Fatal("failure did not persist waiting bound", m.DirectoryOnline)
		}
		for range 20 {
			if err := f.RequestDirectoryRefresh(ctx, ""); err != nil {
				t.Fatal(err)
			}
		}
		if err := f.warmDirectoryBatch(ctx, 1); err != nil {
			t.Fatal(err)
		}
		if source.count("root") != 2 {
			t.Fatal("manual retry bypassed waiting bound", source.count("root"))
		}
		rows, err := f.List(ctx, "")
		if err != nil || len(rows) != 1 {
			t.Fatal("failure lost old listing", rows, err)
		}
		raw, err := New(Dependencies{Store: s, Source: source, MountID: f.scope.MountID, ProfileID: f.scope.ProfileID})
		if err != nil {
			t.Fatal(err)
		}
		restarted := raw.(*Fs)
		if err := restarted.RequestDirectoryRefresh(ctx, ""); err != nil {
			t.Fatal(err)
		}
		if err := restarted.warmDirectoryBatch(ctx, 1); err != nil {
			t.Fatal(err)
		}
		if source.count("root") != 2 {
			t.Fatal("restart cleared waiting bound")
		}
	})
}

func TestDirectoryStatusSeparatesCoverageAgeAndPending(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f, _, _, _ := cacheFixture(t, &metadataTreeSource{tree: map[string][]larkcli.DriveItem{"root": {{Token: "child", Name: "Child", Type: "folder"}}}})
		if _, err := f.List(ctx, ""); err != nil {
			t.Fatal(err)
		}
		time.Sleep(31 * time.Second)
		if err := f.RequestDirectoryRefresh(ctx, ""); err != nil {
			t.Fatal(err)
		}
		status, err := f.DirectoryStatus(ctx)
		if err != nil || status.Cached != 1 || status.Stale != 2 || status.Pending != 1 || status.LastRefreshed.IsZero() {
			t.Fatal("coverage does not describe age or queue", status, err)
		}
		if len(status.Recent) != 1 || status.Recent[0].Remote != "" || status.Recent[0].State != "queued" {
			t.Fatal("selected directory is invisible in status", status.Recent)
		}
	})
}

func TestPriorityCompletedRowsDoNotConsumeNormalFairnessTurn(t *testing.T) {
	source := &metadataTreeSource{tree: map[string][]larkcli.DriveItem{"root": {{Token: "one", Name: "One", Type: "folder"}, {Token: "two", Name: "Two", Type: "folder"}}}}
	f, _, _, _ := cacheFixture(t, source)
	if _, err := f.List(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if err := f.warmDirectoryBatch(ctx, 1); err != nil {
		t.Fatal(err)
	}
	one, err := f.directory(ctx, "One")
	if err != nil {
		t.Fatal(err)
	}
	f.warmSeen[one.ID] = true // Previously completed by priority, still in normal cursor.
	f.directorySchedule.mu.Lock()
	f.directorySchedule.burst = 3
	f.directorySchedule.mu.Unlock()
	if err := f.RequestDirectoryRefresh(ctx, "One"); err != nil {
		t.Fatal(err)
	}
	if err := f.warmDirectoryBatch(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if source.count("two") != 1 {
		t.Fatal("completed priority row consumed the normal scan's turn")
	}
}

func TestDirectoryRefreshScopeRejectsFilesAndUnsafePaths(t *testing.T) {
	f, _, _, _ := cacheFixture(t, &metadataTreeSource{tree: map[string][]larkcli.DriveItem{"root": {{Token: "file", Name: "File", Type: "file"}}}})
	if _, err := f.List(ctx, ""); err != nil {
		t.Fatal(err)
	}
	for _, remote := range []string{"File", "../x", "/tmp", "C:/tmp", "C:tmp", "a\\b"} {
		if err := f.RequestDirectoryRefresh(ctx, remote); err == nil {
			t.Fatal("accepted invalid directory", remote)
		}
	}
}

func TestCatalogRefreshQueuesVisitedParentsWithoutNetwork(t *testing.T) {
	source := &metadataTreeSource{tree: map[string][]larkcli.DriveItem{"root": {{Token: "target", Name: "Target", Type: "folder"}}}}
	f, _, _, _ := cacheFixture(t, source)
	if _, err := f.List(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.List(ctx, "Target"); err != nil {
		t.Fatal(err)
	}
	if err := f.RequestVisitedDirectoryRefresh(ctx); err != nil {
		t.Fatal(err)
	}
	if source.count("root") != 1 || source.count("target") != 1 {
		t.Fatal("catalog refresh blocked on network")
	}
	status, err := f.DirectoryStatus(ctx)
	if err != nil || status.Pending != 2 {
		t.Fatal(status, err)
	}
}
