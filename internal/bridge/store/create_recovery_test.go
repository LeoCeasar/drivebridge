package store

import (
	"errors"
	"testing"
	"time"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/stretchr/testify/require"
)

func recoveryCreate(t *testing.T, s *Store) (model.Entry, model.Job, model.CreateOp) {
	t.Helper()
	en, job := stage(t, s, entry(t, s, "recover.txt"), "original")
	_, _, err := s.Prepare(ctx, job.ID)
	require.NoError(t, err)
	require.NoError(t, s.RecordPlan(ctx, job.ID, nil, job.Local))
	op, err := s.PrepareCreate(ctx, job.ID, "root-resource", "recover.txt")
	require.NoError(t, err)
	return en, job, op
}

// Losing the first failure/candidate, or resetting the check budget during a
// manual recheck, would make an uncertain mutation either unsafe or endless.
func TestCreateRecoveryFiniteChecksAndCandidate(t *testing.T) {
	s, _, _ := openTest(t)
	en, job, op := recoveryCreate(t, s)
	now := time.Now().UTC()
	require.NoError(t, s.MarkCreateUncertain(ctx, op.ID, "partial-token", "r1", "request_timeout", now))
	require.NoError(t, s.MarkCreateUncertain(ctx, op.ID, "", "", "permission_denied", now.Add(time.Minute)))
	require.ErrorIs(t, s.MarkCreateUncertain(ctx, op.ID, "other", "r2", "permission_denied", now), model.ErrIdentityChanged)
	for i := 1; i <= 3; i++ {
		got, err := s.RecordCreateCheck(ctx, op.ID, "network_or_protocol", "network_or_protocol", now.Add(time.Duration(i)*time.Minute))
		require.NoError(t, err)
		require.Equal(t, i, got.CheckAttempts)
		require.Equal(t, "request_timeout", got.FailureCode)
		require.Equal(t, "partial-token", got.CandidateToken)
		require.Equal(t, "r1", got.CandidateRevision)
		require.Equal(t, i == 3, got.NeedsAttention)
	}
	journal, err := s.Journal(ctx, job.ID)
	require.NoError(t, err)
	require.Equal(t, model.StatusBlocked, journal.Job.State)
	require.Equal(t, "creation_needs_attention", journal.ErrorCode)
	require.ErrorIs(t, s.ResumeJob(ctx, job.ID), ErrInvalidState)
	req := model.CreateRecoveryRequest{JobID: job.ID, Action: "recheck", ExpectedGeneration: en.Generation, ExpectedLocal: en.Local}
	_, err = s.RecheckCreate(ctx, req)
	require.NoError(t, err)
	got, err := s.CreateForJob(ctx, job.ID)
	require.NoError(t, err)
	require.False(t, got.NeedsAttention)
	require.Equal(t, 3, got.CheckAttempts)
	require.Equal(t, "unknown", got.State)
	got, err = s.RecordCreateCheck(ctx, op.ID, "content_mismatch", "unknown_result", now.Add(4*time.Minute))
	require.NoError(t, err)
	require.True(t, got.NeedsAttention)
	req.Action = "recreate"
	req.ConfirmRecreate = true
	_, _, err = s.RecreateCreate(ctx, req)
	require.Error(t, err)
}

// Legacy journals recorded no initial failure time or class. Their staged time
// must end the budget immediately without inventing the initiating failure.
func TestCreateRecoveryLegacyTimeout(t *testing.T) {
	s, _, _ := openTest(t)
	_, job, op := recoveryCreate(t, s)
	require.NoError(t, s.MarkCreateUnknown(ctx, op.ID))
	journal, err := s.Journal(ctx, job.ID)
	require.NoError(t, err)
	got, err := s.RecordCreateCheck(ctx, op.ID, "not_found", "permission_denied", journal.StagedAt.Add(16*time.Minute))
	require.NoError(t, err)
	require.True(t, got.NeedsAttention)
	require.Equal(t, 1, got.CheckAttempts)
	require.Equal(t, "unknown_result", got.FailureCode)
	require.Equal(t, journal.StagedAt, got.FirstUncertainAt)
	require.Equal(t, "not_found", got.CheckReason)
}

