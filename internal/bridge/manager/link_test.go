package manager

import (
	"context"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/store"
	"github.com/rclone/rclone/lib/larkcli"
)

func TestLinkLookupDeadlineIsDistinctFromOtherNetworkFailures(t *testing.T) {
	m, id, _, _ := fixture(t, nil)
	if _, err := m.Start(context.Background(), id, true); err != nil {
		t.Fatal(err)
	}
	s := m.localWorker(id).services.store
	e, err := s.ObserveEntry(context.Background(), store.Observation{Remote: "uncached.html", Kind: model.KindFile, Token: "file"})
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := m.spec(context.Background(), id)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, err = m.link(ctx, filepath.Join(spec.Mount.Mountpoint, e.Remote), func(ctx context.Context, _ mountSpec, _ linkTarget) (string, error) {
		<-ctx.Done()
		return "", ctx.Err()
	})
	info := ErrorInfo(err)
	if info.Code != "link_lookup_timeout" || !info.Retryable || !errors.Is(err, context.DeadlineExceeded) || ExitCode(err) != 5 {
		t.Fatalf("timeout not classified: %+v %v", info, err)
	}
	ordinary := errors.New("provider connection failed")
	_, err = m.link(context.Background(), filepath.Join(spec.Mount.Mountpoint, e.Remote), func(context.Context, mountSpec, linkTarget) (string, error) { return "", ordinary })
	if !errors.Is(err, ordinary) || ErrorInfo(err).Code == "link_lookup_timeout" {
		t.Fatalf("ordinary failure reclassified: %v", err)
	}
	lateCtx, lateCancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer lateCancel()
	_, err = m.link(lateCtx, filepath.Join(spec.Mount.Mountpoint, e.Remote), func(ctx context.Context, _ mountSpec, _ linkTarget) (string, error) {
		<-ctx.Done()
		return "", larkcli.ErrAuthRequired
	})
	if ErrorInfo(err).Code != "unauthorized" {
		t.Fatalf("authorization failure hidden by deadline: %v", err)
	}
}

