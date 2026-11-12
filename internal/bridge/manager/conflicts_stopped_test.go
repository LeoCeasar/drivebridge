package manager

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/rclone/rclone/internal/bridge/config"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
	syncer "github.com/rclone/rclone/internal/bridge/sync"
	"github.com/rclone/rclone/lib/larkcli"
)

func stoppedConflictFixture(t *testing.T, archived bool, body []byte) (*Manager, model.MountID, model.Entry, model.Conflict) {
	t.Helper()
	ctx := context.Background()
	m, id, _, _ := fixture(t, nil)
	_, s := offlineInstallationStore(t, m, id)
	entry, err := s.ObserveEntry(ctx, store.Observation{Remote: "local.bin", Token: "missing-file", Kind: model.KindFile})
	if err != nil {
		t.Fatal(err)
	}
	entry, _, err = s.Stage(ctx, entry.ID, nil, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	c := model.Conflict{ID: model.ConflictID(uuid.NewString()), EntryID: entry.ID, Generation: entry.Generation, Local: entry.Local, Reason: "fixture"}
	if err = s.SaveConflict(ctx, c); err != nil {
		t.Fatal(err)
	}
	if err = s.SaveDraft(ctx, c.ID, strings.NewReader("unfinished draft")); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	if archived {
		if err = m.updateConfig(ctx, func(c *config.Config) error { c.Mounts[0].Archived = true; return nil }); err != nil {
			t.Fatal(err)
		}
	}
	// No installed CLI or managed identity is needed by these local-state tests.
	m.options.CLIExecutable = filepath.Join(t.TempDir(), "unavailable-cli")
	m.spawn = func(context.Context, []string, string) (int, error) {
		t.Error("conflict control started a stopped worker")
		return 0, ErrUnavailable
	}
	return m, id, entry, c
}

func TestStoppedConflictUnavailableRetainsCapturesAndDrafts(t *testing.T) {
	for _, archived := range []bool{false, true} {
		name := "stopped"
		if archived {
			name = "archived"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			m, mountID, entry, c := stoppedConflictFixture(t, archived, []byte("accepted local body"))
			view, err := m.Conflict(ctx, c.ID)
			if err != nil || !view.Stale || view.RefreshWarning == "" || string(view.Local) != "accepted local body" || string(view.Draft) != "unfinished draft" {
				t.Fatalf("unavailable review lost saved capture: %+v %v", view, err)
			}
			for _, action := range []string{model.ResolveAcceptDelete, model.ResolveRestoreLocal} {
				err = m.ResolveConflict(ctx, model.ResolveRequest{Action: action, ConflictID: c.ID, ExpectedGeneration: entry.Generation})
				if err == nil {
					t.Fatal("unverified resolution succeeded")
				}
				view, err = m.Conflict(ctx, c.ID)
				if err != nil || string(view.Draft) != "unfinished draft" || string(view.Local) != "accepted local body" {
					t.Fatal("empty delete/restore request overwrote saved content", view, err)
				}
			}
			if err = m.ResolveConflict(ctx, model.ResolveRequest{ConflictID: c.ID, ExpectedGeneration: entry.Generation, Content: []byte("new human draft")}); err == nil {
				t.Fatal("unverified ordinary resolution succeeded")
			}
			view, err = m.Conflict(ctx, c.ID)
			if err != nil || string(view.Draft) != "new human draft" {
				t.Fatal("failed ordinary resolve did not retain human draft", view, err)
			}
			status, err := m.Status(ctx, mountID)
			if err != nil || status.Running || status.Queued != 1 || status.Conflicts != 1 || m.localWorker(mountID) != nil {
				t.Fatal("offline review changed worker/queue lifecycle", status, err)
			}
			mount, err := m.ResolveMount(ctx, string(mountID))
			if err != nil || mount.Archived != archived {
				t.Fatal("offline control changed archive state", mount, err)
			}
		})
	}
}

