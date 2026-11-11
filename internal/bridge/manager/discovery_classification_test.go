package manager

import (
	"context"
	"errors"
	"fmt"
	"github.com/rclone/rclone/internal/bridge/discovery"
	"github.com/rclone/rclone/lib/larkcli"
	"testing"
)

type classificationGraph struct {
	tree  map[string][]larkcli.DriveItem
	fail  map[string]bool
	calls []string
}

func (g *classificationGraph) List(_ context.Context, token string) ([]larkcli.DriveItem, error) {
	g.calls = append(g.calls, token)
	if g.fail[token] {
		return nil, errors.New("private upstream response")
	}
	items, ok := g.tree[token]
	if !ok {
		return nil, errors.New("missing current list")
	}
	return items, nil
}
func graphFolder(token, name, parent string) larkcli.DriveItem {
	return larkcli.DriveItem{Token: token, Name: name, Type: "folder", ParentToken: parent}
}
func TestManualFolderClassificationUsesCurrentPersonalTree(t *testing.T) {
	g := &classificationGraph{tree: map[string][]larkcli.DriveItem{"": {graphFolder("personalRoot", "Personal", "")}, "personalRoot": {graphFolder("nested", "Nested", "personalRoot")}, "nested": {graphFolder("target", "Target", "nested")}}}
	got, err := classifyManualFolder(context.Background(), g, discovery.Snapshot{}, DiscoveryCandidate{Token: "target", Name: "Target"})
	if err != nil || got.Placement != "personal" || got.CanonicalRootToken != "personalRoot" || got.RootName != "Personal" || len(got.RelativePath) != 2 || got.RelativePath[0] != "Nested" {
		t.Fatalf("%+v %v", got, err)
	}
}
func TestManualFolderClassificationReusesKnownSharedAncestor(t *testing.T) {
	snapshot := discovery.Snapshot{Entries: []discovery.Entry{{Token: "sharedRoot", Name: "Shared", Memberships: []string{discovery.SharedHidden}}}}
	g := &classificationGraph{tree: map[string][]larkcli.DriveItem{"": {graphFolder("personalRoot", "Personal", "")}, "personalRoot": {}, "sharedRoot": {graphFolder("target", "Target", "sharedRoot")}}}
	got, err := classifyManualFolder(context.Background(), g, snapshot, DiscoveryCandidate{Token: "target", Name: "Target"})
	if err != nil || got.Placement != "shared" || got.CanonicalRootToken != "sharedRoot" || len(got.RelativePath) != 1 {
		t.Fatalf("%+v %v", got, err)
	}
}
func TestManualFolderClassificationRequiresCompleteExclusion(t *testing.T) {
	snapshot := discovery.Snapshot{Entries: []discovery.Entry{{Token: "sharedRoot", Name: "Shared", Memberships: []string{discovery.SharedVisible}}}}
	for _, failed := range []string{"personalRoot", "sharedRoot"} {
		g := &classificationGraph{tree: map[string][]larkcli.DriveItem{"": {graphFolder("personalRoot", "Personal", "")}, "personalRoot": {}, "sharedRoot": {}}, fail: map[string]bool{failed: true}}
		if _, err := classifyManualFolder(context.Background(), g, snapshot, DiscoveryCandidate{Token: "target", Name: "Target"}); !errors.Is(err, ErrDiscoveryUnclassified) {
			t.Fatalf("incomplete tree guessed sharing: %v", err)
		}
	}
	g := &classificationGraph{tree: map[string][]larkcli.DriveItem{"": {graphFolder("personalRoot", "Personal", "")}, "personalRoot": {}, "sharedRoot": {}}}
	got, err := classifyManualFolder(context.Background(), g, snapshot, DiscoveryCandidate{Token: "target", Name: "Target"})
	if err != nil || got.Placement != "shared" || got.CanonicalRootToken != "target" {
		t.Fatalf("complete exclusion: %+v %v", got, err)
	}
}
func TestManualFolderClassificationHonorsExistingNestedAnchor(t *testing.T) {
	snapshot := discovery.Snapshot{Entries: []discovery.Entry{{Token: "outer", Name: "Outer", Memberships: []string{discovery.SharedVisible}}, {Token: "inner", Name: "Inner", Memberships: []string{discovery.SharedHidden}}}}
	g := &classificationGraph{tree: map[string][]larkcli.DriveItem{"": {}, "outer": {graphFolder("inner", "Inner", "outer")}, "inner": {graphFolder("target", "Target", "inner")}}}
	got, err := classifyManualFolder(context.Background(), g, snapshot, DiscoveryCandidate{Token: "target", Name: "Target"})
	if err != nil || got.CanonicalRootToken != "inner" || len(got.RelativePath) != 1 {
		t.Fatalf("nested anchor identity changed: %+v %v", got, err)
	}
}
func TestManualFolderClassificationRejectsCyclesAndParentMismatch(t *testing.T) {
	for _, children := range [][]larkcli.DriveItem{{graphFolder("root", "Root", "root")}, {graphFolder("child", "Child", "differentParent")}} {
		g := &classificationGraph{tree: map[string][]larkcli.DriveItem{"": {graphFolder("root", "Root", "")}, "root": children}}
		if _, err := classifyManualFolder(context.Background(), g, discovery.Snapshot{}, DiscoveryCandidate{Token: "target", Name: "Target"}); !errors.Is(err, ErrDiscoveryUnclassified) {
			t.Fatal(err)
		}
	}
}

