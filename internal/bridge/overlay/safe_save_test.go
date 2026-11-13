package overlay

import (
	"errors"
	fsconfig "github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
	"github.com/rclone/rclone/lib/larkcli"
	"github.com/rclone/rclone/vfs"
	"github.com/rclone/rclone/vfs/vfscommon"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestTextEditBackupSavePreservesCloudIdentity(t *testing.T) {
	for _, viaRename := range []bool{false, true} {
		t.Run(map[bool]string{false: "recreate", true: "rename-replacement"}[viaRename], func(t *testing.T) {
			f, s, cloud := fixture(t)
			cloud.items = []larkcli.DriveItem{{Token: "stable-token", Name: "saved.txt", Type: "file"}}
			cloud.bodies["stable-token"] = "old cloud bytes"
			listed(t, f)
			original, err := f.NewObject(ctx, "saved.txt")
			if err != nil {
				t.Fatal(err)
			}
			if body(t, original) != "old cloud bytes" {
				t.Fatal("missing original baseline")
			}
			backup, err := f.Move(ctx, original, "saved.txt.sb-1234abcd-Ab12Cd")
			if err != nil {
				t.Fatal(err)
			}
			actions, err := s.NamespaceActions(ctx)
			if err != nil || len(actions) != 1 {
				t.Fatalf("backup rename must remain explicitly pending: %+v %v", actions, err)
			}
			work, err := s.NamespaceJournal(ctx, actions[0].ID)
			if err != nil || !work.SafeSaveBackup || work.SafeSaveCompleted || work.Dispatched {
				t.Fatal("pending backup is not protected", work, err)
			}
			var savedID model.EntryID
			if viaRename {
				temporary := put(t, f, ".~save", "new local bytes")
				saved, err := f.Move(ctx, temporary, "saved.txt")
				if err != nil {
					t.Fatal(err)
				}
				entry, err := s.EntryByRemote(ctx, saved.Remote())
				if err != nil {
					t.Fatal(err)
				}
				savedID = entry.ID
			} else {
				saved := put(t, f, "saved.txt", "new local bytes")
				entry, err := s.EntryByRemote(ctx, saved.Remote())
				if err != nil {
					t.Fatal(err)
				}
				savedID = entry.ID
			}
			saved, err := s.Entry(ctx, savedID)
			if err != nil || saved.Token != "stable-token" || saved.CloudName != "saved.txt" {
				t.Fatalf("save replaced cloud identity/name: %+v %v", saved, err)
			}
			if body(t, backup) != "old cloud bytes" {
				t.Fatal("backup bytes lost")
			}
			if err := backup.Remove(ctx); err != nil {
				t.Fatal(err)
			}
			actions, err = s.NamespaceActions(ctx)
			for _, action := range actions {
				if action.Kind == "delete" || action.Kind == "rename" {
					t.Fatalf("temporary backup dispatched remotely: %+v", action)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if err = s.Validate(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSafeSavePendingSurvivesRestartAndAcceptsIdenticalReplacement(t *testing.T) {
	f, s, cloud := fixture(t)
	cloud.items = []larkcli.DriveItem{{Token: "stable-token", Name: "saved.txt", Type: "file"}}
	cloud.bodies["stable-token"] = "old cloud bytes"
	listed(t, f)
	original, err := f.NewObject(ctx, "saved.txt")
	if err != nil {
		t.Fatal(err)
	}
	if body(t, original) != "old cloud bytes" {
		t.Fatal("missing baseline")
	}
	backup, err := f.Move(ctx, original, "saved.txt.sb-1234abcd-Ab12Cd")
	if err != nil {
		t.Fatal(err)
	}
	originalEntry, err := s.EntryByToken(ctx, "stable-token")
	if err != nil {
		t.Fatal(err)
	}
	reader, err := s.ReadBlob(ctx, originalEntry.Local)
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Dir(filepath.Dir(reader.(interface{ Name() string }).Name()))
	reader.Close()
	scope, err := s.Scope(ctx)
	if err != nil {
		t.Fatal(err)
	}
	actions, err := s.NamespaceActions(ctx)
	if err != nil || len(actions) != 1 {
		t.Fatal(actions, err)
	}
	operation := actions[0].ID
	if err = s.BeginNamespaceDispatch(ctx, operation, "rename"); !errors.Is(err, store.ErrSafeSavePending) {
		t.Fatal("pending backup allowed remote dispatch", err)
	}
	if err = s.Validate(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(directory)
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
	raw, err := New(Dependencies{Store: reopened, Source: cloud, ProfileID: scope.ProfileID, MountID: scope.MountID})
	if err != nil {
		t.Fatal(err)
	}
	recovered := raw.(*Fs)
	actions, err = reopened.NamespaceActions(ctx)
	if err != nil || len(actions) != 1 {
		t.Fatal("restart silently consumed backup rename", actions, err)
	}
	shadow, err := recovered.NewObject(ctx, backup.Remote())
	if err != nil || body(t, shadow) != "old cloud bytes" {
		t.Fatal("backup bytes lost on restart", err)
	}
	// Merely restoring the name is not a completed save.
	restored, resumed, err := reopened.ResumeSafeSave(ctx, "saved.txt", model.KindFile)
	if err != nil || !resumed {
		t.Fatal(restored, resumed, err)
	}
	actions, err = reopened.NamespaceActions(ctx)
	if err != nil || len(actions) != 1 {
		t.Fatal("name alone guessed completion", actions, err)
	}
	// Accepting the same durable bytes explicitly completes the replacement.
	saved := put(t, recovered, "saved.txt", "old cloud bytes")
	current, err := reopened.EntryByRemote(ctx, saved.Remote())
	if err != nil || current.ID != originalEntry.ID || current.Token != "stable-token" {
		t.Fatal(current, err)
	}
	actions, err = reopened.NamespaceActions(ctx)
	if err != nil || len(actions) != 0 {
		t.Fatal(actions, err)
	}
	if err = reopened.Validate(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestSafeSaveBackupRejectsPendingAndUnknownOriginal(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		f, s, cloud := fixture(t)
		cloud.items = []larkcli.DriveItem{{Token: "stable-token", Name: "saved.txt", Type: "file"}}
		cloud.bodies["stable-token"] = "baseline"
		listed(t, f)
		original, err := f.NewObject(ctx, "saved.txt")
		if err != nil {
			t.Fatal(err)
		}
		_ = body(t, original)
		edited := put(t, f, "saved.txt", "pending edit")
		if unknown {
			jobs, _ := s.Pending(ctx)
			if _, _, err = s.Prepare(ctx, jobs[0].ID); err != nil {
				t.Fatal(err)
			}
			if err = s.MarkUnknown(ctx, jobs[0].ID); err != nil {
				t.Fatal(err)
			}
		}
		if _, err = f.Move(ctx, edited, "saved.txt.sb-1234abcd-Ab12Cd"); !errors.Is(err, store.ErrBusy) {
			t.Fatal("unsafe original parked", err)
		}
		current, err := s.EntryByRemote(ctx, "saved.txt")
		if err != nil || current.Token != "stable-token" || body(t, edited) != "pending edit" {
			t.Fatal(current, err)
		}
		pending, err := s.Pending(ctx)
		if err != nil || len(pending) != 1 {
			t.Fatal(pending, err)
		}
	}
}

func TestSameShapeRegularRenameWithoutCausalBasenameRemainsOrdinary(t *testing.T) {
	f, s, cloud := fixture(t)
	cloud.items = []larkcli.DriveItem{{Token: "stable-token", Name: "saved.txt", Type: "file"}}
	cloud.bodies["stable-token"] = "baseline"
	listed(t, f)
	original, err := f.NewObject(ctx, "saved.txt")
	if err != nil {
		t.Fatal(err)
	}
	_ = body(t, original)
	renamed, err := f.Move(ctx, original, "other.txt.sb-1234abcd-Ab12Cd")
	if err != nil {
		t.Fatal(err)
	}
	entry, err := s.EntryByRemote(ctx, renamed.Remote())
	if err != nil || entry.Token != "stable-token" || f.LocalOnly(renamed.Remote()) {
		t.Fatal(entry, err)
	}
	actions, err := s.NamespaceActions(ctx)
	if err != nil || len(actions) != 1 {
		t.Fatal(actions, err)
	}
	work, err := s.NamespaceJournal(ctx, actions[0].ID)
	if err != nil || work.SafeSaveBackup {
		t.Fatal("generic .sb regular file was filtered", work, err)
	}
}

func TestSafeSaveBackupWithManagedVFSKeepsBothByteSnapshots(t *testing.T) {
	f, s, cloud := fixture(t)
	cloud.items = []larkcli.DriveItem{{Token: "stable-token", Name: "saved.txt", Type: "file"}}
	cloud.bodies["stable-token"] = "old cloud bytes"
	listed(t, f)
	oldCache := fsconfig.GetCacheDir()
	cacheRoot, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := fsconfig.SetCacheDir(filepath.Join(cacheRoot, "cache")); err != nil {
		t.Fatal(err)
	}
	defer fsconfig.SetCacheDir(oldCache)
	options := vfscommon.Opt
	options.CacheMode = vfscommon.CacheModeFull
	options.CachePollInterval = 0
	options.HandleCaching = 0
	managed, err := vfs.NewManaged(ctx, f, &options)
	if err != nil {
		t.Fatal(err)
	}
	defer managed.Shutdown()
	reader, err := managed.Open("saved.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = io.ReadAll(reader); err != nil {
		t.Fatal(err)
	}
	if err = reader.Release(); err != nil {
		t.Fatal(err)
	}
	if err = managed.Rename("saved.txt", "saved.txt.sb-1234abcd-Ab12Cd"); err != nil {
		t.Fatal(err)
	}
	writer, err := managed.OpenFile("saved.txt", os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = writer.Write([]byte("new complete bytes")); err != nil {
		t.Fatal(err)
	}
	if err = writer.Close(); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"saved.txt": "new complete bytes", "saved.txt.sb-1234abcd-Ab12Cd": "old cloud bytes"} {
		reader, err = managed.Open(name)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			t.Fatal(err)
		}
		if err = reader.Release(); err != nil {
			t.Fatal(err)
		}
		if string(data) != want {
			t.Fatalf("%s: %q", name, data)
		}
	}
	current, err := s.EntryByRemote(ctx, "saved.txt")
	if err != nil || current.Token != "stable-token" {
		t.Fatal(current, err)
	}
	if err = managed.Remove("saved.txt.sb-1234abcd-Ab12Cd"); err != nil {
		t.Fatal(err)
	}
	if err = s.Validate(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestSafeSavePreparationCannotRacePendingJobClaim(t *testing.T) {
	for i := 0; i < 8; i++ {
		f, s, cloud := fixture(t)
		cloud.items = []larkcli.DriveItem{{Token: "stable-token", Name: "saved.txt", Type: "file"}}
		cloud.bodies["stable-token"] = "baseline"
		listed(t, f)
		original, err := f.NewObject(ctx, "saved.txt")
		if err != nil {
			t.Fatal(err)
		}
		_ = body(t, original)
		pendingObject := put(t, f, "saved.txt", "pending local bytes")
		jobs, err := s.Pending(ctx)
		if err != nil || len(jobs) != 1 {
			t.Fatal(jobs, err)
		}
		start := make(chan struct{})
		var done sync.WaitGroup
		done.Add(2)
		var backupErr, claimErr error
		go func() {
			defer done.Done()
			<-start
			_, backupErr = f.BeginManagedRename(ctx, "saved.txt", "saved.txt.sb-1234abcd-Ab12Cd")
		}()
		go func() { defer done.Done(); <-start; _, _, claimErr = s.Prepare(ctx, jobs[0].ID) }()
		close(start)
		done.Wait()
		if !errors.Is(backupErr, store.ErrBusy) {
			t.Fatal("pending claim allowed parking", backupErr)
		}
		if claimErr != nil && !errors.Is(claimErr, store.ErrFrozen) {
			t.Fatal(claimErr)
		}
		current, err := s.EntryByRemote(ctx, "saved.txt")
		if err != nil || current.Token != "stable-token" || body(t, pendingObject) != "pending local bytes" {
			t.Fatal(current, err)
		}
		if _, err = s.EntryByRemote(ctx, "saved.txt.sb-1234abcd-Ab12Cd"); !errors.Is(err, store.ErrNotFound) {
			t.Fatal("race created backup shadow", err)
		}
	}
}

func TestSafeSaveShadowExplicitRenameDoesNotRemainFiltered(t *testing.T) {
	f, s, cloud := fixture(t)
	cloud.items = []larkcli.DriveItem{{Token: "stable-token", Name: "saved.txt", Type: "file"}}
	cloud.bodies["stable-token"] = "baseline"
	listed(t, f)
	original, err := f.NewObject(ctx, "saved.txt")
	if err != nil {
		t.Fatal(err)
	}
	_ = body(t, original)
	backup, err := f.Move(ctx, original, "saved.txt.sb-1234abcd-Ab12Cd")
	if err != nil {
		t.Fatal(err)
	}
	renamed, err := f.Move(ctx, backup, "user-backup.txt")
	if err != nil {
		t.Fatal(err)
	}
	current, err := s.EntryByRemote(ctx, renamed.Remote())
	if err != nil || f.LocalOnly(renamed.Remote()) {
		t.Fatal("explicit user rename stayed filtered", current, err)
	}
	jobs, err := s.Pending(ctx)
	if err != nil || len(jobs) != 1 || jobs[0].EntryID != current.ID {
		t.Fatal(jobs, err)
	}
	if body(t, renamed) != "baseline" {
		t.Fatal("backup bytes lost")
	}
	actions, err := s.NamespaceActions(ctx)
	if err != nil || len(actions) < 1 {
		t.Fatal("original backup obligation silently completed", actions, err)
	}
	work, err := s.NamespaceJournal(ctx, actions[0].ID)
	if err != nil || !work.SafeSaveBackup || work.SafeSaveCompleted {
		t.Fatal(work, err)
	}
}
