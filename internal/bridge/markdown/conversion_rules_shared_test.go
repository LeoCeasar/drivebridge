package markdown

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/rclone/rclone/lib/file"
)

func TestConversionRulesSavePreservesOpenReaderAndRevision(t *testing.T) {
	config := conversionRulesFixturePath(t)
	if err := EnsureConversionRulesFile(config); err != nil {
		t.Fatal(err)
	}
	path := ConversionRulesPath(config)
	before, err := ReadConversionRulesFile(path)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := file.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	after := []byte(`{"version":1,"rules":[{"id":"synthetic","source":"BEFORE","target":"AFTER","context":"paragraph","match":"literal"}]}`)
	if _, err = SaveConversionRulesFile(context.Background(), path, ConversionRulesRevision(before), after); err != nil {
		t.Fatal(err)
	}
	old, err := io.ReadAll(reader)
	if err != nil || !bytes.Equal(old, before) {
		t.Fatal("existing reader lost its complete snapshot", err)
	}
	current, err := ReadConversionRulesFile(path)
	if err != nil || !bytes.Equal(current, after) {
		t.Fatal("new reader did not see the complete replacement", err)
	}
	if _, err = SaveConversionRulesFile(context.Background(), path, ConversionRulesRevision(before), before); !errors.Is(err, ErrConversionRulesChanged) {
		t.Fatal("stale revision accepted", err)
	}
	current, err = ReadConversionRulesFile(path)
	if err != nil || !bytes.Equal(current, after) {
		t.Fatal("stale writer changed rules", err)
	}
}
