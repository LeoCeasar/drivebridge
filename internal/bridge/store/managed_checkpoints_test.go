package store

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/stretchr/testify/require"
	bolt "go.etcd.io/bbolt"
)

func retainCheckpoint(t *testing.T, s *Store, e model.Entry) string {
	t.Helper()
	key := uuid.NewString()
	require.NoError(t, s.RetainManagedEntry(ctx, key, e.ID, e.Visible))
	return key
}

func commitManagedNamespace(t *testing.T, s *Store, kind, src, dst string, inputs ...model.ManagedCheckpoint) model.NamespaceOp {
	t.Helper()
	op, err := s.PrepareNamespace(ctx, model.NamespaceOp{Kind: kind, Src: src, Dst: dst})
	require.NoError(t, err)
	require.NoError(t, s.BindManagedNamespace(ctx, op.ID, inputs))
	_, err = s.SealNamespace(ctx, op.ID)
	require.NoError(t, err)
	_, err = s.CommitNamespace(ctx, op.ID)
	require.NoError(t, err)
	require.NoError(t, s.FinishNamespace(ctx, op.ID))
	return op
}

func TestManagedPendingReplaceKeepsDestinationIdentityAndDefersCloud(t *testing.T) {
	s, _, _ := openTest(t)
	dst := bind(t, s, entry(t, s, "final"), "stable-token")
	dst, _ = stage(t, s, dst, "previous destination")
	src := entry(t, s, "temp")
	key := retainCheckpoint(t, s, src)
	op := commitManagedNamespace(t, s, "replace", src.Remote, dst.Remote, model.ManagedCheckpoint{Journal: key, Sequence: 1, Size: 16, Remote: src.Remote})
	current, err := s.EntryByRemote(ctx, "final")
	require.NoError(t, err)
	require.Equal(t, dst.ID, current.ID)
	require.Equal(t, "stable-token", current.Token)
	require.Equal(t, dst.Local, current.Local, "WAL-only replacement must retain the previous immutable body")
	jobs, err := s.Pending(ctx)
	require.NoError(t, err)
	for _, j := range jobs {
		require.NotEmpty(t, j.Local, "namespace commit must never queue an empty body")
	}
	actions, err := s.NamespaceActions(ctx)
	require.NoError(t, err)
	require.Empty(t, actions)
	require.ErrorIs(t, s.BeginNamespaceDispatch(ctx, op.ID, "cleanup_source"), ErrBusy)
	b, err := s.PutBlob(ctx, strings.NewReader("replacement body"))
	require.NoError(t, err)
	accepted, err := s.AcceptManagedCheckpoint(ctx, key, 1, b, false)
	require.NoError(t, err)
	require.Equal(t, dst.ID, accepted.ID)
	require.Equal(t, "stable-token", accepted.Token)
	require.Equal(t, b, accepted.Local)
	require.NoError(t, s.ReleaseManagedBase(ctx, key))
	actions, err = s.NamespaceActions(ctx)
	require.NoError(t, err)
	require.Len(t, actions, 1)
}

func TestManagedCheckpointHashFinishingAfterRenameUsesCurrentRoute(t *testing.T) {
	s, _, _ := openTest(t)
	original := entry(t, s, "before")
	key := retainCheckpoint(t, s, original)
	b, err := s.PutBlob(ctx, strings.NewReader("hashed before namespace commit"))
	require.NoError(t, err)
	commitManagedNamespace(t, s, "rename", "before", "after", model.ManagedCheckpoint{Journal: key, Sequence: 1, Size: 29, Remote: "before"})
	accepted, err := s.AcceptManagedCheckpoint(ctx, key, 1, b, false)
	require.NoError(t, err)
	require.Equal(t, original.ID, accepted.ID)
	require.Equal(t, "after", accepted.Remote)
	require.Equal(t, b, accepted.Local)
	_, err = s.EntryByRemote(ctx, "before")
	require.ErrorIs(t, err, ErrNotFound)
}

