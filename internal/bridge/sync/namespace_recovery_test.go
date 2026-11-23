package syncer

import (
	"context"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
	"github.com/rclone/rclone/lib/larkcli"
)

type namespaceRecoveryCloud struct {
	*fixtureCloud
	folders     int
	listErr     error
	listHook    func()
	keepDeleted bool
}

func (c *namespaceRecoveryCloud) CreateFolder(_ context.Context, parent, name string) (string, error) {
	c.folders++
	token := "created-folder"
	c.items = append(c.items, larkcli.DriveItem{Token: token, Name: name, Type: "folder", ParentToken: parent})
	return token, nil
}
func (c *namespaceRecoveryCloud) List(ctx context.Context, parent string) ([]larkcli.DriveItem, error) {
	if c.listHook != nil {
		c.listHook()
	}
	if c.listErr != nil {
		return nil, c.listErr
	}
	var items []larkcli.DriveItem
	for _, item := range c.items {
		if item.ParentToken == "" || item.ParentToken == parent {
			items = append(items, item)
		}
	}
	return items, nil
}
func (c *namespaceRecoveryCloud) Delete(ctx context.Context, token string, kind model.Kind) error {
	if c.keepDeleted {
		c.deletes++
		return model.ErrUnknownResult
	}
	return c.fixtureCloud.Delete(ctx, token, kind)
}
func finishRecoveryNamespace(t *testing.T, s *store.Store, op model.NamespaceOp) (model.NamespaceOp, model.Entry) {
	t.Helper()
	out, err := s.PrepareNamespace(bg, op)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SealNamespace(bg, out.ID); err != nil {
		t.Fatal(err)
	}
	entry, err := s.CommitNamespace(bg, out.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.FinishNamespace(bg, out.ID); err != nil {
		t.Fatal(err)
	}
	return out, entry
}

func TestUnknownDeleteReconcilesAbsenceWithoutReplaying(t *testing.T) {
	s, e, c, entry := setup(t, model.KindFile)
	c.loseResponse = true
	op, _ := finishRecoveryNamespace(t, s, model.NamespaceOp{Kind: "delete", Src: entry.Remote})
	if err := e.syncNamespace(bg); !errors.Is(err, model.ErrUnknownResult) {
		t.Fatal(err)
	}
	// A same-name replacement belongs to a different object and must survive.
	c.items = append(c.items, larkcli.DriveItem{Token: "replacement", Name: entry.Remote, Type: "file", ParentToken: "root"})
	if err := e.syncNamespace(bg); err != nil {
		t.Fatal(err)
	}
	work, err := s.NamespaceJournal(bg, op.ID)
	if err != nil || work.ActionPending || work.Dispatched || c.deletes != 1 || len(c.items) != 1 || c.items[0].Token != "replacement" {
		t.Fatalf("work=%+v deletes=%d items=%+v err=%v", work, c.deletes, c.items, err)
	}
}

func TestUnknownDeleteDoesNotBlockIndependentMkdir(t *testing.T) {
	s, e, c, entry := setup(t, model.KindFile)
	cloud := &namespaceRecoveryCloud{fixtureCloud: c, keepDeleted: true}
	e.source = cloud
	op, _ := finishRecoveryNamespace(t, s, model.NamespaceOp{Kind: "delete", Src: entry.Remote})
	if err := e.syncNamespace(bg); !errors.Is(err, model.ErrUnknownResult) {
		t.Fatal(err)
	}
	_, folder := finishRecoveryNamespace(t, s, model.NamespaceOp{Kind: "mkdir", Dst: "independent"})
	if err := e.syncNamespace(bg); !errors.Is(err, model.ErrUnknownResult) {
		t.Fatal(err)
	}
	current, err := s.Entry(bg, folder.ID)
	work, _ := s.NamespaceJournal(bg, op.ID)
	if err != nil || current.Token == "" || cloud.folders != 1 || c.deletes != 1 || !work.Dispatched || !work.ActionPending {
		t.Fatalf("folder=%+v creates=%d deletes=%d work=%+v %v", current, cloud.folders, c.deletes, work, err)
	}
}

func TestUnknownDeleteFailedListingKeepsJournal(t *testing.T) {
	s, e, c, entry := setup(t, model.KindFile)
	cloud := &namespaceRecoveryCloud{fixtureCloud: c, keepDeleted: true}
	e.source = cloud
	op, _ := finishRecoveryNamespace(t, s, model.NamespaceOp{Kind: "delete", Src: entry.Remote})
	_ = e.syncNamespace(bg)
	cloud.listErr = context.DeadlineExceeded
	if err := e.syncNamespace(bg); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	work, _ := s.NamespaceJournal(bg, op.ID)
	if !work.ActionPending || !work.Dispatched || c.deletes != 1 {
		t.Fatal("uncertain delete journal lost", work, c.deletes)
	}
}

func TestPendingParentHasDedicatedWaitingClassification(t *testing.T) {
	s, e, _, _ := setup(t, model.KindFile)
	finishRecoveryNamespace(t, s, model.NamespaceOp{Kind: "mkdir", Dst: "new-parent"})
	_, err := e.parentToken(bg, "new-parent/child.txt")
	if classify(err) != "parent_pending" || !transient(classify(err)) {
		t.Fatalf("parent wait classified as %s: %v", classify(err), err)
	}
}

func TestNamespaceFailureKeepsRelatedPathsOrdered(t *testing.T) {
	s, e, c, entry := setup(t, model.KindFile)
	cloud := &namespaceRecoveryCloud{fixtureCloud: c, keepDeleted: true}
	e.source = cloud
	finishRecoveryNamespace(t, s, model.NamespaceOp{Kind: "delete", Src: entry.Remote})
	_ = e.syncNamespace(bg)
	// A new object at the same path must not cross the unconfirmed delete.
	_, replacement := finishRecoveryNamespace(t, s, model.NamespaceOp{Kind: "mkdir", Dst: entry.Remote})
	_, independent := finishRecoveryNamespace(t, s, model.NamespaceOp{Kind: "mkdir", Dst: "other"})
	_ = e.syncNamespace(bg)
	blocked, _ := s.Entry(bg, replacement.ID)
	ready, _ := s.Entry(bg, independent.ID)
	if blocked.Token != "" || ready.Token == "" || cloud.folders != 1 {
		t.Fatalf("related=%+v independent=%+v creates=%d", blocked, ready, cloud.folders)
	}
}

type slowNamespaceCloud struct {
	*namespaceRecoveryCloud
	slowParent string
}

func (c *slowNamespaceCloud) List(ctx context.Context, parent string) ([]larkcli.DriveItem, error) {
	if parent == c.slowParent {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return c.namespaceRecoveryCloud.List(ctx, parent)
}
func TestNamespaceSlowOperationAllowsIndependentWork(t *testing.T) {
	s, e, c, _ := setup(t, model.KindFile)
	_, err := s.ObserveEntry(bg, store.Observation{Remote: "slow", Kind: model.KindFolder, Token: "slow-parent"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.ObserveEntry(bg, store.Observation{Remote: "slow/file", Kind: model.KindFile, Token: "slow-file"})
	if err != nil {
		t.Fatal(err)
	}
	cloud := &slowNamespaceCloud{&namespaceRecoveryCloud{fixtureCloud: c}, "slow-parent"}
	e.source = cloud
	e.opt.SliceDuration = 250 * time.Millisecond
	finishRecoveryNamespace(t, s, model.NamespaceOp{Kind: "delete", Src: "slow/file"})
	_, folder := finishRecoveryNamespace(t, s, model.NamespaceOp{Kind: "mkdir", Dst: "fast"})
	ctx, cancel := context.WithTimeout(bg, 5*time.Second)
	defer cancel()
	_ = e.syncNamespace(ctx)
	current, err := s.Entry(bg, folder.ID)
	if err != nil || current.Token == "" {
		t.Fatalf("slow unrelated delete blocked folder: %+v %v", current, err)
	}
}

func TestParentReadyMetadataCompletionReadmitsWaitingChild(t *testing.T) {
	for _, running := range []bool{false, true} {
		t.Run(map[bool]string{false: "idle", true: "finishing pass"}[running], func(t *testing.T) {
			s, e, c, _ := setup(t, model.KindFile)
			cloud := &namespaceRecoveryCloud{fixtureCloud: c}
			e.source = cloud
			finishRecoveryNamespace(t, s, model.NamespaceOp{Kind: "mkdir", Dst: "parent"})
			child, err := s.EnsureEntry(bg, "parent/child.txt", model.KindFile)
			if err != nil {
				t.Fatal(err)
			}
			_, job, err := s.Stage(bg, child.ID, nil, strings.NewReader("child"))
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err = s.Prepare(bg, job.ID); err != nil {
				t.Fatal(err)
			}
			if err = s.PauseJobWithDiagnostic(bg, job.ID, model.StatusPaused, "parent_pending", nil); err != nil {
				t.Fatal(err)
			}
			d := newBackgroundDispatcher(bg, e)
			d.epoch = 1
			if running {
				d.entries[child.ID] = &dispatchWork{running: true, epoch: 1, cutoff: job.Generation}
				d.active = 1
			}
			if err = e.syncNamespace(bg); err != nil {
				t.Fatal(err)
			}
			d.completed(dispatchResult{metadata: true})
			if running {
				d.completed(dispatchResult{id: child.ID, cutoff: job.Generation, epoch: 1, err: errParentPending})
			}
			current, err := s.Job(bg, job.ID)
			if err != nil || current.State != model.StatusQueued || len(d.queue) != 1 || d.queue[0] != child.ID {
				t.Fatalf("parent-ready admission lost: job=%+v queue=%+v %v", current, d.queue, err)
			}
		})
	}
}

func TestUnknownDeleteRejectsParentChangeDuringReadback(t *testing.T) {
	s, e, c, _ := setup(t, model.KindFile)
	parent, err := s.ObserveEntry(bg, store.Observation{Remote: "parent", Kind: model.KindFolder, Token: "parent-token"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := s.ObserveEntry(bg, store.Observation{Remote: "parent/file", Kind: model.KindFile, Token: "child-token"})
	if err != nil {
		t.Fatal(err)
	}
	c.items = append(c.items, larkcli.DriveItem{Token: child.Token, Name: "file", Type: "file", ParentToken: parent.Token})
	cloud := &namespaceRecoveryCloud{fixtureCloud: c, keepDeleted: true}
	e.source = cloud
	op, _ := finishRecoveryNamespace(t, s, model.NamespaceOp{Kind: "delete", Src: child.Remote})
	if err = e.syncNamespace(bg); !errors.Is(err, model.ErrUnknownResult) {
		t.Fatal(err)
	}
	cloud.items = nil
	cloud.listHook = func() {
		cloud.listHook = nil
		finishRecoveryNamespace(t, s, model.NamespaceOp{Kind: "rename", Src: "parent", Dst: "moved-parent"})
	}
	if err = e.syncNamespace(bg); err == nil {
		t.Fatal("stale parent absence accepted")
	}
	work, _ := s.NamespaceJournal(bg, op.ID)
	if !work.Dispatched || !work.ActionPending || c.deletes != 1 {
		t.Fatal("stale readback lost delete intent", work, c.deletes)
	}
}

func TestUnknownDeleteUsesOriginalParentAfterReplacement(t *testing.T) {
	s, e, c, _ := setup(t, model.KindFile)
	parent, err := s.ObserveEntry(bg, store.Observation{Remote: "parent", Kind: model.KindFolder, Token: "original-parent"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := s.ObserveEntry(bg, store.Observation{Remote: "parent/file", Kind: model.KindFile, Token: "original-child"})
	if err != nil {
		t.Fatal(err)
	}
	c.items = []larkcli.DriveItem{{Token: child.Token, Name: "file", Type: "file", ParentToken: parent.Token}}
	cloud := &namespaceRecoveryCloud{fixtureCloud: c, keepDeleted: true}
	e.source = cloud
	op, _ := finishRecoveryNamespace(t, s, model.NamespaceOp{Kind: "delete", Src: child.Remote})
	_ = e.syncNamespace(bg)
	finishRecoveryNamespace(t, s, model.NamespaceOp{Kind: "rename", Src: "parent", Dst: "moved"})
	_, err = s.ObserveEntry(bg, store.Observation{Remote: "parent", Kind: model.KindFolder, Token: "replacement-parent"})
	if err != nil {
		t.Fatal(err)
	}
	_ = e.syncNamespace(bg)
	work, _ := s.NamespaceJournal(bg, op.ID)
	if !work.Dispatched || !work.ActionPending {
		t.Fatal("replacement parent falsely settled original delete", work)
	}
}

func TestUnknownChildDeleteRecoversWhenParentLocallyDeleted(t *testing.T) {
	s, e, c, _ := setup(t, model.KindFile)
	parent, err := s.ObserveEntry(bg, store.Observation{Remote: "parent", Kind: model.KindFolder, Token: "parent-token"})
	if err != nil {
		t.Fatal(err)
	}
	child, err := s.ObserveEntry(bg, store.Observation{Remote: "parent/file", Kind: model.KindFile, Token: "child-token"})
	if err != nil {
		t.Fatal(err)
	}
	c.items = []larkcli.DriveItem{{Token: parent.Token, Name: "parent", Type: "folder", ParentToken: "root"}, {Token: child.Token, Name: "file", Type: "file", ParentToken: parent.Token}}
	cloud := &namespaceRecoveryCloud{fixtureCloud: c}
	e.source = cloud
	op, _ := finishRecoveryNamespace(t, s, model.NamespaceOp{Kind: "delete", Src: child.Remote})
	if err = s.BeginNamespaceDispatch(bg, op.ID, "delete"); err != nil {
		t.Fatal(err)
	}
	c.items = c.items[:1] // lost result after child was removed
	parentOp, _ := finishRecoveryNamespace(t, s, model.NamespaceOp{Kind: "delete", Src: parent.Remote})
	if err = e.syncNamespace(bg); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{op.ID, parentOp.ID} {
		w, _ := s.NamespaceJournal(bg, id)
		if w.ActionPending {
			t.Fatal("ordered deletion stuck", w)
		}
	}
	if c.deletes != 1 {
		t.Fatal("child delete replayed", c.deletes)
	}
}

func TestUnknownDeleteBlocksProviderTokenAlias(t *testing.T) {
	s, e, c, entry := setup(t, model.KindFile)
	cloud := &namespaceRecoveryCloud{fixtureCloud: c, keepDeleted: true}
	e.source = cloud
	finishRecoveryNamespace(t, s, model.NamespaceOp{Kind: "delete", Src: entry.Remote})
	_ = e.syncNamespace(bg)
	alias, err := s.ObserveEntry(bg, store.Observation{Remote: "other-name", Kind: model.KindFile, Token: entry.Token})
	if err != nil {
		t.Fatal(err)
	}
	finishRecoveryNamespace(t, s, model.NamespaceOp{Kind: "delete", Src: alias.Remote})
	_ = e.syncNamespace(bg)
	if c.deletes != 1 {
		t.Fatal("alias crossed unresolved provider mutation", c.deletes)
	}
}

func TestNamespaceFairnessAcrossMetadataPasses(t *testing.T) {
	// Exercise scheduling deadlines without charging real Windows filesystem I/O
	// against a 250ms simulated provider budget.
	synctest.Test(t, func(t *testing.T) {
		s, e, c, _ := setup(t, model.KindFile)
		_, err := s.ObserveEntry(bg, store.Observation{Remote: "slow", Kind: model.KindFolder, Token: "slow-parent"})
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"a", "b", "c"} {
			_, err = s.ObserveEntry(bg, store.Observation{Remote: "slow/" + name, Kind: model.KindFile, Token: "slow-" + name})
			if err != nil {
				t.Fatal(err)
			}
			finishRecoveryNamespace(t, s, model.NamespaceOp{Kind: "delete", Src: "slow/" + name})
		}
		_, folder := finishRecoveryNamespace(t, s, model.NamespaceOp{Kind: "mkdir", Dst: "fast"})
		e.source = &slowNamespaceCloud{&namespaceRecoveryCloud{fixtureCloud: c}, "slow-parent"}
		e.opt.SliceDuration = 100 * time.Millisecond
		for i := 0; i < 2; i++ {
			ctx, cancel := context.WithTimeout(bg, 250*time.Millisecond)
			_ = e.syncNamespace(ctx)
			cancel()
		}
		current, _ := s.Entry(bg, folder.ID)
		if current.Token == "" {
			t.Fatal("independent work starved across passes")
		}
	})
}

type slowBackupNamespaceCloud struct{ *namespaceRecoveryCloud }

func (c *slowBackupNamespaceCloud) ObserveFile(ctx context.Context, token string) (ObservedFile, error) {
	select {
	case <-time.After(120 * time.Millisecond):
		return c.fixtureCloud.ObserveFile(ctx, token)
	case <-ctx.Done():
		return ObservedFile{}, ctx.Err()
	}
}
func TestNamespaceBackupMayExceedReadbackSlice(t *testing.T) {
	s, e, c, entry := setup(t, model.KindFile)
	e.source = &slowBackupNamespaceCloud{&namespaceRecoveryCloud{fixtureCloud: c}}
	e.opt.SliceDuration = 40 * time.Millisecond
	op, _ := finishRecoveryNamespace(t, s, model.NamespaceOp{Kind: "delete", Src: entry.Remote})
	ctx, cancel := context.WithTimeout(bg, 5*time.Second)
	defer cancel()
	if err := e.syncNamespace(ctx); err != nil {
		t.Fatal(err)
	}
	w, _ := s.NamespaceJournal(bg, op.ID)
	if w.ActionPending || c.deletes != 1 {
		t.Fatal("backup aborted at scheduling slice", w, c.deletes)
	}
}
