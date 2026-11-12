package manager

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
)

// The status route reports the cache size observed by the periodic sweep; it
// never walks the cache on a poll and stays absent until a sweep has measured.
func TestWorkerStatusReportsObservedCacheBytesOnlyAfterMeasurement(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	mount := model.MountID(uuid.NewString())
	if err = s.BindScope(ctx, store.Scope{MountID: mount, ProfileID: model.ProfileID(uuid.NewString()), RemoteID: model.RemoteID(uuid.NewString()), FolderToken: "fixture", IdentityHash: strings.Repeat("a", 64)}); err != nil {
		t.Fatal(err)
	}
	observed := int64(-1)
	w := &worker{ready: true, spec: mountSpec{Mount: model.Mount{ID: mount}}, services: &workerServices{store: s, runtime: &fakeRuntime{done: make(chan error)}, cacheBytes: func() (int64, bool) { return observed, observed >= 0 }}}
	status, err := w.status(ctx)
	if err != nil || status.CacheBytes != nil {
		t.Fatal("unmeasured cache must be omitted, not reported as zero", status, err)
	}
	if raw, _ := json.Marshal(status); strings.Contains(string(raw), "cache_bytes") {
		t.Fatal(string(raw))
	}
	observed = 1234
	status, err = w.status(ctx)
	if err != nil || status.CacheBytes == nil || *status.CacheBytes != 1234 {
		t.Fatal(status, err)
	}
}