func TestManagedDeletedAndDisplacedCheckpointsAreHistoryOnly(t *testing.T) {
	for _, operation := range []string{"delete", "replace"} {
		t.Run(operation, func(t *testing.T) {
			s, _, _ := openTest(t)
			old := bind(t, s, entry(t, s, "final"), "stable-token")
			old, _ = stage(t, s, old, "old immutable bytes")
			oldKey := retainCheckpoint(t, s, old)
			if operation == "delete" {
				commitManagedNamespace(t, s, "delete", "final", "", model.ManagedCheckpoint{Journal: oldKey, Sequence: 1, Size: 8})
			} else {
				src := entry(t, s, "temp")
				src, _ = stage(t, s, src, "new destination")
				commitManagedNamespace(t, s, "replace", "temp", "final", model.ManagedCheckpoint{Journal: oldKey, Sequence: 1, Size: 8})
			}
			before, err := s.Entry(ctx, old.ID)
			require.NoError(t, err)
			b, err := s.PutBlob(ctx, strings.NewReader("late old checkpoint"))
			require.NoError(t, err)
			_, err = s.AcceptManagedCheckpoint(ctx, oldKey, 1, b, false)
			require.NoError(t, err)
			after, err := s.Entry(ctx, old.ID)
			require.NoError(t, err)
			require.Equal(t, before, after, "history acceptance cannot overwrite or resurrect the live entry")
			events, err := s.HistoryEvents(ctx, 0, 500)
			require.NoError(t, err)
			found := false
			for _, ev := range events {
				if ev.Entry.ID == old.ID && ev.Bodies["local"] == b {
					found = true
				}
			}
			require.True(t, found, "displaced/deleted saved bytes must reach archive history")
		})
	}
}

func TestManagedCheckpointReceiptsSurviveRestartAndIgnoreOlderSequence(t *testing.T) {
	s, dir, scope := openTest(t)
	e := entry(t, s, "file")
	key := retainCheckpoint(t, s, e)
	newest, err := s.PutBlob(ctx, strings.NewReader("newest"))
	require.NoError(t, err)
	accepted, err := s.AcceptManagedCheckpoint(ctx, key, 2, newest, false)
	require.NoError(t, err)
	require.NoError(t, s.Close())
	s, err = Open(dir)
	require.NoError(t, err)
	defer s.Close()
	require.NoError(t, s.BindScope(ctx, scope))
	duplicate, err := s.AcceptManagedCheckpoint(ctx, key, 2, newest, false)
	require.NoError(t, err)
	require.Equal(t, accepted.Generation, duplicate.Generation)
	older, err := s.PutBlob(ctx, strings.NewReader("older"))
	require.NoError(t, err)
	_, err = s.AcceptManagedCheckpoint(ctx, key, 1, older, false)
	require.NoError(t, err)
	current, err := s.Entry(ctx, e.ID)
	require.NoError(t, err)
	require.Equal(t, accepted, current)
	_, err = s.AcceptManagedCheckpoint(ctx, key, 2, older, false)
	require.ErrorIs(t, err, model.ErrIdentityChanged)
	events, err := s.HistoryEvents(ctx, 0, 500)
	require.NoError(t, err)
	found := false
	for _, ev := range events {
		if ev.Entry.ID == e.ID && ev.Bodies["local"] == older {
			found = true
		}
	}
	require.True(t, found, "an out-of-order checkpoint still preserves its saved version in history")
	count := len(events)
	_, err = s.AcceptManagedCheckpoint(ctx, key, 1, older, false)
	require.NoError(t, err)
	events, err = s.HistoryEvents(ctx, 0, 500)
	require.NoError(t, err)
	require.Len(t, events, count, "a duplicate archive callback must not create another version")
	_, err = s.AcceptManagedCheckpoint(ctx, key, 1, newest, false)
	require.ErrorIs(t, err, model.ErrIdentityChanged)
}