type classificationMetaGraph struct {
	*classificationGraph
	metadata map[string]DiscoveryCandidate
}

func (g classificationMetaGraph) FolderMeta(_ context.Context, token string) (DiscoveryCandidate, error) {
	meta, ok := g.metadata[token]
	if !ok {
		return DiscoveryCandidate{}, errors.New("metadata unavailable")
	}
	return meta, nil
}
func TestManualFolderClassificationUsesConfirmedRootWithoutTreeWalk(t *testing.T) {
	for _, placement := range []string{"personal", "shared"} {
		g := &classificationGraph{}
		snapshot := discovery.Snapshot{Entries: []discovery.Entry{{Token: "target", Name: "Old", Placement: placement}}}
		got, err := classifyManualFolder(context.Background(), g, snapshot, DiscoveryCandidate{Token: "target", Name: "Canonical"})
		if err != nil || got.Placement != placement || got.CanonicalRootToken != "target" || len(g.calls) != 0 {
			t.Fatalf("known root walked whole tree: %+v %v", got, err)
		}
	}
}
func TestManualFolderClassificationUsesVerifiedImmediateParentID(t *testing.T) {
	snapshot := discovery.Snapshot{Entries: []discovery.Entry{{Token: "root", Name: "Root", Memberships: []string{discovery.Personal}}}}
	g := classificationMetaGraph{classificationGraph: &classificationGraph{tree: map[string][]larkcli.DriveItem{"root": {graphFolder("target", "Target", "root")}}}, metadata: map[string]DiscoveryCandidate{"root": {Token: "root", Name: "Root", nodeID: "456"}}}
	got, err := classifyManualFolder(context.Background(), g, snapshot, DiscoveryCandidate{Token: "target", Name: "Target", parentID: "456"})
	if err != nil || got.Placement != "personal" || got.CanonicalRootToken != "root" || len(g.calls) != 1 || g.calls[0] != "root" {
		t.Fatalf("parent edge was not verified directly: %+v %v", got, err)
	}
	g.tree["root"] = nil
	if _, err = classifyManualFolder(context.Background(), g, snapshot, DiscoveryCandidate{Token: "target", Name: "Target", parentID: "456"}); !errors.Is(err, ErrDiscoveryUnclassified) {
		t.Fatal("metadata parent id alone guessed placement", err)
	}
}
func TestManualFolderClassificationStopsAtBudgetAndCrossRootCycle(t *testing.T) {
	g := &classificationGraph{tree: map[string][]larkcli.DriveItem{"": {graphFolder("root", "Root", "")}}}
	for i := 0; i < classificationMaxFolders; i++ {
		token := fmt.Sprintf("folder%d", i)
		g.tree["root"] = append(g.tree["root"], graphFolder(token, "Folder", "root"))
		g.tree[token] = nil
	}
	if _, err := classifyManualFolder(context.Background(), g, discovery.Snapshot{}, DiscoveryCandidate{Token: "target", Name: "Target"}); !errors.Is(err, ErrDiscoveryUnclassified) {
		t.Fatal("budget exhaustion guessed placement", err)
	}
	g = &classificationGraph{tree: map[string][]larkcli.DriveItem{"": {}, "first": {graphFolder("second", "Second", "first")}, "second": {graphFolder("first", "First", "second")}}}
	snapshot := discovery.Snapshot{Entries: []discovery.Entry{{Token: "first", Name: "First", Memberships: []string{discovery.SharedVisible}}, {Token: "second", Name: "Second", Memberships: []string{discovery.SharedHidden}}}}
	if _, err := classifyManualFolder(context.Background(), g, snapshot, DiscoveryCandidate{Token: "target", Name: "Target"}); !errors.Is(err, ErrDiscoveryUnclassified) {
		t.Fatal("cyclic roots allowed complete exclusion", err)
	}
}
