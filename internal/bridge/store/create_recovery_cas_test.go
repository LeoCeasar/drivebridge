package store

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/stretchr/testify/require"
	bolt "go.etcd.io/bbolt"
)

// Decode the same immutable evidence an API caller receives. This exercises the
// wire contract together with the store's transactional comparison.
func recoveryCycleRequest(t *testing.T, en model.Entry, job model.Job, cycle string, version int) model.CreateRecoveryRequest {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"job_id": job.ID, "action": "recheck", "expected_generation": en.Generation, "expected_local": en.Local, "expected_diagnostic_cycle": cycle, "expected_policy_version": version})
	require.NoError(t, err)
	var req model.CreateRecoveryRequest
	require.NoError(t, json.Unmarshal(raw, &req))
	return req
}
func pauseCreationProtocol(t *testing.T, s *Store, id string) model.CreateOp {
	t.Helper()
	var op model.CreateOp
	for range 3 {
		var err error
		op, err = s.RecordCreateObservation(ctx, id, "protocol_error", testOnlineFailure("protocol_error", "document_read", time.Now()), false)
		require.NoError(t, err)
	}
	require.True(t, op.NeedsAttention)
	return op
}

// Fails if rechecking compares only local generation/blob and can therefore
// restart a later diagnostic pause with an earlier user's unchanged snapshot.
func TestRecheckCreateProtocolRestartRequiresObservedCycle(t *testing.T) {
	s, _, _ := openTest(t)
	en, job, op := recoveryCreate(t, s)
	require.NoError(t, s.MarkCreateUnknown(ctx, op.ID))
	paused := pauseCreationProtocol(t, s, op.ID)
	for _, bad := range []struct {
		name, cycle string
		version     int
	}{
		{name: "missing"},
		{name: "old cycle", cycle: "old-cycle", version: paused.RecoveryVersion},
		{name: "old policy", cycle: paused.DiagnosticCycle, version: paused.RecoveryVersion + 1},
		{name: "missing policy", cycle: paused.DiagnosticCycle},
	} {
		t.Run(bad.name, func(t *testing.T) {
			_, err := s.RecheckCreate(ctx, recoveryCycleRequest(t, en, job, bad.cycle, bad.version))
			require.ErrorIs(t, err, model.ErrVersionChanged)
			after, err := s.CreateForJob(ctx, job.ID)
			require.NoError(t, err)
			require.Equal(t, paused, after)
		})
	}
	req := recoveryCycleRequest(t, en, job, paused.DiagnosticCycle, paused.RecoveryVersion)
	_, err := s.RecheckCreate(ctx, req)
	require.NoError(t, err)
	restarted, err := s.CreateForJob(ctx, job.ID)
	require.NoError(t, err)
	require.NotEmpty(t, restarted.DiagnosticCycle)
	require.NotEqual(t, paused.DiagnosticCycle, restarted.DiagnosticCycle)
	require.Zero(t, restarted.UnclassifiedProbes)
	require.False(t, restarted.NeedsAttention)
	require.Equal(t, paused.CheckAttempts, restarted.CheckAttempts)
	require.Equal(t, paused.RequestFailures, restarted.RequestFailures)
	// The first successful restart consumes that exact cycle, even before another pause.
	_, err = s.RecheckCreate(ctx, req)
	require.ErrorIs(t, err, model.ErrVersionChanged)
	later := pauseCreationProtocol(t, s, op.ID)
	require.Equal(t, restarted.DiagnosticCycle, later.DiagnosticCycle)
	_, err = s.RecheckCreate(ctx, req)
	require.ErrorIs(t, err, model.ErrVersionChanged)
	after, err := s.CreateForJob(ctx, job.ID)
	require.NoError(t, err)
	require.Equal(t, later, after)
	_, err = s.RecheckCreate(ctx, recoveryCycleRequest(t, en, job, later.DiagnosticCycle, later.RecoveryVersion))
	require.NoError(t, err)
}

// Fails if the compatibility path for old records remains reusable after its
// first migration, allowing delayed no-cycle requests to reset newer budgets.
func TestRecheckCreateLegacyCycleMigratesOnce(t *testing.T) {
	s, _, _ := openTest(t)
	en, job, op := recoveryCreate(t, s)
	op.RecoveryVersion = 0
	op.DiagnosticCycle = ""
	op.CheckAttempts = 4
	op.UnclassifiedProbes = 3
	op.NeedsAttention = true
	op.PausedReason = "protocol_error"
	op.State = "unknown"
	require.NoError(t, s.update(ctx, func(tx *bolt.Tx) error {
		if err := put(tx, "createops", op.ID, op); err != nil {
			return err
		}
		j, err := jobTx(tx, job.ID)
		if err != nil {
			return err
		}
		return saveCreateCheckState(tx, op, j)
	}))
	legacyReq := recoveryCycleRequest(t, en, job, "", 0)
	_, err := s.RecheckCreate(ctx, legacyReq)
	require.NoError(t, err)
	migrated, err := s.CreateForJob(ctx, job.ID)
	require.NoError(t, err)
	require.NotEmpty(t, migrated.DiagnosticCycle)
	require.Equal(t, 1, migrated.RecoveryVersion)
	require.Equal(t, 4, migrated.LegacyCheckAttempts)
	require.Equal(t, 4, migrated.CheckAttempts)
	require.Zero(t, migrated.UnclassifiedProbes)
	_, err = s.RecordCreateObservation(ctx, op.ID, "protocol_error", testOnlineFailure("protocol_error", "document_read", time.Now()), false)
	require.NoError(t, err)
	_, err = s.RecheckCreate(ctx, legacyReq)
	require.NoError(t, err)
	coalesced, err := s.CreateForJob(ctx, job.ID)
	require.NoError(t, err)
	require.Equal(t, migrated.DiagnosticCycle, coalesced.DiagnosticCycle)
	require.Equal(t, 1, coalesced.UnclassifiedProbes)
	require.Equal(t, 4, coalesced.LegacyCheckAttempts)
	for range 2 {
		_, err = s.RecordCreateObservation(ctx, op.ID, "protocol_error", testOnlineFailure("protocol_error", "document_read", time.Now()), false)
		require.NoError(t, err)
	}
	paused, err := s.CreateForJob(ctx, job.ID)
	require.NoError(t, err)
	require.True(t, paused.NeedsAttention)
	_, err = s.RecheckCreate(ctx, legacyReq)
	require.ErrorIs(t, err, model.ErrVersionChanged)
	after, err := s.CreateForJob(ctx, job.ID)
	require.NoError(t, err)
	require.Equal(t, paused, after)
}

// Ordinary legacy callers may still request observation, without obtaining the
// power to reset a modern cycle or its accumulated diagnostic probes.
func TestRecheckCreateBeforeProtocolPauseRemainsCompatible(t *testing.T) {
	s, _, _ := openTest(t)
	en, job, op := recoveryCreate(t, s)
	require.NoError(t, s.MarkCreateUnknown(ctx, op.ID))
	before, err := s.RecordCreateObservation(ctx, op.ID, "protocol_error", testOnlineFailure("protocol_error", "document_read", time.Now()), false)
	require.NoError(t, err)
	_, err = s.RecheckCreate(ctx, recoveryCycleRequest(t, en, job, "", 0))
	require.NoError(t, err)
	after, err := s.CreateForJob(ctx, job.ID)
	require.NoError(t, err)
	require.Equal(t, before.DiagnosticCycle, after.DiagnosticCycle)
	require.Equal(t, 1, after.UnclassifiedProbes)
	require.True(t, after.ObservationOnly)
}
