package store

import (
	"errors"
	"strings"
	"testing"

	"github.com/rclone/rclone/internal/bridge/model"
)

func TestNamespaceClassificationCommitsOnlyWithRename(t *testing.T) {
	s, _, _ := openTest(t)
	e := entry(t, s, "draft.tmp")
	e, _ = stage(t, s, e, "# note")
	op, err := s.PrepareNamespace(ctx, model.NamespaceOp{Kind: "rename", Src: e.Remote, Dst: "note.md"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ConfigureNamespaceKind(ctx, op.ID, model.KindDocx); err != nil {
		t.Fatal(err)
	}
	current, _ := s.Entry(ctx, e.ID)
	if current.Kind != model.KindFile {
		t.Fatal("classification published before commit")
	}
	if err = s.AbortNamespace(ctx, op.ID); err != nil {
		t.Fatal(err)
	}
	current, _ = s.Entry(ctx, e.ID)
	if current.Kind != model.KindFile || read(t, s, current.Local) != "# note" {
		t.Fatal("abort changed content or classification")
	}
	op, err = s.PrepareNamespace(ctx, model.NamespaceOp{Kind: "rename", Src: e.Remote, Dst: "note.md"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ConfigureNamespaceKind(ctx, op.ID, model.KindDocx); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SealNamespace(ctx, op.ID); err != nil {
		t.Fatal(err)
	}
	current, err = s.CommitNamespace(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.Kind != model.KindDocx || current.Remote != "note.md" || current.ID != e.ID {
		t.Fatal(current)
	}
}

func TestUnboundTempReplacementKeepsDocxKind(t *testing.T) {
	s, _, _ := openTest(t)
	dst, err := s.ObserveEntry(ctx, Observation{Remote: "note.md", Kind: model.KindDocx, Token: "existing-doc"})
	if err != nil {
		t.Fatal(err)
	}
	src := entry(t, s, "note.tmp")
	src, _ = stage(t, s, src, "# replacement")
	op, err := s.PrepareNamespace(ctx, model.NamespaceOp{Kind: "replace", Src: src.Remote, Dst: dst.Remote})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.SealNamespace(ctx, op.ID); err != nil {
		t.Fatal(err)
	}
	got, err := s.CommitNamespace(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != model.KindDocx || got.Token != dst.Token || got.ID != dst.ID || read(t, s, got.Local) != "# replacement" {
		t.Fatal(got)
	}
}

func TestNamespaceReclassifiesOversizeLocalContentButNotBoundIdentity(t *testing.T) {
	s, _, _ := openTest(t)
	e := bind(t, s, entry(t, s, "old.bin"), "bound")
	op, err := s.PrepareNamespace(ctx, model.NamespaceOp{Kind: "rename", Src: e.Remote, Dst: "old.md"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ConfigureNamespaceKind(ctx, op.ID, model.KindDocx); !errors.Is(err, model.ErrUnsupportedContent) {
		t.Fatal(err)
	}
	if err = s.AbortNamespace(ctx, op.ID); err != nil {
		t.Fatal(err)
	}
	e = entry(t, s, "large.tmp")
	e, _ = stage(t, s, e, strings.Repeat("a", (10<<20)+1))
	op, err = s.PrepareNamespace(ctx, model.NamespaceOp{Kind: "rename", Src: e.Remote, Dst: "large.md"})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.ConfigureNamespaceKind(ctx, op.ID, model.KindDocx); err != nil {
		t.Fatal(err)
	}
	if _, err = s.SealNamespace(ctx, op.ID); err != nil {
		t.Fatal(err)
	}
	current, err := s.CommitNamespace(ctx, op.ID)
	if err != nil {
		t.Fatalf("oversize text must remain locally saved: %v", err)
	}
	if current.Kind != model.KindDocx || current.Local != e.Local || current.Status != model.StatusBlocked {
		t.Fatalf("local document state: %+v", current)
	}
}

func TestEntryByTokenRetainsDeletedIdentity(t *testing.T) {
	s, _, _ := openTest(t)
	e := bind(t, s, entry(t, s, "removed.bin"), "stable-token")
	op, err := s.PrepareNamespace(ctx, model.NamespaceOp{Kind: "delete", Src: e.Remote})
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
	got, err := s.EntryByToken(ctx, e.Token)
	if err != nil || got.ID != e.ID {
		t.Fatalf("%+v %v", got, err)
	}
	meta, err := s.Metadata(ctx, got.ID)
	if err != nil || !meta.Tombstone {
		t.Fatalf("%+v %v", meta, err)
	}
}
