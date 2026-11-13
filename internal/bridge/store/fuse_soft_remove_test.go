package store

import (
	"strings"
	"testing"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/stretchr/testify/require"
)

const testFuseHidden = ".fuse_hidden0000006900000002"

func TestFuseSoftRemoveReplacementKeepsIdentityAndOldJournalHistory(t *testing.T) {
	s, _, _ := openTest(t)
	original := bind(t, s, entry(t, s, "document.md"), "stable-token")
	original, _ = stage(t, s, original, "original body")
	oldKey := retainCheckpoint(t, s, original)
	hiddenOp := commitManagedNamespace(t, s, "rename", original.Remote, testFuseHidden)
	actions, err := s.NamespaceActions(ctx)
	require.NoError(t, err)
	require.Empty(t, actions, "libfuse's temporary name must never become a cloud rename")
	require.Error(t, s.BeginNamespaceDispatch(ctx, hiddenOp.ID, "rename"))
	source := entry(t, s, "replacement.tmp")
	newKey := retainCheckpoint(t, s, source)
	replacement := commitManagedNamespace(t, s, "rename", source.Remote, original.Remote, model.ManagedCheckpoint{Journal: newKey, Sequence: 1, Size: 11})
	current, err := s.EntryByRemote(ctx, original.Remote)
	require.NoError(t, err)
	require.Equal(t, original.ID, current.ID)
	require.Equal(t, original.Token, current.Token)
	require.Equal(t, "replace", replacement.Kind)
	shadow, err := s.EntryByRemote(ctx, testFuseHidden)
	require.NoError(t, err)
	require.NotEqual(t, original.ID, shadow.ID)
	require.Empty(t, shadow.Token)
	newer, err := s.PutBlob(ctx, strings.NewReader("latest body"))
	require.NoError(t, err)
	_, err = s.AcceptManagedCheckpoint(ctx, newKey, 1, newer, false)
	require.NoError(t, err)
	// Closing the original OS handle deletes only its hidden, tokenless generation.
	commitManagedNamespace(t, s, "delete", testFuseHidden, "", model.ManagedCheckpoint{Journal: oldKey, Sequence: 1, Size: 13})
	old, err := s.PutBlob(ctx, strings.NewReader("old late save"))
	require.NoError(t, err)
	_, err = s.AcceptManagedCheckpoint(ctx, oldKey, 1, old, false)
	require.NoError(t, err)
	current, err = s.EntryByRemote(ctx, original.Remote)
	require.NoError(t, err)
	require.Equal(t, original.ID, current.ID)
	require.Equal(t, newer, current.Local)
	require.Equal(t, "stable-token", current.Token)
	events, err := s.HistoryEvents(ctx, 0, 500)
	require.NoError(t, err)
	found := false
	for _, event := range events {
		if event.Bodies["local"] == old {
			found = true
			require.Equal(t, original.ID, event.Entry.ID)
			require.Equal(t, original.Remote, event.Entry.Remote)
			require.False(t, event.Deleted, "hidden close must not delete the live logical file in Git history")
		}
	}
	require.True(t, found)
	require.NoError(t, s.Validate(ctx))
}

func TestFuseSoftRemoveWithoutReplacementRemainsARealDelete(t *testing.T) {
	s, _, _ := openTest(t)
	original := bind(t, s, entry(t, s, "document.md"), "stable-token")
	original, _ = stage(t, s, original, "old")
	commitManagedNamespace(t, s, "rename", original.Remote, testFuseHidden)
	deletion := commitManagedNamespace(t, s, "delete", testFuseHidden, "")
	actions, err := s.NamespaceActions(ctx)
	require.NoError(t, err)
	require.Len(t, actions, 1)
	require.Equal(t, deletion.ID, actions[0].ID)
	require.Equal(t, original.ID, actions[0].SrcID)
	require.Equal(t, "delete", actions[0].Kind)
	recreated := entry(t, s, "document.md")
	require.NotEqual(t, original.ID, recreated.ID, "a completed real unlink must not revive an old identity")
	require.Empty(t, recreated.Token)
}