func TestManagedLocalNamespacePreservesActivePredecessorAndDefersAction(t *testing.T) {
	s, _, _ := openTest(t)
	e := bind(t, s, entry(t, s, "before"), "token")
	e, predecessor := stage(t, s, e, "upload in progress")
	_, _, err := s.Prepare(ctx, predecessor.ID)
	require.NoError(t, err)
	op := commitManagedNamespace(t, s, "rename", "before", "after")
	actions, err := s.NamespaceActions(ctx)
	require.NoError(t, err)
	require.Empty(t, actions)
	require.ErrorIs(t, s.BeginNamespaceDispatch(ctx, op.ID, "rename"), ErrBusy)
	require.NoError(t, s.view(ctx, func(tx *bolt.Tx) error {
		r, err := jobTx(tx, predecessor.ID)
		require.NoError(t, err)
		require.False(t, r.Superseded)
		require.Equal(t, model.StatusMerging, r.Job.State)
		return nil
	}))
}

func TestManagedNamespacePendingSizeAndRecoveryReceipts(t *testing.T) {
	s, _, _ := openTest(t)
	src := entry(t, s, "before")
	key := retainCheckpoint(t, s, src)
	cp := model.ManagedCheckpoint{Journal: key, Sequence: 7, Size: 4321, ModTime: time.Unix(123, 0).UTC(), Remote: "before", CacheInfo: json.RawMessage(`{"journal":"recover"}`)}
	op := commitManagedNamespace(t, s, "rename", "before", "after", cp)
	info, err := s.ManagedNamespaceCheckpoints(ctx, op.ID)
	require.NoError(t, err)
	require.Equal(t, []model.ManagedCheckpoint{cp}, info)
	size, modTime, pending, err := s.PendingManagedSize(ctx, src.ID)
	require.NoError(t, err)
	require.True(t, pending)
	require.EqualValues(t, 4321, size)
	require.Equal(t, cp.ModTime, modTime)
	b, err := s.PutBlob(ctx, strings.NewReader("accepted"))
	require.NoError(t, err)
	_, err = s.AcceptManagedCheckpoint(ctx, key, 7, b, false)
	require.NoError(t, err)
	_, _, pending, err = s.PendingManagedSize(ctx, src.ID)
	require.NoError(t, err)
	require.False(t, pending)
}

func TestManagedOlderJournalCannotOverwriteNewerJournal(t *testing.T) {
	s, _, _ := openTest(t)
	e := entry(t, s, "file")
	oldKey := retainCheckpoint(t, s, e)
	newKey := retainCheckpoint(t, s, e)
	newer, err := s.PutBlob(ctx, strings.NewReader("new journal"))
	require.NoError(t, err)
	accepted, err := s.AcceptManagedCheckpoint(ctx, newKey, 1, newer, false)
	require.NoError(t, err)
	older, err := s.PutBlob(ctx, strings.NewReader("older journal finishes late"))
	require.NoError(t, err)
	_, err = s.AcceptManagedCheckpoint(ctx, oldKey, 9, older, false)
	require.NoError(t, err)
	current, err := s.Entry(ctx, e.ID)
	require.NoError(t, err)
	require.Equal(t, accepted, current)
}