func TestStoppedConflictEngineRejectsChangedScopeBeforeAuth(t *testing.T) {
	ctx := context.Background()
	m, mountID, _, _ := stoppedConflictFixture(t, true, []byte("saved content"))
	var out struct{}
	if err := m.withStore(ctx, mountID, "/conflict", nil, &out, func(s *store.Store) error {
		spec, err := m.spec(ctx, mountID)
		if err != nil {
			return err
		}
		spec.Profile.IdentityHash = strings.Repeat("b", 64)
		if _, err = m.buildConflictEngine(ctx, spec, s); !errors.Is(err, model.ErrIdentityChanged) {
			t.Fatal("changed scope reached managed authentication", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Unimplemented Source methods panic if any mutation or body download is
// attempted. These explicit controls need only a complete metadata listing.
type stoppedConflictSource struct {
	syncer.Source
	lists atomic.Int32
	err   error
	items []larkcli.DriveItem
}

func (s *stoppedConflictSource) List(context.Context, string) ([]larkcli.DriveItem, error) {
	s.lists.Add(1)
	return s.items, s.err
}

func stoppedEngineBuilder(t *testing.T, source *stoppedConflictSource, mountID model.MountID) conflictEngineBuilder {
	t.Helper()
	return func(ctx context.Context, spec mountSpec, s *store.Store) (conflictEngine, error) {
		if spec.Mount.ID != mountID || spec.Profile.ID == "" {
			t.Fatal("fallback used a different mount/profile", spec)
		}
		return syncer.New(syncer.Dependencies{Store: s, Source: source, CheckIdentity: func(context.Context) error { return nil }, Publisher: syncer.PublisherFunc(func(context.Context, model.PublishCandidate) error {
			t.Fatal("stopped control attempted runtime publication")
			return ErrUnavailable
		})}, syncer.Options{})
	}
}

func TestArchivedConflictReadOnlyResolutionQueuesWithoutStarting(t *testing.T) {
	for _, action := range []string{model.ResolveRestoreLocal, model.ResolveAcceptDelete} {
		t.Run(action, func(t *testing.T) {
			ctx := context.Background()
			m, mountID, entry, c := stoppedConflictFixture(t, true, []byte("binary\x00local"))
			source := &stoppedConflictSource{}
			build := stoppedEngineBuilder(t, source, mountID)
			var view ConflictView
			err := m.withStore(ctx, mountID, "/conflict", struct{ ID model.ConflictID }{c.ID}, &view, func(s *store.Store) error {
				var err error
				view, err = m.stoppedConflict(ctx, s, c.ID, build)
				return err
			})
			if err != nil || view.Stale || view.Conflict.Reason != model.ConflictRemoteDeleted || source.lists.Load() != 1 {
				t.Fatal("explicit review did not confirm absent remote", view, source.lists.Load(), err)
			}
			request := model.ResolveRequest{Action: action, ConflictID: c.ID, ExpectedGeneration: entry.Generation, ExpectedRemoteRevision: view.Conflict.RemoteRevision}
			var out struct{}
			err = m.withStore(ctx, mountID, "/resolve", request, &out, func(s *store.Store) error {
				return m.stoppedResolve(ctx, s, request, build)
			})
			if err != nil || source.lists.Load() != 2 {
				t.Fatal("resolution did not recheck fresh absence", source.lists.Load(), err)
			}
			if err = m.withStore(ctx, mountID, "/conflicts", nil, &[]ConflictView{}, func(s *store.Store) error {
				conflicts, err := s.Conflicts(ctx)
				if err != nil || len(conflicts) != 0 {
					t.Fatal("resolved conflict still active", conflicts, err)
				}
				pending, err := s.Pending(ctx)
				if err != nil {
					return err
				}
				if action == model.ResolveAcceptDelete {
					if len(pending) != 0 {
						t.Fatal("accept-delete retained active upload", pending)
					}
					metadata, err := s.Metadata(ctx, entry.ID)
					if err != nil || !metadata.Tombstone {
						t.Fatal("accept-delete did not tombstone missing entry", metadata, err)
					}
				} else {
					current, err := s.Entry(ctx, entry.ID)
					if err != nil || current.Token != "" || current.Local != entry.Local || len(pending) != 1 || pending[0].State != model.StatusQueued {
						t.Fatal("binary restore did not queue original bytes for creation", current, pending, err)
					}
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			mount, err := m.ResolveMount(ctx, string(mountID))
			if err != nil || !mount.Archived || m.localWorker(mountID) != nil {
				t.Fatal("explicit resolution started/restored archived mount", mount, err)
			}
		})
	}
}

func TestStoppedResolutionListingFailurePreservesQueuedData(t *testing.T) {
	ctx := context.Background()
	m, mountID, entry, c := stoppedConflictFixture(t, true, []byte("saved body"))
	source := &stoppedConflictSource{}
	build := stoppedEngineBuilder(t, source, mountID)
	var view ConflictView
	var out struct{}
	err := m.withStore(ctx, mountID, "/conflict", nil, &out, func(s *store.Store) error {
		var err error
		view, err = m.stoppedConflict(ctx, s, c.ID, build)
		return err
	})
	if err != nil || view.Conflict.Reason != model.ConflictRemoteDeleted {
		t.Fatal(view, err)
	}
	source.err = errors.New("incomplete listing")
	request := model.ResolveRequest{Action: model.ResolveAcceptDelete, ConflictID: c.ID, ExpectedGeneration: entry.Generation, ExpectedRemoteRevision: view.Conflict.RemoteRevision}
	err = m.withStore(ctx, mountID, "/resolve", request, &out, func(s *store.Store) error {
		if err := m.stoppedResolve(ctx, s, request, build); !errors.Is(err, source.err) {
			t.Fatal("incomplete proof did not fail resolution", err)
		}
		source.err = nil
		source.items = []larkcli.DriveItem{{Token: entry.Token, Name: entry.CloudName, Type: "file", ParentToken: "fixture_folder"}}
		if err := m.stoppedResolve(ctx, s, request, build); !errors.Is(err, model.ErrVersionChanged) {
			t.Fatal("reappearing remote did not reject deletion resolution", err)
		}
		view, err := findSummary(ctx, s, c.ID)
		if err != nil || view.Entry.Local != entry.Local || view.Entry.Generation != entry.Generation {
			t.Fatal("failed listing altered accepted generation", view, err)
		}
		body, text, err := blobText(ctx, s, view.Conflict.Draft)
		if err != nil || !text || string(body) != "unfinished draft" {
			t.Fatal("failed resolution altered draft", string(body), err)
		}
		pending, err := s.Pending(ctx)
		if err != nil || len(pending) != 1 || pending[0].Local != entry.Local {
			t.Fatal("failed resolution lost queued work", pending, err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestRunningConflictControlsUseWorkerIPC(t *testing.T) {
	ctx := context.Background()
	m, mountID, entry, c := stoppedConflictFixture(t, false, []byte("binary\x00local"))
	m.options.CLIExecutable = ""
	source := &stoppedConflictSource{}
	build := stoppedEngineBuilder(t, source, mountID)
	baseFactory := m.factory
	m.factory = func(ctx context.Context, spec mountSpec) (*workerServices, error) {
		services, err := baseFactory(ctx, spec)
		if err != nil {
			return nil, err
		}
		engine, err := build(ctx, spec, services.store)
		if err != nil {
			_ = services.store.Close()
			return nil, err
		}
		services.refreshConflict = engine.RefreshConflict
		services.resolve = func(ctx context.Context, r model.ResolveRequest) error {
			_, _, err := engine.Resolve(ctx, r)
			return err
		}
		return services, nil
	}
	// The fixture starts only its in-process fake mount/runtime. A fallback to
	// managed auth would fail because no managed CLI is configured.
	workerCtx, cancel := context.WithCancel(ctx)
	workerDone := make(chan struct{})
	t.Cleanup(func() {
		cancel()
		select {
		case <-workerDone:
		case <-time.After(2 * time.Second):
			t.Error("fixture worker did not stop")
		}
	})
	m.spawn = func(context.Context, []string, string) (int, error) {
		go func() { defer close(workerDone); _ = m.RunWorker(workerCtx, mountID) }()
		return 1, nil
	}
	if _, err := m.Start(ctx, mountID, true); err != nil {
		t.Fatal(err)
	}
	view, err := m.Conflict(ctx, c.ID)
	if err != nil || view.Stale || view.Conflict.Reason != model.ConflictRemoteDeleted || source.lists.Load() != 1 {
		t.Fatal("review did not use owning worker", view, source.lists.Load(), err)
	}
	if err = m.ResolveConflict(ctx, model.ResolveRequest{Action: model.ResolveRestoreLocal, ConflictID: c.ID, ExpectedGeneration: entry.Generation, ExpectedRemoteRevision: view.Conflict.RemoteRevision}); err != nil || source.lists.Load() != 2 {
		t.Fatal("resolution did not use owning worker", source.lists.Load(), err)
	}
	w := m.localWorker(mountID)
	if w == nil {
		t.Fatal("control lost its running worker")
	}
	current, err := w.services.store.Entry(ctx, entry.ID)
	if err != nil || current.Token != "" || current.Local != entry.Local {
		t.Fatal("running restore changed accepted binary content", current, err)
	}
}
