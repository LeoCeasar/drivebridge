package ui

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/rclone/rclone/internal/bridge/auth"
	"github.com/rclone/rclone/internal/bridge/manager"
	"github.com/rclone/rclone/internal/bridge/model"
)

type snapshotReader struct {
	busy             model.MountID
	failConflicts    bool
	entered, release chan struct{}
}

func (s *snapshotReader) Status(ctx context.Context, id model.MountID) (model.MountStatus, error) {
	if s.entered != nil {
		close(s.entered)
		s.entered = nil
		<-s.release
	}
	if id == s.busy && !s.failConflicts {
		return model.MountStatus{}, manager.ErrPending
	}
	return model.MountStatus{MountID: id, Running: true, LocalSaved: true, CloudState: "conflict", Queued: 3, Conflicts: 1}, nil
}
func (s *snapshotReader) Conflicts(ctx context.Context, id model.MountID) ([]manager.ConflictView, error) {
	if id == s.busy && s.failConflicts {
		return nil, manager.ErrPending
	}
	return []manager.ConflictView{{Conflict: model.Conflict{ID: model.ConflictID("conflict-" + string(id))}, Entry: model.Entry{Remote: "draft.md"}}}, nil
}
func TestStatusSnapshotRetainsConflictsAndCountsDuringContention(t *testing.T) {
	for _, failConflicts := range []bool{false, true} {
		t.Run(map[bool]string{false: "status", true: "conflicts"}[failConflicts], func(t *testing.T) {
			var cache statusSnapshots
			mounts := []MountView{{ID: "busy"}, {ID: "healthy"}}
			source := &snapshotReader{}
			initial := cache.read(context.Background(), mounts, source)
			if initial.Partial || len(initial.Conflicts) != 2 {
				t.Fatal(initial)
			}
			source.busy = "busy"
			source.failConflicts = failConflicts
			next := cache.read(context.Background(), mounts, source)
			if !next.Partial || len(next.Conflicts) != 2 || next.Mounts[0].State.Queued != 3 || next.Mounts[0].StatusWarning != "pending" || next.Mounts[1].StatusWarning != "" {
				t.Fatal(next)
			}
			if next.Mounts[0].StatusUpdatedAt == "" {
				t.Fatal("cached result has no observation time")
			}
			source.busy = ""
			next = cache.read(context.Background(), mounts, source)
			if next.Partial || next.Mounts[0].StatusWarning != "" {
				t.Fatal(next)
			}
		})
	}
}
func TestStatusSnapshotNeverInventsCleanStateWithoutHistory(t *testing.T) {
	var cache statusSnapshots
	result := cache.read(context.Background(), []MountView{{ID: "busy"}}, &snapshotReader{busy: "busy"})
	if !result.Partial || result.Mounts[0].State.CloudState != "unknown" || result.Mounts[0].State.LocalSaved || result.Mounts[0].StatusUpdatedAt != "" {
		t.Fatal(result)
	}
}
func TestStatusSnapshotConcurrentPollReturnsPreviousSnapshot(t *testing.T) {
	var cache statusSnapshots
	mounts := []MountView{{ID: "live"}}
	cache.read(context.Background(), mounts, &snapshotReader{})
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		cache.read(context.Background(), mounts, &snapshotReader{entered: entered, release: release})
	}()
	<-entered
	next := cache.read(context.Background(), mounts, &snapshotReader{})
	close(release)
	<-done
	if !next.Partial || next.Mounts[0].State.Queued != 3 || len(next.Conflicts) != 1 {
		t.Fatal(next)
	}
}

type statusFailureBackend struct{ *fakeBackend }

func (b statusFailureBackend) Status(context.Context) (StatusView, error) {
	return StatusView{}, errors.Join(manager.ErrPending, errors.New("private transport detail"))
}
func TestStatusFailureUsesLocalStatusMessage(t *testing.T) {
	h, b, c, _, _ := fixture(t)
	h.backend = statusFailureBackend{b}
	res := request(t, c, h, "GET", "/api/status", nil, "", "")
	defer res.Body.Close()
	var got map[string]any
	if err := json.NewDecoder(res.Body).Decode(&got); err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 503 || got["code"] != "status_unavailable" || strings.Contains(got["message"].(string), "云端") || strings.Contains(got["message"].(string), "草稿") {
		t.Fatal(res.Status, got)
	}
}
func TestLocalContentionIsNotReportedAsCloudOffline(t *testing.T) {
	w := httptest.NewRecorder()
	apiError(w, manager.ErrPending)
	var got map[string]any
	json.Unmarshal(w.Body.Bytes(), &got)
	if w.Code != http.StatusServiceUnavailable || got["code"] != "local_busy" {
		t.Fatal(w.Code, got)
	}
}

func TestStatusWarningKeepsAccountFailuresActionable(t *testing.T) {
	if statusWarningCode(auth.ErrUserMissing) != "auth_required" || statusWarningCode(model.ErrIdentityChanged) != "identity_changed" || statusWarningCode(context.DeadlineExceeded) != "worker_unavailable" {
		t.Fatal("actionable account/service failures were hidden as ordinary contention")
	}
	if got := statusWarningCode(errors.New("private CLI detail")); got != "operation_failed" {
		t.Fatal(got)
	}
}
