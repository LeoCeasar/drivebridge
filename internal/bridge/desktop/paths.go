package desktop

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Executables resolves the paired CLI/desktop layout without relying on the
// login session's PATH (Finder and login tasks often have a minimal environment).
func Executables(executable string) (worker, app string) {
	if actual, err := filepath.EvalSymlinks(executable); err == nil {
		executable = actual
	}
	executable, _ = filepath.Abs(executable)
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	base := filepath.Dir(executable)
	worker, app = executable, executable
	regular := func(path string) bool { s, e := os.Stat(path); return e == nil && s.Mode().IsRegular() }
	if strings.TrimSuffix(strings.ToLower(filepath.Base(executable)), ".exe") == "drivebridge-desktop" {
		candidate := filepath.Join(base, "drivebridge"+ext)
		if filepath.Base(base) == "MacOS" && filepath.Base(filepath.Dir(base)) == "Contents" {
			candidate = filepath.Clean(filepath.Join(base, "..", "..", "..", "bin", "drivebridge"))
		}
		if regular(candidate) {
			worker = candidate
		}
	} else {
		for _, candidate := range []string{filepath.Join(base, "drivebridge-desktop"+ext), filepath.Clean(filepath.Join(base, "..", "DriveBridge.app", "Contents", "MacOS", "drivebridge-desktop"))} {
			if regular(candidate) {
				app = candidate
				break
			}
		}
	}
	return worker, app
}
