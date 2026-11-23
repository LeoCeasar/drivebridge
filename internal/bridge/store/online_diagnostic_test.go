package store

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/stretchr/testify/require"
	bolt "go.etcd.io/bbolt"
)

func testOnlineFailure(cause, phase string, at time.Time) model.OnlineFailure {
	return model.OnlineFailure{SchemaVersion: 1, ID: "diagnostic", Operation: "create", Phase: phase, Cause: cause, Outcome: "dispatched_unconfirmed", Action: "observe_only", At: at}
}

func TestOnlineDiagnosticsRetainFirstLatestAndBoundedHistory(t *testing.T) {
	s, _, _ := openTest(t)
	_, job, _ := recoveryCreate(t, s)
	now := time.Now().UTC()
	for i := range 12 {
		failure := testOnlineFailure("auth_required", fmt.Sprintf("phase_%d", i), now.Add(time.Duration(i)*time.Second))
		require.NoError(t, s.RecordJobOnlineFailure(ctx, job.ID, failure))
	}
	j, err := s.Journal(ctx, job.ID)
	require.NoError(t, err)
	require.Equal(t, "phase_0", j.Online.First.Phase)
	require.Equal(t, "phase_11", j.Online.Last.Phase)
	require.Len(t, j.Online.History, 8)
	require.Equal(t, "dispatched_unconfirmed", j.Online.Last.Outcome)
	require.NoError(t, s.RecordJobOnlineFailure(ctx, job.ID, *j.Online.Last))
	after, err := s.Journal(ctx, job.ID)
	require.NoError(t, err)
	require.Len(t, after.Online.History, 8)
	require.Equal(t, job.Local, after.Job.Local)
}

func TestCreateObservationTransportFailuresDoNotConsumeValidChecks(t *testing.T) {
	s, _, _ := openTest(t)
	_, job, op := recoveryCreate(t, s)
	require.NoError(t, s.MarkCreateUncertain(ctx, op.ID, "", "", "request_timeout", time.Now()))
	now := time.Now().UTC()
	for i := range 5 {
		failure := testOnlineFailure("auth_required", "list_parent", now.Add(time.Duration(i)*time.Minute))
		got, err := s.RecordCreateObservation(ctx, op.ID, "auth_required", failure, false)
		require.NoError(t, err)
		require.False(t, got.NeedsAttention)
		require.Zero(t, got.EffectiveChecks)
		require.Equal(t, i+1, got.RequestFailures)
	}
	j, err := s.Journal(ctx, job.ID)
	require.NoError(t, err)
	require.Equal(t, "auth_required", j.Online.Last.Cause)
	require.Equal(t, "list_parent", j.Online.Last.Phase)
	require.Equal(t, model.StatusAwaitingVerification, j.Job.State)
	for i := range 3 {
		got, err := s.RecordCreateObservation(ctx, op.ID, "not_found", testOnlineFailure("resource_missing_unconfirmed", "list_parent", now), true)
		require.NoError(t, err)
		require.Equal(t, i+1, got.EffectiveChecks)
		require.Equal(t, i == 2, got.NeedsAttention)
	}
}

func TestCreateObservationLegacyCountsAndProtocolPause(t *testing.T) {
	s, _, _ := openTest(t)
	en, job, op := recoveryCreate(t, s)
	// Simulate a persisted pre-policy record instead of treating a new op as legacy.
	op.RecoveryVersion = 0
	require.NoError(t, s.update(ctx, func(tx *bolt.Tx) error { return put(tx, "createops", op.ID, op) }))
	require.NoError(t, s.MarkCreateUnknown(ctx, op.ID))
	_, err := s.RecordCreateCheck(ctx, op.ID, "network_or_protocol", "unknown_result", time.Now())
	require.NoError(t, err)
	for i := range 3 {
		got, err := s.RecordCreateObservation(ctx, op.ID, "protocol_error", testOnlineFailure("protocol_error", "document_read", time.Now()), false)
		require.NoError(t, err)
		require.Equal(t, 1, got.LegacyCheckAttempts)
		require.Zero(t, got.EffectiveChecks)
		require.Equal(t, i == 2, got.NeedsAttention)
	}
	paused, err := s.CreateForJob(ctx, job.ID)
	require.NoError(t, err)
	_, err = s.RecheckCreate(context.Background(), model.CreateRecoveryRequest{JobID: job.ID, Action: "recheck", ExpectedGeneration: en.Generation, ExpectedLocal: en.Local, ExpectedDiagnosticCycle: paused.DiagnosticCycle, ExpectedPolicyVersion: paused.RecoveryVersion})
	require.NoError(t, err)
	got, err := s.CreateForJob(ctx, job.ID)
	require.NoError(t, err)
	require.True(t, got.ObservationOnly)
	require.Zero(t, got.UnclassifiedProbes)
	require.Equal(t, 4, got.CheckAttempts)
}

