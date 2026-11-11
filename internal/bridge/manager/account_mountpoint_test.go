package manager

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestAccountMountpointPlatformPolicy(t *testing.T) {
	for _, goos := range []string{"windows", "darwin", "linux"} {
		for _, private := range []bool{false, true} {
			name := goos + "/runtime"
			if private {
				name = goos + "/account"
			}
			t.Run(name, func(t *testing.T) {
				root, err := filepath.EvalSymlinks(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
				target := filepath.Join(root, "DriveBridge", "account-id")
				for attempt := 0; attempt < 2; attempt++ {
					if err := prepareMountpointForOS(target, goos, private); err != nil {
						t.Fatal(err)
					}
					parent, err := os.Stat(filepath.Dir(target))
					if err != nil || !parent.IsDir() {
						t.Fatalf("parent not available: %v %v", parent, err)
					}
					info, err := os.Lstat(target)
					if goos == "windows" {
						if !os.IsNotExist(err) {
							t.Fatalf("WinFsp target must remain absent: %v %v", info, err)
						}
					} else if err != nil || !info.IsDir() {
						t.Fatalf("Unix target must be an empty directory: %v %v", info, err)
					}
				}
				if goos == "windows" {
					if err := os.Mkdir(target, 0700); err != nil {
						t.Fatal(err)
					}
					if err := prepareMountpointForOS(target, goos, private); !errors.Is(err, ErrPending) {
						t.Fatalf("existing empty WinFsp target accepted: %v", err)
					}
				}
				marker := filepath.Join(target, "existing-user-data")
				if err := os.WriteFile(marker, []byte("preserve"), 0600); err != nil {
					t.Fatal(err)
				}
				if err := prepareMountpointForOS(target, goos, private); !errors.Is(err, ErrPending) {
					t.Fatalf("occupied target accepted: %v", err)
				}
				if data, err := os.ReadFile(marker); err != nil || string(data) != "preserve" {
					t.Fatalf("existing data changed: %q %v", data, err)
				}
			})
		}
	}
}

func TestAccountAndWorkerMountpointPreparationUseSamePlatform(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "DriveBridge", "account-id")
	if err := prepareAccountMountpoint(target); err != nil {
		t.Fatal(err)
	}
	if err := validateMountpoint(target); err != nil {
		t.Fatal(err)
	}
	_, err = os.Lstat(target)
	if runtime.GOOS == "windows" {
		if !os.IsNotExist(err) {
			t.Fatalf("runtime created WinFsp endpoint: %v", err)
		}
	} else if err != nil {
		t.Fatal(err)
	}
}

func TestAccountMountpointRejectsAliasesAndFiles(t *testing.T) {
	for _, goos := range []string{"windows", "darwin", "linux"} {
		t.Run(goos, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(root, "file")
			if err := os.WriteFile(file, []byte("preserve"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := prepareMountpointForOS(file, goos, true); !errors.Is(err, ErrConfig) {
				t.Fatalf("file target accepted: %v", err)
			}
			if err := prepareMountpointForOS("relative-target", goos, true); !errors.Is(err, ErrConfig) {
				t.Fatalf("relative target accepted: %v", err)
			}
			alias := filepath.Join(root, "alias")
			if err := os.Symlink(root, alias); err != nil {
				if runtime.GOOS == "windows" {
					t.Skipf("symlink creation requires Windows privileges: %v", err)
				}
				t.Fatal(err)
			}
			if err := prepareMountpointForOS(filepath.Join(alias, "missing-target"), goos, true); err == nil {
				t.Fatal("symlink ancestor accepted")
			}
			if _, err := os.Lstat(filepath.Join(root, "missing-target")); !os.IsNotExist(err) {
				t.Fatalf("created through alias: %v", err)
			}
		})
	}
}
