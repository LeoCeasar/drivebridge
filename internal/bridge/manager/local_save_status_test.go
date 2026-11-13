package manager

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
	"github.com/stretchr/testify/require"
)

type localFailureRuntime struct {
	*fakeRuntime
	saveFailed, archiveFailed []string
}

func (r *localFailureRuntime) LocalSaveStatus() (bool, int, []string) {
	return len(r.saveFailed) == 0, 1, r.archiveFailed
}
func (r *localFailureRuntime) LocalSaveFailures() []string { return r.saveFailed }

func TestWorkerStatusDistinguishesLocalConfirmationAndArchiveFailure(t *testing.T) {
	ctx := context.Background()
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	s, err := store.Open(root)
	require.NoError(t, err)
	defer s.Close()
	mount := model.MountID(uuid.NewString())
	require.NoError(t, s.BindScope(ctx, store.Scope{MountID: mount, ProfileID: model.ProfileID(uuid.NewString()), RemoteID: model.RemoteID(uuid.NewString()), FolderToken: "fixture", IdentityHash: strings.Repeat("a", 64)}))
	runtime := &localFailureRuntime{fakeRuntime: &fakeRuntime{done: make(chan error)}, saveFailed: []string{"unsaved.md"}, archiveFailed: []string{"saved.md"}}
	w := &worker{ready: true, spec: mountSpec{Mount: model.Mount{ID: mount}}, services: &workerServices{store: s, runtime: runtime}}
	status, err := w.status(ctx)
	require.NoError(t, err)
	require.False(t, status.LocalSaved)
	require.Equal(t, "local_save_failed", status.LastError, "unconfirmed durability must take priority")
	require.Equal(t, "blocked", status.CloudState)
	issues := map[string]model.SyncIssue{}
	for _, issue := range status.Issues {
		issues[issue.Name] = issue
	}
	require.Equal(t, "local_save_failed", issues["unsaved.md"].Code)
	require.False(t, issues["unsaved.md"].LocalSaved)
	require.Equal(t, "local_archive_failed", issues["saved.md"].Code)
	runtime.saveFailed = nil
	status, err = w.status(ctx)
	require.NoError(t, err)
	require.True(t, status.LocalSaved, "archive failure does not invalidate local durability")
	require.Equal(t, "local_archive_failed", status.LastError)
	runtime.archiveFailed = nil
	status, err = w.status(ctx)
	require.NoError(t, err)
	require.Empty(t, status.LastError)
	require.Empty(t, status.Issues)
}