// Recreating the old input or failing to retire its active journal would lose
// a newer save or prevent the explicitly authorized replacement from running.
func TestCreateRecoveryRecreateLatestAtomically(t *testing.T) {
	s, dir, scope := openTest(t)
	en, old, op := recoveryCreate(t, s)
	now := time.Now().UTC()
	require.NoError(t, s.MarkCreateUncertain(ctx, op.ID, "", "", "unknown_result", now.Add(-16*time.Minute)))
	_, err := s.RecordCreateCheck(ctx, op.ID, "not_found", "unknown_result", now)
	require.NoError(t, err)
	latest, unsent := stage(t, s, en, "latest version")
	req := model.CreateRecoveryRequest{JobID: old.ID, Action: "recreate", ExpectedGeneration: latest.Generation, ExpectedLocal: latest.Local, ConfirmRecreate: true}
	stale := req
	stale.ExpectedGeneration--
	_, _, err = s.RecreateCreate(ctx, stale)
	require.ErrorIs(t, err, model.ErrGenerationChanged)
	stale = req
	stale.ExpectedLocal = old.Local
	_, _, err = s.RecreateCreate(ctx, stale)
	require.ErrorIs(t, err, model.ErrVersionChanged)
	stale = req
	stale.ConfirmRecreate = false
	_, _, err = s.RecreateCreate(ctx, stale)
	require.Error(t, err)
	replacement, newjob, err := s.RecreateCreate(ctx, req)
	require.NoError(t, err)
	require.Equal(t, latest.Generation+1, replacement.Generation)
	require.Equal(t, latest.Local, newjob.Local)
	require.Equal(t, "latest version", read(t, s, newjob.Local))
	require.Equal(t, model.StatusQueued, newjob.State)
	require.Empty(t, replacement.Desired)
	archived, err := s.Job(ctx, old.ID)
	require.NoError(t, err)
	require.Equal(t, model.StatusBlocked, archived.State)
	got, err := s.CreateForJob(ctx, old.ID)
	require.NoError(t, err)
	require.Equal(t, "recreated", got.State)
	require.Equal(t, newjob.ID, got.ReplacedBy)
	pending, err := s.Pending(ctx)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, newjob.ID, pending[0].ID)
	require.NotEqual(t, unsent.ID, pending[0].ID)
	creates, err := s.PendingCreates(ctx)
	require.NoError(t, err)
	require.Empty(t, creates)
	require.Equal(t, "original", read(t, s, old.Local))
	require.NoError(t, s.Validate(ctx))
	require.NoError(t, s.Close())
	s, err = Open(dir)
	require.NoError(t, err)
	defer s.Close()
	require.NoError(t, s.BindScope(ctx, scope))
	got, err = s.CreateForJob(ctx, old.ID)
	require.NoError(t, err)
	require.Equal(t, "recreated", got.State)
	require.NoError(t, s.Validate(ctx))
	_, _, err = s.Prepare(ctx, old.ID)
	require.True(t, errors.Is(err, ErrNotFound))
	_, _, err = s.Prepare(ctx, newjob.ID)
	require.NoError(t, err)
}

func TestCreateRecoveryVerifiedBindingClearsAttention(t *testing.T) {
	s, _, _ := openTest(t)
	_, job, op := recoveryCreate(t, s)
	now := time.Now().UTC()
	require.NoError(t, s.MarkCreateUncertain(ctx, op.ID, "candidate", "rev", "request_timeout", now))
	_, err := s.RecordCreateCheck(ctx, op.ID, "content_mismatch", "unknown_result", now.Add(16*time.Minute))
	require.NoError(t, err)
	_, err = s.BindCreated(ctx, op.ID, "different", "rev")
	require.ErrorIs(t, err, model.ErrIdentityChanged)
	got, err := s.BindCreated(ctx, op.ID, "candidate", "rev")
	require.NoError(t, err)
	require.False(t, got.NeedsAttention)
	require.Empty(t, got.CheckReason)
	require.Equal(t, "request_timeout", got.FailureCode)
	journal, err := s.Journal(ctx, job.ID)
	require.NoError(t, err)
	require.Empty(t, journal.ErrorCode)
	require.Equal(t, model.StatusAwaitingVerification, journal.Job.State)
}

func TestCreateRecoveryNewFailureBudgetStartsAtDispatch(t *testing.T) {
	s, _, _ := openTest(t)
	_, _, op := recoveryCreate(t, s)
	now := time.Now().UTC().Add(24 * time.Hour)
	require.NoError(t, s.MarkCreateUncertain(ctx, op.ID, "", "", "request_timeout", now))
	got, err := s.RecordCreateCheck(ctx, op.ID, "not_found", "unknown_result", now.Add(time.Minute))
	require.NoError(t, err)
	require.False(t, got.NeedsAttention)
	require.Equal(t, now, got.FirstUncertainAt)
}

