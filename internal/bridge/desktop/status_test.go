package desktop

import (
	"testing"

	"github.com/rclone/rclone/internal/bridge/model"
)

func TestAggregateNeverReportsUnconfirmedDataSynced(t *testing.T) {
	for _, change := range []func(*model.MountStatus){
		func(s *model.MountStatus) { s.Queued = 1 },
		func(s *model.MountStatus) { s.Inflight = 1 },
		func(s *model.MountStatus) { s.VFSPending = 1 },
		func(s *model.MountStatus) { s.LocalSaved = false },
		func(s *model.MountStatus) { s.CloudState = "awaiting_publish" },
		func(s *model.MountStatus) { s.CloudState = "awaiting_verification" },
		func(s *model.MountStatus) { s.CloudState = "" },
	} {
		s := model.MountStatus{Running: true, LocalSaved: true, CloudState: "synced"}
		change(&s)
		if got := Aggregate([]MountView{{Mount: Mount{ID: "a", Desired: true}, Status: s}}); got.Kind == "synced" {
			t.Fatalf("unconfirmed status marked synced: %+v", s)
		}
	}
}

func TestAggregateRetainsPartialFailures(t *testing.T) {
	good := MountView{Mount: Mount{ID: "a", Desired: true}, Status: model.MountStatus{Running: true, LocalSaved: true, CloudState: "synced"}}
	if got := Aggregate([]MountView{good}); got.Kind != "synced" {
		t.Fatal(got)
	}
	bad := MountView{Mount: Mount{ID: "b", Desired: true}, Error: "unauthorized"}
	got := Aggregate([]MountView{good, bad})
	if got.Kind != "unauthorized" || len(got.Mounts) != 2 {
		t.Fatal(got)
	}
	bad.Error = ""
	bad.Status.Conflicts = 2
	if got = Aggregate([]MountView{good, bad}); got.Kind != "conflict" {
		t.Fatal(got)
	}
	bad.Status = model.MountStatus{}
	bad.Desired = false
	if got = Aggregate([]MountView{good, bad}); got.Kind == "synced" {
		t.Fatal(got)
	}
}