func TestUnknownCancellationCannotAutoResumeRemainingWrites(t *testing.T) {
	s, _, _ := openTest(t)
	_, job, _ := recoveryCreate(t, s)
	f := testOnlineFailure("canceled", "write_dispatch", time.Now())
	f.CancelOrigin = "unspecified_cancel"
	require.NoError(t, s.RecordJobOnlineFailure(ctx, job.ID, f))
	due, err := s.JobOnlineDue(ctx, job.ID, time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.False(t, due)
	require.NoError(t, s.ClearJobOnlinePause(ctx, job.ID))
	due, err = s.JobOnlineDue(ctx, job.ID, time.Now().Add(time.Hour))
	require.NoError(t, err)
	require.True(t, due)
}

func TestObservationCannotReleaseCancellationOrBusinessPause(t *testing.T) {
	for _, reason := range []string{"canceled", "multiple_matches", "content_mismatch", "location_changed", "identity_changed"} {
		t.Run(reason, func(t *testing.T) {
			s, _, _ := openTest(t)
			en, job, op := recoveryCreate(t, s)
			require.NoError(t, s.MarkCreateUnknown(ctx, op.ID))
			f := testOnlineFailure(reason, "list_parent", time.Now())
			f.CancelOrigin = "unspecified_cancel"
			_, err := s.RecordCreateObservation(ctx, op.ID, reason, f, false)
			require.NoError(t, err)
			if reason != "canceled" {
				_, err = s.RecheckCreate(ctx, model.CreateRecoveryRequest{JobID: job.ID, Action: "recheck", ExpectedGeneration: en.Generation, ExpectedLocal: en.Local})
				require.NoError(t, err)
			}
			require.NoError(t, s.RecordCreationObserved(ctx, op.ID, "candidate", "r1"))
			if reason == "canceled" {
				due, err := s.JobOnlineDue(ctx, job.ID, time.Now().Add(time.Hour))
				require.NoError(t, err)
				require.False(t, due)
			} else {
				got, err := s.CreateForJob(ctx, job.ID)
				require.NoError(t, err)
				require.True(t, got.NeedsAttention)
			}
		})
	}
}

func TestBusinessPauseSurvivesLaterFailureThreshold(t *testing.T) {
	s, _, _ := openTest(t)
	en, job, op := recoveryCreate(t, s)
	require.NoError(t, s.MarkCreateUnknown(ctx, op.ID))
	_, err := s.RecordCreateObservation(ctx, op.ID, "multiple_matches", testOnlineFailure("multiple_candidates", "list_parent", time.Now()), true)
	require.NoError(t, err)
	for range 3 {
		_, err = s.RecheckCreate(ctx, model.CreateRecoveryRequest{JobID: job.ID, Action: "recheck", ExpectedGeneration: en.Generation, ExpectedLocal: en.Local})
		require.NoError(t, err)
		_, err = s.RecordCreateObservation(ctx, op.ID, "not_found", testOnlineFailure("resource_missing_unconfirmed", "list_parent", time.Now()), true)
		require.NoError(t, err)
	}
	_, err = s.RecheckCreate(ctx, model.CreateRecoveryRequest{JobID: job.ID, Action: "recheck", ExpectedGeneration: en.Generation, ExpectedLocal: en.Local})
	require.NoError(t, err)
	require.NoError(t, s.RecordCreationObserved(ctx, op.ID, "candidate", "r1"))
	got, err := s.CreateForJob(ctx, job.ID)
	require.NoError(t, err)
	require.True(t, got.NeedsAttention)
	require.Equal(t, "multiple_matches", got.PausedReason)
}

func TestServiceWaitCannotOverflowLocalJitter(t *testing.T) {
	s, _, _ := openTest(t)
	_, _, op := recoveryCreate(t, s)
	require.NoError(t, s.MarkCreateUnknown(ctx, op.ID))
	now := time.Now()
	f := testOnlineFailure("rate_limited", "list_parent", now)
	f.RetryAfterSeconds = int64((1<<63-1)/int64(time.Second)) + 1
	got, err := s.RecordCreateObservation(ctx, op.ID, "rate_limited", f, false)
	require.NoError(t, err)
	require.True(t, got.NextCheckAt.After(now.Add(200*365*24*time.Hour)))
}
