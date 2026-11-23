package overlay

import (
	"context"
	"errors"
	"github.com/rclone/rclone/internal/bridge/model"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rclone/rclone/lib/larkcli"
)

func TestDirectoryRegressionColdBackoff(t *testing.T) {
	src := &metadataTreeSource{fail: map[string]error{"root": context.DeadlineExceeded}}
	f, s, _, _ := cacheFixture(t, src)
	for range 4 {
		if _, err := f.List(ctx, ""); err == nil {
			t.Fatal("expected previous or current failed observation")
		}
	}
	en, err := f.directory(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	m, err := s.Metadata(ctx, en.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := src.count("root"); got != 1 {
		t.Fatalf("got %d cloud reads for 4 rapid opens (durable failures=%d, waiting=%t), want 1", got, m.DirectoryOnline.RequestFailures, m.DirectoryOnline.NotBefore.After(time.Now()))
	}
}
func TestDirectoryRegressionLocalPartialList(t *testing.T) {
	src := &metadataTreeSource{tree: map[string][]larkcli.DriveItem{"root": {{Token: "new", Name: "CloudOnly", Type: "folder"}}}}
	f, s, _, _ := cacheFixture(t, src)
	_, err := s.EnsureEntry(ctx, "Local", model.KindFolder)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := f.List(ctx, "")
	if err != nil || len(rows) != 1 {
		t.Fatal(rows, err)
	}
	view, err := f.DirectoryStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if view.Pending != 1 {
		t.Fatalf("root contains a local child and has never had a complete cloud listing; opening it left %d priority requests, want 1", view.Pending)
	}
}
func TestDirectoryRegressionOrdinaryInFlightAutoDedup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		src := &metadataTreeSource{}
		f, _, _, _ := cacheFixture(t, src)
		if _, err := f.List(ctx, ""); err != nil {
			t.Fatal(err)
		}
		time.Sleep(31 * time.Second)
		src.blocked = "root"
		src.entered = make(chan struct{}, 1)
		src.release = make(chan struct{})
		done := make(chan error, 1)
		go func() { done <- f.warmDirectoryBatch(ctx, 2) }()
		<-src.entered
		for range 5 {
			if _, err := f.List(ctx, ""); err != nil {
				t.Fatal(err)
			}
		}
		close(src.release)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if n := src.count("root"); n != 2 {
			t.Fatalf("one initial cold read and one ordinary refresh plus five automatic cache hits resulted in %d cloud reads, want 2", n)
		}
	})
}

type regressionCatalog struct {
	*metadataTreeSource
	issue error
}

