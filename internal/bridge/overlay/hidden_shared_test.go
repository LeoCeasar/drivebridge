package overlay

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/rclone/rclone/fs"
	"github.com/rclone/rclone/internal/bridge/discovery"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/lib/larkcli"
)

func hiddenFixture(t *testing.T) (*Fs, *accountTestSource, *bool) {
	t.Helper()
	f, _, source, _, _ := accountFixture(t, []discovery.Entry{
		{Token: "hidden", Name: "Hidden", Memberships: []string{discovery.SharedHidden}},
		{Token: "hidden-sibling", Name: "Hidden extra", Memberships: []string{discovery.SharedVisible}},
		{Token: "both", Name: "Both", Memberships: []string{discovery.SharedVisible, discovery.SharedHidden}},
		{Token: "manual", Name: "Manual", Memberships: []string{discovery.SharedHidden, discovery.Manual}, Manual: true},
	})
	enabled := true
	f.source.(*accountSource).options.SyncHiddenShared = func(context.Context) (bool, error) { return enabled, nil }
	source.tree["hidden"] = []larkcli.DriveItem{{Token: "body", Name: "body.md", Type: "file", ParentToken: "hidden"}, {Token: "sub", Name: "Sub", Type: "folder", ParentToken: "hidden"}}
	source.tree["sub"] = []larkcli.DriveItem{{Token: "cold", Name: "cold.md", Type: "file", ParentToken: "sub"}}
	accountList(t, f, "")
	accountList(t, f, "共享目录/Hidden")
	accountList(t, f, "共享目录/Hidden/Sub")
	return f, source, &enabled
}
func TestHiddenSharedSwitchHidesCachedRootAndRetainsAcceptedData(t *testing.T) {
	f, source, enabled := hiddenFixture(t)
	entry, err := f.store.EntryByToken(ctx, "body")
	if err != nil {
		t.Fatal(err)
	}
	entry, _, err = f.store.Stage(ctx, entry.ID, nil, strings.NewReader("accepted local pending\n"))
	if err != nil {
		t.Fatal(err)
	}
	before, _ := f.store.Pending(ctx)
	*enabled = false
	rows := accountList(t, f, "共享目录")
	if rows["共享目录/Hidden"] != nil || rows["共享目录/Both"] == nil || rows["共享目录/Manual"] == nil || rows["共享目录/Hidden extra"] == nil {
		t.Fatal(rows)
	}
	if _, err = f.List(ctx, "共享目录/Hidden"); !errors.Is(err, fs.ErrorDirNotFound) {
		t.Fatal("disabled subtree list", err)
	}
	after, _ := f.store.Pending(ctx)
	saved, _ := f.store.Entry(ctx, entry.ID)
	if len(after) != len(before) || saved.Local != entry.Local || saved.Token != "body" {
		t.Fatal("data removed", after, saved)
	}
	beforeCalls := len(source.parents)
	if err = f.WarmDirectories(ctx); err != nil {
		t.Fatal(err)
	}
	for _, token := range source.parents[beforeCalls:] {
		if token == "hidden" || token == "sub" {
			t.Fatal("disabled metadata downloaded", token)
		}
	}
	allowed, err := f.ShouldSync(ctx, entry.ID)
	if err != nil || allowed {
		t.Fatal("pending upload not paused", allowed, err)
	}
	*enabled = true
	rows = accountList(t, f, "共享目录")
	if rows["共享目录/Hidden"] == nil {
		t.Fatal("root not restored")
	}
	allowed, err = f.ShouldSync(ctx, entry.ID)
	if err != nil || !allowed {
		t.Fatal("pending upload not resumed", allowed, err)
	}
	restored, _ := f.store.Entry(ctx, entry.ID)
	if restored.Local != entry.Local || restored.ID != entry.ID {
		t.Fatal("identity changed")
	}
}
func TestHiddenSharedSwitchRejectsColdRemoteBodyOnly(t *testing.T) {
	f, source, enabled := hiddenFixture(t)
	*enabled = false
	before := source.observes + source.fetches
	if _, err := f.MaterializeManaged(ctx, "共享目录/Hidden/Sub/cold.md"); !errors.Is(err, fs.ErrorObjectNotFound) {
		t.Fatal(err)
	}
	if source.observes+source.fetches != before {
		t.Fatal("cold content fetched")
	}
}
func TestHiddenSharedPolicyPathBoundariesAndFailure(t *testing.T) {
	f, _, enabled := hiddenFixture(t)
	*enabled = false
	for _, tc := range []struct {
		p    string
		want bool
	}{{"", true}, {"个人目录", true}, {"共享目录/Hidden", false}, {"共享目录/Hidden/new.md", false}, {"共享目录/Hidden extra/new.md", true}, {"共享目录/Both/a.md", true}, {"共享目录/Manual/new.md", true}} {
		got, err := f.SyncPathAllowed(ctx, tc.p)
		if err != nil || got != tc.want {
			t.Fatalf("%q: %v %v", tc.p, got, err)
		}
	}
	f.source.(*accountSource).options.SyncHiddenShared = func(context.Context) (bool, error) { return false, model.ErrIdentityChanged }
	if allowed, err := f.SyncPathAllowed(ctx, "共享目录/Hidden/a.md"); allowed || !errors.Is(err, model.ErrIdentityChanged) {
		t.Fatal(allowed, err)
	}
}

func TestHiddenSharedPendingRenameCannotBypassPausedSource(t *testing.T) {
	f, _, enabled := hiddenFixture(t)
	body, err := f.store.EntryByToken(ctx, "body")
	if err != nil {
		t.Fatal(err)
	}
	body, _, err = f.store.Stage(ctx, body.ID, nil, strings.NewReader("new local bytes\n"))
	if err != nil {
		t.Fatal(err)
	}
	op, err := f.store.PrepareNamespace(ctx, model.NamespaceOp{Kind: "rename", Src: body.Remote, Dst: "共享目录/Both/from-hidden.md"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.SealNamespace(ctx, op.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = f.store.CommitNamespace(ctx, op.ID); err != nil {
		t.Fatal(err)
	}
	if err = f.store.FinishNamespace(ctx, op.ID); err != nil {
		t.Fatal(err)
	}
	*enabled = false
	if allowed, err := f.SyncEntryAllowed(ctx, body.ID); err != nil || allowed {
		t.Fatal("rename bypassed disabled source", allowed, err)
	}
	*enabled = true
	if allowed, err := f.SyncEntryAllowed(ctx, body.ID); err != nil || !allowed {
		t.Fatal("rename did not resume", allowed, err)
	}
}

func TestHiddenSharedManualCoveredChildKeepsCanonicalAnchor(t *testing.T) {
	f, _, _, snapshot, _ := accountFixture(t, []discovery.Entry{
		{Token: "anchor", Name: "Hidden anchor", Memberships: []string{discovery.SharedHidden}},
		{Token: "explicit-child", Name: "Added child", Manual: true, Placement: "shared", CanonicalRootToken: "anchor", RelativePath: []string{"Added child"}, Memberships: []string{discovery.Manual}},
	})
	f.source.(*accountSource).options.SyncHiddenShared = func(context.Context) (bool, error) { return false, nil }
	rows := accountList(t, f, "共享目录")
	if rows["共享目录/Hidden anchor"] == nil {
		t.Fatal("manual child lost its canonical path", snapshot.Entries)
	}
}
