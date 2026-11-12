package platform

import (
	"errors"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
)

var ErrInvalidFolder = errors.New("invalid folder path")

var windowsDrivePath = regexp.MustCompile(`^[A-Za-z]:([\\/].*)?$`)

// FolderOpenCommand returns the file-manager command for goos. The path is the
// sole argument and never passes through a shell. Only absolute local paths
// are accepted, so an argument can never be parsed as an option or URL.
func FolderOpenCommand(goos, path string) (string, []string, error) {
	if path == "" || strings.ContainsAny(path, "\x00\r\n") {
		return "", nil, ErrInvalidFolder
	}
	switch goos {
	case "windows":
		if !windowsDrivePath.MatchString(path) && !strings.HasPrefix(path, `\`) {
			return "", nil, ErrInvalidFolder
		}
		if len(path) == 2 {
			path += `\`
		}
		return "explorer.exe", []string{path}, nil
	case "darwin":
		if !strings.HasPrefix(path, "/") {
			return "", nil, ErrInvalidFolder
		}
		return "open", []string{path}, nil
	default:
		if !strings.HasPrefix(path, "/") {
			return "", nil, ErrInvalidFolder
		}
		return "xdg-open", []string{path}, nil
	}
}

// OpenFolder shows an existing directory in the platform file manager
// (Explorer, Finder or the desktop's default). File managers frequently exit
// non-zero after a successful launch, so only a failure to start is an error.
func OpenFolder(path string) error {
	name, args, err := FolderOpenCommand(runtime.GOOS, path)
	if err != nil {
		return err
	}
	if !DesktopAvailable() {
		return ErrNoDesktop
	}
	info, err := os.Stat(args[0])
	if err != nil || !info.IsDir() {
		return ErrInvalidFolder
	}
	cmd := exec.Command(name, args...)
	if err = cmd.Start(); err != nil {
		return err
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
