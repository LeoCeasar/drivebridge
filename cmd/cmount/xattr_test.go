//go:build cmount && ((linux && cgo) || (darwin && cgo) || (freebsd && cgo) || (openbsd && cgo) || windows)

package cmount

import (
	"runtime"
	"testing"

	"github.com/rclone/rclone/cmd/mountlib"
	"github.com/winfsp/cgofuse/fuse"
)

func TestFSKitAppleXattrCompatibility(t *testing.T) {
	for _, tc := range []struct {
		name    string
		options *mountlib.Options
		ignore  bool
	}{
		{"missing-options", nil, false},
		{"explicit-fskit", &mountlib.Options{NoAppleXattr: true, ExtraOptions: []string{"backend=fskit"}}, true},
		{"trimmed-fskit", &mountlib.Options{NoAppleXattr: true, ExtraOptions: []string{" backend=fskit "}}, true},
		{"flag-required", &mountlib.Options{ExtraOptions: []string{"backend=fskit"}}, false},
		{"kernel-unchanged", &mountlib.Options{NoAppleXattr: true}, false},
		{"other-backend", &mountlib.Options{NoAppleXattr: true, ExtraOptions: []string{"backend=vfs"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			adapter := &FS{opt: tc.options}
			ignore := tc.ignore && runtime.GOOS == "darwin"
			wantSet, wantGet, wantList := -fuse.ENOSYS, -fuse.ENOSYS, -fuse.ENOSYS
			if ignore {
				wantSet, wantGet, wantList = 0, -fuse.ENOATTR, 0
			}
			if got := adapter.Setxattr("file", "com.apple.FinderInfo", []byte("metadata"), 0); got != wantSet {
				t.Fatalf("Setxattr=%d want=%d", got, wantSet)
			}
			if got, value := adapter.Getxattr("file", "com.apple.FinderInfo"); got != wantGet || value != nil {
				t.Fatalf("Getxattr=(%d,%v) want=(%d,nil)", got, value, wantGet)
			}
			if got := adapter.Removexattr("file", "com.apple.FinderInfo"); got != wantSet {
				t.Fatalf("Removexattr=%d want=%d", got, wantSet)
			}
			if got := adapter.Listxattr("file", func(string) bool { t.Fatal("discarded attributes must not be enumerated"); return false }); got != wantList {
				t.Fatalf("Listxattr=%d want=%d", got, wantList)
			}
			// Other namespaces retain the unsupported result, even when Finder's
			// Apple metadata is explicitly disabled.
			if got := adapter.Setxattr("file", "user.keep", []byte("value"), 0); got != -fuse.ENOSYS {
				t.Fatalf("non-Apple Setxattr result changed: %d", got)
			}
			if got, _ := adapter.Getxattr("file", "user.keep"); got != -fuse.ENOSYS {
				t.Fatalf("non-Apple Getxattr result changed: %d", got)
			}
			if got := adapter.Removexattr("file", "user.keep"); got != -fuse.ENOSYS {
				t.Fatalf("non-Apple Removexattr result changed: %d", got)
			}
		})
	}
}
