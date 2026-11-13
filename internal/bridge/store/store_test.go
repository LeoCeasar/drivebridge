package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/model"
	bolt "go.etcd.io/bbolt"
)

var ctx = context.Background()

func testScope() Scope {
	return Scope{MountID: model.MountID(uuid.NewString()), ProfileID: model.ProfileID(uuid.NewString()), RemoteID: model.RemoteID(uuid.NewString()), IdentityHash: "verified-identity", FolderToken: "root-resource"}
}
func openTest(t *testing.T) (*Store, string, Scope) {
	t.Helper()
	d, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	s, e := Open(d)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = s.Close() })
	sc := testScope()
	if e = s.BindScope(ctx, sc); e != nil {
		t.Fatal(e)
	}
	return s, d, sc
}
func entry(t *testing.T, s *Store, p string) model.Entry {
	t.Helper()
	e, err := s.EnsureEntry(ctx, p, model.KindFile)
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func stage(t *testing.T, s *Store, e model.Entry, body string) (model.Entry, model.Job) {
	t.Helper()
	x, j, err := s.Stage(ctx, e.ID, nil, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return x, j
}
func fileSnapshot(t *testing.T, s *Store, token, body string) model.Snapshot {
	t.Helper()
	h := sha256.Sum256([]byte(body))
	now := time.Now().UTC()
	sn, err := s.SaveFileSnapshot(ctx, token, model.FileIdentity{Kind: model.FileIdentityObservation, ObservationID: uuid.NewString(), SHA256: hex.EncodeToString(h[:]), StartedAt: now, CompletedAt: now}, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	return sn
}
func bind(t *testing.T, s *Store, e model.Entry, token string) model.Entry {
	t.Helper()
	x, err := s.ObserveEntry(ctx, Observation{Remote: e.Remote, Kind: e.Kind, Token: token})
	if err != nil {
		t.Fatal(err)
	}
	return x
}
func read(t *testing.T, s *Store, b model.BlobID) string {
	t.Helper()
	r, err := s.ReadBlob(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	v, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(v)
}
func TestStageSurvivesRestartAndScopeBinding(t *testing.T) {
	s, d, sc := openTest(t)
	e := entry(t, s, "data.bin")
	e, j := stage(t, s, e, "accepted")
	if e.Status == model.StatusSynced {
		t.Fatal("unobserved upload marked synced")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := Open(d)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, _, err = s.Stage(ctx, e.ID, nil, strings.NewReader("no binding")); !errors.Is(err, ErrUnbound) {
		t.Fatalf("binding: %v", err)
	}
	bad := sc
	bad.IdentityHash = "other"
	if err = s.BindScope(ctx, bad); !errors.Is(err, model.ErrIdentityChanged) {
		t.Fatal(err)
	}
	if err = s.BindScope(ctx, sc); err != nil {
		t.Fatal(err)
	}
	p, err := s.Pending(ctx)
	if err != nil || len(p) != 1 || p[0].ID != j.ID || read(t, s, e.Local) != "accepted" {
		t.Fatalf("pending %v %v", p, err)
	}
}
func TestConfirmG1KeepsG2Pending(t *testing.T) {
	s, _, _ := openTest(t)
	e := bind(t, s, entry(t, s, "a"), "token")
	e, j1 := stage(t, s, e, "g1")
	if _, _, err := s.Prepare(ctx, j1.ID); err != nil {
		t.Fatal(err)
	}
	e, j2 := stage(t, s, e, "g2")
	sn := fileSnapshot(t, s, "token", "g1")
	c, err := s.Confirm(ctx, j1.ID, sn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CommitPublication(ctx, c); !errors.Is(err, model.ErrGenerationChanged) {
		t.Fatal(err)
	}
	got, _ := s.Entry(ctx, e.ID)
	if got.Local != j2.Local || got.LocalCheckpoint != j1.Local || got.RemoteCheckpoint != sn.ID || got.Status != model.StatusQueued {
		t.Fatalf("lost g2: %+v", got)
	}
}
func TestPrepareRebindsAcceptedCheckpoints(t *testing.T) {
	s, _, _ := openTest(t)
	e := bind(t, s, entry(t, s, "a"), "token")
	e, j1 := stage(t, s, e, "g1")
	s.Prepare(ctx, j1.ID)
	_, j2 := stage(t, s, e, "g2")
	wait, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if _, _, err := s.Prepare(wait, j2.ID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("prepare must wait: %v", err)
	}
	sn := fileSnapshot(t, s, "token", "merged g1")
	if _, err := s.Confirm(ctx, j1.ID, sn); err != nil {
		t.Fatal(err)
	}
	got, _, err := s.Prepare(ctx, j2.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.BaseLocal != j1.Local || got.BaseRemote != sn.ID {
		t.Fatalf("stale bases: %+v", got)
	}
}
func TestPublishCASRejectsNewGeneration(t *testing.T) {
	s, _, _ := openTest(t)
	e := bind(t, s, entry(t, s, "a"), "token")
	sn := fileSnapshot(t, s, "token", "remote")
	c, err := s.OfferRemote(ctx, e.ID, e.Generation, sn, sn.FileData)
	if err != nil {
		t.Fatal(err)
	}
	stage(t, s, e, "new")
	if _, err = s.CommitPublication(ctx, c); !errors.Is(err, model.ErrGenerationChanged) {
		t.Fatal(err)
	}
}
func TestUnknownWriteSurvivesRestart(t *testing.T) {
	s, d, sc := openTest(t)
	_, j := stage(t, s, entry(t, s, "a"), "uncertain")
	s.Prepare(ctx, j.ID)
	if err := s.MarkUnknown(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err := Open(d)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.BindScope(ctx, sc)
	p, _ := s.Pending(ctx)
	if len(p) != 1 || p[0].State != model.StatusAwaitingVerification {
		t.Fatalf("lost unknown %+v", p)
	}
	if _, _, err = s.Prepare(ctx, j.ID); !errors.Is(err, model.ErrUnknownResult) {
		t.Fatal(err)
	}
}
func TestFileObservationAdoptionAndMetadata(t *testing.T) {
	s, _, _ := openTest(t)
	e, err := s.ObserveEntry(ctx, Observation{Remote: "binary", Kind: model.KindFile, Token: "t", SourceURL: "https://opaque", ModTime: time.Time{}})
	if err != nil {
		t.Fatal(err)
	}
	m, _ := s.Metadata(ctx, e.ID)
	e, err = s.ObserveEntry(ctx, Observation{Remote: "binary", Kind: model.KindFile, Token: "t"})
	if err != nil {
		t.Fatal(err)
	}
	m2, _ := s.Metadata(ctx, e.ID)
	if m.FirstObserved.IsZero() || m.FirstObserved != m2.FirstObserved || m2.SourceURL != "https://opaque" {
		t.Fatal(m, m2)
	}
	sn := fileSnapshot(t, s, "t", "\x00binary\xff")
	e, err = s.AdoptFileSnapshot(ctx, e.ID, e.Generation, sn.ID)
	if err != nil || e.Local != sn.FileData || read(t, s, e.Visible) != "\x00binary\xff" {
		t.Fatal(e, err)
	}
	stage(t, s, e, "local edit")
	if _, err = s.AdoptFileSnapshot(ctx, e.ID, e.Generation+1, sn.ID); err == nil {
		t.Fatal("adoption overwrote edits")
	}
	if _, err = s.ObserveEntry(ctx, Observation{Remote: e.Remote, Kind: e.Kind, Token: "switch"}); !errors.Is(err, model.ErrIdentityChanged) {
		t.Fatal(err)
	}
}
func TestNamespaceReplaceKeepsDestinationIdentity(t *testing.T) {
	s, _, _ := openTest(t)
	src, _ := stage(t, s, entry(t, s, "tmp"), "new")
	dst := bind(t, s, entry(t, s, "final"), "destination-token")
	old := fileSnapshot(t, s, dst.Token, "old")
	dst, err := s.AdoptFileSnapshot(ctx, dst.ID, 0, old.ID)
	if err != nil {
		t.Fatal(err)
	}
	op, err := s.PrepareNamespace(ctx, model.NamespaceOp{Kind: "replace", Src: "tmp", Dst: "final"})
	if err != nil {
		t.Fatal(err)
	} // Flush may durably stage under scheduling freeze.
	src, flush := stage(t, s, src, "flushed")
	short, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
	defer cancel()
	if _, _, err = s.Prepare(short, flush.ID); !errors.Is(err, ErrFrozen) {
		t.Fatal(err)
	}
	op, err = s.SealNamespace(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.Stage(ctx, src.ID, nil, strings.NewReader("too late")); !errors.Is(err, ErrFrozen) {
		t.Fatal(err)
	}
	got, err := s.CommitNamespace(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != dst.ID || got.Token != dst.Token || got.Generation != dst.Generation+1 || read(t, s, got.Local) != "flushed" {
		t.Fatalf("replacement %+v", got)
	}
	if _, err = s.EntryByRemote(ctx, "tmp"); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if err = s.AbortNamespace(ctx, op.ID); err == nil {
		t.Fatal("committed journal rolled back")
	}
	p, _ := s.PendingNamespace(ctx)
	if len(p) != 1 || p[0].State != "committed" {
		t.Fatal(p)
	}
	if err = s.FinishNamespace(ctx, op.ID); err != nil {
		t.Fatal(err)
	}
	p, _ = s.PendingNamespace(ctx)
	if len(p) != 0 {
		t.Fatal(p)
	}
	if read(t, s, old.FileData) != "old" {
		t.Fatal("lost previous")
	}
}
func TestCreateTokenBindingCrashRecovery(t *testing.T) {
	s, d, sc := openTest(t)
	e, j := stage(t, s, entry(t, s, "new"), "input")
	s.Prepare(ctx, j.ID)
	op, err := s.PrepareCreate(ctx, j.ID, "parent", "new")
	if err != nil {
		t.Fatal(err)
	}
	if op.Input != j.Local || op.TemporaryName == "new" || op.TemporaryName == "" {
		t.Fatal(op)
	}
	s.MarkCreateUnknown(ctx, op.ID)
	s.Close()
	s, err = Open(d)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.BindScope(ctx, sc)
	ops, _ := s.PendingCreates(ctx)
	if len(ops) != 1 || ops[0].State != "unknown" {
		t.Fatal(ops)
	}
	if _, err = s.BindCreated(ctx, op.ID, "token", "rev"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.BindCreated(ctx, op.ID, "different", "rev"); !errors.Is(err, model.ErrIdentityChanged) {
		t.Fatal(err)
	}
	if err = s.FinishCreate(ctx, op.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishCreate(ctx, op.ID); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Entry(ctx, e.ID)
	if got.Token != "token" || got.Status == model.StatusSynced {
		t.Fatal(got)
	}
}
func TestReferencedBlobNeverGC(t *testing.T) {
	s, _, _ := openTest(t)
	e, j := stage(t, s, entry(t, s, "pending"), "pending bytes")
	backup := fileSnapshot(t, s, "backup-token", "backup")
	if err := s.ProtectBackup(ctx, j.ID, backup.ID); err != nil {
		t.Fatal(err)
	}
	leaseID, err := s.PutBlob(ctx, strings.NewReader("lease"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.ReadBlob(ctx, leaseID)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	orphan, err := s.PutBlob(ctx, strings.NewReader("orphan"))
	if err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(90 * 24 * time.Hour)
	if _, err = s.GC(ctx, GCOptions{Now: future}); err != nil {
		t.Fatal(err)
	}
	if read(t, s, e.Local) != "pending bytes" || read(t, s, backup.FileData) != "backup" {
		t.Fatal("accepted lost")
	}
	if _, err = os.Stat(s.blobPath(leaseID)); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ReadBlob(ctx, orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan %v", err)
	}
	r.Close()
	if _, err = s.GC(ctx, GCOptions{Now: future}); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(s.blobPath(leaseID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
}

type failingIO struct{ fail string }

func (f failingIO) SyncFile(v *os.File) error {
	if f.fail == "fsync" {
		return errors.New("injected fsync")
	}
	return v.Sync()
}
func (f failingIO) Rename(a, b string) error {
	if f.fail == "rename" {
		return errors.New("injected rename")
	}
	return os.Rename(a, b)
}
func (f failingIO) SyncDir(p string) error {
	if f.fail == "dirsync" {
		return errors.New("injected dirsync")
	}
	d, e := os.Open(p)
	if e != nil {
		return e
	}
	defer d.Close()
	return d.Sync()
}
func TestBlobCrashBeforeTxn(t *testing.T) {
	for _, point := range []string{"fsync", "rename", "dirsync", "txn"} {
		t.Run(point, func(t *testing.T) {
			s, _, _ := openTest(t)
			e := entry(t, s, "a")
			if point == "txn" {
				s.db.Close()
			} else {
				s.io = failingIO{point}
			}
			if _, _, err := s.Stage(ctx, e.ID, nil, strings.NewReader("must not accept")); err == nil {
				t.Fatal("fault accepted")
			}
			if point != "txn" {
				got, _ := s.Entry(ctx, e.ID)
				if got.Generation != 0 || got.Local != "" {
					t.Fatal(got)
				}
			}
		})
	}
}
func TestCanceledCopyAndHashMismatch(t *testing.T) {
	s, _, _ := openTest(t)
	c, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.PutBlob(c, strings.NewReader("cancel")); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	now := time.Now()
	_, err := s.SaveFileSnapshot(ctx, "t", model.FileIdentity{Kind: model.FileIdentityServerVersion, Version: "v", SHA256: strings.Repeat("0", 64), StartedAt: now, CompletedAt: now}, strings.NewReader("wrong"))
	if err == nil {
		t.Fatal("hash mismatch accepted")
	}
}
func TestConflictResolvePlanResultsAndBackup(t *testing.T) {
	s, _, _ := openTest(t)
	e := bind(t, s, entry(t, s, "a"), "t")
	e, j := stage(t, s, e, "local")
	s.Prepare(ctx, j.ID)
	if err := s.RecordPlan(ctx, j.ID, []model.Operation{{ID: "operation", Kind: "replace"}}, e.Local); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordOperation(ctx, j.ID, 0, json.RawMessage(`{"result":"ok"}`)); err != nil {
		t.Fatal(err)
	}
	c := model.Conflict{EntryID: e.ID, Generation: e.Generation, Local: e.Local, Remote: e.Local, RemoteRevision: "r"}
	if err := s.SaveConflict(ctx, c); err != nil {
		t.Fatal(err)
	}
	cs, err := s.Conflicts(ctx)
	if err != nil || len(cs) != 1 {
		t.Fatal(cs, err)
	}
	if _, _, err = s.Resolve(ctx, model.ResolveRequest{ConflictID: cs[0].ID, ExpectedGeneration: e.Generation, ExpectedRemoteRevision: "wrong", Content: []byte("resolution")}); !errors.Is(err, model.ErrVersionChanged) {
		t.Fatal(err)
	}
	got, nj, err := s.Resolve(ctx, model.ResolveRequest{ConflictID: cs[0].ID, ExpectedGeneration: e.Generation, ExpectedRemoteRevision: "r", Content: []byte("resolution")})
	if err != nil || got.Generation != e.Generation+1 || nj.State != model.StatusQueued {
		t.Fatal(got, nj, err)
	}
	var backup bytes.Buffer
	if err = s.Backup(ctx, &backup); err != nil {
		t.Fatal(err)
	}
	if backup.Len() == 0 {
		t.Fatal("empty backup")
	}
}
func TestSchemaAndTraversalRejection(t *testing.T) {
	s, d, _ := openTest(t)
	for _, p := range []string{"../x", "/x", "a//b", "a/../b", "a\\b", "."} {
		if _, err := s.EnsureEntry(ctx, p, model.KindFile); err == nil {
			t.Fatalf("accepted %q", p)
		}
	}
	s.Close()
	db, err := bolt.Open(filepath.Join(d, "store.db"), 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	db.Update(func(tx *bolt.Tx) error { return tx.Bucket([]byte("meta")).Put([]byte("schema_version"), []byte("999")) })
	db.Close()
	if s, err := Open(d); err == nil {
		s.Close()
		t.Fatal("future schema accepted")
	}
}

func TestReopenInflightRequiresVerification(t *testing.T) {
	s, d, sc := openTest(t)
	_, j := stage(t, s, entry(t, s, "uncertain"), "input")
	if _, _, err := s.Prepare(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err := Open(d)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.BindScope(ctx, sc); err != nil {
		t.Fatal(err)
	}
	got, err := s.Job(ctx, j.ID)
	if err != nil || got.State != model.StatusAwaitingVerification {
		t.Fatalf("interrupted inflight must reconcile: %+v %v", got, err)
	}
}
func TestRootAndNamespaceActionIntents(t *testing.T) {
	s, d, sc := openTest(t)
	root, err := s.EnsureEntry(ctx, "", model.KindFolder)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.PrepareNamespace(ctx, model.NamespaceOp{Kind: "delete", Src: root.Remote}); err == nil {
		t.Fatal("deleted root")
	}
	op, err := s.PrepareNamespace(ctx, model.NamespaceOp{Kind: "mkdir", Dst: "directory"})
	if err != nil {
		t.Fatal(err)
	}
	s.SealNamespace(ctx, op.ID)
	dir, err := s.CommitNamespace(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	actions, _ := s.NamespaceActions(ctx)
	if len(actions) != 0 {
		t.Fatal("action dispatched before Finish")
	}
	s.Close()
	s, err = Open(d)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	s.BindScope(ctx, sc)
	pending, _ := s.PendingNamespace(ctx)
	if len(pending) != 1 || pending[0].State != "committed" {
		t.Fatal(pending)
	}
	if err = s.FinishNamespace(ctx, op.ID); err != nil {
		t.Fatal(err)
	}
	actions, _ = s.NamespaceActions(ctx)
	if len(actions) != 1 || actions[0].ResultID != dir.ID {
		t.Fatal(actions)
	}
	if err := s.AcknowledgeNamespaceAction(ctx, op.ID); !errors.Is(err, ErrInvalidState) {
		t.Fatal(err)
	}
	if err := s.BindNamespaceFolder(ctx, op.ID, "new-folder"); err != nil {
		t.Fatal(err)
	}
	if err := s.AcknowledgeNamespaceAction(ctx, op.ID); err != nil {
		t.Fatal(err)
	}
	actions, _ = s.NamespaceActions(ctx)
	if len(actions) != 0 {
		t.Fatal(actions)
	}
}
func TestPlanBlobIncludesPersistedOperationIDs(t *testing.T) {
	s, _, _ := openTest(t)
	e, j := stage(t, s, entry(t, s, "a"), "input")
	s.Prepare(ctx, j.ID)
	if err := s.RecordPlan(ctx, j.ID, []model.Operation{{Kind: "replace"}}, e.Local); err != nil {
		t.Fatal(err)
	}
	journal, err := s.Journal(ctx, j.ID)
	if err != nil {
		t.Fatal(err)
	}
	var plan []model.Operation
	if err = json.Unmarshal([]byte(read(t, s, journal.Job.Plan)), &plan); err != nil {
		t.Fatal(err)
	}
	if len(plan) != 1 || plan[0].ID == "" || plan[0].ID != journal.Job.OperationIDs[0] {
		t.Fatalf("plan and journal identity differ %+v %+v", plan, journal)
	}
}
func TestSnapshotRetentionLatestTenOrThirtyDays(t *testing.T) {
	s, _, _ := openTest(t)
	var snapshots []model.Snapshot
	for i := 0; i < 12; i++ {
		snapshots = append(snapshots, fileSnapshot(t, s, "same-token", strings.Repeat("x", i+1)))
	}
	old := time.Now().Add(-60 * 24 * time.Hour)
	if err := s.db.Update(func(tx *bolt.Tx) error {
		for i, sn := range snapshots {
			var r snapshotRecord
			if err := get(tx, "snapshots", string(sn.ID), &r); err != nil {
				return err
			}
			r.SavedAt = old.Add(time.Duration(i) * time.Minute)
			if i == 0 {
				r.SavedAt = time.Now()
			}
			if err := put(tx, "snapshots", string(sn.ID), r); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GC(ctx, GCOptions{Now: time.Now()}); err != nil {
		t.Fatal(err)
	}
	for i, sn := range snapshots {
		_, err := s.Snapshot(ctx, sn.ID)
		if i == 1 || i == 2 {
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("old extra %d retained: %v", i, err)
			}
		} else if err != nil {
			t.Fatalf("protected %d removed: %v", i, err)
		}
	}
}
func TestSpaceAdmissionDoesNotDeleteAcceptedData(t *testing.T) {
	s, _, _ := openTest(t)
	e, _ := stage(t, s, entry(t, s, "accepted"), "protected")
	s.space = func(_ context.Context, _ string, size int64) error {
		if size >= LargeSnapshotBytes {
			return ErrSpace
		}
		return nil
	}
	if _, _, err := s.Stage(ctx, e.ID, nil, io.LimitReader(zeroReader{}, LargeSnapshotBytes)); !errors.Is(err, ErrSpace) {
		t.Fatal(err)
	}
	got, _ := s.Entry(ctx, e.ID)
	if got.Generation != e.Generation || read(t, s, got.Local) != "protected" {
		t.Fatal(got)
	}
}

type zeroReader struct{}

func (zeroReader) Read(p []byte) (int, error) { clear(p); return len(p), nil }
func TestInitialAdoptionCannotResurrectTombstone(t *testing.T) {
	s, _, _ := openTest(t)
	e := bind(t, s, entry(t, s, "dead"), "token")
	sn := fileSnapshot(t, s, "token", "download")
	op, err := s.PrepareNamespace(ctx, model.NamespaceOp{Kind: "delete", Src: e.Remote})
	if err != nil {
		t.Fatal(err)
	}
	s.SealNamespace(ctx, op.ID)
	s.CommitNamespace(ctx, op.ID)
	s.FinishNamespace(ctx, op.ID)
	if _, err = s.AdoptFileSnapshot(ctx, e.ID, e.Generation, sn.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("adopted deleted entry: %v", err)
	}
}

func TestConfirmTargetInflightAcrossNamespaceReplacement(t *testing.T) {
	s, _, _ := openTest(t)
	src, _ := stage(t, s, entry(t, s, "source"), "new-target")
	dst := bind(t, s, entry(t, s, "target"), "target-token")
	dst, oldjob := stage(t, s, dst, "old-target")
	if _, _, err := s.Prepare(ctx, oldjob.ID); err != nil {
		t.Fatal(err)
	}
	op, err := s.PrepareNamespace(ctx, model.NamespaceOp{Kind: "replace", Src: src.Remote, Dst: dst.Remote})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SealNamespace(ctx, op.ID); err != nil {
		t.Fatal(err)
	}
	next, err := s.CommitNamespace(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	sn := fileSnapshot(t, s, dst.Token, "old-target")
	candidate, err := s.Confirm(ctx, oldjob.ID, sn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CommitPublication(ctx, candidate); !errors.Is(err, model.ErrGenerationChanged) {
		t.Fatal(err)
	}
	if err = s.FinishNamespace(ctx, op.ID); err != nil {
		t.Fatal(err)
	}
	jobs, _ := s.Pending(ctx)
	var newest model.Job
	for _, j := range jobs {
		if j.EntryID == dst.ID {
			newest = j
		}
	}
	prepared, current, err := s.Prepare(ctx, newest.ID)
	if err != nil || prepared.BaseRemote != sn.ID || prepared.BaseLocal != oldjob.Local || current.Local != next.Local {
		t.Fatal(prepared, current, err)
	}
}
func TestRenameAbortAndFrozenTree(t *testing.T) {
	s, _, _ := openTest(t)
	if _, err := s.EnsureEntry(ctx, "dir", model.KindFolder); err != nil {
		t.Fatal(err)
	}
	e, original := stage(t, s, entry(t, s, "dir/file"), "old")
	op, err := s.PrepareNamespace(ctx, model.NamespaceOp{Kind: "rename", Src: "dir", Dst: "moved"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.EnsureEntry(ctx, "dir/new", model.KindFile); !errors.Is(err, ErrFrozen) {
		t.Fatal(err)
	}
	e, flushed := stage(t, s, e, "flush")
	if err = s.AbortNamespace(ctx, op.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.Prepare(ctx, original.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("obsolete queued generation remained dispatchable: %v", err)
	}
	if read(t, s, original.Local) != "old" {
		t.Fatal("coalescing lost original accepted bytes")
	}
	if _, _, err = s.Prepare(ctx, flushed.ID); err != nil {
		t.Fatal(err)
	}
	activeRename, err := s.PrepareNamespace(ctx, model.NamespaceOp{Kind: "rename", Src: "dir", Dst: "moved"})
	if err != nil {
		t.Fatalf("local rename must preserve active upload work: %v", err)
	}
	if err = s.AbortNamespace(ctx, activeRename.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.EntryByRemote(ctx, "dir/file"); err != nil {
		t.Fatal(err)
	}
	if read(t, s, e.Local) != "flush" {
		t.Fatal("abort lost accepted flush")
	}
}
func TestCASRemoteCheckpointAndCleanRefresh(t *testing.T) {
	s, _, _ := openTest(t)
	e := bind(t, s, entry(t, s, "a"), "token")
	sn1 := fileSnapshot(t, s, "token", "first")
	sn2 := fileSnapshot(t, s, "token", "second")
	c1, err := s.OfferRemote(ctx, e.ID, 0, sn1, sn1.FileData)
	if err != nil {
		t.Fatal(err)
	}
	c2, err := s.OfferRemote(ctx, e.ID, 0, sn2, sn2.FileData)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.CommitPublication(ctx, c2); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CommitPublication(ctx, c1); !errors.Is(err, model.ErrVersionChanged) {
		t.Fatal(err)
	}
	got, _ := s.Entry(ctx, e.ID)
	if got.Visible != sn2.FileData || got.LocalCheckpoint != sn2.FileData || got.Status != model.StatusSynced {
		t.Fatal(got)
	}
}
func TestRetainsAllSnapshotsInsideThirtyDays(t *testing.T) {
	s, _, _ := openTest(t)
	var snapshots []model.Snapshot
	for i := 0; i < 12; i++ {
		snapshots = append(snapshots, fileSnapshot(t, s, "token", strings.Repeat("z", i+1)))
	}
	if _, err := s.GC(ctx, GCOptions{Now: time.Now().Add(29 * 24 * time.Hour), CleanBudgetBytes: 1}); err != nil {
		t.Fatal(err)
	}
	for _, sn := range snapshots {
		if _, err := s.Snapshot(ctx, sn.ID); err != nil {
			t.Fatal(err)
		}
		if read(t, s, sn.FileData) == "" {
			t.Fatal("lost retained content")
		}
	}
}
func TestSingleWorkerAndScopeReadGuard(t *testing.T) {
	s, d, _ := openTest(t)
	if second, err := Open(d); !errors.Is(err, ErrLocked) {
		if second != nil {
			second.Close()
		}
		t.Fatalf("second writer: %v", err)
	}
	e, _ := stage(t, s, entry(t, s, "a"), "bytes")
	s.Close()
	s, err := Open(d)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err = s.Entry(ctx, e.ID); !errors.Is(err, ErrUnbound) {
		t.Fatalf("unbound entry read: %v", err)
	}
	if r, err := s.ReadBlob(ctx, e.Local); !errors.Is(err, ErrUnbound) {
		if r != nil {
			r.Close()
		}
		t.Fatalf("unbound blob read: %v", err)
	}
}

func TestDiscoveryCannotBindPendingLocalCreation(t *testing.T) {
	s, _, _ := openTest(t)
	e, _ := stage(t, s, entry(t, s, "local"), "local work")
	if _, err := s.ObserveEntry(ctx, Observation{Remote: e.Remote, Kind: e.Kind, Token: "unrelated-resource"}); !errors.Is(err, model.ErrIdentityChanged) {
		t.Fatalf("pending creation rebound: %v", err)
	}
	got, _ := s.Entry(ctx, e.ID)
	if got.Token != "" || got.Local != e.Local {
		t.Fatal(got)
	}
}
func TestFilePublicationCannotMixSnapshotAndVisibleBytes(t *testing.T) {
	s, _, _ := openTest(t)
	e := bind(t, s, entry(t, s, "a"), "token")
	sn := fileSnapshot(t, s, e.Token, "observed")
	wrong, err := s.PutBlob(ctx, strings.NewReader("other bytes"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.OfferRemote(ctx, e.ID, e.Generation, sn, wrong); !errors.Is(err, model.ErrUnsupportedContent) {
		t.Fatalf("mixed snapshot and bytes: %v", err)
	}
}
func TestPersistenceFailuresReopenWithoutAcceptance(t *testing.T) {
	for _, point := range []string{"fsync", "rename", "dirsync", "txn"} {
		t.Run(point, func(t *testing.T) {
			s, d, sc := openTest(t)
			e := entry(t, s, "file")
			if point == "txn" {
				if err := s.db.Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				s.io = failingIO{point}
			}
			if _, _, err := s.Stage(ctx, e.ID, nil, strings.NewReader("not accepted")); err == nil {
				t.Fatal("accepted despite fault")
			}
			s.Close()
			s, err := Open(d)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if err = s.BindScope(ctx, sc); err != nil {
				t.Fatal(err)
			}
			got, err := s.Entry(ctx, e.ID)
			if err != nil || got.Generation != 0 || got.Local != "" {
				t.Fatal(got, err)
			}
			jobs, err := s.Pending(ctx)
			if err != nil || len(jobs) != 0 {
				t.Fatal(jobs, err)
			}
		})
	}
}
func TestPrepareWaiterWakesAndRebinds(t *testing.T) {
	s, _, _ := openTest(t)
	e := bind(t, s, entry(t, s, "a"), "token")
	e, j1 := stage(t, s, e, "one")
	if _, _, err := s.Prepare(ctx, j1.ID); err != nil {
		t.Fatal(err)
	}
	_, j2 := stage(t, s, e, "two")
	type outcome struct {
		j   model.Job
		err error
	}
	result := make(chan outcome, 1)
	wait, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	go func() { j, _, err := s.Prepare(wait, j2.ID); result <- outcome{j, err} }()
	sn := fileSnapshot(t, s, e.Token, "one")
	if _, err := s.Confirm(ctx, j1.ID, sn); err != nil {
		t.Fatal(err)
	}
	got := <-result
	if got.err != nil || got.j.BaseLocal != j1.Local || got.j.BaseRemote != sn.ID {
		t.Fatal(got)
	}
}

func TestOlderConfirmationPreservesNewGenerationConflict(t *testing.T) {
	s, _, _ := openTest(t)
	e := bind(t, s, entry(t, s, "a"), "token")
	e, j1 := stage(t, s, e, "g1")
	if _, _, err := s.Prepare(ctx, j1.ID); err != nil {
		t.Fatal(err)
	}
	e, _ = stage(t, s, e, "g2")
	if err := s.SaveConflict(ctx, model.Conflict{EntryID: e.ID, Generation: e.Generation, Local: e.Local, Reason: "new generation conflict"}); err != nil {
		t.Fatal(err)
	}
	sn := fileSnapshot(t, s, e.Token, "g1")
	if _, err := s.Confirm(ctx, j1.ID, sn); err != nil {
		t.Fatal(err)
	}
	got, _ := s.Entry(ctx, e.ID)
	if got.Status != model.StatusConflict || got.Local != e.Local {
		t.Fatalf("G1 cleared G2 conflict: %+v", got)
	}
}
func TestPrepareCannotDispatchQueuedGenerationsOutOfOrder(t *testing.T) {
	s, _, _ := openTest(t)
	e, j1 := stage(t, s, entry(t, s, "a"), "first")
	if _, _, err := s.Prepare(ctx, j1.ID); err != nil {
		t.Fatal(err)
	}
	_, j2 := stage(t, s, e, "second")
	wait, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if _, _, err := s.Prepare(wait, j2.ID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("G2 jumped pending G1: %v", err)
	}
	jobs, err := s.Pending(ctx)
	if err != nil || len(jobs) != 2 || jobs[0].ID != j1.ID || jobs[1].ID != j2.ID {
		t.Fatal(jobs, err)
	}
}

func TestPublicationRecoveryAfterConfirmBeforePublish(t *testing.T) {
	s, d, sc := openTest(t)
	e := bind(t, s, entry(t, s, "a"), "token")
	e, j := stage(t, s, e, "input")
	if _, _, err := s.Prepare(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	sn := fileSnapshot(t, s, e.Token, "input")
	if _, err := s.Confirm(ctx, j.ID, sn); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err := Open(d)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.BindScope(ctx, sc); err != nil {
		t.Fatal(err)
	}
	candidates, err := s.PendingPublications(ctx)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("lost accepted publication: %v %v", candidates, err)
	}
	got, err := s.CommitPublication(ctx, candidates[0])
	if err != nil || got.Status != model.StatusSynced || got.Visible != sn.FileData {
		t.Fatal(got, err)
	}
	candidates, err = s.PendingPublications(ctx)
	if err != nil || len(candidates) != 0 {
		t.Fatal(candidates, err)
	}
}