func TestManagedSafeSaveBackupJournalCannotOverwriteRestoredOriginal(t *testing.T) {
	s, _, _ := openTest(t)
	original := bind(t, s, entry(t, s, "document.txt"), "stable-token")
	snapshot := fileSnapshot(t, s, original.Token, "baseline")
	var err error
	original, err = s.AdoptFileSnapshot(ctx, original.ID, original.Generation, snapshot.ID)
	require.NoError(t, err)
	backupKey := retainCheckpoint(t, s, original)
	backup, err := s.PrepareEditorBackup(ctx, original.Remote, "document.txt~")
	require.NoError(t, err)
	require.NoError(t, s.BindManagedNamespace(ctx, backup.ID, []model.ManagedCheckpoint{{Journal: backupKey, Sequence: 1, Size: 10}}))
	_, err = s.SealNamespace(ctx, backup.ID)
	require.NoError(t, err)
	shadow, err := s.CommitNamespace(ctx, backup.ID)
	require.NoError(t, err)
	require.NoError(t, s.FinishNamespace(ctx, backup.ID))
	restored, resumed, err := s.ResumeSafeSave(ctx, "document.txt", model.KindFile)
	require.NoError(t, err)
	require.True(t, resumed)
	newKey := retainCheckpoint(t, s, restored)
	fresh, err := s.PutBlob(ctx, strings.NewReader("new editor replacement"))
	require.NoError(t, err)
	current, err := s.AcceptManagedCheckpoint(ctx, newKey, 1, fresh, false)
	require.NoError(t, err)
	oldBody, err := s.PutBlob(ctx, strings.NewReader("old backup checkpoint"))
	require.NoError(t, err)
	oldAccepted, err := s.AcceptManagedCheckpoint(ctx, backupKey, 1, oldBody, false)
	require.NoError(t, err)
	require.Equal(t, shadow.ID, oldAccepted.ID)
	actual, err := s.Entry(ctx, original.ID)
	require.NoError(t, err)
	require.Equal(t, current, actual)
	events, err := s.HistoryEvents(ctx, 0, 500)
	require.NoError(t, err)
	found := false
	for _, ev := range events {
		if ev.Entry.ID == original.ID && ev.Bodies["local"] == oldBody {
			found = true
		}
	}
	require.True(t, found)
}

func TestManagedNamespaceReadsOnlyAffectedCatalogRows(t *testing.T) {
	s, _, _ := openTest(t)
	original := entry(t, s, "before")
	require.NoError(t, s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("entries")).Put([]byte("unrelated"), []byte("invalid JSON"))
	}))
	op := commitManagedNamespace(t, s, "rename", original.Remote, "after")
	require.NotEmpty(t, op.ID)
	current, err := s.EntryByRemote(ctx, "after")
	require.NoError(t, err)
	require.Equal(t, original.ID, current.ID)
}

func TestManagedDeleteLetsAdmittedPredecessorFinishVerification(t *testing.T) {
	s, _, _ := openTest(t)
	e := bind(t, s, entry(t, s, "file"), "token")
	e, job := stage(t, s, e, "upload")
	_, _, err := s.Prepare(ctx, job.ID)
	require.NoError(t, err)
	commitManagedNamespace(t, s, "delete", e.Remote, "")
	require.NoError(t, s.RecordPlan(ctx, job.ID, nil, e.Local), "admitted predecessor may finish before cloud deletion")
	snapshot := fileSnapshot(t, s, e.Token, "upload")
	_, err = s.Confirm(ctx, job.ID, snapshot)
	require.NoError(t, err)
	actions, err := s.NamespaceActions(ctx)
	require.NoError(t, err)
	require.Len(t, actions, 1)
	metadata, err := s.Metadata(ctx, e.ID)
	require.NoError(t, err)
	require.True(t, metadata.Tombstone)
}

func TestManagedNamespaceLocalityOverridesArchiveCaller(t *testing.T) {
	for _, localOnly := range []bool{false, true} {
		t.Run(fmt.Sprint(localOnly), func(t *testing.T) {
			s, _, _ := openTest(t)
			e := entry(t, s, "temp")
			key := retainCheckpoint(t, s, e)
			op, err := s.PrepareNamespace(ctx, model.NamespaceOp{Kind: "rename", Src: "temp", Dst: "renamed"})
			require.NoError(t, err)
			require.NoError(t, s.ConfigureNamespaceLocality(ctx, op.ID, map[model.EntryID]bool{e.ID: localOnly}, nil))
			require.NoError(t, s.BindManagedNamespace(ctx, op.ID, []model.ManagedCheckpoint{{Journal: key, Sequence: 1, Size: 4}}))
			_, err = s.SealNamespace(ctx, op.ID)
			require.NoError(t, err)
			_, err = s.CommitNamespace(ctx, op.ID)
			require.NoError(t, err)
			require.NoError(t, s.FinishNamespace(ctx, op.ID))
			b, err := s.PutBlob(ctx, strings.NewReader("body"))
			require.NoError(t, err)
			_, err = s.AcceptManagedCheckpoint(ctx, key, 1, b, !localOnly)
			require.NoError(t, err)
			require.NoError(t, s.view(ctx, func(tx *bolt.Tx) error {
				return each[jobRecord](tx, "jobs", func(r jobRecord) error {
					if r.Job.EntryID == e.ID {
						require.Equal(t, localOnly, r.LocalReceipt)
					}
					return nil
				})
			}))
		})
	}
}

