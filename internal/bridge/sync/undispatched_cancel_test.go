package syncer

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/rclone/rclone/internal/bridge/cloud"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
	"github.com/rclone/rclone/lib/larkcli"
)

// The transport can prove it never started a child after a credential wait has
// already canceled its caller. Keep that proof independent of caller lifetime.
type cancelUndispatchedCloud struct {
	*fixtureCloud
	reject   string
	cancel   context.CancelFunc
	onReject func()
}

func (c *cancelUndispatchedCloud) rejected(kind string) error {
	if c.reject != kind {
		return nil
	}
	c.reject = ""
	if c.onReject != nil {
		c.onReject()
	}
	c.cancel()
	return errors.Join(cloud.ErrNotDispatched, context.DeadlineExceeded)
}
func (c *cancelUndispatchedCloud) ApplyDoc(ctx context.Context, token, revision string, ops []model.Operation) (cloud.ApplyResult, error) {
	if err := c.rejected("apply"); err != nil {
		return cloud.ApplyResult{}, err
	}
	return c.fixtureCloud.ApplyDoc(ctx, token, revision, ops)
}
func (c *cancelUndispatchedCloud) UploadFile(ctx context.Context, parent, token, name string, in io.Reader) (larkcli.UploadResult, error) {
	if err := c.rejected("upload"); err != nil {
		return larkcli.UploadResult{}, err
	}
	return c.fixtureCloud.UploadFile(ctx, parent, token, name, in)
}
func (c *cancelUndispatchedCloud) CreateDoc(ctx context.Context, parent, title string, xml []byte) (cloud.Document, error) {
	if err := c.rejected("create"); err != nil {
		return cloud.Document{}, err
	}
	return c.fixtureCloud.CreateDoc(ctx, parent, title, xml)
}
func (c *cancelUndispatchedCloud) Rename(ctx context.Context, token string, kind model.Kind, name string) error {
	if err := c.rejected("rename"); err != nil {
		return err
	}
	return c.fixtureCloud.Rename(ctx, token, kind, name)
}
func (c *cancelUndispatchedCloud) List(_ context.Context, parent string) ([]larkcli.DriveItem, error) {
	var out []larkcli.DriveItem
	for _, item := range c.items {
		if item.ParentToken == parent {
			out = append(out, item)
		}
	}
	return out, nil
}
func (c *cancelUndispatchedCloud) Move(_ context.Context, token string, _ model.Kind, parent string) error {
	if err := c.rejected("move"); err != nil {
		return err
	}
	for i := range c.items {
		if c.items[i].Token == token {
			c.items[i].ParentToken = parent
		}
	}
	return nil
}
func (c *cancelUndispatchedCloud) Delete(ctx context.Context, token string, kind model.Kind) error {
	if err := c.rejected("delete"); err != nil {
		return err
	}
	return c.fixtureCloud.Delete(ctx, token, kind)
}
func (c *cancelUndispatchedCloud) CreateFolder(_ context.Context, parent, name string) (string, error) {
	if err := c.rejected("folder"); err != nil {
		return "", err
	}
	c.items = append(c.items, larkcli.DriveItem{Token: "created-folder", Name: name, Type: "folder", ParentToken: parent})
	return "created-folder", nil
}

func TestCanceledUndispatchedWritePersistsEvidenceAndNewGeneration(t *testing.T) {
	for _, kind := range []model.Kind{model.KindDocx, model.KindFile} {
		for _, newerSave := range []bool{false, true} {
			t.Run(string(kind)+map[bool]string{false: "", true: "/new-save"}[newerSave], func(t *testing.T) {
				s, e, f, en := setup(t, kind)
				content, latest, reject := "wanted bytes", "newest bytes", "upload"
				if kind == model.KindDocx {
					content, latest, reject = "alpha changed\n\nbeta two\n", "alpha changed\n\nbeta newest\n", "apply"
				}
				en = stageContent(t, s, en, content)
				jobs, _ := s.Pending(bg)
				ctx, cancel := context.WithCancel(bg)
				defer cancel()
				c := &cancelUndispatchedCloud{fixtureCloud: f, reject: reject, cancel: cancel}
				if newerSave {
					c.onReject = func() { en = stageContent(t, s, en, latest) }
				}
				e.source = c
				if err := e.SyncEntry(ctx, en.ID); !errors.Is(err, cloud.ErrNotDispatched) || !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("transport proof lost: %v", err)
				}
				journal, err := s.Journal(bg, jobs[0].ID)
				if err != nil || journal.Job.State != model.StatusPaused {
					t.Fatalf("not-dispatched journal left in flight: %+v %v", journal, err)
				}
				if kind == model.KindDocx {
					var result stepResult
					if json.Unmarshal(journal.Results[0], &result) != nil || !result.NotDispatched {
						t.Fatal("not-dispatched operation evidence lost")
					}
				} else if journal.Job.Plan != "" || journal.Desired != "" {
					t.Fatal("undispatched file plan retained")
				}
				current, _ := s.Entry(bg, en.ID)
				if current.Generation != en.Generation || (newerSave && blobText(t, s, current.Local) != latest) {
					t.Fatal("new save overwritten by rejection", current)
				}
				for range 3 {
					if err := e.Retry(bg, en.ID); err != nil && !errors.Is(err, model.ErrGenerationChanged) {
						t.Fatalf("safe retry blocked: %v", err)
					}
				}
				current, _ = s.Entry(bg, en.ID)
				if current.Status != model.StatusSynced {
					t.Fatal("safe retry never completed", current)
				}
				if newerSave && ((kind == model.KindDocx && !strings.Contains(f.xml, "beta newest")) || (kind == model.KindFile && string(f.file) != latest)) {
					t.Fatal("newest generation was not uploaded")
				}
			})
		}
	}
}

