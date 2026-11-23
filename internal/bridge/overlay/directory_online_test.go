package overlay

import (
	"github.com/rclone/rclone/lib/larkcli"
	"testing"
)

func TestDirectoryFailurePreservesRowsAndPreciseDiagnostic(t *testing.T) {
	source := &metadataTreeSource{tree: map[string][]larkcli.DriveItem{"root": {{Token: "kept", Name: "kept.txt", Type: "file", ParentToken: "root"}}}, fail: map[string]error{}}
	f, s, _, _ := cacheFixture(t, source)
	if _, err := f.List(ctx, ""); err != nil {
		t.Fatal(err)
	}
	source.fail["root"] = larkcli.ErrAuthRequired
	if err := f.RefreshDirectory(ctx, ""); err == nil {
		t.Fatal("missing refresh failure")
	}
	entries, err := s.Entries(ctx, "")
	if err != nil || len(entries) != 1 {
		t.Fatal("old rows lost", entries, err)
	}
	stats, err := f.DirectoryStatus(ctx)
	if err != nil || stats.Online == nil || stats.Online.Last.Cause != "auth_required" || stats.Online.Last.Phase != "list_parent" {
		t.Fatalf("lost diagnostic: %+v %v", stats, err)
	}
	delete(source.fail, "root")
	if err := f.RefreshDirectory(ctx, ""); err != nil {
		t.Fatal(err)
	}
	stats, err = f.DirectoryStatus(ctx)
	if err != nil || stats.Failed != 0 || stats.Online != nil {
		t.Fatalf("active failure retained: %+v %v", stats, err)
	}
}
