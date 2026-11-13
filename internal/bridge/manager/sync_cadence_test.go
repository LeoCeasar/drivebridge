package manager

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/stretchr/testify/require"
)

func TestGracefulStopForcesPendingSyncInsteadOfWaitingFiveMinutes(t *testing.T) {
	m, id, _, _ := fixture(t, nil)
	_, err := m.Start(context.Background(), id, true)
	require.NoError(t, err)
	w := m.localWorker(id)
	s := w.services.store
	e, err := s.EnsureEntry(context.Background(), ".DS_Store", model.KindFile)
	require.NoError(t, err)
	_, _, err = s.Stage(context.Background(), e.ID, nil, strings.NewReader("durable metadata"))
	require.NoError(t, err)
	w.services.syncNow = func(ctx context.Context) error { _, err := s.ReconcileSystemMetadata(ctx); return err }
	require.NoError(t, m.Stop(context.Background(), id, time.Second))
}

func TestPreservingStopNeverForcesCloudSync(t *testing.T) {
	m, id, _, _ := fixture(t, nil)
	_, err := m.Start(context.Background(), id, true)
	require.NoError(t, err)
	w := m.localWorker(id)
	s := w.services.store
	e, err := s.EnsureEntry(context.Background(), "file", model.KindFile)
	require.NoError(t, err)
	_, _, err = s.Stage(context.Background(), e.ID, nil, strings.NewReader("preserved queued bytes"))
	require.NoError(t, err)
	w.services.syncNow = func(context.Context) error { t.Error("preserving stop must not request cloud work"); return nil }
	require.NoError(t, w.stopPolicy(context.Background(), time.Second, true))
}

func TestStatusReportsEffectiveFixedCloudCadence(t *testing.T) {
	m, id, _, _ := fixture(t, nil)
	_, err := m.Start(context.Background(), id, true)
	require.NoError(t, err)
	status, err := m.Status(context.Background(), id)
	require.NoError(t, err)
	require.Equal(t, "5m0s", status.CloudSyncInterval)
}
