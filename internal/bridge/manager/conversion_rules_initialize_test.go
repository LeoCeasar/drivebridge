package manager

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"

	"github.com/rclone/rclone/internal/bridge/markdown"
)

func TestInitializeConfigCreatesConversionRulesWithoutReplacingUserEdits(t *testing.T) {
	m, _, _, _ := fixture(t, nil)
	path := markdown.ConversionRulesPath(m.ConfigFile())
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("created before initialization: %v", err)
	}
	if err := m.InitializeConfig(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(data, markdown.DefaultConversionRulesJSON()) {
		t.Fatalf("defaults %q, %v", data, err)
	}
	edited := append(markdown.DefaultConversionRulesJSON(), []byte("\n\n")...)
	if err := os.WriteFile(path, edited, 0600); err != nil {
		t.Fatal(err)
	}
	if err := m.InitializeConfig(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(path)
	if err != nil || !bytes.Equal(data, edited) {
		t.Fatalf("user edits replaced %q, %v", data, err)
	}
}

func TestInitializeConfigCanceledDoesNotCreateConversionRules(t *testing.T) {
	m, _, _, _ := fixture(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.InitializeConfig(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("initialize: %v", err)
	}
	if _, err := os.Stat(markdown.ConversionRulesPath(m.ConfigFile())); !os.IsNotExist(err) {
		t.Fatalf("created after canceled config initialization: %v", err)
	}
}