func TestNamespaceQueuesReadOnlyIndexedIntents(t *testing.T) {
	s, _, _ := openTest(t)
	require.NoError(t, s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte("namespace")).Put([]byte("unrelated-finished-history"), []byte("invalid JSON"))
	}))
	pending, err := s.PendingNamespace(ctx)
	require.NoError(t, err)
	require.Empty(t, pending)
	actions, err := s.NamespaceActions(ctx)
	require.NoError(t, err)
	require.Empty(t, actions)
}

func TestManagedOversizeDocumentCanBeReducedWithoutStuckPredecessor(t *testing.T) {
	s, _, _ := openTest(t)
	e, err := s.EnsureEntry(ctx, "document.md", model.KindDocx)
	require.NoError(t, err)
	key := retainCheckpoint(t, s, e)
	large, err := s.PutBlob(ctx, strings.NewReader(strings.Repeat("x", int(LargeSnapshotBytes)+1)))
	require.NoError(t, err)
	blocked, err := s.AcceptManagedCheckpoint(ctx, key, 1, large, false)
	require.NoError(t, err)
	require.Equal(t, model.StatusBlocked, blocked.Status)
	small, err := s.PutBlob(ctx, strings.NewReader("smaller supported document"))
	require.NoError(t, err)
	accepted, err := s.AcceptManagedCheckpoint(ctx, key, 2, small, false)
	require.NoError(t, err)
	require.Equal(t, model.StatusQueued, accepted.Status)
	require.NoError(t, s.ReleaseManagedBase(ctx, key))
	jobs, err := s.Pending(ctx)
	require.NoError(t, err)
	require.Len(t, jobs, 1, "the never-dispatched oversize predecessor must not block the later save")
	_, _, err = s.Prepare(ctx, jobs[0].ID)
	require.NoError(t, err)
}

func TestManagedNamespaceAtomicCommitPreservesIdentityAndRetry(t *testing.T) {
	s, _, _ := openTest(t)
	dst := bind(t, s, entry(t, s, "final"), "stable-token")
	dst, _ = stage(t, s, dst, "baseline")
	src := entry(t, s, "temp")
	key := retainCheckpoint(t, s, src)
	op, err := s.PrepareNamespace(ctx, model.NamespaceOp{Kind: "replace", Src: src.Remote, Dst: dst.Remote})
	require.NoError(t, err)
	inputs := []model.ManagedCheckpoint{{Journal: key, Sequence: 1, Size: 123}}
	current, err := s.CommitManagedNamespace(ctx, op.ID, inputs)
	require.NoError(t, err)
	require.Equal(t, dst.ID, current.ID)
	require.Equal(t, dst.Token, current.Token)
	require.Equal(t, dst.Local, current.Local)
	duplicate, err := s.CommitManagedNamespace(ctx, op.ID, inputs)
	require.NoError(t, err)
	require.Equal(t, current, duplicate)
	inputs[0].Sequence = 2
	_, err = s.CommitManagedNamespace(ctx, op.ID, inputs)
	require.ErrorIs(t, err, model.ErrIdentityChanged)
	require.NoError(t, s.FinishNamespace(ctx, op.ID))
}

