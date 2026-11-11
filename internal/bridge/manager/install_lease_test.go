package manager

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rclone/rclone/internal/bridge/config"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
)

func installationGates(m *Manager) {
	acquire := func(ctx context.Context, shared bool) (func(), error) {
		l, err := privateLock(filepath.Join(m.domain, "auth", "locks", "lifecycle.lock"))
		if err != nil {
			return nil, err
		}
		var ok bool
		if shared {
			ok, err = l.TryRLockContext(ctx, time.Millisecond)
		} else {
			ok, err = l.TryLockContext(ctx, time.Millisecond)
		}
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, ErrPending
		}
		return func() { _ = l.Unlock() }, nil
	}
	m.startLease = func(ctx context.Context) (func(), error) { return acquire(ctx, true) }
	m.lifecycleLease = func(ctx context.Context) (func(), error) { return acquire(ctx, false) }
}
func offlineInstallationStore(t *testing.T, m *Manager, id model.MountID) (mountSpec, *store.Store) {
	t.Helper()
	spec, err := m.spec(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.recordOwner(spec); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(spec.Paths.Root)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.BindScope(context.Background(), ownershipFor(m.ConfigFile(), spec).Scope); err != nil {
		t.Fatal(err)
	}
	return spec, s
}
func TestInstallationLeaseStopsOtherConfigAndRetainsAcceptedState(t *testing.T) {
	ctx := context.Background()
	m, id, _, _ := fixture(t, nil)
	installationGates(m)
	other, otherID, _, _ := fixture(t, nil)
	other.domain = m.domain
	installationGates(other)
	if _, err := other.Start(ctx, otherID, true); err != nil {
		t.Fatal(err)
	}
	s := other.localWorker(otherID).services.store
	want := map[model.EntryID]model.Entry{}
	for _, name := range []string{"queued", "unknown", "conflict"} {
		e, err := s.EnsureEntry(ctx, name, model.KindFile)
		if err != nil {
			t.Fatal(err)
		}
		e, j, err := s.Stage(ctx, e.ID, nil, strings.NewReader("retained "+name))
		if err != nil {
			t.Fatal(err)
		}
		if name == "unknown" {
			if _, _, err = s.Prepare(ctx, j.ID); err != nil {
				t.Fatal(err)
			}
			if err = s.RecordPlan(ctx, j.ID, nil, e.Local); err != nil {
				t.Fatal(err)
			}
			if err = s.MarkUnknown(ctx, j.ID); err != nil {
				t.Fatal(err)
			}
		}
		if name == "conflict" {
			if err = s.SaveConflict(ctx, model.Conflict{EntryID: e.ID, Generation: e.Generation, Local: e.Local, Reason: "fixture"}); err != nil {
				t.Fatal(err)
			}
		}
		want[e.ID] = e
	}
	// The invoking config has an offline store; the other config's worker is live.
	_, local := offlineInstallationStore(t, m, id)
	local.Close()
	called := false
	err := m.WithInstallationLease(ctx, func() error {
		called = true
		if other.localWorker(otherID) != nil {
			t.Fatal("callback before worker exit")
		}
		spec, _ := other.spec(ctx, otherID)
		if got, e := store.Open(spec.Paths.Root); !errors.Is(e, store.ErrLocked) {
			if got != nil {
				got.Close()
			}
			t.Fatal("store ownership not held", e)
		}
		for _, e := range want {
			raw, err := os.ReadFile(filepath.Join(spec.Paths.Blobs, string(e.Local)))
			if err != nil || string(raw) != "retained "+e.Remote {
				t.Fatal(err, string(raw))
			}
		}
		return nil
	})
	if err != nil || !called {
		t.Fatal(err)
	}
	spec, _ := other.spec(ctx, otherID)
	reopened, err := store.Open(spec.Paths.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err = reopened.BindScope(ctx, ownershipFor(other.ConfigFile(), spec).Scope); err != nil {
		t.Fatal(err)
	}
	jobs, err := reopened.Pending(ctx)
	if err != nil || len(jobs) != 3 {
		t.Fatal(jobs, err)
	}
	states := map[model.Status]bool{}
	for _, j := range jobs {
		states[j.State] = true
	}
	if !states[model.StatusQueued] || !states[model.StatusAwaitingVerification] || !states[model.StatusConflict] {
		t.Fatal(states)
	}
	for id, e := range want {
		got, _ := reopened.Entry(ctx, id)
		if got.Local != e.Local || got.Generation != e.Generation {
			t.Fatal(got, e)
		}
	}
}
func TestInstallationLeaseHoldsHistoricalRootsAndBlocksStartsAndWriters(t *testing.T) {
	ctx := context.Background()
	m, id, _, _ := fixture(t, nil)
	installationGates(m)
	spec, s := offlineInstallationStore(t, m, id)
	e, _ := s.EnsureEntry(ctx, "pending", model.KindFile)
	_, _, err := s.Stage(ctx, e.ID, nil, strings.NewReader("historical bytes"))
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	// Remove the mount from the current config; its persisted ownership is still binding.
	c, err := m.LoadConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c.Mounts = nil
	if err = config.Save(m.ConfigFile(), c); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(spec.Paths.Root, "store.db"))
	entered, release := make(chan struct{}), make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- m.WithInstallationLease(ctx, func() error { close(entered); <-release; return nil }) }()
	select {
	case <-entered:
	case err := <-done:
		t.Fatal(err)
	case <-time.After(5 * time.Second):
		t.Fatal("lease did not enter")
	}
	// A different, not-yet-used config has no ownership folder to enumerate; its managed writers still share the global gate.
	other, otherID, _, _ := fixture(t, nil)
	other.domain = m.domain
	installationGates(other)
	start := make(chan error, 1)
	go func() {
		short, cancel := context.WithTimeout(ctx, 60*time.Millisecond)
		defer cancel()
		_, e := other.Start(short, otherID, true)
		start <- e
	}()
	writer := make(chan error, 1)
	go func() {
		short, cancel := context.WithTimeout(ctx, 60*time.Millisecond)
		defer cancel()
		_, e := other.AddProfile(short, "blocked", "feishu")
		writer <- e
	}()
	if err := <-start; err == nil {
		t.Fatal("worker start entered lease")
	}
	if err := <-writer; err == nil {
		t.Fatal("config writer entered lease")
	}
	if got, e := store.Open(spec.Paths.Root); !errors.Is(e, store.ErrLocked) {
		if got != nil {
			got.Close()
		}
		t.Fatal(e)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(spec.Paths.Root, "store.db"))
	if !bytes.Equal(before, after) {
		t.Fatal("lease rewrote retained database")
	}
	if _, err = other.AddProfile(ctx, "after", "feishu"); err != nil {
		t.Fatal("writer admission not released", err)
	}
}
func TestInstallationLeaseRefusesMissingWrongOrDirtyRoot(t *testing.T) {
	for _, mode := range []string{"missing", "wrong", "dirty", "corrupt"} {
		t.Run(mode, func(t *testing.T) {
			m, id, _, _ := fixture(t, nil)
			installationGates(m)
			spec, s := offlineInstallationStore(t, m, id)
			s.Close()
			switch mode {
			case "missing":
				if err := os.Remove(filepath.Join(spec.Paths.Root, "store.db")); err != nil {
					t.Fatal(err)
				}
			case "wrong":
				owners, _ := m.owners(context.Background())
				o := owners[0]
				o.Scope.IdentityHash = "wrong"
				if err := atomicJSON(filepath.Join(m.ownerDir(), string(id)+"-"+hashPath(o.Root)+".json"), o); err != nil {
					t.Fatal(err)
				}
			case "dirty":
				if err := os.MkdirAll(spec.Paths.VFS, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(spec.Paths.VFS, "unaccepted"), []byte("only copy"), 0600); err != nil {
					t.Fatal(err)
				}
			case "corrupt":
				if err := os.WriteFile(filepath.Join(spec.Paths.Root, "cache-manifest.json"), []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			called := false
			err := m.WithInstallationLease(context.Background(), func() error { called = true; return nil })
			if err == nil || called {
				t.Fatal("unsafe callback", err)
			}
			short, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			release, err := m.startLease(short)
			if err != nil {
				t.Fatal("failed lease leaked global lock", err)
			}
			release()
		})
	}
}
func TestInstallationLeaseCancellationAndCallbackErrorReleaseOwnership(t *testing.T) {
	m, id, _, _ := fixture(t, nil)
	installationGates(m)
	spec, s := offlineInstallationStore(t, m, id)
	s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	if err := m.WithInstallationLease(ctx, func() error { called = true; return nil }); err == nil || called {
		t.Fatal(err)
	}
	sentinel := errors.New("installer failed")
	if err := m.WithInstallationLease(context.Background(), func() error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	s, err := store.Open(spec.Paths.Root)
	if err != nil {
		t.Fatal("callback error leaked ownership", err)
	}
	s.Close()
}
func TestInstallationLeaseRejectsOpenHandlesWithoutDiscardingCache(t *testing.T) {
	m, id, runtime, _ := fixture(t, nil)
	installationGates(m)
	if _, err := m.Start(context.Background(), id, true); err != nil {
		t.Fatal(err)
	}
	runtime.mu.Lock()
	runtime.readers = 1
	runtime.pending = true
	runtime.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	called := false
	if err := m.WithInstallationLease(ctx, func() error { called = true; return nil }); err == nil || called {
		t.Fatal(err)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.unmounted || !runtime.pending {
		t.Fatal("unsafe forced stop")
	}
}

func TestInstallationLeaseAllowsUnstartedConfiguredMountWithoutCreatingStore(t *testing.T) {
	for _, emptyDirectory := range []bool{false, true} {
		t.Run(fmt.Sprint(emptyDirectory), func(t *testing.T) {
			m, id, _, _ := fixture(t, nil)
			installationGates(m)
			spec, err := m.spec(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			if emptyDirectory {
				if err = os.MkdirAll(spec.Paths.Root, 0700); err != nil {
					t.Fatal(err)
				}
			}
			called := false
			if err = m.WithInstallationLease(context.Background(), func() error { called = true; return nil }); err != nil || !called {
				t.Fatalf("unstarted mount blocks installer: called=%v err=%v", called, err)
			}
			contents, err := os.ReadDir(spec.Paths.Root)
			if emptyDirectory {
				if err != nil || len(contents) != 0 {
					t.Fatal("installation wrote into unused root", contents, err)
				}
			} else if !os.IsNotExist(err) {
				t.Fatal("installation created unused root", contents, err)
			}
		})
	}
}

func TestInstallationLeaseStillRejectsHistoricalMissingStore(t *testing.T) {
	m, id, _, _ := fixture(t, nil)
	installationGates(m)
	spec, err := m.spec(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.recordOwner(spec); err != nil {
		t.Fatal(err)
	}
	called := false
	if err = m.WithInstallationLease(context.Background(), func() error { called = true; return nil }); err == nil || called {
		t.Fatalf("missing historical store accepted: called=%v err=%v", called, err)
	}
	if _, err = os.Lstat(spec.Paths.Root); !os.IsNotExist(err) {
		t.Fatal("missing historical root was created", err)
	}
}

func TestInstallationLeaseRejectsUnaccountedNonemptyConfiguredRoot(t *testing.T) {
	for _, content := range []string{"file", "directory", "valid-store"} {
		t.Run(content, func(t *testing.T) {
			m, id, _, _ := fixture(t, nil)
			installationGates(m)
			spec, err := m.spec(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			if err = os.MkdirAll(spec.Paths.Root, 0700); err != nil {
				t.Fatal(err)
			}
			switch content {
			case "file":
				if err = os.WriteFile(filepath.Join(spec.Paths.Root, "unaccounted"), []byte("retain"), 0600); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err = os.Mkdir(filepath.Join(spec.Paths.Root, "empty-child"), 0700); err != nil {
					t.Fatal(err)
				}
			case "valid-store":
				s, err := store.Open(spec.Paths.Root)
				if err != nil {
					t.Fatal(err)
				}
				if err = s.BindScope(context.Background(), ownershipFor(m.ConfigFile(), spec).Scope); err != nil {
					t.Fatal(err)
				}
				if err = s.Close(); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadDir(spec.Paths.Root)
			if err != nil {
				t.Fatal(err)
			}
			called := false
			err = m.WithInstallationLease(context.Background(), func() error { called = true; return nil })
			if err == nil || called {
				t.Fatalf("unaccounted occupied root accepted: called=%v err=%v", called, err)
			}
			after, err := os.ReadDir(spec.Paths.Root)
			if err != nil || len(after) != len(before) {
				t.Fatal("unaccounted root changed", before, after, err)
			}
		})
	}
}
