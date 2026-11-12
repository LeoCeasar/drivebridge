package store

import (
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/rclone/rclone/internal/bridge/model"
	bolt "go.etcd.io/bbolt"
)

func TestCancelUnsentLocalFileRetainsHistoryWithoutCloudDelete(t *testing.T) {
	for _, sample := range []struct {
		body  string
		state model.Status
	}{
		{"", model.StatusQueued}, {"complete locally accepted payload", model.StatusQueued},
		{"", model.StatusBlocked}, {"complete locally accepted payload", model.StatusPaused},
	} {
		body := sample.body
		t.Run(string(sample.state)+"/"+body, func(t *testing.T) {
			s, dir, scope := openTest(t)
			e, job := stage(t, s, entry(t, s, "placeholder.txt"), body)
			if sample.state != model.StatusQueued {
				if _, _, err := s.Prepare(ctx, job.ID); err != nil {
					t.Fatal(err)
				}
				if err := s.PauseJob(ctx, job.ID, sample.state, "empty_file_unsupported"); err != nil {
					t.Fatal(err)
				}
				job, _ = s.Job(ctx, job.ID)
			}
			cancelled, err := s.CancelUnsentLocalFile(ctx, e.ID, e.Generation)
			if err != nil || !cancelled {
				t.Fatalf("cancel=%v %v", cancelled, err)
			}
			if _, err = s.EntryByRemote(ctx, e.Remote); !errors.Is(err, ErrNotFound) {
				t.Fatal(err)
			}
			pending, err := s.Pending(ctx)
			if err != nil || len(pending) != 0 {
				t.Fatal(pending, err)
			}
			actions, err := s.NamespaceActions(ctx)
			if err != nil || len(actions) != 0 {
				t.Fatal("local cancellation queued cloud delete", actions, err)
			}
			history, err := s.Job(ctx, job.ID)
			if err != nil || history.State != job.State || read(t, s, history.Local) != body {
				t.Fatalf("history changed: %+v %v", history, err)
			}
			var journal namespaceRecord
			if err = s.view(ctx, func(tx *bolt.Tx) error {
				return each[namespaceRecord](tx, "namespace", func(r namespaceRecord) error {
					if r.Op.SrcID == e.ID {
						journal = r
					}
					return nil
				})
			}); err != nil {
				t.Fatal(err)
			}
			if !journal.LocalCancel || journal.ActionPending || journal.Dispatched || journal.Op.State != "finished" {
				t.Fatalf("not a local-only cancellation journal: %+v", journal)
			}
			if err = s.BeginNamespaceDispatch(ctx, journal.Op.ID, "delete"); !errors.Is(err, ErrInvalidState) {
				t.Fatal("cancelled local file allowed cloud delete", err)
			}
			if err = s.Validate(ctx); err != nil {
				t.Fatal(err)
			}
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if err = reopened.BindScope(ctx, scope); err != nil {
				t.Fatal(err)
			}
			if err = reopened.Validate(ctx); err != nil {
				t.Fatal(err)
			}
			old, err := reopened.Job(ctx, job.ID)
			if err != nil || old.State == model.StatusSynced || read(t, reopened, old.Local) != body {
				t.Fatal("restart lost unconfirmed history", old, err)
			}
			recreated, err := reopened.EnsureEntry(ctx, e.Remote, model.KindFile)
			if err != nil || recreated.ID == e.ID {
				t.Fatal("cancelled identity reused", recreated, err)
			}
		})
	}
}