func TestFuseSoftRemoveDoesNotReviveAfterExplicitCreate(t *testing.T) {
	s, _, _ := openTest(t)
	original := bind(t, s, entry(t, s, "document.md"), "original-token")
	original, _ = stage(t, s, original, "old")
	commitManagedNamespace(t, s, "rename", original.Remote, testFuseHidden)
	created := entry(t, s, original.Remote)
	require.NotEqual(t, original.ID, created.ID)
	commitManagedNamespace(t, s, "delete", created.Remote, "")
	next := entry(t, s, "next.tmp")
	next, _ = stage(t, s, next, "new")
	commitManagedNamespace(t, s, "rename", next.Remote, original.Remote)
	current, err := s.EntryByRemote(ctx, original.Remote)
	require.NoError(t, err)
	require.Equal(t, next.ID, current.ID)
	require.Empty(t, current.Token)
	commitManagedNamespace(t, s, "delete", testFuseHidden, "")
	current, err = s.EntryByRemote(ctx, original.Remote)
	require.NoError(t, err)
	require.Equal(t, next.ID, current.ID)
}

func TestFuseSoftRemoveClassificationRequiresCanonicalSibling(t *testing.T) {
	for _, hidden := range []string{".fuse_hidden-not-generated", ".fuse_hidden000000690000000", "other/.fuse_hidden0000006900000002", "renamed.md"} {
		t.Run(hidden, func(t *testing.T) {
			s, _, _ := openTest(t)
			original := bind(t, s, entry(t, s, "document.md"), "original-token")
			original, _ = stage(t, s, original, "old")
			if strings.HasPrefix(hidden, "other/") {
				_, err := s.EnsureEntry(ctx, "other", model.KindFolder)
				require.NoError(t, err)
			}
			commitManagedNamespace(t, s, "rename", original.Remote, hidden)
			next := entry(t, s, "next.tmp")
			next, _ = stage(t, s, next, "new")
			commitManagedNamespace(t, s, "rename", next.Remote, original.Remote)
			current, err := s.EntryByRemote(ctx, original.Remote)
			require.NoError(t, err)
			require.Equal(t, next.ID, current.ID)
			old, err := s.EntryByRemote(ctx, hidden)
			require.NoError(t, err)
			require.Equal(t, original.ID, old.ID)
		})
	}
}

func TestFuseSoftRemoveAbortedReplacementLeavesOriginalRecoverable(t *testing.T) {
	s, _, _ := openTest(t)
	original := bind(t, s, entry(t, s, "document.md"), "stable-token")
	original, _ = stage(t, s, original, "old")
	commitManagedNamespace(t, s, "rename", original.Remote, testFuseHidden)
	replacement := entry(t, s, "next.tmp")
	replacement, _ = stage(t, s, replacement, "new")
	op, err := s.PrepareNamespace(ctx, model.NamespaceOp{Kind: "rename", Src: replacement.Remote, Dst: original.Remote})
	require.NoError(t, err)
	require.Equal(t, original.ID, op.DstID)
	require.NoError(t, s.AbortNamespace(ctx, op.ID))
	hidden, err := s.EntryByRemote(ctx, testFuseHidden)
	require.NoError(t, err)
	require.Equal(t, original.ID, hidden.ID)
	_, err = s.EntryByRemote(ctx, original.Remote)
	require.ErrorIs(t, err, ErrNotFound)
	commitManagedNamespace(t, s, "rename", replacement.Remote, original.Remote)
	current, err := s.EntryByRemote(ctx, original.Remote)
	require.NoError(t, err)
	require.Equal(t, original.ID, current.ID)
	require.NoError(t, s.Validate(ctx))
}