func TestCanceledUndispatchedCreateAndRenameRemainRetryable(t *testing.T) {
	for _, reject := range []string{"create", "upload", "rename"} {
		t.Run(reject, func(t *testing.T) {
			kind := model.KindDocx
			if reject == "upload" {
				kind = model.KindFile
			}
			s, e, f, _ := setup(t, kind)
			en, err := s.EnsureEntry(bg, "new.md", kind)
			if err != nil {
				t.Fatal(err)
			}
			en = stageContent(t, s, en, "new body\n")
			ctx, cancel := context.WithCancel(bg)
			defer cancel()
			e.source = &cancelUndispatchedCloud{fixtureCloud: f, reject: reject, cancel: cancel}
			if err = e.SyncEntry(ctx, en.ID); !errors.Is(err, cloud.ErrNotDispatched) {
				t.Fatal("transport proof lost", err)
			}
			creates, err := s.PendingCreates(bg)
			if err != nil {
				t.Fatal(err)
			}
			if reject == "rename" {
				if len(creates) != 1 || creates[0].State != "bound" {
					t.Fatal("rejected rename remains unknown", creates)
				}
			} else if len(creates) != 0 {
				t.Fatal("rejected creation remains unknown", creates)
			}
			if err = e.Retry(bg, en.ID); err != nil {
				t.Fatal("safe creation retry blocked", err)
			}
			current, _ := s.Entry(bg, en.ID)
			if current.Status != model.StatusSynced || f.creates > 1 || f.writes > 1 {
				t.Fatal("retry duplicated or failed creation", current, f.creates, f.writes)
			}
		})
	}
}

func TestCanceledUndispatchedNamespaceStepsRemainRetryable(t *testing.T) {
	for _, step := range []string{"move", "rename", "delete", "folder", "folder-rename"} {
		t.Run(step, func(t *testing.T) {
			s, e, f, en := setup(t, model.KindFile)
			op := model.NamespaceOp{Kind: "rename", Src: en.Remote, Dst: "renamed.bin"}
			reject := step
			switch step {
			case "move":
				if _, err := s.ObserveEntry(bg, store.Observation{Remote: "destination", Kind: model.KindFolder, Token: "destination-token"}); err != nil {
					t.Fatal(err)
				}
				op.Dst = "destination/note.md"
			case "delete":
				op = model.NamespaceOp{Kind: "delete", Src: en.Remote}
			case "folder", "folder-rename":
				op = model.NamespaceOp{Kind: "mkdir", Dst: "new-folder"}
				if step == "folder-rename" {
					reject = "rename"
				}
			}
			prepared, err := s.PrepareNamespace(bg, op)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = s.SealNamespace(bg, prepared.ID); err != nil {
				t.Fatal(err)
			}
			if _, err = s.CommitNamespace(bg, prepared.ID); err != nil {
				t.Fatal(err)
			}
			if err = s.FinishNamespace(bg, prepared.ID); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(bg)
			defer cancel()
			e.source = &cancelUndispatchedCloud{fixtureCloud: f, reject: reject, cancel: cancel}
			if err = e.syncNamespace(ctx); !errors.Is(err, cloud.ErrNotDispatched) {
				t.Fatal("transport proof lost", err)
			}
			work, err := s.NamespaceJournal(bg, prepared.ID)
			if err != nil || work.Dispatched {
				t.Fatalf("known rejected namespace step remains dispatched: %+v %v", work, err)
			}
			if err = e.syncNamespace(bg); err != nil {
				t.Fatal("safe namespace retry blocked", err)
			}
		})
	}
}
