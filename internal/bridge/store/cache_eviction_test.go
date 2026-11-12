package store

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/model"
	bolt "go.etcd.io/bbolt"
)

func cleanCacheEntry(t *testing.T, s *Store, p, body string) model.Entry {
	t.Helper()
	e := bind(t, s, entry(t, s, p), "token-"+p)
	sn := fileSnapshot(t, s, e.Token, body)
	e, err := s.AdoptFileSnapshot(ctx, e.ID, e.Generation, sn.ID)
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func TestCleanCacheEvictPreservesPlaceholderAndRehydratesAcrossRestart(t *testing.T) {
	s, dir, scope := openTest(t)
	e := cleanCacheEntry(t, s, "download.bin", "downloaded bytes")
	ageCacheBlobs(t, s)
	before := time.Now().UTC()
	cs, err := s.CleanCacheCandidates(ctx)
	if err != nil || len(cs) != 1 || cs[0].Bytes != 16 || cs[0].AccessedAt.Before(before) {
		t.Fatalf("candidates %+v %v", cs, err)
	}
	old := before.Add(-8 * 24 * time.Hour)
	if err = s.TouchBodyAccess(ctx, e.ID, old); err != nil {
		t.Fatal(err)
	}
	m, _ := s.Metadata(ctx, e.ID)
	if m.LastBodyAccess.Before(before) {
		t.Fatal("old touch moved LRU backwards")
	}
	acknowledgeHistory(t, s)
	ok, err := s.EvictCleanEntry(ctx, e.ID, e.Generation+1)
	if err != nil || ok {
		t.Fatalf("stale generation: %v %v", ok, err)
	}
	acknowledgeHistory(t, s)
	ok, err = s.EvictCleanEntry(ctx, e.ID, e.Generation)
	if err != nil || !ok {
		t.Fatalf("evict: %v %v", ok, err)
	}
	got, err := s.EntryByRemote(ctx, e.Remote)
	if err != nil || got.ID != e.ID || got.Token != e.Token || got.Generation != e.Generation || got.Local != "" || got.Visible != "" || got.RemoteCheckpoint != "" {
		t.Fatalf("placeholder %+v %v", got, err)
	}
	if _, err = os.Stat(s.blobPath(e.Local)); !os.IsNotExist(err) {
		t.Fatalf("body not reclaimed: %v", err)
	}
	if err = s.Validate(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err = s.BindScope(ctx, scope); err != nil {
		t.Fatal(err)
	}
	if err = s.Validate(ctx); err != nil {
		t.Fatal(err)
	}
	sn := fileSnapshot(t, s, e.Token, "downloaded again")
	got, err = s.AdoptFileSnapshot(ctx, e.ID, e.Generation, sn.ID)
	if err != nil || read(t, s, got.Local) != "downloaded again" {
		t.Fatalf("rehydrate %+v %v", got, err)
	}
}
func TestCleanCacheEvictsConfirmedEditAndTerminalHistory(t *testing.T) {
	s, _, _ := openTest(t)
	e := cleanCacheEntry(t, s, "edited.bin", "before")
	e, j := stage(t, s, e, "after")
	if _, _, err := s.Prepare(ctx, j.ID); err != nil {
		t.Fatal(err)
	}
	sn := fileSnapshot(t, s, e.Token, "after")
	c, err := s.Confirm(ctx, j.ID, sn)
	if err != nil {
		t.Fatal(err)
	}
	e, err = s.CommitPublication(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	// Finished create records also retain input until explicitly pruned.
	err = s.update(ctx, func(tx *bolt.Tx) error {
		return put(tx, "createops", "finished", model.CreateOp{ID: "finished", JobID: j.ID, EntryID: e.ID, Input: j.Local, TemporaryName: "unique", State: "finished"})
	})
	if err != nil {
		t.Fatal(err)
	}
	ageCacheBlobs(t, s)
	acknowledgeHistory(t, s)
	ok, err := s.EvictCleanEntry(ctx, e.ID, e.Generation)
	if err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	if _, err = s.Job(ctx, j.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("job retained: %v", err)
	}
	if err = s.Validate(ctx); err != nil {
		t.Fatal(err)
	}
	for _, b := range []model.BlobID{j.Local, j.BaseLocal} {
		if _, err = os.Stat(s.blobPath(b)); !os.IsNotExist(err) {
			t.Fatalf("history retained %s: %v", b, err)
		}
	}
}
func TestCleanCacheProtectsDurableWork(t *testing.T) {
	cases := []struct {
		name    string
		protect func(*testing.T, *Store, model.Entry)
	}{
		{"queued", func(t *testing.T, s *Store, e model.Entry) { stage(t, s, e, "pending") }},
		{"inflight", func(t *testing.T, s *Store, e model.Entry) {
			_, j := stage(t, s, e, "pending")
			if _, _, err := s.Prepare(ctx, j.ID); err != nil {
				t.Fatal(err)
			}
		}},
		{"unknown", func(t *testing.T, s *Store, e model.Entry) {
			_, j := stage(t, s, e, "pending")
			s.Prepare(ctx, j.ID)
			if err := s.MarkUnknown(ctx, j.ID); err != nil {
				t.Fatal(err)
			}
		}},
		{"frozen", func(t *testing.T, s *Store, e model.Entry) {
			if _, err := s.PrepareNamespace(ctx, model.NamespaceOp{Kind: "rename", Src: e.Remote, Dst: "other.bin"}); err != nil {
				t.Fatal(err)
			}
		}},
		{"conflict", func(t *testing.T, s *Store, e model.Entry) {
			if err := s.update(ctx, func(tx *bolt.Tx) error {
				return put(tx, "conflicts", "conflict", model.Conflict{ID: model.ConflictID(uuid.NewString()), EntryID: e.ID, Local: e.Local, Resolved: true})
			}); err != nil {
				t.Fatal(err)
			}
		}},
		{"recovery-entry", func(t *testing.T, s *Store, e model.Entry) {
			if err := s.update(ctx, func(tx *bolt.Tx) error {
				return put(tx, "recovery", "entry", recoveryRecord{Entries: []model.Entry{e}})
			}); err != nil {
				t.Fatal(err)
			}
		}},
		{"recovery-snapshot", func(t *testing.T, s *Store, e model.Entry) {
			if err := s.update(ctx, func(tx *bolt.Tx) error {
				return put(tx, "recovery", "snapshot", recoveryRecord{SnapshotID: e.RemoteCheckpoint})
			}); err != nil {
				t.Fatal(err)
			}
		}},
		{"publication", func(t *testing.T, s *Store, e model.Entry) {
			sn := fileSnapshot(t, s, e.Token, "cloud")
			if _, err := s.OfferRemote(ctx, e.ID, e.Generation, sn, ""); err != nil {
				t.Fatal(err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, _, _ := openTest(t)
			e := cleanCacheEntry(t, s, "protected.bin", "safe")
			tc.protect(t, s, e)
			now, _ := s.Entry(ctx, e.ID)
			acknowledgeHistory(t, s)
			ok, err := s.EvictCleanEntry(ctx, e.ID, now.Generation)
			if err != nil || ok {
				t.Fatalf("unsafe eviction %v %v", ok, err)
			}
			if read(t, s, e.Local) != "safe" {
				t.Fatal("lost body")
			}
			if err = s.Validate(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestCleanCacheLeaseAndSharedBlobProtection(t *testing.T) {
	s, _, _ := openTest(t)
	a := cleanCacheEntry(t, s, "a.bin", "shared")
	b := cleanCacheEntry(t, s, "b.bin", "shared")
	r, err := s.ReadBlob(ctx, a.Local)
	if err != nil {
		t.Fatal(err)
	}
	acknowledgeHistory(t, s)
	ok, err := s.EvictCleanEntry(ctx, a.ID, a.Generation)
	if err != nil || ok {
		t.Fatalf("leased: %v %v", ok, err)
	}
	r.Close()
	ageCacheBlobs(t, s)
	acknowledgeHistory(t, s)
	ok, err = s.EvictCleanEntry(ctx, a.ID, a.Generation)
	if err != nil || !ok {
		t.Fatalf("shared: %v %v", ok, err)
	}
	if read(t, s, b.Local) != "shared" {
		t.Fatal("deleted another entry's body")
	}
	if err = s.Validate(ctx); err != nil {
		t.Fatal(err)
	}
	acknowledgeHistory(t, s)
	ok, err = s.EvictCleanEntry(ctx, b.ID, b.Generation)
	if err != nil || !ok {
		t.Fatalf("last: %v %v", ok, err)
	}
	if _, err = os.Stat(s.blobPath(a.Local)); !os.IsNotExist(err) {
		t.Fatalf("last body retained: %v", err)
	}
}
func TestCleanCacheRejectsLocalReceipt(t *testing.T) {
	s, _, _ := openTest(t)
	e := entry(t, s, "local.bin")
	e, _, err := s.StageLocalOnly(ctx, e.ID, nil, strings.NewReader("unuploaded"))
	if err != nil {
		t.Fatal(err)
	}
	acknowledgeHistory(t, s)
	ok, err := s.EvictCleanEntry(ctx, e.ID, e.Generation)
	if err != nil || ok {
		t.Fatalf("%v %v", ok, err)
	}
}

func ageCacheBlobs(t *testing.T, s *Store) {
	t.Helper()
	files, err := os.ReadDir(filepath.Join(s.dir, "blobs"))
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now().Add(-PreparedBlobGrace - time.Hour)
	for _, f := range files {
		if err = os.Chtimes(filepath.Join(s.dir, "blobs", f.Name()), at, at); err != nil {
			t.Fatal(err)
		}
	}
}
func TestCleanCacheEvictionRespectsPreparationGrace(t *testing.T) {
	s, _, _ := openTest(t)
	e := cleanCacheEntry(t, s, "young.bin", "same content")
	ageCacheBlobs(t, s)
	// A second producer may be between PutBlob and its accepting transaction.
	b, err := s.PutBlob(ctx, strings.NewReader("same content"))
	if err != nil {
		t.Fatal(err)
	}
	if b != e.Local {
		t.Fatal("different content address")
	}
	acknowledgeHistory(t, s)
	ok, err := s.EvictCleanEntry(ctx, e.ID, e.Generation)
	if err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	if read(t, s, b) != "same content" {
		t.Fatal("prepared data lost")
	}
	if _, err = s.GC(ctx, GCOptions{}); err != nil {
		t.Fatal(err)
	}
	if read(t, s, b) != "same content" {
		t.Fatal("prepared data collected")
	}
	ageCacheBlobs(t, s)
	if _, err = s.GC(ctx, GCOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(s.blobPath(b)); !os.IsNotExist(err) {
		t.Fatalf("old clean body retained: %v", err)
	}
	if err = s.Validate(ctx); err != nil {
		t.Fatal(err)
	}
}

// Entry status alone is insufficient: a newer clean entry can coexist with
// historical dispatch uncertainty or recovery records that must remain durable.
func TestCleanCacheRechecksHistoricalJournals(t *testing.T) {
	for _, state := range []model.Status{model.StatusQueued, model.StatusMerging, model.StatusUploading, model.StatusAwaitingVerification, model.StatusAwaitingPublish, model.StatusSynced} {
		t.Run(string(state), func(t *testing.T) {
			s, _, _ := openTest(t)
			e := cleanCacheEntry(t, s, "historical.bin", "clean")
			j := model.Job{ID: model.JobID(uuid.NewString()), EntryID: e.ID, Generation: e.Generation, Local: e.Local, BaseRemote: e.RemoteCheckpoint, State: state}
			err := s.update(ctx, func(tx *bolt.Tx) error { return saveJob(tx, jobRecord{Job: j}) })
			if err != nil {
				t.Fatal(err)
			}
			acknowledgeHistory(t, s)
			ok, err := s.EvictCleanEntry(ctx, e.ID, e.Generation)
			if err != nil || ok {
				t.Fatalf("unconfirmed historical job: %v %v", ok, err)
			}
			if _, err = s.Job(ctx, j.ID); err != nil {
				t.Fatal("lost protected job", err)
			}
			if err = s.Validate(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestCleanCacheRecoveryProtectsConfirmedJobAndFinishedNamespace(t *testing.T) {
	for _, kind := range []string{"job", "namespace", "namespace-pending", "namespace-dispatched"} {
		t.Run(kind, func(t *testing.T) {
			s, _, _ := openTest(t)
			e := cleanCacheEntry(t, s, "backup.bin", "protected")
			err := s.update(ctx, func(tx *bolt.Tx) error {
				if kind == "job" {
					j := model.Job{ID: model.JobID(uuid.NewString()), EntryID: e.ID, Local: e.Local, BaseRemote: e.RemoteCheckpoint, State: model.StatusSynced}
					if err := saveJob(tx, jobRecord{Job: j, CompletedAt: time.Now()}); err != nil {
						return err
					}
					return put(tx, "recovery", "protected-job", recoveryRecord{JobID: j.ID})
				}
				r := namespaceRecord{Op: model.NamespaceOp{ID: "historical-namespace", State: "finished", SrcID: e.ID, Src: e.Remote, Input: e.Local}, Before: []model.Entry{e}}
				r.ActionPending = kind == "namespace-pending"
				r.Dispatched = kind == "namespace-dispatched"
				if err := put(tx, "namespace", r.Op.ID, r); err != nil {
					return err
				}
				if kind == "namespace" {
					return put(tx, "recovery", "protected-namespace", recoveryRecord{NamespaceID: r.Op.ID})
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			cs, err := s.CleanCacheCandidates(ctx)
			if err != nil || len(cs) != 0 {
				t.Fatalf("protected candidate %+v %v", cs, err)
			}
			acknowledgeHistory(t, s)
			ok, err := s.EvictCleanEntry(ctx, e.ID, e.Generation)
			if err != nil || ok {
				t.Fatalf("%v %v", ok, err)
			}
			if err = s.Validate(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestCleanCachePrunesOnlyConfirmedNamespaceHistory(t *testing.T) {
	s, _, _ := openTest(t)
	e := cleanCacheEntry(t, s, "terminal.bin", "clean")
	err := s.update(ctx, func(tx *bolt.Tx) error {
		return put(tx, "namespace", "terminal", namespaceRecord{Op: model.NamespaceOp{ID: "terminal", State: "finished", SrcID: e.ID, Src: e.Remote, Input: e.Local}, Before: []model.Entry{e}})
	})
	if err != nil {
		t.Fatal(err)
	}
	ageCacheBlobs(t, s)
	acknowledgeHistory(t, s)
	ok, err := s.EvictCleanEntry(ctx, e.ID, e.Generation)
	if err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	if _, err = s.NamespaceJournal(ctx, "terminal"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("terminal history retained: %v", err)
	}
	if err = s.Validate(ctx); err != nil {
		t.Fatal(err)
	}
}
func TestCleanCacheDocxEvictsSidecarsAndPreservesPreparedBlob(t *testing.T) {
	s, _, _ := openTest(t)
	e, err := s.ObserveEntry(ctx, Observation{Remote: "document.md", Kind: model.KindDocx, Token: "document-token"})
	if err != nil {
		t.Fatal(err)
	}
	putBody := func(body string) model.BlobID {
		b, err := s.PutBlob(ctx, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		return b
	}
	sn := model.Snapshot{Token: e.Token, XML: putBody("xml"), Markdown: putBody("markdown"), BlockMap: putBody("blocks")}
	c, err := s.OfferRemote(ctx, e.ID, e.Generation, sn, "")
	if err != nil {
		t.Fatal(err)
	}
	e, err = s.CommitPublication(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	ageCacheBlobs(t, s)
	prepared := putBody("prepared separately")
	acknowledgeHistory(t, s)
	ok, err := s.EvictCleanEntry(ctx, e.ID, e.Generation)
	if err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	for _, b := range []model.BlobID{sn.XML, sn.Markdown, sn.BlockMap} {
		if _, err = os.Stat(s.blobPath(b)); !os.IsNotExist(err) {
			t.Fatalf("sidecar retained %s: %v", b, err)
		}
	}
	if read(t, s, prepared) != "prepared separately" {
		t.Fatal("unrelated prepared blob lost")
	}
	if err = s.Validate(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestCleanCacheAccessCASProtectsReadAfterCandidateScan(t *testing.T) {
	s, _, _ := openTest(t)
	e := cleanCacheEntry(t, s, "recently-read.bin", "body")
	cs, err := s.CleanCacheCandidates(ctx)
	if err != nil || len(cs) != 1 {
		t.Fatalf("%+v %v", cs, err)
	}
	candidate := cs[0]
	if err = s.TouchBodyAccess(ctx, e.ID, candidate.AccessedAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	acknowledgeHistory(t, s)
	ok, err := s.EvictCleanEntry(ctx, e.ID, e.Generation, candidate.AccessedAt)
	if err != nil || ok {
		t.Fatalf("stale access CAS: %v %v", ok, err)
	}
	if read(t, s, e.Local) != "body" {
		t.Fatal("recently read body lost")
	}
	fresh, err := s.CleanCacheCandidates(ctx)
	if err != nil || len(fresh) != 1 {
		t.Fatalf("%+v %v", fresh, err)
	}
	acknowledgeHistory(t, s)
	ok, err = s.EvictCleanEntry(ctx, e.ID, e.Generation, fresh[0].AccessedAt)
	if err != nil || !ok {
		t.Fatalf("current access CAS: %v %v", ok, err)
	}
	if err = s.Validate(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestCleanCacheEvictionRejectsDelayedSnapshotAcceptance(t *testing.T) {
	s, _, _ := openTest(t)
	e := cleanCacheEntry(t, s, "delayed.bin", "old body")
	sn, err := s.Snapshot(ctx, e.RemoteCheckpoint)
	if err != nil {
		t.Fatal(err)
	}
	sn.ID = model.SnapshotID(uuid.NewString())
	prepared, err := s.prepareSnapshot(sn)
	if err != nil {
		t.Fatal(err)
	}
	ageCacheBlobs(t, s)
	acknowledgeHistory(t, s)
	ok, err := s.EvictCleanEntry(ctx, e.ID, e.Generation)
	if err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	// Model a caller whose preflight completed before the eviction but whose
	// accepting transaction ran afterwards. The commit must recheck its edges.
	err = s.update(ctx, func(tx *bolt.Tx) error { return s.saveSnapshot(tx, prepared) })
	if !os.IsNotExist(err) {
		t.Fatalf("accepted missing snapshot body: %v", err)
	}
	if err = s.Validate(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestCleanCacheEvictsSupersededUnsentAutosavesAfterCurrentConfirmation(t *testing.T) {
	s, _, _ := openTest(t)
	e := cleanCacheEntry(t, s, "autosave.bin", "original")
	e, old := stage(t, s, e, "intermediate autosave")
	e, current := stage(t, s, e, "final save")
	prior, err := s.Journal(ctx, old.ID)
	if err != nil {
		t.Fatal(err)
	}
	if prior.Job.State != model.StatusQueued {
		t.Fatal("expected queued predecessor")
	}
	if err = s.view(ctx, func(tx *bolt.Tx) error {
		r, err := jobTx(tx, old.ID)
		if err != nil {
			return err
		}
		if !r.Superseded || !unsentJob(r) {
			t.Fatal("predecessor has no unsent supersession proof")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.Prepare(ctx, current.ID); err != nil {
		t.Fatal(err)
	}
	sn := fileSnapshot(t, s, e.Token, "final save")
	c, err := s.Confirm(ctx, current.ID, sn)
	if err != nil {
		t.Fatal(err)
	}
	e, err = s.CommitPublication(ctx, c)
	if err != nil {
		t.Fatal(err)
	}
	candidates, err := s.CleanCacheCandidates(ctx)
	if err != nil || len(candidates) != 1 {
		t.Fatalf("autosave candidates %+v %v", candidates, err)
	}
	ageCacheBlobs(t, s)
	acknowledgeHistory(t, s)
	ok, err := s.EvictCleanEntry(ctx, e.ID, e.Generation)
	if err != nil || !ok {
		t.Fatalf("confirmed final save with unsent predecessor: %v %v", ok, err)
	}
	for _, j := range []model.Job{old, current} {
		if _, err = s.Job(ctx, j.ID); !errors.Is(err, ErrNotFound) {
			t.Fatalf("terminal job retained %s: %v", j.ID, err)
		}
	}
	for _, b := range []model.BlobID{old.Local, current.Local} {
		if _, err = os.Stat(s.blobPath(b)); !os.IsNotExist(err) {
			t.Fatalf("terminal body retained %s: %v", b, err)
		}
	}
	if err = s.Validate(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestCleanCacheProtectsAmbiguousSupersededHistory(t *testing.T) {
	for _, kind := range []string{"unknown", "dispatched", "create", "recovery", "missing-current-confirmation"} {
		t.Run(kind, func(t *testing.T) {
			s, _, _ := openTest(t)
			e := cleanCacheEntry(t, s, "ambiguous.bin", "original")
			e, old := stage(t, s, e, "old save")
			e, current := stage(t, s, e, "final save")
			if _, _, err := s.Prepare(ctx, current.ID); err != nil {
				t.Fatal(err)
			}
			sn := fileSnapshot(t, s, e.Token, "final save")
			c, err := s.Confirm(ctx, current.ID, sn)
			if err != nil {
				t.Fatal(err)
			}
			e, err = s.CommitPublication(ctx, c)
			if err != nil {
				t.Fatal(err)
			}
			err = s.update(ctx, func(tx *bolt.Tx) error {
				r, err := jobTx(tx, old.ID)
				if err != nil {
					return err
				}
				switch kind {
				case "unknown":
					r.Job.State = model.StatusAwaitingVerification
				case "dispatched":
					r.Job.Plan = old.Local
					r.Job.OperationIDs = []string{"old-operation"}
					r.Operations = []model.Operation{{ID: "old-operation"}}
				case "create":
					if err = put(tx, "createops", "finished-old", model.CreateOp{ID: "finished-old", EntryID: e.ID, JobID: old.ID, Input: old.Local, TemporaryName: "unique", State: "finished"}); err != nil {
						return err
					}
				case "recovery":
					if err = put(tx, "recovery", "old-job", recoveryRecord{JobID: old.ID}); err != nil {
						return err
					}
				case "missing-current-confirmation":
					latest, er := jobTx(tx, current.ID)
					if er != nil {
						return er
					}
					latest.CompletedAt = time.Time{}
					if err = saveJob(tx, latest); err != nil {
						return err
					}
				}
				return saveJob(tx, r)
			})
			if err != nil {
				t.Fatal(err)
			}
			acknowledgeHistory(t, s)
			ok, err := s.EvictCleanEntry(ctx, e.ID, e.Generation)
			if err != nil || ok {
				t.Fatalf("unsafe superseded history eviction: %v %v", ok, err)
			}
			if read(t, s, old.Local) != "old save" || read(t, s, current.Local) != "final save" {
				t.Fatal("protected history lost")
			}
			if err = s.Validate(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// These eviction cases start after an archive has issued durable receipts.
// Unacknowledged history retention is exercised separately in history_test.go.
func acknowledgeHistory(t *testing.T, s *Store) {
	t.Helper()
	var after uint64
	for {
		events, err := s.HistoryEvents(ctx, after, 500)
		if err != nil {
			t.Fatal(err)
		}
		if len(events) == 0 {
			return
		}
		for _, ev := range events {
			if err = s.AckHistory(ctx, ev.Sequence, ev.ID); err != nil {
				t.Fatal(err)
			}
			after = ev.Sequence
		}
	}
}