func TestManagedNamespaceAtomicCommitRollsBackBindingAndSeal(t *testing.T) {
	s, _, _ := openTest(t)
	src := entry(t, s, "before")
	key := retainCheckpoint(t, s, src)
	op, err := s.PrepareNamespace(ctx, model.NamespaceOp{Kind: "rename", Src: src.Remote, Dst: "after"})
	require.NoError(t, err)
	_, err = s.CommitManagedNamespace(ctx, op.ID, []model.ManagedCheckpoint{{Journal: key, Sequence: 1, Size: 10}, {Journal: uuid.NewString(), Sequence: 1, Size: 10}})
	require.ErrorIs(t, err, ErrIntegrity)
	_, _, pending, err := s.PendingManagedSize(ctx, src.ID)
	require.NoError(t, err)
	require.False(t, pending, "valid first ref must roll back when a later ref is invalid")
	pendingOps, err := s.PendingNamespace(ctx)
	require.NoError(t, err)
	require.Len(t, pendingOps, 1)
	require.Equal(t, "prepared", pendingOps[0].State)
	_, err = s.EntryByRemote(ctx, "after")
	require.ErrorIs(t, err, ErrNotFound)
	require.NoError(t, s.AbortNamespace(ctx, op.ID))

	dst := entry(t, s, "destination")
	op, err = s.PrepareNamespace(ctx, model.NamespaceOp{Kind: "replace", Src: src.Remote, Dst: dst.Remote})
	require.NoError(t, err)
	_, err = s.CommitManagedNamespace(ctx, op.ID, nil)
	require.Error(t, err, "unmaterialized input without a WAL ref must fail")
	pendingOps, err = s.PendingNamespace(ctx)
	require.NoError(t, err)
	require.Len(t, pendingOps, 1)
	require.Equal(t, "prepared", pendingOps[0].State, "failed commit must also roll back seal")
}

func TestPendingManagedSizeReadsOnlyLiveDescriptor(t *testing.T) {
	s, _, _ := openTest(t)
	e := entry(t, s, "before")
	olderKey := retainCheckpoint(t, s, e)
	newerKey := retainCheckpoint(t, s, e)
	body, err := s.PutBlob(ctx, strings.NewReader("first accepted version"))
	require.NoError(t, err)
	_, err = s.AcceptManagedCheckpoint(ctx, newerKey, 1, body, false)
	require.NoError(t, err)
	op, err := s.PrepareNamespace(ctx, model.NamespaceOp{Kind: "rename", Src: "before", Dst: "after"})
	require.NoError(t, err)
	_, err = s.CommitManagedNamespace(ctx, op.ID, []model.ManagedCheckpoint{{Journal: newerKey, Sequence: 2, Size: 123, ModTime: time.Unix(456, 0).UTC()}})
	require.NoError(t, err)
	require.NoError(t, s.FinishNamespace(ctx, op.ID))
	require.NoError(t, s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket([]byte(managedRoutesBucket)).Put([]byte(olderKey), []byte("invalid historical descriptor"))
	}))
	size, modTime, pending, err := s.PendingManagedSize(ctx, e.ID)
	require.NoError(t, err)
	require.True(t, pending)
	require.EqualValues(t, 123, size)
	require.Equal(t, time.Unix(456, 0).UTC(), modTime)
}

func TestPrepareManagedRenameClassifiesAndConfiguresInOneTransaction(t *testing.T) {
	s, _, _ := openTest(t)
	src := entry(t, s, "draft.tmp")
	op, err := s.PrepareManagedRename(ctx, model.NamespaceOp{Kind: "rename", Src: src.Remote, Dst: "document.md"}, false, model.KindDocx, func(remote string, kind model.Kind) bool { return strings.HasSuffix(remote, ".tmp") }, nil)
	require.NoError(t, err)
	require.NoError(t, s.view(ctx, func(tx *bolt.Tx) error {
		var r namespaceRecord
		require.NoError(t, get(tx, "namespace", op.ID, &r))
		require.Equal(t, model.KindDocx, r.ResultKind)
		require.False(t, r.ResultLocalOnly[src.ID])
		require.True(t, r.PromoteLocalOnly[src.ID])
		return nil
	}))
	current, err := s.Entry(ctx, src.ID)
	require.NoError(t, err)
	require.Equal(t, model.KindFile, current.Kind, "configuration must not change kind before commit")
	require.Equal(t, op.ID, current.FrozenBy)
}

