package markdown

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
)

func conversionRulesFixturePath(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(root, "settings", "config.json")
}

func TestEnsureConversionRulesFileInitializesPrivateDefaultsAndPreservesExisting(t *testing.T) {
	configFile := conversionRulesFixturePath(t)
	if err := EnsureConversionRulesFile(configFile); err != nil {
		t.Fatal(err)
	}
	path := ConversionRulesPath(configFile)
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, DefaultConversionRulesJSON()) {
		t.Fatalf("defaults %q, %v", data, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		t.Fatalf("file mode %v", info.Mode())
	}
	if _, err := LoadConversionRules(path); err != nil {
		t.Fatal(err)
	}
	existing := []byte("existing user edits, even malformed")
	if err := os.WriteFile(path, existing, 0600); err != nil {
		t.Fatal(err)
	}
	if err := EnsureConversionRulesFile(configFile); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(path)
	if err != nil || !bytes.Equal(data, existing) {
		t.Fatalf("existing file replaced %q, %v", data, err)
	}
}

func TestEnsureConversionRulesFileConcurrentPublicationIsComplete(t *testing.T) {
	configFile := conversionRulesFixturePath(t)
	path := ConversionRulesPath(configFile)
	var wg sync.WaitGroup
	done := make(chan struct{})
	failures := make(chan error, 24)
	for range 24 {
		wg.Add(1)
		go func() { defer wg.Done(); failures <- EnsureConversionRulesFile(configFile) }()
	}
	go func() { wg.Wait(); close(done) }()
	observing := true
	for observing {
		data, err := os.ReadFile(path)
		if err == nil && !bytes.Equal(data, DefaultConversionRulesJSON()) {
			t.Fatalf("partially published defaults: %q", data)
		}
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		select {
		case <-done:
			observing = false
		default:
			runtime.Gosched()
		}
	}
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, DefaultConversionRulesJSON()) {
		t.Fatalf("defaults %q, %v", data, err)
	}
	leftovers, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".conversion-rules-*"))
	if err != nil || len(leftovers) != 0 {
		t.Fatalf("temporary files %v, %v", leftovers, err)
	}
}

func TestEnsureConversionRulesFileRejectsSymlinkAndDirectory(t *testing.T) {
	t.Run("leaf", func(t *testing.T) {
		configFile := conversionRulesFixturePath(t)
		if err := os.MkdirAll(filepath.Dir(configFile), 0700); err != nil {
			t.Fatal(err)
		}
		target := filepath.Join(filepath.Dir(configFile), "elsewhere")
		if err := os.WriteFile(target, []byte("untouched"), 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, ConversionRulesPath(configFile)); err != nil {
			if runtime.GOOS == "windows" {
				t.Skip(err)
			}
			t.Fatal(err)
		}
		if err := EnsureConversionRulesFile(configFile); err == nil {
			t.Fatal("symlink accepted")
		}
		data, err := os.ReadFile(target)
		if err != nil || string(data) != "untouched" {
			t.Fatalf("symlink target changed: %q %v", data, err)
		}
	})
	t.Run("ancestor", func(t *testing.T) {
		configFile := conversionRulesFixturePath(t)
		target := filepath.Join(filepath.Dir(filepath.Dir(configFile)), "other")
		if err := os.Mkdir(target, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Dir(configFile)); err != nil {
			if runtime.GOOS == "windows" {
				t.Skip(err)
			}
			t.Fatal(err)
		}
		if err := EnsureConversionRulesFile(configFile); err == nil {
			t.Fatal("ancestor symlink accepted")
		}
		if _, err := os.Stat(filepath.Join(target, "conversion-rules.json")); !os.IsNotExist(err) {
			t.Fatalf("wrote through alias: %v", err)
		}
	})
	t.Run("directory", func(t *testing.T) {
		configFile := conversionRulesFixturePath(t)
		if err := os.MkdirAll(ConversionRulesPath(configFile), 0700); err != nil {
			t.Fatal(err)
		}
		if err := EnsureConversionRulesFile(configFile); err == nil {
			t.Fatal("directory accepted as file")
		}
	})
	if err := EnsureConversionRulesFile("relative/config.json"); err == nil {
		t.Fatal("relative config path accepted")
	}
}