func (s *regressionCatalog) accountDirectories(context.Context) ([]accountDirectory, error) {
	return nil, s.issue
}
func TestDirectoryRegressionAdoptedListingNotifiesDespiteCatalogError(t *testing.T) {
	src := &regressionCatalog{metadataTreeSource: &metadataTreeSource{tree: map[string][]larkcli.DriveItem{"root": {{Token: "old", Name: "Old", Type: "folder"}}}}}
	f, s, _, _ := cacheFixture(t, src)
	if _, err := f.List(ctx, ""); err != nil {
		t.Fatal(err)
	}
	src.tree["root"] = append(src.tree["root"], larkcli.DriveItem{Token: "new", Name: "New", Type: "folder"})
	src.issue = errors.New("unrelated catalog unavailable")
	notices := 0
	if err := f.RequestDirectoryRefresh(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if err := f.warmDirectoryBatch(ctx, 1, func(string) { notices++ }); err == nil {
		t.Fatal("expected catalog failure")
	}
	rows, err := s.Entries(ctx, "")
	if err != nil || len(rows) != 2 {
		t.Fatal(rows, err)
	}
	if notices != 1 {
		t.Fatalf("both rows adopted but %d VFS notifications, want 1", notices)
	}
}

func TestDirectoryRegressionReplacedParentDuringRead(t *testing.T) {
	src := &metadataTreeSource{tree: map[string][]larkcli.DriveItem{"root": {{Token: "old-dir", Name: "Parent", Type: "folder"}}}}
	f, s, _, _ := cacheFixture(t, src)
	if _, err := f.List(ctx, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := f.List(ctx, "Parent"); err != nil {
		t.Fatal(err)
	}
	old, err := f.directory(ctx, "Parent")
	if err != nil {
		t.Fatal(err)
	}
	src.tree["old-dir"] = []larkcli.DriveItem{{Token: "old-child", Name: "OldChild", Type: "file"}}
	src.blocked = "old-dir"
	src.entered = make(chan struct{}, 1)
	src.release = make(chan struct{})
	if err := f.RequestDirectoryRefresh(ctx, "Parent"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- f.warmDirectoryBatch(ctx, 1) }()
	select {
	case <-src.entered:
	case <-time.After(time.Second):
		t.Fatal("not entered")
	}
	if err := f.Rmdir(ctx, "Parent"); err != nil {
		close(src.release)
		<-done
		t.Fatal("remove", err)
	}
	if err := f.Mkdir(ctx, "Parent"); err != nil {
		close(src.release)
		<-done
		t.Fatal("mkdir", err)
	}
	fresh, err := f.directory(ctx, "Parent")
	if err != nil || fresh.ID == old.ID {
		close(src.release)
		<-done
		t.Fatal("failed to replace", err)
	}
	close(src.release)
	readErr := <-done
	rows, err := s.Entries(ctx, "Parent")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) > 0 {
		t.Fatalf("old read populated replacement parent with %q token=%q despite returning %v", rows[0].Remote, rows[0].Token, readErr)
	}
}
func TestDirectoryRegressionRequestWakesWhenDelayedTurnDue(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		src := &metadataTreeSource{}
		f, s, _, _ := cacheFixture(t, src)
		root, err := f.directory(ctx, "")
		if err != nil {
			t.Fatal(err)
		}
		if err = s.RecordDirectoryOnline(ctx, root, &model.OnlineFailure{Cause: "request_timeout", At: time.Now()}); err != nil {
			t.Fatal(err)
		}
		if err := f.RequestDirectoryRefresh(ctx, ""); err != nil {
			t.Fatal(err)
		}
		<-f.DirectoryRefreshWake()
		if err := f.warmDirectoryBatch(ctx, 1); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Minute)
		if err := f.RequestDirectoryRefresh(ctx, ""); err != nil {
			t.Fatal(err)
		}
		select {
		case <-f.DirectoryRefreshWake():
		default:
			t.Fatalf("due priority directory cannot wake idle indexer even after user clicks refresh again")
		}
	})
}
func TestDirectoryRegressionScopedWakeReadsUnrelated(t *testing.T) {
	src := &metadataTreeSource{tree: map[string][]larkcli.DriveItem{"root": {{Token: "target", Name: "Target", Type: "folder"}, {Token: "sibling", Name: "Sibling", Type: "folder"}}}}
	f, _, _, _ := cacheFixture(t, src)
	if err := f.WarmDirectories(ctx); err != nil {
		t.Fatal(err)
	}
	old := src.count("sibling")
	if err := f.RequestDirectoryRefresh(ctx, "Target"); err != nil {
		t.Fatal(err)
	}
	<-f.DirectoryRefreshWake()
	if err := f.RefreshRequestedDirectories(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if n := src.count("sibling"); n != old {
		t.Fatalf("after exhausted background scan, manual refresh of Target made %d more cloud calls for unrelated fresh Sibling", n-old)
	}
}

func TestDirectoryRegressionLocalCoverageIsNotCloudRefresh(t *testing.T) {
	src := &metadataTreeSource{}
	f, s, _, scope := cacheFixture(t, src)
	_, err := s.EnsureEntry(ctx, "Unuploaded", model.KindFolder)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.EnsureEntry(ctx, "Unuploaded/Local", model.KindFile)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := New(Dependencies{Store: s, Source: src, MountID: scope.MountID, ProfileID: scope.ProfileID})
	if err != nil {
		t.Fatal(err)
	}
	f = raw.(*Fs)
	status, err := f.DirectoryStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.LastRefreshed.IsZero() == false && len(src.calls) == 0 {
		t.Fatalf("last successful directory refresh shown as %v with zero provider calls, including offline restart and never-uploaded local folder", status.LastRefreshed)
	}
}

func TestDirectoryRegressionScheduledDispatchRechecksPersistentWait(t *testing.T) {
	src := &metadataTreeSource{}
	f, s, _, _ := cacheFixture(t, src)
	if _, err := f.List(ctx, ""); err != nil {
		t.Fatal(err)
	}
	root, err := f.directory(ctx, "")
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RecordDirectoryOnline(ctx, root, &model.OnlineFailure{At: time.Now(), Cause: "rate_limited", RetryAfterSeconds: 120}); err != nil {
		t.Fatal(err)
	}
	if err = f.refreshDirectoryIdentity(ctx, "", false, &root); !errors.Is(err, ErrDirectoryWaiting) {
		t.Fatal("scheduled dispatch ignored newer service wait", err)
	}
	if src.count("root") != 1 {
		t.Fatal("scheduled dispatch bypassed persistent admission")
	}
}