func TestPrepareManagedRenamePreservesAdmittedProtocolInsteadOfRejectingName(t *testing.T) {
	for _, evidence := range []string{"active upload", "create journal"} {
		t.Run(evidence, func(t *testing.T) {
			s, _, _ := openTest(t)
			src, job := stage(t, s, entry(t, s, "before.tmp"), "active upload")
			if evidence == "active upload" {
				_, _, err := s.Prepare(ctx, job.ID)
				require.NoError(t, err)
			} else {
				_, _, err := s.Prepare(ctx, job.ID)
				require.NoError(t, err)
				_, err = s.PrepareCreate(ctx, job.ID, "root-token", "before.tmp")
				require.NoError(t, err)
				require.NoError(t, s.PauseJob(ctx, job.ID, model.StatusPaused, "permission_denied"))
			}
			classifiedKind := model.Kind("")
			op, err := s.PrepareManagedRename(ctx, model.NamespaceOp{Kind: "rename", Src: src.Remote, Dst: "after.md"}, false, model.KindDocx, func(remote string, kind model.Kind) bool {
				if remote == "after.md" {
					classifiedKind = kind
				}
				return false
			}, nil)
			require.NoError(t, err, "admitted background work must not reject the local name change")
			require.Equal(t, model.KindFile, classifiedKind)
			renamed, err := s.CommitManagedNamespace(ctx, op.ID, nil)
			require.NoError(t, err)
			require.Equal(t, "after.md", renamed.Remote)
			require.Equal(t, model.KindFile, renamed.Kind, "an existing file protocol must not turn into a document upload")
			require.NoError(t, s.FinishNamespace(ctx, op.ID))
			actions, err := s.NamespaceActions(ctx)
			require.NoError(t, err)
			require.Empty(t, actions, "namespace cloud action must wait for admitted create/upload identity")
			require.ErrorIs(t, s.AcknowledgeNamespaceAction(ctx, op.ID), ErrBusy, "stale action lists cannot retire an admitted identity's name change")
			require.NoError(t, s.view(ctx, func(tx *bolt.Tx) error {
				r, err := jobTx(tx, job.ID)
				require.NoError(t, err)
				require.False(t, r.Superseded)
				return nil
			}))
		})
	}
}

func TestPrepareManagedRenameResolvesOriginalSiblingInsideTransaction(t *testing.T) {
	s, _, _ := openTest(t)
	original := entry(t, s, "original.txt")
	shadow := entry(t, s, "backup.tmp")
	require.NoError(t, s.update(ctx, func(tx *bolt.Tx) error {
		var m Metadata
		if err := get(tx, "metadata", string(shadow.ID), &m); err != nil {
			return err
		}
		m.SafeSaveOriginal = original.ID
		return put(tx, "metadata", string(shadow.ID), m)
	}))
	op, err := s.PrepareManagedRename(ctx, model.NamespaceOp{Kind: "rename", Src: shadow.Remote, Dst: "original.txt.sb-copy"}, false, model.KindFile, func(string, model.Kind) bool { return false }, func(src, dst string) bool { return src == "original.txt" && dst == "original.txt.sb-copy" })
	require.NoError(t, err)
	require.NoError(t, s.view(ctx, func(tx *bolt.Tx) error {
		var r namespaceRecord
		require.NoError(t, get(tx, "namespace", op.ID, &r))
		require.True(t, r.ResultLocalOnly[shadow.ID])
		return nil
	}))
}

