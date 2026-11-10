package platform

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

func StateDir() (string, error) {
	root, err := windows.KnownFolderPath(windows.FOLDERID_LocalAppData, 0)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "DriveBridge", "State"), nil
}

// SecurePrivate protects the DACL from inheritance and grants only this user full access.
func SecurePrivate(path string, directory bool) error {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return err
	}
	inherit := ""
	if directory {
		inherit = "OICI"
	}
	sd, err := windows.SecurityDescriptorFromString(fmt.Sprintf("D:P(A;%s;FA;;;%s)", inherit, user.User.Sid.String()))
	if err != nil {
		return err
	}
	acl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil)
}
func ReplaceFile(from, to string) error {
	a, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	b, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	return windows.MoveFileEx(a, b, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH)
}

// Windows replacement requests write-through; directory fsync is not portable here.
func SyncDir(string) error { return nil }

// ValidMountpoint accepts a drive letter or absolute directory without checking occupancy.
func ValidMountpoint(path string) bool {
	if strings.ContainsRune(path, 0) {
		return false
	}
	if (len(path) == 2 || len(path) == 3 && (path[2] == '\\' || path[2] == '/')) && len(path) >= 2 && path[1] == ':' && ((path[0] >= 'A' && path[0] <= 'Z') || (path[0] >= 'a' && path[0] <= 'z')) {
		return true
	}
	return filepath.IsAbs(path) && filepath.Clean(path) != filepath.VolumeName(path)+string(filepath.Separator)
}

// All reparse points are excluded from private storage; ModeSymlink alone misses
// junctions, which Go exposes as ModeIrregular. Inspect the Windows attribute.
func privatePathAlias(path string, _ os.FileInfo) (bool, error) {
	name, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return false, err
	}
	attrs, err := windows.GetFileAttributes(name)
	if err != nil {
		return false, &os.PathError{Op: "GetFileAttributes", Path: path, Err: err}
	}
	return attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT != 0, nil
}
