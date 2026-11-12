package overlay

import (
	"fmt"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
	"path"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpeningMountReconcilesLegacySystemMetadata(t *testing.T) {
	for _, name := range []string{".DS_Store", "._cloud-origin.txt", "Thumbs.db"} {
		t.Run(name, func(t *testing.T) {
			f, s, source := fixture(t)
			e, err := s.EnsureEntry(ctx, name, model.KindFile)
			if err != nil {
				t.Fatal(err)
			}
			_, old, err := s.Stage(ctx, e.ID, nil, strings.NewReader("old metadata"))
			if err != nil {
				t.Fatal(err)
			}
			e, latest, err := s.Stage(ctx, e.ID, nil, strings.NewReader("latest metadata"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err = New(Dependencies{Store: s, Source: source, ProfileID: f.scope.ProfileID, MountID: f.scope.MountID}); err != nil {
				t.Fatal(err)
			}
			pending, err := s.Pending(ctx)
			if err != nil || len(pending) != 0 {
				t.Fatalf("legacy auxiliary jobs still block sync: %+v %v", pending, err)
			}
			for _, job := range []model.Job{old, latest} {
				journal, err := s.Journal(ctx, job.ID)
				if err != nil || !journal.LocalReceipt || journal.Job.State == model.StatusSynced {
					t.Fatal("false cloud confirmation", journal, err)
				}
				r, err := s.ReadBlob(ctx, job.Local)
				if err != nil {
					t.Fatal("payload lost", err)
				}
				r.Close()
			}
			if err := s.Validate(ctx); err != nil {
				t.Fatal(err)
			}
			if e.Token != "" {
				t.Fatal("unexpected cloud identity")
			}
		})
	}
}

func TestOpeningMountDoesNotHideUnsafeLegacyMetadata(t *testing.T) {
	for _, scenario := range []string{"ordinary", "conflict", "unknown", "create", "bound"} {
		t.Run(scenario, func(t *testing.T) {
			f, s, source := fixture(t)
			name := ".DS_Store"
			if scenario == "ordinary" {
				name = "important.md"
			}
			e, err := s.EnsureEntry(ctx, name, model.KindFile)
			if scenario == "bound" {
				e, err = s.ObserveEntry(ctx, store.Observation{Remote: name, Kind: model.KindFile, Token: "cloud-token"})
			}
			if err != nil {
				t.Fatal(err)
			}
			e, job, err := s.Stage(ctx, e.ID, nil, strings.NewReader("preserve"))
			if err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "conflict":
				err = s.SaveConflict(ctx, model.Conflict{EntryID: e.ID, Generation: e.Generation, Local: e.Local, Reason: "protected"})
			case "unknown", "create":
				_, _, err = s.Prepare(ctx, job.ID)
				if err != nil {
					t.Fatal(err)
				}
				if scenario == "unknown" {
					err = s.MarkUnknown(ctx, job.ID)
				} else {
					_, err = s.PrepareCreate(ctx, job.ID, "root", name)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err = New(Dependencies{Store: s, Source: source, ProfileID: f.scope.ProfileID, MountID: f.scope.MountID}); err != nil {
				t.Fatal(err)
			}
			pending, err := s.Pending(ctx)
			if err != nil || len(pending) != 1 || pending[0].ID != job.ID {
				t.Fatal("protected work hidden", pending, err)
			}
		})
	}
}

func TestPersistentDSStoreHasDurableReceiptWithoutCloudPending(t *testing.T) {
	f, s, _ := fixture(t)
	object := put(t, f, ".DS_Store", "durable Finder metadata")
	entry, err := s.EntryByRemote(ctx, object.Remote())
	if err != nil {
		t.Fatal(err)
	}
	pending, err := s.Pending(ctx)
	if err != nil || len(pending) != 0 {
		t.Fatalf("local-only metadata in cloud outbox: %+v %v", pending, err)
	}
	if entry.Status == model.StatusSynced || body(t, object) != "durable Finder metadata" {
		t.Fatal("metadata lost or falsely cloud confirmed", entry)
	}
	if err := object.Remove(ctx); err != nil {
		t.Fatal(err)
	}
	historical, err := s.ReadBlob(ctx, entry.Local)
	if err != nil {
		t.Fatal("local receipt removal lost payload", err)
	}
	historical.Close()
	if err := s.Validate(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestSafeSaveSubtreeRenamePromotesReceiptAtomically(t *testing.T) {
	f, s, _ := fixture(t)
	temporary := "report.txt.sb-1234abcd-Ab12Cd"
	if err := f.Mkdir(ctx, temporary); err != nil {
		t.Fatal(err)
	}
	object := put(t, f, temporary+"/report.txt", "complete saved bytes")
	before, err := s.EntryByRemote(ctx, object.Remote())
	if err != nil {
		t.Fatal(err)
	}
	pending, err := s.Pending(ctx)
	if err != nil || len(pending) != 0 {
		t.Fatal(pending, err)
	}
	operation, err := f.BeginManagedRename(ctx, object.Remote(), "report.txt")
	if err != nil {
		t.Fatal(err)
	}
	result, err := f.RenameManaged(ctx, operation)
	if err != nil {
		t.Fatal(err)
	}
	// Promotion must already be durable before Finish or cache reconciliation.
	after, err := s.EntryByRemote(ctx, "report.txt")
	if err != nil || after.Generation <= before.Generation {
		t.Fatal("rename did not promote receipt", after, err)
	}
	pending, err = s.Pending(ctx)
	if err != nil || len(pending) != 1 || pending[0].EntryID != after.ID || pending[0].Local != before.Local {
		t.Fatal(pending, err)
	}
	journal, err := s.Journal(ctx, pending[0].ID)
	if err != nil || journal.StagedAt.IsZero() {
		t.Fatal("promotion lost write-back clock", journal, err)
	}
	if err = f.FinishManagedRename(ctx, operation); err != nil {
		t.Fatal(err)
	}
	should, err := f.ShouldSync(ctx, after.ID)
	if err != nil || !should || body(t, result) != "complete saved bytes" {
		t.Fatal(should, err)
	}
	if err = s.Validate(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestWholeSafeSaveFolderPromotionPersistsOrderedMkdirIntents(t *testing.T) {
	f, s, source := fixture(t)
	temporary := "report.txt.sb-1234abcd-Ab12Cd"
	for _, directory := range []string{temporary, temporary + "/nested"} {
		if err := f.Mkdir(ctx, directory); err != nil {
			t.Fatal(err)
		}
	}
	object := put(t, f, temporary+"/nested/report.txt", "complete promoted subtree bytes")
	original, err := s.EntryByRemote(ctx, object.Remote())
	if err != nil {
		t.Fatal(err)
	}
	actions, err := s.NamespaceActions(ctx)
	if err != nil || len(actions) != 0 {
		t.Fatal("local-only mkdir entered cloud outbox", actions, err)
	}
	operation, err := f.BeginManagedRename(ctx, temporary, "saved")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.RenameManaged(ctx, operation); err != nil {
		t.Fatal(err)
	}
	actions, err = s.NamespaceActions(ctx)
	if err != nil || len(actions) != 0 {
		t.Fatal("mkdir dispatched before cache reconciliation", actions, err)
	}
	reader, err := s.ReadBlob(ctx, original.Local)
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Dir(filepath.Dir(reader.(interface{ Name() string }).Name()))
	reader.Close()
	scope, err := s.Scope(ctx)
	if err != nil {
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
	raw, err := New(Dependencies{Store: reopened, Source: source, ProfileID: scope.ProfileID, MountID: scope.MountID})
	if err != nil {
		t.Fatal(err)
	}
	recovered := raw.(*Fs)
	if err = recovered.ReplayManagedNamespace(ctx, func(src, dst string) error {
		if src != temporary || dst != "saved" {
			t.Fatalf("unexpected replay: %s %s", src, dst)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	actions, err = reopened.NamespaceActions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var folders []model.NamespaceOp
	for _, action := range actions {
		if action.Kind == "mkdir" {
			folders = append(folders, action)
		} else if err = reopened.AcknowledgeNamespaceAction(ctx, action.ID); err != nil {
			t.Fatal(err)
		}
	}
	if len(folders) != 2 || folders[0].Dst != "saved" || folders[1].Dst != "saved/nested" {
		t.Fatalf("mkdir intent order not parent-first: %+v", folders)
	}
	// Exercise the standard cloud namespace receipt primitives in parent order.
	for index, folder := range folders {
		parent, err := reopened.EntryByRemote(ctx, path.Dir(folder.Dst))
		if path.Dir(folder.Dst) == "." {
			parent, err = reopened.EntryByRemote(ctx, "")
		}
		if err != nil || parent.Token == "" {
			t.Fatalf("child admitted before parent token: %+v %v", parent, err)
		}
		if err = reopened.BeginNamespaceDispatch(ctx, folder.ID, "create_folder"); err != nil {
			t.Fatal(err)
		}
		token := fmt.Sprintf("bound-folder-%d", index)
		if err = reopened.BindNamespaceFolder(ctx, folder.ID, token); err != nil {
			t.Fatal(err)
		}
		if err = reopened.ConfirmNamespaceStep(ctx, folder.ID, "create_folder"); err != nil {
			t.Fatal(err)
		}
		if err = reopened.AcknowledgeNamespaceAction(ctx, folder.ID); err != nil {
			t.Fatal(err)
		}
	}
	jobs, err := reopened.Pending(ctx)
	if err != nil || len(jobs) != 1 || jobs[0].EntryID != original.ID {
		t.Fatal(jobs, err)
	}
	entry, err := reopened.EntryByRemote(ctx, "saved/nested/report.txt")
	if err != nil || entry.Generation <= original.Generation || entry.Status == model.StatusSynced {
		t.Fatal("file promotion lost or falsely synced", entry, err)
	}
	journal, err := reopened.Journal(ctx, jobs[0].ID)
	if err != nil || journal.LocalReceipt || journal.StagedAt.IsZero() {
		t.Fatal(journal, err)
	}
	if err = reopened.Validate(ctx); err != nil {
		t.Fatal(err)
	}
}
