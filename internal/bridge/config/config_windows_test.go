package config

import (
	"os"
	"reflect"
	"testing"

	"golang.org/x/sys/windows"
)

func TestWindowsLoadWhileDeleteHandleOpen(t *testing.T) {
	p := testPaths(t)
	want := testConfig(p)
	if err := SaveWithPaths(p, want); err != nil {
		t.Fatal(err)
	}
	name, err := windows.UTF16PtrFromString(p.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	// A rename operation opens a DELETE handle. Plain os.ReadFile cannot open
	// the same file until that handle closes because it omits FILE_SHARE_DELETE.
	handle, err := windows.CreateFile(name, windows.GENERIC_READ|windows.DELETE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	reader := os.NewFile(uintptr(handle), p.ConfigFile)
	defer reader.Close()
	got, err := LoadWithPaths(p)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("reader did not see the complete existing configuration: %+v", got)
	}
}

func TestWindowsDeniedReplacementPreservesConfig(t *testing.T) {
	p := testPaths(t)
	want := testConfig(p)
	if err := SaveWithPaths(p, want); err != nil {
		t.Fatal(err)
	}
	// A reader that does not share DELETE may refuse replacement. Save must
	// return that error without removing or changing the existing configuration.
	reader, err := os.Open(p.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	replacement := testConfig(p)
	replacement.Profiles[0].Name = "replacement"
	if err := SaveWithPaths(p, replacement); err == nil {
		t.Fatal("replacement unexpectedly succeeded without delete sharing")
	}
	got, err := LoadWithPaths(p)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("failed replacement changed the original configuration: %+v", got)
	}
}
