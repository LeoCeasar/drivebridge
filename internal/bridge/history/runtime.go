package history

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/rclone/rclone/internal/bridge/platform"
)

func bundledGit(executable string) (string, bool, error) {
	executable, err := filepath.EvalSymlinks(executable)
	if err != nil {
		return "", false, err
	}
	dir := filepath.Dir(executable)
	packaged := false
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	for i := 0; i < 6; i++ {
		if _, err := os.Stat(filepath.Join(dir, "manifest.json")); err == nil {
			packaged = true
		}
		root := filepath.Join(dir, "libexec", "git")
		candidate := filepath.Join(root, "bin", "git"+suffix)
		if _, err := os.Stat(candidate); err == nil {
			if err := platform.CheckNoSymlinks(candidate); err != nil {
				return "", true, err
			}
			meta := filepath.Join(root, "runtime.json")
			if err := platform.CheckNoSymlinks(meta); err != nil {
				return "", true, err
			}
			b, err := os.ReadFile(meta)
			if err != nil || len(b) > 4<<20 {
				return "", true, ErrGitUnavailable
			}
			var m struct {
				Schema       int               `json:"schema_version"`
				Platform     string            `json:"platform"`
				Architecture string            `json:"architecture"`
				Files        map[string]string `json:"files"`
			}
			if json.Unmarshal(b, &m) != nil || m.Schema != 1 || m.Platform != runtime.GOOS || m.Architecture != runtime.GOARCH {
				return "", true, ErrGitUnavailable
			}
			expected := m.Files["bin/git"+suffix]
			if !validHash(expected) {
				return "", true, ErrGitUnavailable
			}
			f, err := os.Open(candidate)
			if err != nil {
				return "", true, err
			}
			h := sha256.New()
			_, err = io.Copy(h, f)
			f.Close()
			if err != nil || hex.EncodeToString(h.Sum(nil)) != expected {
				return "", true, ErrGitUnavailable
			}
			return candidate, true, nil
		} else if !os.IsNotExist(err) {
			return "", true, err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", packaged, nil
}

var gitVersionPattern = regexp.MustCompile(`git version ([0-9]+)\.([0-9]+)\.`)

func resolveGit(ctx context.Context, override string) (string, error) {
	path := override
	if path == "" {
		exe, err := os.Executable()
		if err != nil {
			return "", err
		}
		var packaged bool
		path, packaged, err = bundledGit(exe)
		if err != nil {
			return "", errors.Join(ErrGitUnavailable, err)
		}
		if path == "" && packaged {
			return "", ErrGitUnavailable
		}
		if path == "" {
			path, err = exec.LookPath("git")
			if err != nil {
				return "", ErrGitUnavailable
			}
		}
	}
	resolved, err := exec.LookPath(path)
	if err != nil {
		return "", errors.Join(ErrGitUnavailable, err)
	}
	path = resolved
	path, err = filepath.Abs(path)
	if err != nil {
		return "", err
	}
	check, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	raw, err := platform.BackgroundCommand(check, path, "--version").Output()
	if err != nil {
		return "", errors.Join(ErrGitUnavailable, err)
	}
	fields := gitVersionPattern.FindStringSubmatch(string(raw))
	if len(fields) != 3 {
		return "", ErrGitUnavailable
	}
	major, _ := strconv.Atoi(fields[1])
	minor, _ := strconv.Atoi(fields[2])
	if major < 2 || major == 2 && minor < 42 {
		return "", ErrGitUnavailable
	}
	return strings.TrimSpace(path), nil
}
