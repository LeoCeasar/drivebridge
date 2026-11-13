package history

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

func TestMergeTextUsesRealGitAndNeverReturnsConflictMarkers(t *testing.T) {
	base := []byte("first\nmiddle\nlast\n")
	local := []byte("local first\nmiddle\nlast\n")
	remote := []byte("first\nmiddle\ncloud last\n")
	got, clean, err := MergeText(context.Background(), base, local, remote)
	if err != nil || !clean || string(got) != "local first\nmiddle\ncloud last\n" {
		t.Fatalf("clean merge %q %v %v", got, clean, err)
	}
	got, clean, err = MergeText(context.Background(), base, local, []byte("remote first\nmiddle\nlast\n"))
	if err != nil || clean || got != nil {
		t.Fatalf("conflict leaked output %q %v %v", got, clean, err)
	}
}
func TestMergeTextBoundsInputsAndCancellation(t *testing.T) {
	_, _, err := MergeText(context.Background(), nil, bytes.Repeat([]byte("x"), (10<<20)+1), nil)
	if !errors.Is(err, ErrLimit) {
		t.Fatalf("limit %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err = MergeText(ctx, []byte("a"), []byte("b"), []byte("a"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel %v", err)
	}
	_, _, err = MergeText(context.Background(), []byte("a"), []byte{0}, []byte("a"))
	if !errors.Is(err, ErrIntegrity) {
		t.Fatalf("binary %v", err)
	}
}
func TestMergeTextIgnoresHostGitConfiguration(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "1")
	t.Setenv("GIT_CONFIG_KEY_0", "merge.conflictStyle")
	t.Setenv("GIT_CONFIG_VALUE_0", "invalid-value")
	got, clean, err := MergeText(context.Background(), []byte("one\ntwo\nthree\n"), []byte("local\ntwo\nthree\n"), []byte("one\ntwo\nremote\n"))
	if err != nil || !clean || string(got) != "local\ntwo\nremote\n" {
		t.Fatalf("host config affected merge %q %v %v", got, clean, err)
	}
}
