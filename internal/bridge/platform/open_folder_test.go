package platform

import (
	"errors"
	"reflect"
	"testing"
)

func TestFolderOpenCommandUsesPlatformOpenerWithoutShell(t *testing.T) {
	for _, tc := range []struct {
		goos, path, name string
		args             []string
	}{
		{"windows", `C:\Users\me\DriveBridge\acct`, "explorer.exe", []string{`C:\Users\me\DriveBridge\acct`}},
		{"windows", "X:", "explorer.exe", []string{`X:\`}},
		{"darwin", "/Users/me/DriveBridge", "open", []string{"/Users/me/DriveBridge"}},
		{"linux", "/home/me/DriveBridge", "xdg-open", []string{"/home/me/DriveBridge"}},
		{"freebsd", "/mnt/drive", "xdg-open", []string{"/mnt/drive"}},
	} {
		name, args, err := FolderOpenCommand(tc.goos, tc.path)
		if err != nil || name != tc.name || !reflect.DeepEqual(args, tc.args) {
			t.Fatalf("%s %q: %q %q %v", tc.goos, tc.path, name, args, err)
		}
	}
}

func TestFolderOpenCommandRejectsOptionsURLsAndRelativePaths(t *testing.T) {
	for _, tc := range [][2]string{
		{"windows", ""}, {"windows", `relative\path`}, {"windows", "/e,C:\\"}, {"windows", "https://example.com"}, {"windows", "C:\\a\nb"},
		{"darwin", "-a"}, {"darwin", "relative"}, {"darwin", "/a\x00b"},
		{"linux", "--help"}, {"linux", "file:///etc"}, {"linux", "~/x"},
	} {
		if _, _, err := FolderOpenCommand(tc[0], tc[1]); !errors.Is(err, ErrInvalidFolder) {
			t.Fatalf("%q was accepted for %s", tc[1], tc[0])
		}
	}
}