func TestCreateRecoveryRenameFailureHasItsOwnInitialCause(t *testing.T) {
	s, _, _ := openTest(t)
	_, job, op := recoveryCreate(t, s)
	_, err := s.BindCreated(ctx, op.ID, "created-token", "rev")
	require.NoError(t, err)
	require.NoError(t, s.MarkCreateRenaming(ctx, op.ID))
	now := time.Now().UTC().Add(24 * time.Hour)
	require.NoError(t, s.MarkCreateUncertain(ctx, op.ID, "", "", "permission_denied", now))
	got, err := s.CreateForJob(ctx, job.ID)
	require.NoError(t, err)
	require.Equal(t, "renaming", got.State)
	require.Equal(t, "permission_denied", got.FailureCode)
	require.Equal(t, now, got.FirstUncertainAt)
}

func TestCreateRecoveryRejectsConflictingLatestSave(t *testing.T) {
	s, _, _ := openTest(t)
	en, job, op := recoveryCreate(t, s)
	now := time.Now().UTC()
	require.NoError(t, s.MarkCreateUncertain(ctx, op.ID, "", "", "unknown_result", now))
	_, err := s.RecordCreateCheck(ctx, op.ID, "not_found", "unknown_result", now.Add(16*time.Minute))
	require.NoError(t, err)
	latest, _ := stage(t, s, en, "latest")
	require.NoError(t, s.SaveConflict(ctx, model.Conflict{EntryID: en.ID, Generation: latest.Generation, Local: latest.Local, Reason: "overlapping_edits"}))
	req := model.CreateRecoveryRequest{JobID: job.ID, Action: "recreate", ExpectedGeneration: latest.Generation, ExpectedLocal: latest.Local, ConfirmRecreate: true}
	_, _, err = s.RecreateCreate(ctx, req)
	require.ErrorIs(t, err, ErrBusy)
	got, err := s.CreateForJob(ctx, job.ID)
	require.NoError(t, err)
	require.Equal(t, "unknown", got.State)
	require.Empty(t, got.ReplacedBy)
	current, err := s.Entry(ctx, en.ID)
	require.NoError(t, err)
	require.Equal(t, latest.Generation, current.Generation)
}

func TestCreateRecoveryConfirmedRenameClearsFailure(t *testing.T) {
	s, _, _ := openTest(t)
	en, job, op := recoveryCreate(t, s)
	_, err := s.BindCreated(ctx, op.ID, "created-token", "rev")
	require.NoError(t, err)
	require.NoError(t, s.MarkCreateRenaming(ctx, op.ID))
	now := time.Now().UTC()
	require.NoError(t, s.MarkCreateUncertain(ctx, op.ID, "", "", "request_timeout", now))
	_, err = s.RecordCreateCheck(ctx, op.ID, "rename_unconfirmed", "unknown_result", now.Add(16*time.Minute))
	require.NoError(t, err)
	_, err = s.RecheckCreate(ctx, model.CreateRecoveryRequest{JobID: job.ID, Action: "recheck", ExpectedGeneration: en.Generation, ExpectedLocal: en.Local})
	require.NoError(t, err)
	require.NoError(t, s.FinishCreate(ctx, op.ID))
	journal, err := s.Journal(ctx, job.ID)
	require.NoError(t, err)
	require.Empty(t, journal.ErrorCode)
	got, err := s.CreateForJob(ctx, job.ID)
	require.NoError(t, err)
	require.Empty(t, got.CheckReason)
	require.False(t, got.NeedsAttention)
}

func TestCreateRecoveryIdempotentBindingClearsFailureWithoutResettingRename(t *testing.T) {
	s, _, _ := openTest(t)
	en, job, op := recoveryCreate(t, s)
	_, err := s.BindCreated(ctx, op.ID, "created-token", "rev")
	require.NoError(t, err)
	require.NoError(t, s.MarkCreateRenaming(ctx, op.ID))
	now := time.Now().UTC()
	require.NoError(t, s.MarkCreateUncertain(ctx, op.ID, "", "", "request_timeout", now))
	_, err = s.RecordCreateCheck(ctx, op.ID, "rename_unconfirmed", "unknown_result", now.Add(16*time.Minute))
	require.NoError(t, err)
	_, err = s.RecheckCreate(ctx, model.CreateRecoveryRequest{JobID: job.ID, Action: "recheck", ExpectedGeneration: en.Generation, ExpectedLocal: en.Local})
	require.NoError(t, err)
	got, err := s.BindCreated(ctx, op.ID, "created-token", "rev")
	require.NoError(t, err)
	require.Equal(t, "renaming", got.State)
	require.Empty(t, got.CheckReason)
	journal, err := s.Journal(ctx, job.ID)
	require.NoError(t, err)
	require.Empty(t, journal.ErrorCode)
}

