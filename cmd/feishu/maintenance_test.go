package feishu

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMaintenanceChild(t *testing.T) {
	if os.Getenv("DB_TEST_INSTALL_CHILD") != "1" {
		return
	}
	if err := verifyInstallationProof(context.Background(), os.Getenv(installationProofEnv)); err != nil {
		os.Exit(31)
	}
	root := os.Getenv("DB_TEST_INSTALL_ROOT")
	if err := os.WriteFile(filepath.Join(root, "started"), []byte(strings.Join(os.Args, "\n")), 0600); err != nil {
		os.Exit(32)
	}
	for {
		if _, err := os.Stat(filepath.Join(root, "release")); err == nil {
			break
		}
		time.Sleep(time.Millisecond)
	}
	os.Exit(0)
}
func TestMaintenanceWaitsForChildAfterCancellationAndPassesLiteralArgv(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DB_TEST_INSTALL_CHILD", "1")
	t.Setenv("DB_TEST_INSTALL_ROOT", root)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	released := make(chan struct{})
	done := make(chan error, 1)
	lease := func(ctx context.Context, fn func() error) error { defer close(released); return fn() }
	literal := "$(not-a-shell); spaced argument"
	go func() {
		done <- executeInstallation(ctx, lease, []string{exe, "-test.run=^TestMaintenanceChild$", "--", literal}, nil, io.Discard, io.Discard)
	}()
	deadline := time.After(5 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(root, "started")); err == nil {
			break
		}
		select {
		case err := <-done:
			t.Fatal(err)
		case <-deadline:
			t.Fatal("child did not start")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	select {
	case <-released:
		t.Fatal("lease released while child alive")
	case <-time.After(30 * time.Millisecond):
	}
	raw, _ := os.ReadFile(filepath.Join(root, "started"))
	if !strings.Contains(string(raw), literal) {
		t.Fatal(string(raw))
	}
	if err := os.WriteFile(filepath.Join(root, "release"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	<-released
}
func TestMaintenanceProofExpiresAndCannotBeForged(t *testing.T) {
	proof, closeProof, err := startInstallationProof()
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyInstallationProof(context.Background(), proof); err != nil {
		t.Fatal(err)
	}
	if err := verifyInstallationProof(context.Background(), proof+"x"); err == nil {
		t.Fatal("accepted forged proof")
	}
	closeProof()
	if err := verifyInstallationProof(context.Background(), proof); err == nil {
		t.Fatal("accepted expired proof")
	}
}
func TestMaintenanceAcquisitionAndChildFailures(t *testing.T) {
	sentinel := errors.New("busy")
	lease := func(context.Context, func() error) error { return sentinel }
	if err := executeInstallation(context.Background(), lease, []string{"unused"}, nil, io.Discard, io.Discard); !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	lease = func(_ context.Context, fn func() error) error { return fn() }
	if err := executeInstallation(context.Background(), lease, []string{filepath.Join(t.TempDir(), "missing")}, nil, io.Discard, io.Discard); err == nil {
		t.Fatal("missing child accepted")
	}
}
func TestMaintenanceRequiresArgvSeparator(t *testing.T) {
	root := NewCommand(CommandOptions{})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	root.SetArgs([]string{"maintenance", "exec", "echo", "unsafe"})
	if err := root.Execute(); err == nil {
		t.Fatal("missing -- accepted")
	}
}

func TestMaintenanceProofRejectsWrongIdentityAndEndpoint(t *testing.T) {
	encoded, closeProof, err := startInstallationProof()
	if err != nil {
		t.Fatal(err)
	}
	defer closeProof()
	raw, _ := base64.RawURLEncoding.DecodeString(encoded)
	var p installationProof
	if err := json.Unmarshal(raw, &p); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*installationProof){
		func(p *installationProof) { p.User = "different-user" },
		func(p *installationProof) { p.Token = strings.Repeat("0", 64) },
		func(p *installationProof) { p.Address = "http://example.invalid:80" },
		func(p *installationProof) { p.Address += "/other" },
	} {
		copy := p
		change(&copy)
		raw, _ := json.Marshal(copy)
		if err := verifyInstallationProof(context.Background(), base64.RawURLEncoding.EncodeToString(raw)); err == nil {
			t.Fatal("invalid proof accepted", copy.Address)
		}
	}
}
