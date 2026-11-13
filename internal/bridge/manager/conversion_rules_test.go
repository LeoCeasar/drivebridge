package manager

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManagerRejectsInvalidUserConversionRulesAtStartup(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	rules := filepath.Join(root, "conversion-rules.json")
	if err = os.WriteFile(rules, []byte(`{"version":1,"rules":[{"source":"<br>"}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	m, err := New(Options{ConfigFile: filepath.Join(root, "config.json")})
	if err == nil {
		if m != nil {
			_ = m.Close()
		}
		t.Fatal("invalid conversion rules silently ignored")
	}
	if !strings.Contains(err.Error(), rules) {
		t.Fatalf("missing actionable rules path: %v", err)
	}
}