func TestLinkUsesProjectedIdentityThroughWorker(t *testing.T) {
	m, id, _, _ := fixture(t, nil)
	ctx := context.Background()
	if _, err := m.Start(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	s := m.localWorker(id).services.store
	e, err := s.ObserveEntry(ctx, store.Observation{Remote: "方案~123.md", Kind: model.KindDocx, Token: "cloud-doc", SourceURL: "https://tenant.feishu.cn/docx/cloud-doc"})
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := m.spec(ctx, id)
	got, err := m.Link(ctx, filepath.Join(spec.Mount.Mountpoint, e.Remote))
	if err != nil || got.URL != "https://tenant.feishu.cn/docx/cloud-doc" || got.EntryID != e.ID || !got.Cached {
		t.Fatalf("%+v %v", got, err)
	}
	for _, p := range []string{spec.Mount.Mountpoint + "-other/方案~123.md", "relative.md", filepath.Join(spec.Mount.Mountpoint, "..", "outside")} {
		if _, err = m.Link(ctx, p); !errors.Is(err, ErrLinkUnsupported) {
			t.Fatalf("accepted %q: %v", p, err)
		}
	}
}

func TestLinkPersistsOnlineLookupForLaterOfflineCopies(t *testing.T) {
	for _, stopped := range []bool{false, true} {
		t.Run(map[bool]string{false: "worker", true: "stopped"}[stopped], func(t *testing.T) {
			ctx := context.Background()
			m, id, _, _ := fixture(t, nil)
			if _, err := m.Start(ctx, id, true); err != nil {
				t.Fatal(err)
			}
			s := m.localWorker(id).services.store
			e, err := s.ObserveEntry(ctx, store.Observation{Remote: "cached-link.md", Kind: model.KindDocx, Token: "stable-token"})
			if err != nil {
				t.Fatal(err)
			}
			spec, _ := m.spec(ctx, id)
			if stopped {
				if err := m.Stop(ctx, id, 0); err != nil {
					t.Fatal(err)
				}
			}
			lookups := 0
			lookup := func(context.Context, mountSpec, linkTarget) (string, error) {
				lookups++
				return "https://tenant.feishu.cn/docx/stable-token", nil
			}
			first, err := m.link(ctx, filepath.Join(spec.Mount.Mountpoint, e.Remote), lookup)
			if err != nil || first.Cached {
				t.Fatalf("first %+v %v", first, err)
			}
			second, err := m.link(ctx, filepath.Join(spec.Mount.Mountpoint, e.Remote), func(context.Context, mountSpec, linkTarget) (string, error) {
				lookups++
				return "", errors.New("offline")
			})
			if err != nil || !second.Cached || second.URL != first.URL || lookups != 1 {
				t.Fatalf("cache lost: %+v %v lookups=%d", second, err, lookups)
			}
			current, err := m.linkTarget(ctx, id, e.Remote)
			if err != nil || current.Entry.Generation != e.Generation || current.Entry.Local != e.Local {
				t.Fatalf("body mutated: %+v %v", current, err)
			}
		})
	}
}

func TestLinkVirtualCacheSurvivesURLLessObservationWithoutChangingProjection(t *testing.T) {
	ctx := context.Background()
	m, id, _, _ := fixture(t, nil)
	if _, err := m.Start(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	s := m.localWorker(id).services.store
	observation := store.Observation{Remote: "sheet.unsupported.txt", Kind: model.KindFile, Token: "sheet-token", VirtualType: "sheet"}
	e, err := s.ObserveEntry(ctx, observation)
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := m.spec(ctx, id)
	_, err = m.link(ctx, filepath.Join(spec.Mount.Mountpoint, e.Remote), func(context.Context, mountSpec, linkTarget) (string, error) {
		return "https://tenant.feishu.cn/sheets/sheet-token", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	metadata, _ := s.Metadata(ctx, e.ID)
	if metadata.SourceURL != "" {
		t.Fatalf("lookup changed projected virtual content: %s", metadata.SourceURL)
	}
	if _, err = s.ObserveEntry(ctx, observation); err != nil {
		t.Fatal(err)
	}
	got, err := m.link(ctx, filepath.Join(spec.Mount.Mountpoint, e.Remote), func(context.Context, mountSpec, linkTarget) (string, error) { return "", errors.New("offline") })
	if err != nil || !got.Cached || got.URL != "https://tenant.feishu.cn/sheets/sheet-token" {
		t.Fatalf("virtual cache lost: %+v %v", got, err)
	}
}

func TestLinkRejectsUnpublishedAndSyntheticItems(t *testing.T) {
	m, id, _, _ := fixture(t, nil)
	ctx := context.Background()
	if _, err := m.Start(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	s := m.localWorker(id).services.store
	if _, err := s.EnsureEntry(ctx, "draft.md", model.KindFile); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ObserveEntry(ctx, store.Observation{Remote: "共享目录", Kind: model.KindFolder, Token: "drivebridge-account-example-shared", SourceURL: "https://tenant.feishu.cn/drive/folder/fake"}); err != nil {
		t.Fatal(err)
	}
	spec, _ := m.spec(ctx, id)
	for _, tc := range []struct {
		path string
		want error
	}{{"draft.md", ErrLinkNotUploaded}, {"共享目录", ErrLinkUnsupported}} {
		if _, err := m.Link(ctx, filepath.Join(spec.Mount.Mountpoint, tc.path)); !errors.Is(err, tc.want) {
			t.Fatalf("%s: %v", tc.path, err)
		}
	}
}

func TestLinkRechecksIdentityAfterLookup(t *testing.T) {
	ctx := context.Background()
	m, id, _, _ := fixture(t, nil)
	if _, err := m.Start(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	s := m.localWorker(id).services.store
	e, err := s.ObserveEntry(ctx, store.Observation{Remote: "file.txt", Kind: model.KindFile, Token: "original"})
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := m.spec(ctx, id)
	_, err = m.link(ctx, filepath.Join(spec.Mount.Mountpoint, e.Remote), func(context.Context, mountSpec, linkTarget) (string, error) {
		op, err := s.PrepareNamespace(ctx, model.NamespaceOp{Kind: "rename", Src: e.Remote, Dst: "renamed.txt"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.SealNamespace(ctx, op.ID); err != nil {
			t.Fatal(err)
		}
		if _, err = s.CommitNamespace(ctx, op.ID); err != nil {
			t.Fatal(err)
		}
		return "https://tenant.feishu.cn/file/original", nil
	})
	if err == nil {
		t.Fatalf("changed object accepted: %v", err)
	}
}

func TestLinkPreservesPendingLocalContentAndWorksWhenStopped(t *testing.T) {
	ctx := context.Background()
	m, id, _, _ := fixture(t, nil)
	if _, err := m.Start(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	s := m.localWorker(id).services.store
	e, err := s.ObserveEntry(ctx, store.Observation{Remote: "项目 ' $(test),草稿.md", Kind: model.KindDocx, Token: "doc", SourceURL: "https://tenant.feishu.cn/docx/doc"})
	if err != nil {
		t.Fatal(err)
	}
	e, _, err = s.Stage(ctx, e.ID, nil, strings.NewReader("pending local content"))
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := m.spec(ctx, id)
	got, err := m.Link(ctx, filepath.Join(spec.Mount.Mountpoint, e.Remote))
	if err != nil || !got.PendingChanges {
		t.Fatalf("%+v %v", got, err)
	}
	current, err := s.Entry(ctx, e.ID)
	if err != nil || current.Local != e.Local || current.Generation != e.Generation {
		t.Fatal("link modified local save", current, err)
	}
	// A separate clean entry demonstrates locked store fallback after worker stop.
	clean, err := s.ObserveEntry(ctx, store.Observation{Remote: "folder", Kind: model.KindFolder, Token: "folder", SourceURL: "https://tenant.feishu.cn/drive/folder/folder"})
	if err != nil {
		t.Fatal(err)
	}
	// Use another mount for stop, since the pending edit correctly prevents stop.
	m2, id2, _, _ := fixture(t, nil)
	if _, err = m2.Start(ctx, id2, true); err != nil {
		t.Fatal(err)
	}
	s2 := m2.localWorker(id2).services.store
	if _, err = s2.ObserveEntry(ctx, store.Observation{Remote: clean.Remote, Kind: clean.Kind, Token: clean.Token, SourceURL: "https://tenant.feishu.cn/drive/folder/folder"}); err != nil {
		t.Fatal(err)
	}
	if err = m2.Stop(ctx, id2, 0); err != nil {
		t.Fatal(err)
	}
	sp2, _ := m2.spec(ctx, id2)
	got, err = m2.Link(ctx, filepath.Join(sp2.Mount.Mountpoint, clean.Remote))
	if err != nil || !got.Cached {
		t.Fatalf("stopped %+v %v", got, err)
	}
}

func TestLinkUnicodeSelection(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "windows" {
		t.Skip("platform path normalization")
	}
	ctx := context.Background()
	m, id, _, _ := fixture(t, nil)
	if _, err := m.Start(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	s := m.localWorker(id).services.store
	if _, err := s.ObserveEntry(ctx, store.Observation{Remote: "café.md", Kind: model.KindDocx, Token: "doc", SourceURL: "https://tenant.feishu.cn/docx/doc"}); err != nil {
		t.Fatal(err)
	}
	sp, _ := m.spec(ctx, id)
	selection := "cafe\u0301.md"
	if runtime.GOOS == "windows" {
		selection = "CAFE\u0301.MD"
	}
	got, err := m.Link(ctx, filepath.Join(sp.Mount.Mountpoint, selection))
	if err != nil || got.URL == "" {
		t.Fatalf("%+v %v", got, err)
	}
}

func TestShellLinkUsesAuthenticatedDesktopConfig(t *testing.T) {
	m, _, _, _ := fixture(t, nil)
	ctx := context.Background()
	got, err := m.desktopConfigForShell(ctx)
	if err != nil || got != "" {
		t.Fatalf("no desktop: %q %v", got, err)
	}
	release, err := m.DesktopRegister(ctx, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	got, err = m.desktopConfigForShell(ctx)
	if err != nil || got != m.ConfigFile() {
		t.Fatalf("active config: %q %v", got, err)
	}
}

func TestLinkFallbackAndUnsyncedWarning(t *testing.T) {
	ctx := context.Background()
	m, id, _, _ := fixture(t, nil)
	if _, err := m.Start(ctx, id, true); err != nil {
		t.Fatal(err)
	}
	s := m.localWorker(id).services.store
	e, err := s.ObserveEntry(ctx, store.Observation{Remote: "folder", Kind: model.KindFolder, Token: "cloud-folder"})
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := m.spec(ctx, id)
	got, err := m.link(ctx, filepath.Join(spec.Mount.Mountpoint, e.Remote), func(_ context.Context, sp mountSpec, target linkTarget) (string, error) {
		if sp.Profile.ID != spec.Profile.ID || target.Entry.Token != e.Token || target.Entry.Kind != model.KindFolder {
			t.Fatal("wrong cloud binding")
		}
		return "https://tenant.feishu.cn/drive/folder/cloud-folder", nil
	})
	if err != nil || got.Cached || got.URL == "" {
		t.Fatalf("%+v %v", got, err)
	}
}
