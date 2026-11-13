package manager

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/rclone/rclone/internal/bridge/markdown"
)

func settingsDocument(target string) json.RawMessage {
	b, _ := json.Marshal(map[string]any{"version": 1, "rules": []map[string]string{{"id": "custom", "source": "OLD", "target": target, "context": "paragraph", "match": "literal"}}})
	return b
}

func TestConversionRulesSettingsReadsEffectiveDefaultsAndKeepsRuntimeSnapshot(t *testing.T) {
	m, _, _, _ := fixture(t, nil)
	view, err := m.ConversionRules(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if view.Path != markdown.ConversionRulesPath(m.ConfigFile()) || view.RestartRequired {
		t.Fatalf("initial view: %+v", view)
	}
	var doc struct {
		Version int                       `json:"version"`
		Rules   []markdown.ConversionRule `json:"rules"`
	}
	if err := json.Unmarshal(view.Document, &doc); err != nil || doc.Version != 1 || len(doc.Rules) != 4 || doc.Rules[0].Target != "\n" {
		t.Fatalf("effective defaults: %s, %v", view.Document, err)
	}
	disk, err := os.ReadFile(view.Path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(disk)
	if view.Revision != hex.EncodeToString(sum[:]) {
		t.Fatalf("revision does not identify disk: %q", view.Revision)
	}
	before := markdown.ActiveConversionRulesFingerprint()
	saved, err := m.SaveConversionRules(context.Background(), view.Revision, settingsDocument("NEW"))
	if err != nil {
		t.Fatal(err)
	}
	if !saved.RestartRequired || saved.Revision == view.Revision {
		t.Fatalf("saved view: %+v", saved)
	}
	if markdown.ActiveConversionRulesFingerprint() != before {
		t.Fatal("save reconfigured the running snapshot")
	}
	if err := json.Unmarshal(saved.Document, &doc); err != nil || len(doc.Rules) != 5 || doc.Rules[4].Target != "NEW" || doc.Rules[0].Target != "\n" {
		t.Fatalf("effective custom rules: %s, %v", saved.Document, err)
	}
	fresh, err := m.ConversionRules(context.Background())
	if err != nil || fresh.Revision != saved.Revision || !fresh.RestartRequired {
		t.Fatalf("fresh view: %+v, %v", fresh, err)
	}
	restored, err := m.SaveConversionRules(context.Background(), saved.Revision, markdown.DefaultConversionRulesJSON())
	if err != nil || restored.RestartRequired {
		t.Fatalf("restored defaults: %+v, %v", restored, err)
	}
	if runtime.GOOS != "windows" {
		for _, path := range []string{view.Path, view.Path + ".lock", filepath.Dir(view.Path)} {
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			want := os.FileMode(0600)
			if info.IsDir() {
				want = 0700
			}
			if info.Mode().Perm() != want {
				t.Fatalf("%s permissions %v", path, info.Mode())
			}
		}
	}
}

func TestConversionRulesSettingsRejectsInvalidAndStaleWithoutReplacingFile(t *testing.T) {
	m, _, _, _ := fixture(t, nil)
	view, err := m.ConversionRules(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(view.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []json.RawMessage{
		[]byte(`{"version":1,"rules":[{"id":"x","source":"x","context":"paragraph","match":"literal"}]}`),
		[]byte(`{"version":1,"rules":[],"unknown":true}`),
		[]byte(`{"version":1,"rules":[{"id":"simple-break-paragraph","source":"BAD","target":"","context":"paragraph","match":"literal"}]}`),
		settingsDocument("OLD"),
		[]byte(strings.Repeat(" ", 8*1024*1024+1)),
	} {
		if _, err := m.SaveConversionRules(context.Background(), view.Revision, input); !errors.Is(err, ErrConversionRulesInvalid) {
			t.Fatalf("invalid input accepted or unclassified: %v", err)
		}
		after, err := os.ReadFile(view.Path)
		if err != nil || !bytes.Equal(after, original) {
			t.Fatalf("invalid save changed file: %v", err)
		}
	}
	external := append(bytes.Clone(original), '\n')
	if err := os.WriteFile(view.Path, external, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := m.SaveConversionRules(context.Background(), view.Revision, settingsDocument("NEW")); !errors.Is(err, ErrConversionRulesChanged) {
		t.Fatalf("stale save: %v", err)
	}
	after, err := os.ReadFile(view.Path)
	if err != nil || !bytes.Equal(after, external) {
		t.Fatalf("stale save overwrote external edit: %v", err)
	}
}

func TestConversionRulesSettingsConcurrentTabsHaveOneWinner(t *testing.T) {
	m, _, _, _ := fixture(t, nil)
	initial, err := m.ConversionRules(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, target := range []string{"ONE", "TWO"} {
		wg.Add(1)
		go func(target string) {
			defer wg.Done()
			<-start
			_, err := m.SaveConversionRules(context.Background(), initial.Revision, settingsDocument(target))
			results <- err
		}(target)
	}
	close(start)
	wg.Wait()
	close(results)
	success, changed := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, ErrConversionRulesChanged) {
			changed++
		} else {
			t.Fatal(err)
		}
	}
	if success != 1 || changed != 1 {
		t.Fatalf("winners=%d, conflicts=%d", success, changed)
	}
}

func TestConversionRulesSettingsDoesNotInitializeConfigAndHonorsCancellation(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	m := &Manager{options: Options{ConfigFile: filepath.Join(root, "absent", "config.json")}}
	view, err := m.ConversionRules(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Document) == 0 {
		t.Fatal("missing defaults before initialization")
	}
	if _, err := os.Stat(filepath.Dir(m.ConfigFile())); !os.IsNotExist(err) {
		t.Fatalf("read initialized configuration: %v", err)
	}
	if _, err := m.SaveConversionRules(context.Background(), view.Revision, settingsDocument("NEW")); !errors.Is(err, ErrConfig) {
		t.Fatalf("save before config initialization: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(m.ConfigFile())); !os.IsNotExist(err) {
		t.Fatalf("save initialized configuration: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.ConversionRules(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled read: %v", err)
	}
	if _, err := m.SaveConversionRules(ctx, view.Revision, settingsDocument("NEW")); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled save: %v", err)
	}
}

func TestConversionRulesSettingsRejectsSymlink(t *testing.T) {
	m, _, _, _ := fixture(t, nil)
	view, err := m.ConversionRules(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(filepath.Dir(view.Path), "elsewhere.json")
	if err := os.Rename(view.Path, target); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, view.Path); err != nil {
		if runtime.GOOS == "windows" {
			t.Skip(err)
		}
		t.Fatal(err)
	}
	if _, err := m.ConversionRules(context.Background()); err == nil {
		t.Fatal("read followed symlink")
	}
	if _, err := m.SaveConversionRules(context.Background(), view.Revision, settingsDocument("NEW")); err == nil {
		t.Fatal("save followed symlink")
	}
	after, err := os.ReadFile(target)
	if err != nil || !bytes.Equal(after, markdown.DefaultConversionRulesJSON()) {
		t.Fatalf("symlink target changed: %v", err)
	}
}