func TestManagedRepeatedEditorBackupCompletesRestoredWalCycle(t *testing.T) {
	s, _, _ := openTest(t)
	original := bind(t, s, entry(t, s, "document.txt"), "stable-token")
	snapshot := fileSnapshot(t, s, original.Token, "immutable baseline")
	var err error
	original, err = s.AdoptFileSnapshot(ctx, original.ID, original.Generation, snapshot.ID)
	require.NoError(t, err)
	first, err := s.PrepareManagedRename(ctx, model.NamespaceOp{Kind: "rename", Src: "document.txt", Dst: "document.txt~"}, true, model.KindFile, nil, nil)
	require.NoError(t, err)
	_, err = s.CommitManagedNamespace(ctx, first.ID, nil)
	require.NoError(t, err)
	require.NoError(t, s.FinishNamespace(ctx, first.ID))
	restored, resumed, err := s.ResumeSafeSave(ctx, original.Remote, original.Kind)
	require.NoError(t, err)
	require.True(t, resumed)
	key := retainCheckpoint(t, s, restored)
	// No Stage/PutBlob: only the new WAL owns the user's accepted replacement.
	second, err := s.PrepareManagedRename(ctx, model.NamespaceOp{Kind: "rename", Src: "document.txt", Dst: "document.txt~"}, true, model.KindFile, nil, nil)
	require.NoError(t, err, "the next editor cycle cannot wait for immutable archive acceptance")
	oldWork, err := s.NamespaceJournal(ctx, first.ID)
	require.NoError(t, err)
	require.True(t, oldWork.SafeSaveCompleted)
	shadow, err := s.CommitManagedNamespace(ctx, second.ID, []model.ManagedCheckpoint{{Journal: key, Sequence: 1, Size: 123}})
	require.NoError(t, err)
	require.NoError(t, s.FinishNamespace(ctx, second.ID))
	route, err := s.ManagedCheckpointEntry(ctx, key)
	require.NoError(t, err)
	require.Equal(t, shadow.ID, route.ID)
	require.NotEqual(t, original.ID, route.ID)
	stable, err := s.EntryByToken(ctx, original.Token)
	require.NoError(t, err)
	require.Equal(t, original.ID, stable.ID)
	metadata, err := s.Metadata(ctx, original.ID)
	require.NoError(t, err)
	require.True(t, metadata.SafeSaveParked)
	require.Equal(t, second.ID, metadata.SafeSavePending)
	require.NoError(t, s.Validate(ctx))
}

func TestManagedParkedEditorOriginalCannotCompleteAnotherBackup(t *testing.T) {
	s, _, _ := openTest(t)
	original := bind(t, s, entry(t, s, "document.txt"), "stable-token")
	snapshot := fileSnapshot(t, s, original.Token, "baseline")
	var err error
	original, err = s.AdoptFileSnapshot(ctx, original.ID, original.Generation, snapshot.ID)
	require.NoError(t, err)
	key := retainCheckpoint(t, s, original)
	op, err := s.PrepareEditorBackup(ctx, original.Remote, "document.txt~")
	require.NoError(t, err)
	_, err = s.SealNamespace(ctx, op.ID)
	require.NoError(t, err)
	_, err = s.CommitNamespace(ctx, op.ID)
	require.NoError(t, err)
	require.NoError(t, s.FinishNamespace(ctx, op.ID))
	require.NoError(t, s.view(ctx, func(tx *bolt.Tx) error {
		parked, err := entryTx(tx, original.ID)
		require.NoError(t, err)
		allowed, err := editorBackupSource(tx, parked, "")
		require.NoError(t, err)
		require.False(t, allowed)
		return nil
	}))
	work, err := s.NamespaceJournal(ctx, op.ID)
	require.NoError(t, err)
	require.False(t, work.SafeSaveCompleted)
	require.NoError(t, s.ReleaseManagedBase(ctx, key))
}
