package store

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/stretchr/testify/require"
	bolt "go.etcd.io/bbolt"
)

func TestLocalSaveBasePinSurvivesRestartAndProtectsGC(t *testing.T) {
	s, dir, scope := openTest(t)
	b, err := s.PutBlob(ctx, strings.NewReader("immutable base needed by a saved delta"))
	require.NoError(t, err)
	key := uuid.NewString()
	require.NoError(t, s.RetainManagedBase(ctx, key, b))
	require.NoError(t, s.Close())
	s, err = Open(dir)
	require.NoError(t, err)
	defer s.Close()
	require.NoError(t, s.BindScope(ctx, scope))
	require.NoError(t, s.view(ctx, func(tx *bolt.Tx) error {
		require.Equal(t, "3", string(tx.Bucket([]byte("meta")).Get([]byte("schema_version"))), "older applications must reject a journal-aware Store, including after reopening")
		return nil
	}))
	future := time.Now().Add(90 * 24 * time.Hour)
	_, err = s.GC(ctx, GCOptions{Now: future})
	require.NoError(t, err)
	require.Equal(t, "immutable base needed by a saved delta", read(t, s, b))
	require.NoError(t, s.ReconcileManagedBases(ctx, map[string]bool{}))
	_, err = s.GC(ctx, GCOptions{Now: future})
	require.NoError(t, err)
	_, err = os.Stat(s.blobPath(b))
	require.True(t, os.IsNotExist(err))
}

func TestLocalSaveAdmissionBlocksRemoteRefreshAndDeletionBeforeStage(t *testing.T) {
	s, _, _ := openTest(t)
	e, err := s.ObserveEntry(ctx, Observation{Remote: "file", Token: "token", Kind: model.KindFile})
	require.NoError(t, err)
	base := fileSnapshot(t, s, "token", "base")
	e, err = s.AdoptFileSnapshot(ctx, e.ID, e.Generation, base.ID)
	require.NoError(t, err)
	key := uuid.NewString()
	require.NoError(t, s.RetainManagedEntry(ctx, key, e.ID, e.Visible))
	actual := fileSnapshot(t, s, "token", "cloud changed")
	_, err = s.OfferRemote(ctx, e.ID, e.Generation, actual, actual.FileData)
	require.ErrorIs(t, err, ErrBusy, "pending WAL must protect the observed baseline before a sync job exists")
	_, removed, err := s.ObserveRemoteDeletion(ctx, e)
	require.ErrorIs(t, err, ErrBusy)
	require.False(t, removed)
	require.NoError(t, s.ReleaseManagedBase(ctx, key))
	_, err = s.OfferRemote(ctx, e.ID, e.Generation, actual, actual.FileData)
	require.NoError(t, err)
}