func recoveryReady(t *testing.T, s *Store) (model.Entry, model.Job, model.CreateOp) {
	t.Helper()
	en, job, op := recoveryCreate(t, s)
	now := time.Now().UTC()
	require.NoError(t, s.MarkCreateUncertain(ctx, op.ID, "", "", "unknown_result", now))
	_, err := s.RecordCreateCheck(ctx, op.ID, "not_found", "unknown_result", now.Add(16*time.Minute))
	require.NoError(t, err)
	return en, job, op
}

func renameRecoveryEntry(t *testing.T, s *Store, src, dst string) model.Entry {
	t.Helper()
	ns, err := s.PrepareNamespace(ctx, model.NamespaceOp{Kind: "rename", Src: src, Dst: dst})
	require.NoError(t, err)
	_, err = s.SealNamespace(ctx, ns.ID)
	require.NoError(t, err)
	en, err := s.CommitNamespace(ctx, ns.ID)
	require.NoError(t, err)
	require.NoError(t, s.FinishNamespace(ctx, ns.ID))
	return en
}

func TestCreateRecoveryRecreateRejectsPathDriftWithoutContentChange(t *testing.T) {
	s, _, _ := openTest(t)
	before, job, _ := recoveryReady(t, s)
	after := renameRecoveryEntry(t, s, before.Remote, "renamed.txt")
	require.Equal(t, before.Generation, after.Generation)
	require.Equal(t, before.Local, after.Local)
	req := model.CreateRecoveryRequest{JobID: job.ID, Action: "recreate", ExpectedGeneration: before.Generation, ExpectedLocal: before.Local, ConfirmRecreate: true, ExpectedRemote: before.Remote, ExpectedParentToken: "root-resource"}
	_, _, err := s.RecreateCreate(ctx, req)
	require.ErrorIs(t, err, model.ErrVersionChanged)
	current, err := s.Entry(ctx, before.ID)
	require.NoError(t, err)
	require.Equal(t, before.Generation, current.Generation)
}

func TestCreateRecoveryRecreateChecksCurrentParentToken(t *testing.T) {
	for _, nested := range []bool{false, true} {
		t.Run(map[bool]string{false: "root", true: "nested"}[nested], func(t *testing.T) {
			s, _, _ := openTest(t)
			en, job, _ := recoveryReady(t, s)
			if nested {
				_, err := s.ObserveEntry(ctx, Observation{Remote: "parent", Kind: model.KindFolder, Token: "current-parent"})
				require.NoError(t, err)
				en = renameRecoveryEntry(t, s, en.Remote, "parent/recover.txt")
			}
			req := model.CreateRecoveryRequest{JobID: job.ID, Action: "recreate", ExpectedGeneration: en.Generation, ExpectedLocal: en.Local, ConfirmRecreate: true, ExpectedRemote: en.Remote, ExpectedParentToken: "stale-parent"}
			_, _, err := s.RecreateCreate(ctx, req)
			require.ErrorIs(t, err, model.ErrVersionChanged)
			if nested {
				req.ExpectedParentToken = "current-parent"
			} else {
				req.ExpectedParentToken = "root-resource"
			}
			_, _, err = s.RecreateCreate(ctx, req)
			require.NoError(t, err)
		})
	}
}

func TestCreateRecoveryRecordsMovedCandidateReason(t *testing.T) {
	s, _, _ := openTest(t)
	_, _, op := recoveryCreate(t, s)
	now := time.Now().UTC()
	require.NoError(t, s.MarkCreateUncertain(ctx, op.ID, "candidate", "", "request_timeout", now))
	got, err := s.RecordCreateCheck(ctx, op.ID, "location_changed", "unknown_result", now.Add(time.Minute))
	require.NoError(t, err)
	require.Equal(t, "location_changed", got.CheckReason)
}
