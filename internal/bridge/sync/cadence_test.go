package syncer

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/stretchr/testify/require"
)

func TestBackgroundCycleUsesFixedFiveMinuteBoundariesAndLatestSave(t *testing.T) {
	s, e, source, entry := setup(t, model.KindFile)
	now := time.Unix(1000, 0)
	e.opt.Now = func() time.Time { return now }
	e.opt.WriteBack = 24 * time.Hour // Old idle-delay settings do not move a cycle.
	require.NoError(t, e.backgroundCycle(bg))
	entry = stageContent(t, s, entry, "first saved version")
	entry = stageContent(t, s, entry, "latest at first boundary")
	now = now.Add(5*time.Minute - time.Nanosecond)
	require.NoError(t, e.backgroundCycle(bg))
	require.Zero(t, source.writes)
	now = now.Add(time.Nanosecond)
	require.NoError(t, e.backgroundCycle(bg))
	require.Equal(t, 1, source.writes)
	require.Equal(t, "latest at first boundary", string(source.file))
	entry = stageContent(t, s, entry, "next cycle version")
	now = now.Add(5*time.Minute - time.Nanosecond)
	require.NoError(t, e.backgroundCycle(bg))
	require.Equal(t, 1, source.writes)
	now = now.Add(time.Nanosecond)
	require.NoError(t, e.backgroundCycle(bg))
	require.Equal(t, 2, source.writes)
	require.Equal(t, "next cycle version", string(source.file))
	events, err := s.HistoryEvents(bg, 0, 500)
	require.NoError(t, err)
	found := false
	for _, ev := range events {
		if b := ev.Bodies["local"]; b != "" && blobText(t, s, b) == "first saved version" {
			found = true
		}
	}
	require.True(t, found, "coalescing cloud writes must retain every saved history version")
}

func TestBackgroundCycleDefersEditAcceptedDuringUpload(t *testing.T) {
	s, e, source, entry := setup(t, model.KindDocx)
	now := time.Unix(1000, 0)
	e.opt.Now = func() time.Time { return now }
	require.NoError(t, e.backgroundCycle(bg))
	entry = stageContent(t, s, entry, "alpha changed\n\nbeta two\n")
	source.applyHook = func() { stageContent(t, s, entry, "alpha changed\n\nbeta newer\n") }
	now = now.Add(5 * time.Minute)
	require.NoError(t, e.backgroundCycle(bg))
	require.Equal(t, 1, source.writes)
	require.NotContains(t, source.xml, "beta newer")
	current, err := s.Entry(bg, entry.ID)
	require.NoError(t, err)
	require.Contains(t, blobText(t, s, current.Local), "beta newer")
	now = now.Add(time.Minute)
	require.NoError(t, e.backgroundCycle(bg))
	require.Equal(t, 1, source.writes)
	now = now.Add(4 * time.Minute)
	require.NoError(t, e.backgroundCycle(bg))
	require.Contains(t, source.xml, "beta newer")
}

func TestBackgroundBoundaryDoesNotAdmitLaterGeneration(t *testing.T) {
	s, e, source, entry := setup(t, model.KindFile)
	now := time.Unix(1000, 0)
	e.opt.Now = func() time.Time { return now }
	require.NoError(t, e.backgroundCycle(bg))
	entry = stageContent(t, s, entry, "boundary snapshot")
	once := true
	e.deps.ShouldSync = func(_ context.Context, id model.EntryID) (bool, error) {
		if once {
			once = false
			stageContent(t, s, entry, "saved after cycle boundary")
		}
		return true, nil
	}
	now = now.Add(5 * time.Minute)
	require.NoError(t, e.backgroundCycle(bg))
	require.Zero(t, source.writes)
	now = now.Add(5 * time.Minute)
	require.NoError(t, e.backgroundCycle(bg))
	require.Equal(t, "saved after cycle boundary", string(source.file))
}

func TestExplicitRetryBypassesBackgroundCadenceAndOldIdleDelay(t *testing.T) {
	s, e, source, entry := setup(t, model.KindFile)
	now := time.Unix(1000, 0)
	e.opt.Now = func() time.Time { return now }
	e.opt.WriteBack = time.Hour
	require.NoError(t, e.backgroundCycle(bg))
	stageContent(t, s, entry, "explicit sync")
	require.NoError(t, e.Retry(bg, entry.ID))
	require.Equal(t, 1, source.writes)
	require.Equal(t, "explicit sync", string(source.file))
}

func TestBackgroundCycleDoesNotSupersedeAmbiguousPredecessor(t *testing.T) {
	s, e, source, entry := setup(t, model.KindFile)
	now := time.Unix(1000, 0)
	e.opt.Now = func() time.Time { return now }
	require.NoError(t, e.backgroundCycle(bg))
	entry = stageContent(t, s, entry, "possibly dispatched")
	pending, err := s.Pending(bg)
	require.NoError(t, err)
	old := pending[0]
	_, _, err = s.Prepare(bg, old.ID)
	require.NoError(t, err)
	require.NoError(t, s.RecordPlan(bg, old.ID, nil, old.Local))
	require.NoError(t, s.MarkUnknown(bg, old.ID))
	stageContent(t, s, entry, "newest durable save")
	now = now.Add(5 * time.Minute)
	require.NoError(t, e.backgroundCycle(bg))
	require.Zero(t, source.writes)
	journal, err := s.Journal(bg, old.ID)
	require.NoError(t, err)
	require.Equal(t, model.StatusAwaitingVerification, journal.Job.State)
	require.NotEmpty(t, journal.Job.Plan)
	pending, err = s.Pending(bg)
	require.NoError(t, err)
	require.Len(t, pending, 2)
	require.True(t, strings.Contains(blobText(t, s, pending[1].Local), "newest"))
}
