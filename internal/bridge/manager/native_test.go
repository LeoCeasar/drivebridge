package manager

import (
	"context"
	"fmt"
	"github.com/google/uuid"
	"github.com/rclone/rclone/backend/memory"
	"github.com/rclone/rclone/cmd/mountlib"
	fsconfig "github.com/rclone/rclone/fs/config"
	"github.com/rclone/rclone/fs/config/configmap"
	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/vfs"
	"github.com/rclone/rclone/vfs/vfscommon"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"
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

func TestNativeFinderVolumeOptions(t *testing.T) {
	spec := mountSpec{Mount: model.Mount{Name: "团队资料", Mountpoint: "/Users/example/DriveBridge/account"}, Profile: model.Profile{Name: "工作飞书"}}
	for _, tc := range []struct {
		goos, engine string
		local        bool
	}{{"darwin", "cmount", true}, {"linux", "mount2", false}, {"windows", "cmount", false}, {"darwin", "mount2", false}} {
		t.Run(tc.goos+"/"+tc.engine, func(t *testing.T) {
			shared := []string{"original", "sentinel"}
			base := mountlib.Options{ExtraOptions: shared[:1]}
			options := nativeFinderOptions(spec, tc.goos, tc.engine, base)
			if tc.local {
				if options.VolumeName != "DriveBridge — 团队资料" || !reflect.DeepEqual(options.ExtraOptions, []string{"original", "local"}) {
					t.Fatalf("Finder volume options: %#v", options)
				}
			} else if !reflect.DeepEqual(options, base) {
				t.Fatalf("unrelated backend changed: %#v", options)
			}
			if shared[1] != "sentinel" || base.VolumeName != "" {
				t.Fatal("shared defaults mutated")
			}
		})
	}
	account := spec
	account.Remote.AccountRoot = true
	account.Mount.Name = "账号盘-private-id"
	options := nativeFinderOptions(account, "darwin", "cmount", mountlib.Options{ExtraOptions: []string{"local"}})
	if options.VolumeName != "DriveBridge — 工作飞书" || !reflect.DeepEqual(options.ExtraOptions, []string{"local"}) {
		t.Fatalf("account label or duplicate local: %#v", options)
	}
	grouped := nativeFinderOptions(account, "darwin", "cmount", mountlib.Options{VolumeName: "Custom label", ExtraOptions: []string{"nodev,local"}})
	if grouped.VolumeName != "Custom label" || !reflect.DeepEqual(grouped.ExtraOptions, []string{"nodev,local"}) {
		t.Fatal("explicit label or existing grouped local flag changed", grouped)
	}
	account.Mount.MacOSBackend = "fskit"
	fskit := nativeFinderOptions(account, "darwin", "cmount", mountlib.Options{ExtraOptions: []string{"backend=fskit"}, NoAppleXattr: true})
	if fskit.VolumeName != "DriveBridge — 工作飞书" || !reflect.DeepEqual(fskit.ExtraOptions, []string{"backend=fskit"}) || !fskit.NoAppleXattr {
		t.Fatal("Finder label switched FSKit backend or added unsupported kernel flags", fskit)
	}
	account.Mount.MacOSBackend = ""
	account.Profile.Name = "项目,/dev:\\测试\n,allow_other"
	options = nativeFinderOptions(account, "darwin", "cmount", mountlib.Options{})
	if strings.ContainsAny(options.VolumeName, ",:/\\\n") {
		t.Fatalf("volume name can alter mount options: %q", options.VolumeName)
	}
	account.Profile.Name = strings.Repeat("长名称", 100)
	options = nativeFinderOptions(account, "darwin", "cmount", mountlib.Options{})
	if len(options.VolumeName) > 255 || !utf8.ValidString(options.VolumeName) {
		t.Fatal("invalid bounded Unicode volume name")
	}
	account.Profile.Name = ""
	if got := nativeFinderOptions(account, "darwin", "cmount", mountlib.Options{}).VolumeName; got != "DriveBridge" {
		t.Fatal(got)
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
			if options.NoAppleXattr != tc.fskit {
				t.Fatalf("FSKit Finder compatibility requires NoAppleXattr=%v, got %v", tc.fskit, options.NoAppleXattr)
			}
			if shared[1] != "sentinel" {
				t.Fatal("per-mount option overwrote shared defaults")
			}
		})
	}
}

func TestNativeRuntimeCloseJoinsFilesystemShutdown(t *testing.T) {
	for _, adapterShutdown := range []bool{false, true} {
		t.Run(fmt.Sprint(adapterShutdown), func(t *testing.T) {
			f, err := memory.NewFs(context.Background(), "shutdown-"+uuid.NewString(), "", configmap.Simple{})
			if err != nil {
				t.Fatal(err)
			}
			calls := 0
			f.Features().Shutdown = func(ctx context.Context) error {
				if err := ctx.Err(); err != nil {
					t.Fatal("filesystem shutdown inherited canceled mount context", err)
				}
				calls++
				return nil
			}
			options := vfscommon.Opt
			options.CacheMode = vfscommon.CacheModeOff
			options.PollInterval = 0
			v, err := vfs.NewManaged(context.Background(), f, &options)
			if err != nil {
				t.Fatal(err)
			}
			n := &nativeRuntime{v: v, adapterShutdown: adapterShutdown}
			if adapterShutdown {
				v.Shutdown()
			}
			if err := n.Close(); err != nil {
				t.Fatal(err)
			}
			if err := n.Close(); err != nil {
				t.Fatal(err)
			}
			if calls != 1 {
				t.Fatal("native runtime did not join filesystem worker exactly once", calls)
			}
		})
	}
}
