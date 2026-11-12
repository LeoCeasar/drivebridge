package history

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestBundledGitRejectsWrongTargetAndChangedExecutable(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	executable := filepath.Join(root, "drivebridge")
	if err := os.WriteFile(executable, []byte("application"), 0700); err != nil {
		t.Fatal(err)
	}
	suffix := ""
	if runtime.GOOS == "windows" {
		suffix = ".exe"
	}
	gitRoot := filepath.Join(root, "libexec", "git")
	if err := os.MkdirAll(filepath.Join(gitRoot, "bin"), 0700); err != nil {
		t.Fatal(err)
	}
	git := filepath.Join(gitRoot, "bin", "git"+suffix)
	body := []byte("controlled runtime")
	if err := os.WriteFile(git, body, 0700); err != nil {
		t.Fatal(err)
	}
	hash := sha256.Sum256(body)
	metadata := map[string]any{"schema_version": 1, "platform": runtime.GOOS, "architecture": runtime.GOARCH,
		"files": map[string]string{"bin/git" + suffix: hex.EncodeToString(hash[:])}}
	writeMetadata := func() {
		t.Helper()
		b, err := json.Marshal(metadata)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(gitRoot, "runtime.json"), b, 0600); err != nil {
			t.Fatal(err)
		}
	}
	writeMetadata()
	if got, packaged, err := bundledGit(executable); err != nil || !packaged || got != git {
		t.Fatalf("valid runtime: %q %v %v", got, packaged, err)
	}
	metadata["architecture"] = "different-target"
	writeMetadata()
	if got, packaged, err := bundledGit(executable); got != "" || !packaged || !errors.Is(err, ErrGitUnavailable) {
		t.Fatalf("wrong target accepted: %q %v %v", got, packaged, err)
	}
	metadata["architecture"] = runtime.GOARCH
	writeMetadata()
	if err := os.WriteFile(git, []byte("changed"), 0700); err != nil {
		t.Fatal(err)
	}
	if got, packaged, err := bundledGit(executable); got != "" || !packaged || !errors.Is(err, ErrGitUnavailable) {
		t.Fatalf("tampering accepted: %q %v %v", got, packaged, err)
	}
}
