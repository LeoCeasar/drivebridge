package manager

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
)

func TestLinkRecreatedObjectDoesNotReuseOldURL(t *testing.T) {
	ctx := context.Background()
	m, id, _, _ := fixture(t, nil)
	if _, err := m.Start(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	s := m.localWorker(id).services.store
	e, err := s.ObserveEntry(ctx, store.Observation{Remote: "restored.bin", Kind: model.KindFile, Token: "old-token", SourceURL: "https://tenant.feishu.cn/file/old-token"})
	if err != nil {
		t.Fatal(err)
	}
	e, _, err = s.Stage(ctx, e.ID, nil, strings.NewReader("local recovery"))
	if err != nil {
		t.Fatal(err)
	}
	conflict, removed, err := s.ObserveRemoteDeletion(ctx, e)
	if err != nil || removed {
		t.Fatal(conflict, removed, err)
	}
	restored, job, err := s.ResolveRemoteDeletion(ctx, model.ResolveRequest{ConflictID: conflict.ID, ExpectedGeneration: e.Generation, ExpectedRemoteRevision: conflict.RemoteRevision, Action: model.ResolveRestoreLocal})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = s.Prepare(ctx, job.ID); err != nil {
		t.Fatal(err)
	}
	create, err := s.PrepareCreate(ctx, job.ID, "parent", restored.Remote)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.BindCreated(ctx, create.ID, "new-token", ""); err != nil {
		t.Fatal(err)
	}
	spec, _ := m.spec(ctx, id)
	lookedUp := false
	got, err := m.link(ctx, filepath.Join(spec.Mount.Mountpoint, e.Remote), func(_ context.Context, _ mountSpec, target linkTarget) (string, error) {
		lookedUp = true
		if target.Entry.Token != "new-token" {
			t.Fatal(target.Entry)
		}
		return "https://tenant.feishu.cn/file/new-token", nil
	})
	if err != nil || !lookedUp || got.Cached || got.URL != "https://tenant.feishu.cn/file/new-token" {
		t.Fatalf("reused old URL: %+v lookup=%v %v", got, lookedUp, err)
	}
}

func TestLinkRejectsKnownRemoteDeletionBeforeAndAfterLookup(t *testing.T) {
	for _, duringLookup := range []bool{false, true} {
		t.Run(map[bool]string{false: "cached", true: "during query"}[duringLookup], func(t *testing.T) {
			ctx := context.Background()
			m, id, _, _ := fixture(t, nil)
			if _, err := m.Start(ctx, id, true); err != nil {
				t.Fatal(err)
			}
			s := m.localWorker(id).services.store
			link := "https://tenant.feishu.cn/file/deleted"
			sourceURL := link
			if duringLookup {
				sourceURL = ""
			}
			e, err := s.ObserveEntry(ctx, store.Observation{Remote: "deleted.bin", Kind: model.KindFile, Token: "deleted", SourceURL: sourceURL})
			if err != nil {
				t.Fatal(err)
			}
			e, _, err = s.Stage(ctx, e.ID, nil, strings.NewReader("local changes"))
			if err != nil {
				t.Fatal(err)
			}
			deleted := func() {
				t.Helper()
				_, removed, err := s.ObserveRemoteDeletion(ctx, e)
				if err != nil || removed {
					t.Fatal(removed, err)
				}
			}
			if !duringLookup {
				deleted()
			}
			spec, _ := m.spec(ctx, id)
			_, err = m.link(ctx, filepath.Join(spec.Mount.Mountpoint, e.Remote), func(context.Context, mountSpec, linkTarget) (string, error) {
				if duringLookup {
					deleted()
				}
				return link, nil
			})
			if !errors.Is(err, ErrLinkUnavailable) {
				t.Fatalf("known deleted link accepted: %v", err)
			}
		})
	}
}

func TestLinkWindowsDriveRootNormalization(t *testing.T) {
	if got := linkMountRoot("X:", "windows"); got != `X:\` {
		t.Fatalf("%q", got)
	}
	for _, root := range []string{`X:\`, `X:\mount`, `/Volumes/mount`} {
		if got := linkMountRoot(root, "windows"); got != root {
			t.Fatal(got)
		}
	}
	if got := linkMountRoot("X:", "darwin"); got != "X:" {
		t.Fatal(got)
	}
}

func TestLinkIdentityRejectsCaseOnlyRename(t *testing.T) {
	ctx := context.Background()
	m, id, _, _ := fixture(t, nil)
	if _, err := m.Start(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	s := m.localWorker(id).services.store
	if _, err := s.ObserveEntry(ctx, store.Observation{Remote: "report.txt", Kind: model.KindFile, Token: "same-token"}); err != nil {
		t.Fatal(err)
	}
	before, err := readLinkTarget(ctx, s, "report.txt")
	if err != nil {
		t.Fatal(err)
	}
	op, err := s.PrepareNamespace(ctx, model.NamespaceOp{Kind: "rename", Src: "report.txt", Dst: "REPORT.txt"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SealNamespace(ctx, op.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.CommitNamespace(ctx, op.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.FinishNamespace(ctx, op.ID); err != nil {
		t.Fatal(err)
	}
	after, err := readLinkTarget(ctx, s, "REPORT.txt")
	if err != nil {
		t.Fatal(err)
	}
	if sameLinkIdentity(before, after) {
		t.Fatal("case-only rename must invalidate the pending result")
	}
}
