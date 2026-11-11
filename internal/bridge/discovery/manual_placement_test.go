package discovery

import (
	"strings"
	"testing"
)

func TestVerifiedManualPlacementPreservesSourceAndCanonicalAncestor(t *testing.T) {
	binding := Binding{ProfileID: "profile", IdentityHash: strings.Repeat("a", 64), UserID: "123", Origin: "https://team.feishu.cn"}
	snapshot := Snapshot{Binding: binding, Entries: []Entry{{Token: "root", Name: "Root", Memberships: []string{SharedHidden}}}}
	next, err := AddVerifiedManual(snapshot, Entry{Token: "child", Name: "Child", Placement: "shared", CanonicalRootToken: "root", RelativePath: []string{"Child"}})
	if err != nil {
		t.Fatal(err)
	}
	var child Entry
	for _, e := range next.Entries {
		if e.Token == "child" {
			child = e
		}
	}
	if !child.Manual || len(child.Memberships) != 1 || child.Memberships[0] != Manual || child.Placement != "shared" || child.CanonicalRootToken != "root" {
		t.Fatalf("%+v", child)
	}
	if _, err = AddVerifiedManual(snapshot, Entry{Token: "child", Name: "Child", Placement: "shared", CanonicalRootToken: "missing", RelativePath: []string{"Child"}}); err == nil {
		t.Fatal("accepted missing canonical ancestor")
	}
}
func TestUnverifiedManualCannotClaimPlacement(t *testing.T) {
	snapshot := Snapshot{Binding: Binding{ProfileID: "profile", IdentityHash: "hash", UserID: "123", Origin: "https://team.feishu.cn"}}
	next, err := AddManual(snapshot, Entry{Token: "folder", Name: "Folder", Placement: "personal", CanonicalRootToken: "folder", Memberships: []string{Personal}})
	if err != nil {
		t.Fatal(err)
	}
	if next.Entries[0].Placement != "" || next.Entries[0].CanonicalRootToken != "" || has(next.Entries[0].Memberships, Personal) {
		t.Fatal("unverified placement trusted")
	}
}
