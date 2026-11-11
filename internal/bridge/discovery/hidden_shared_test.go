package discovery

import (
	"context"
	"net/url"
	"reflect"
	"testing"
)

func TestHiddenSharedOnlyKeepsExplicitAndVisibleMemberships(t *testing.T) {
	for _, tc := range []struct {
		entry Entry
		want  bool
	}{
		{Entry{Memberships: []string{SharedHidden}}, true},
		{Entry{Memberships: []string{SharedHidden, Personal}}, false},
		{Entry{Memberships: []string{SharedHidden, SharedVisible}}, false},
		{Entry{Memberships: []string{SharedHidden, Manual}}, false},
		{Entry{Memberships: []string{SharedHidden}, Manual: true}, false},
		{Entry{Memberships: []string{SharedHidden}, Placement: "personal"}, false},
		{Entry{Memberships: []string{SharedVisible}}, false},
		{Entry{}, false},
	} {
		if got := HiddenSharedOnly(tc.entry); got != tc.want {
			t.Fatalf("%+v got=%v want=%v", tc.entry, got, tc.want)
		}
	}
}
func TestScanWithoutHiddenSharedSkipsNetworkButRetainsCatalog(t *testing.T) {
	previous := Snapshot{Binding: binding, Complete: true, Entries: []Entry{{Token: "hidden-old", Name: "Hidden old", Memberships: []string{SharedHidden}}}, Sources: map[string]SourceState{SharedHidden: {Status: "complete", Count: 7, Pages: 2}}}
	calls := map[string]int{}
	f := fetchFunc(func(ctx context.Context, p string, q url.Values) ([]byte, error) {
		name := source(p, q)
		calls[name]++
		if name == SharedHidden {
			t.Fatal("disabled hidden source requested")
		}
		return emptyFetcher(ctx, p, q)
	})
	got, err := ScanWithOptions(context.Background(), binding, f, previous, ScanOptions{ExcludeHiddenShared: true})
	if err != nil || !got.Complete || got.Sources[SharedHidden].Status != "disabled" || got.Sources[SharedHidden].Count != 7 || calls[Personal] != 1 || calls[SharedVisible] != 1 {
		t.Fatalf("%+v %v calls=%v", got, err, calls)
	}
	found := false
	for _, e := range got.Entries {
		if e.Token == "hidden-old" {
			found = true
		}
	}
	if !found || !reflect.DeepEqual(previous.Entries[0].Memberships, []string{SharedHidden}) || previous.Sources[SharedHidden].Status != "complete" {
		t.Fatal("old catalog changed", previous)
	}
}
func TestDefaultScanStillRequestsHiddenShared(t *testing.T) {
	calls := 0
	f := fetchFunc(func(ctx context.Context, p string, q url.Values) ([]byte, error) {
		if source(p, q) == SharedHidden {
			calls++
		}
		return emptyFetcher(ctx, p, q)
	})
	got, err := Scan(context.Background(), binding, f, Snapshot{})
	if err != nil || !got.Complete || calls != 1 || got.Sources[SharedHidden].Status != "complete" {
		t.Fatal(got, err, calls)
	}
}
