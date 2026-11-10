package config

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/platform"
	"golang.org/x/sys/windows"
)

// createJunction creates an actual mount-point reparse tag without a shell or
// symlink privilege. These regressions must execute on a native Windows host.
func createJunction(t *testing.T, link, target string) {
	t.Helper()
	if err := os.Mkdir(link, 0700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
			t.Error(err)
		}
	})
	link16, err := windows.UTF16PtrFromString(link)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := windows.CreateFile(link16, windows.GENERIC_WRITE, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil, windows.OPEN_EXISTING, windows.FILE_FLAG_OPEN_REPARSE_POINT|windows.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle)
	substitute, err := windows.UTF16FromString(`\??\` + target)
	if err != nil {
		t.Fatal(err)
	}
	printName, err := windows.UTF16FromString(target)
	if err != nil {
		t.Fatal(err)
	}
	// REPARSE_DATA_BUFFER header followed by the mount-point offsets/lengths and
	// its UTF-16 substitute/print path buffer, with lengths excluding each NUL.
	dataLength := 8 + 2*(len(substitute)+len(printName))
	buffer := make([]byte, 8+dataLength)
	binary.LittleEndian.PutUint32(buffer[0:4], windows.IO_REPARSE_TAG_MOUNT_POINT)
	binary.LittleEndian.PutUint16(buffer[4:6], uint16(dataLength))
	binary.LittleEndian.PutUint16(buffer[10:12], uint16(2*(len(substitute)-1)))
	binary.LittleEndian.PutUint16(buffer[12:14], uint16(2*len(substitute)))
	binary.LittleEndian.PutUint16(buffer[14:16], uint16(2*(len(printName)-1)))
	offset := 16
	for _, name := range [][]uint16{substitute, printName} {
		for _, unit := range name {
			binary.LittleEndian.PutUint16(buffer[offset:offset+2], unit)
			offset += 2
		}
	}
	var returned uint32
	if err := windows.DeviceIoControl(handle, windows.FSCTL_SET_REPARSE_POINT, &buffer[0], uint32(len(buffer)), nil, 0, &returned, nil); err != nil {
		t.Fatal(err)
	}
	linked, err := os.Stat(link)
	if err != nil {
		t.Fatal(err)
	}
	destination, err := os.Stat(target)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(linked, destination) {
		t.Fatal("junction fixture does not resolve to its real target")
	}
}

func TestWindowsPrivateJunctionAncestorRejected(t *testing.T) {
	for _, kind := range []string{"state", "cache"} {
		t.Run(kind, func(t *testing.T) {
			p := testPaths(t)
			c := testConfig(p)
			target := c.Mounts[0].Mountpoint
			if err := os.Mkdir(target, 0700); err != nil {
				t.Fatal(err)
			}
			alias := filepath.Join(filepath.Dir(p.StateDir), "alias")
			createJunction(t, alias, target)
			privateChild := filepath.Join(alias, kind)
			if kind == "state" {
				p.StateDir = privateChild
			} else {
				p.CacheDir = privateChild
			}
			if err := platform.CheckNoSymlinks(privateChild); err == nil {
				t.Fatal("junction ancestor accepted by private-path preflight")
			}
			if err := platform.EnsurePrivateDir(privateChild); err == nil {
				t.Fatal("private directory created through junction")
			}
			if err := Validate(c, p); err == nil {
				t.Fatal("private storage alias hides mount recursion")
			}
			if err := SaveWithPaths(p, c); err == nil {
				t.Fatal("save accepted private junction ancestor")
			}
			if _, err := os.Stat(filepath.Join(target, kind)); !os.IsNotExist(err) {
				t.Fatalf("private data created inside real mount: %v", err)
			}
		})
	}
}

func TestWindowsAuthJunctionEscapeRejected(t *testing.T) {
	for _, component := range []string{"auth", filepath.Join("auth", "identities")} {
		t.Run(component, func(t *testing.T) {
			p := testPaths(t)
			ref := "auth/identities/" + identityID + "/lark"
			r := model.IdentityRegistry{SchemaVersion: 1, Identities: []model.IdentityRecord{{ID: identityID, VerifiedIdentity: model.VerifiedIdentity{Brand: "feishu", AppID: "app", UserOpenID: "user", IdentityHash: "hash"}, AuthRef: ref}}}
			profile := Profile{ID: profileID, Brand: "feishu", AuthRef: ref, IdentityHash: "hash"}
			alias := filepath.Join(p.StateDir, component)
			if err := os.MkdirAll(filepath.Dir(alias), 0700); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(filepath.Dir(p.StateDir), "outside-auth")
			remainder, err := filepath.Rel(component, filepath.FromSlash(ref))
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(target, remainder), 0700); err != nil {
				t.Fatal(err)
			}
			createJunction(t, alias, target)
			if got, err := ResolveAuthRoot(p, r, profile); err == nil {
				t.Fatalf("auth junction escaped private state: %q", got)
			}
		})
	}
}

func TestWindowsMountReparsePathRemainsStructural(t *testing.T) {
	p := testPaths(t)
	c := testConfig(p)
	target := filepath.Join(filepath.Dir(p.StateDir), "mounted-target")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(filepath.Dir(p.StateDir), "mounted")
	createJunction(t, alias, target)
	c.Mounts[0].Mountpoint = alias
	if err := Validate(c, p); err != nil {
		t.Fatalf("mount path treated as private storage: %v", err)
	}
	if err := SaveWithPaths(p, c); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadWithPaths(p); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsDriveMountpointsRemainStructural(t *testing.T) {
	p := testPaths(t)
	c := testConfig(p)
	drive := "Z:"
	if filepath.VolumeName(p.StateDir) == drive {
		drive = "Y:"
	}
	for _, mountpoint := range []string{drive, drive + `\`} {
		c.Mounts[0].Mountpoint = mountpoint
		if err := Validate(c, p); err != nil {
			t.Fatalf("drive %q should need syntax only: %v", mountpoint, err)
		}
	}
}