func TestCancelUnsentLocalFileSafetyMatrix(t *testing.T) {
	for _, caseName := range []string{"token", "remote-checkpoint", "local-checkpoint", "active", "unknown", "plan", "operation-ids", "operations", "results", "desired", "create-journal", "superseded-plan", "frozen"} {
		t.Run(caseName, func(t *testing.T) {
			s, _, _ := openTest(t)
			e, job := stage(t, s, entry(t, s, "protected.txt"), "accepted")
			switch caseName {
			case "active", "unknown", "create-journal":
				if _, _, err := s.Prepare(ctx, job.ID); err != nil {
					t.Fatal(err)
				}
				if caseName == "unknown" {
					if err := s.MarkUnknown(ctx, job.ID); err != nil {
						t.Fatal(err)
					}
				}
				if caseName == "create-journal" {
					if _, err := s.PrepareCreate(ctx, job.ID, "root", "protected.txt"); err != nil {
						t.Fatal(err)
					}
					if err := s.PauseJob(ctx, job.ID, model.StatusBlocked, "empty_file_unsupported"); err != nil {
						t.Fatal(err)
					}
				}
			case "frozen":
				if _, err := s.PrepareNamespace(ctx, model.NamespaceOp{Kind: "rename", Src: e.Remote, Dst: "moved.txt"}); err != nil {
					t.Fatal(err)
				}
			default:
				var protectedSnapshot model.Snapshot
				if caseName == "remote-checkpoint" {
					protectedSnapshot = fileSnapshot(t, s, "remote-token", "remote evidence")
				}
				if err := s.update(ctx, func(tx *bolt.Tx) error {
					current, err := entryTx(tx, e.ID)
					if err != nil {
						return err
					}
					record, err := jobTx(tx, job.ID)
					if err != nil {
						return err
					}
					switch caseName {
					case "token":
						current.Token = "remote-token"
					case "remote-checkpoint":
						current.RemoteCheckpoint = protectedSnapshot.ID
					case "local-checkpoint":
						current.LocalCheckpoint = current.Local
					case "plan":
						record.Job.Plan = record.Job.Local
					case "operation-ids":
						record.Job.OperationIDs = []string{"operation"}
					case "operations":
						record.Operations = []model.Operation{{ID: "operation"}}
					case "results":
						record.Results = map[int]json.RawMessage{0: json.RawMessage(`{}`)}
					case "desired":
						record.Desired = record.Job.Local
					case "superseded-plan":
						record.Superseded = true
						record.Job.Plan = record.Job.Local
					}
					if err := saveEntry(tx, current); err != nil {
						return err
					}
					return saveJob(tx, record)
				}); err != nil {
					t.Fatal(err)
				}
			}
			cancelled, err := s.CancelUnsentLocalFile(ctx, e.ID, e.Generation)
			if err != nil || cancelled {
				t.Fatalf("unsafe cancellation %s: %v %v", caseName, cancelled, err)
			}
			if _, err = s.EntryByRemote(ctx, e.Remote); err != nil {
				t.Fatal("guard removed visible file", err)
			}
			if read(t, s, job.Local) != "accepted" {
				t.Fatal("guard lost accepted bytes")
			}
		})
	}
}

func TestCancelledJobCannotBeClaimedOrPreparedForDispatch(t *testing.T) {
	s, _, _ := openTest(t)
	e, job := stage(t, s, entry(t, s, "cancelled.txt"), "bytes")
	if ok, err := s.CancelUnsentLocalFile(ctx, e.ID, e.Generation); err != nil || !ok {
		t.Fatal(ok, err)
	}
	if _, _, err := s.Prepare(ctx, job.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("stale claim accepted", err)
	}
	if err := s.RecordPlan(ctx, job.ID, nil, job.Local); !errors.Is(err, ErrNotFound) {
		t.Fatal("stale plan accepted", err)
	}
	if _, err := s.PrepareCreate(ctx, job.ID, "root", "cancelled.txt"); !errors.Is(err, ErrNotFound) {
		t.Fatal("stale create accepted", err)
	}
}

func TestLocalCancelAndEngineClaimAreSerialized(t *testing.T) {
	for i := 0; i < 12; i++ {
		s, _, _ := openTest(t)
		e, job := stage(t, s, entry(t, s, "racing.txt"), "bytes")
		start := make(chan struct{})
		var cancelled bool
		var cancelErr, claimErr error
		var done sync.WaitGroup
		done.Add(2)
		go func() {
			defer done.Done()
			<-start
			cancelled, cancelErr = s.CancelUnsentLocalFile(ctx, e.ID, e.Generation)
		}()
		go func() { defer done.Done(); <-start; _, _, claimErr = s.Prepare(ctx, job.ID) }()
		close(start)
		done.Wait()
		if cancelErr != nil {
			t.Fatal(cancelErr)
		}
		if cancelled {
			if !errors.Is(claimErr, ErrNotFound) {
				t.Fatalf("claim survived cancellation: %v", claimErr)
			}
			if err := s.RecordPlan(ctx, job.ID, nil, job.Local); !errors.Is(err, ErrNotFound) {
				t.Fatal(err)
			}
		} else {
			if claimErr != nil {
				t.Fatalf("neither claim nor cancel succeeded: %v", claimErr)
			}
			if err := s.RecordPlan(ctx, job.ID, nil, job.Local); err != nil {
				t.Fatal("winning claim could not dispatch", err)
			}
		}
	}
}
