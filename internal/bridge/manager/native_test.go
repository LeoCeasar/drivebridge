package manager

import (
	"context"
	"fmt"
	"github.com/google/uuid"
	"github.com/rclone/rclone/backend/memory"
	"github.com/rclone/rclone/cmd/mountlib"
	fsconfig "github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/vfs"
	"github.com/rclone/rclone/vfs/vfscommon"
	"path/filepath"
	"reflect"
	"testing"
)

func TestNativeRuntimeCloseThenLateUnmountDoesNotReleaseVFSTwice(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	old := fsconfig.GetCacheDir()
	if err = fsconfig.SetCacheDir(root); err != nil {
		t.Fatal(err)
	}
	defer fsconfig.SetCacheDir(old)
	f, err := memory.NewFs(context.Background(), "managed-"+uuid.NewString(), "", configmap.Simple{})
	if err != nil {
		t.Fatal(err)
	}
	opt := vfscommon.Opt
	opt.PollInterval = 0
	v, err := vfs.NewManaged(context.Background(), f, &opt)
	if err != nil {
		t.Fatal(err)
	}
	n := &nativeRuntime{v: v, point: &mountlib.MountPoint{UnmountFn: func() error { v.Shutdown(); return nil }}}
	if err = n.Close(); err != nil {
		t.Fatal(err)
	}
	_ = n.Unmount()
	if got := fmt.Sprint(v.Stats()["inUse"]); got != "0" {
		t.Fatalf("VFS lifetime released twice: %s", got)
	}
}

func TestNativeMacOSBackendOptions(t *testing.T) {
	for _, tc := range []struct {
		goos, engine, backend string
		valid, fskit          bool
	}{
		{"darwin", "cmount", "", true, false},
		{"darwin", "cmount", "kernel", true, false},
		{"darwin", "cmount", "fskit", true, true},
		{"darwin", "cmount", "unknown", false, false},
		{"darwin", "mount2", "fskit", false, false},
		{"linux", "mount2", "", true, false},
		{"linux", "mount2", "fskit", false, false},
		{"linux", "mount2", "kernel", false, false},
		{"windows", "cmount", "", true, false},
		{"windows", "cmount", "fskit", false, false},
		{"windows", "cmount", "kernel", false, false},
	} {
		t.Run(tc.goos+"/"+tc.engine+"/"+tc.backend, func(t *testing.T) {
			// Leave spare capacity to detect accidental writes into the global default
			// option slice when deriving one mount's backend override.
			shared := []string{"original", "sentinel"}
			base := mountlib.Options{ExtraOptions: shared[:1]}
			options, err := nativeMountOptions(tc.backend, tc.goos, tc.engine, base)
			if !tc.valid {
				if err == nil {
					t.Fatal("unsupported backend/platform accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			want := []string{"original"}
			if tc.fskit {
				want = append(want, "backend=fskit")
			}
			if !reflect.DeepEqual(options.ExtraOptions, want) {
				t.Fatalf("options=%v want=%v", options.ExtraOptions, want)
			}
			if shared[1] != "sentinel" {
				t.Fatal("per-mount option overwrote shared defaults")
			}
		})
	}
}
