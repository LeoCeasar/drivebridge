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
