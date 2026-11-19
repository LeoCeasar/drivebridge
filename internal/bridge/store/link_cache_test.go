package store

import (
	"errors"
	"reflect"
	"testing"

	"github.com/rclone/rclone/internal/bridge/model"
)

func TestCacheSourceURLPreservesEntryAndQueue(t *testing.T) {
	s, dir, scope := openTest(t)
	e, err := s.ObserveEntry(ctx, Observation{Remote: "report.md", Kind: model.KindDocx, Token: "doc"})
	if err != nil {
		t.Fatal(err)
	}
	before, err := s.Pending(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.CacheSourceURL(ctx, e, "", "https://tenant.feishu.cn/docx/doc"); err != nil {
		t.Fatal(err)
	}
	after, _ := s.Entry(ctx, e.ID)
	jobs, _ := s.Pending(ctx)
	if !reflect.DeepEqual(e, after) || !reflect.DeepEqual(before, jobs) {
		t.Fatal("cache changed entry or jobs")
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
	m, err := reopened.Metadata(ctx, e.ID)
	if err != nil || m.CachedLinkURL != "https://tenant.feishu.cn/docx/doc" || m.CachedLinkToken != "doc" || m.CachedLinkKind != "docx" || m.SourceURL != "" {
		t.Fatal(m, err)
	}
}

func TestCacheSourceURLRejectsMismatchedIdentity(t *testing.T) {
	for _, field := range []string{"token", "kind", "remote", "profile", "mount", "virtual", "deleted"} {
		t.Run(field, func(t *testing.T) {
			s, _, _ := openTest(t)
			e, err := s.ObserveEntry(ctx, Observation{Remote: "file.bin", Kind: model.KindFile, Token: "original"})
			if err != nil {
				t.Fatal(err)
			}
			expected := e
			virtual := ""
			switch field {
			case "token":
				expected.Token = "replacement"
			case "kind":
				expected.Kind = model.KindDocx
			case "remote":
				expected.Remote = "renamed.bin"
			case "profile":
				expected.ProfileID = "other"
			case "mount":
				expected.MountID = "other"
			case "virtual":
				virtual = "sheet"
			case "deleted":
				if _, _, err = s.ObserveRemoteDeletion(ctx, e); err != nil {
					t.Fatal(err)
				}
			}
			err = s.CacheSourceURL(ctx, expected, virtual, "https://tenant.feishu.cn/file/original")
			if !errors.Is(err, model.ErrIdentityChanged) && !errors.Is(err, ErrNotFound) {
				t.Fatalf("accepted stale cache: %v", err)
			}
			m, err := s.Metadata(ctx, e.ID)
			if err == nil && m.CachedLinkURL != "" {
				t.Fatal("stale URL stored")
			}
		})
	}
}

func TestCacheSourceURLRejectsDeletedRemoteWithPendingLocalContent(t *testing.T) {
	s, _, _ := openTest(t)
	e, err := s.ObserveEntry(ctx, Observation{Remote: "report.md", Kind: model.KindDocx, Token: "doc"})
	if err != nil {
		t.Fatal(err)
	}
	e, _ = stage(t, s, e, "local changes")
	if _, _, err = s.ObserveRemoteDeletion(ctx, e); err != nil {
		t.Fatal(err)
	}
	err = s.CacheSourceURL(ctx, e, "", "https://tenant.feishu.cn/docx/doc")
	if !errors.Is(err, model.ErrIdentityChanged) {
		t.Fatalf("cached known-deleted object: %v", err)
	}
}