func TestFuseSoftRemoveDoesNotTakeIdentityOfBoundReplacement(t *testing.T) {
	s, _, _ := openTest(t)
	original := bind(t, s, entry(t, s, "document.md"), "original-token")
	original, _ = stage(t, s, original, "old")
	commitManagedNamespace(t, s, "rename", original.Remote, testFuseHidden)
	replacement := bind(t, s, entry(t, s, "other.md"), "other-token")
	replacement, _ = stage(t, s, replacement, "other")
	commitManagedNamespace(t, s, "rename", replacement.Remote, original.Remote)
	current, err := s.EntryByRemote(ctx, original.Remote)
	require.NoError(t, err)
	require.Equal(t, replacement.ID, current.ID)
	require.Equal(t, replacement.Token, current.Token)
}

func TestFuseSoftRemoveRejectsCloudReobservationAtVacatedPath(t *testing.T) {
	s, _, _ := openTest(t)
	original := bind(t, s, entry(t, s, "document.md"), "stable-token")
	original, _ = stage(t, s, original, "old")
	commitManagedNamespace(t, s, "rename", original.Remote, testFuseHidden)
	_, err := s.ObserveEntry(ctx, Observation{Remote: original.Remote, Kind: original.Kind, Token: original.Token})
	require.ErrorIs(t, err, ErrFrozen)
	hidden, err := s.EntryByToken(ctx, original.Token)
	require.NoError(t, err)
	require.Equal(t, testFuseHidden, hidden.Remote)
	_, err = s.EntryByRemote(ctx, original.Remote)
	require.ErrorIs(t, err, ErrNotFound)
}

func TestFuseSoftRemoveUndoRestoresNormalAdmission(t *testing.T) {
	s, _, _ := openTest(t)
	original := bind(t, s, entry(t, s, "document.md"), "stable-token")
	original, job := stage(t, s, original, "old")
	commitManagedNamespace(t, s, "rename", original.Remote, testFuseHidden)
	_, _, err := s.Prepare(ctx, job.ID)
	require.ErrorIs(t, err, ErrFrozen)
	_, err = s.CommitPublication(ctx, model.PublishCandidate{EntryID: original.ID, ExpectedGeneration: original.Generation, ExpectedRemote: original.RemoteCheckpoint})
	require.ErrorIs(t, err, ErrFrozen)
	commitManagedNamespace(t, s, "rename", testFuseHidden, original.Remote)
	current, err := s.EntryByRemote(ctx, original.Remote)
	require.NoError(t, err)
	require.Equal(t, original.ID, current.ID)
	require.Equal(t, original.CloudName, current.CloudName)
	metadata, err := s.Metadata(ctx, original.ID)
	require.NoError(t, err)
	require.Empty(t, metadata.FuseSoftRemove)
	actions, err := s.NamespaceActions(ctx)
	require.NoError(t, err)
	require.Empty(t, actions, "undoing the kernel's internal move is local only")
	_, _, err = s.Prepare(ctx, job.ID)
	require.NoError(t, err)
}

func TestFuseSoftRemoveBoundOccupantEndsPreviousReconciliation(t *testing.T) {
	s, _, _ := openTest(t)
	original := bind(t, s, entry(t, s, "document.md"), "original-token")
	original, _ = stage(t, s, original, "old")
	commitManagedNamespace(t, s, "rename", original.Remote, testFuseHidden)
	occupant := bind(t, s, entry(t, s, "other.md"), "other-token")
	occupant, _ = stage(t, s, occupant, "other")
	commitManagedNamespace(t, s, "rename", occupant.Remote, original.Remote)
	commitManagedNamespace(t, s, "delete", original.Remote, "")
	next := entry(t, s, "next.tmp")
	next, _ = stage(t, s, next, "new")
	commitManagedNamespace(t, s, "rename", next.Remote, original.Remote)
	current, err := s.EntryByRemote(ctx, original.Remote)
	require.NoError(t, err)
	require.Equal(t, next.ID, current.ID)
	require.Empty(t, current.Token)
}
