package feishu

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestMaintenanceChild(t *testing.T) {
	if os.Getenv("DB_TEST_INSTALL_CHILD") != "1" {
		return
	}
	if os.Getenv(installationProofEnv) != "" {
		os.Exit(30)
	}
	identity := maintenanceTestIdentityAt(os.Getenv("DB_TEST_INSTALL_ROOT"))
	if err := verifyInstallationProofAs(context.Background(), "", identity); err != nil {
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
	root := maintenanceTestHome(t)
	t.Setenv("DB_TEST_INSTALL_CHILD", "1")
	t.Setenv("DB_TEST_INSTALL_ROOT", root)
	t.Setenv(installationProofEnv, "inherited-secret-must-be-stripped")
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
		done <- executeInstallationAs(ctx, lease, []string{exe, "-test.run=^TestMaintenanceChild$", "--", literal}, nil, io.Discard, io.Discard, maintenanceTestIdentityAt(root))
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
	if err := verifyInstallationProofAs(context.Background(), "", maintenanceTestIdentityAt(root)); err != nil {
		t.Fatalf("proof ended before child after cancellation: %v", err)
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
	identity := maintenanceTestIdentityAt(maintenanceTestHome(t))
	proof, closeProof, err := startInstallationProofAs(identity)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyInstallationProofAs(context.Background(), proof, identity); err != nil {
		t.Fatal(err)
	}
	if err := verifyInstallationProofAs(context.Background(), proof+"x", identity); err == nil {
		t.Fatal("accepted forged proof")
	}
	closeProof()
	if err := verifyInstallationProofAs(context.Background(), proof, identity); err == nil {
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
	if err := executeInstallationAs(context.Background(), lease, []string{filepath.Join(t.TempDir(), "missing")}, nil, io.Discard, io.Discard, maintenanceTestIdentityAt(maintenanceTestHome(t))); err == nil {
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
	identity := maintenanceTestIdentityAt(maintenanceTestHome(t))
	encoded, closeProof, err := startInstallationProofAs(identity)
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
		if err := verifyInstallationProofAs(context.Background(), base64.RawURLEncoding.EncodeToString(raw), identity); err == nil {
			t.Fatal("invalid proof accepted", copy.Address)
		}
	}
}

func TestMaintenanceProofSurvivesSanitizedEnvironment(t *testing.T) {
	identity := maintenanceTestIdentityAt(maintenanceTestHome(t))
	_, closeProof, err := startInstallationProofAs(identity)
	if err != nil {
		t.Fatal(err)
	}
	defer closeProof()
	if err := verifyInstallationProofAs(context.Background(), "", identity); err != nil {
		t.Fatalf("sanitized installer cannot discover held lease: %v", err)
	}
}

func maintenanceTestHome(t *testing.T) string {
	t.Helper()
	// macOS temporary directories can contain /var -> /private/var aliases.
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return home
}

func maintenanceTestIdentityAt(home string) installationIdentity {
	return func() (*user.User, error) {
		current, err := canonicalInstallationUser()
		if err != nil {
			return nil, err
		}
		current.HomeDir = home
		return current, nil
	}
}

func TestMaintenanceDescriptorStaleFailsClosed(t *testing.T) {
	identity := maintenanceTestIdentityAt(maintenanceTestHome(t))
	encoded, closeProof, err := startInstallationProofAs(identity)
	if err != nil {
		t.Fatal(err)
	}
	current, _ := identity()
	path := installationDescriptorPath(current)
	closeProof()
	if err := os.WriteFile(path, []byte(encoded), 0600); err != nil {
		t.Fatal(err)
	}
	if err := verifyInstallationProofAs(context.Background(), "", identity); err == nil {
		t.Fatal("stale descriptor accepted")
	}
}

func TestMaintenanceDescriptorCleanupPreservesReplacement(t *testing.T) {
	identity := maintenanceTestIdentityAt(maintenanceTestHome(t))
	_, closeOld, err := startInstallationProofAs(identity)
	if err != nil {
		t.Fatal(err)
	}
	defer closeOld()
	_, closeNew, err := startInstallationProofAs(identity)
	if err != nil {
		t.Fatal(err)
	}
	defer closeNew()
	closeOld()
	if err := verifyInstallationProofAs(context.Background(), "", identity); err != nil {
		t.Fatalf("old cleanup removed new proof: %v", err)
	}
	closeNew()
	current, _ := identity()
	if _, err := os.Lstat(installationDescriptorPath(current)); !os.IsNotExist(err) {
		t.Fatalf("own descriptor not removed: %v", err)
	}
}

func TestMaintenanceDescriptorRejectsSymlinkAndCleanupDoesNotFollowIt(t *testing.T) {
	identity := maintenanceTestIdentityAt(maintenanceTestHome(t))
	encoded, closeProof, err := startInstallationProofAs(identity)
	if err != nil {
		t.Fatal(err)
	}
	defer closeProof()
	current, _ := identity()
	path := installationDescriptorPath(current)
	target := filepath.Join(current.HomeDir, "target")
	if err := os.WriteFile(target, []byte(encoded), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		if runtime.GOOS == "windows" {
			t.Skip(err)
		}
		t.Fatal(err)
	}
	if err := verifyInstallationProofAs(context.Background(), "", identity); err == nil {
		t.Fatal("symlink descriptor accepted")
	}
	if _, cleanup, err := startInstallationProofAs(identity); err == nil {
		cleanup()
		t.Fatal("published through symlink")
	}
	closeProof()
	if info, err := os.Lstat(path); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("cleanup changed replacement symlink: %v", err)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatalf("cleanup removed symlink target: %v", err)
	}
}

func TestMaintenanceDescriptorRejectsDifferentOSIdentity(t *testing.T) {
	identity := maintenanceTestIdentityAt(maintenanceTestHome(t))
	_, closeProof, err := startInstallationProofAs(identity)
	if err != nil {
		t.Fatal(err)
	}
	defer closeProof()
	wrong := func() (*user.User, error) {
		u, err := identity()
		if err == nil {
			u.Uid = "different-uid-or-sid"
		}
		return u, err
	}
	if err := verifyInstallationProofAs(context.Background(), "", wrong); err == nil {
		t.Fatal("descriptor accepted different UID/SID")
	}
}

func TestMaintenanceDescriptorRejectsSymlinkDirectory(t *testing.T) {
	home := maintenanceTestHome(t)
	target := filepath.Join(home, "target")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(home, ".drivebridge-install")); err != nil {
		if runtime.GOOS == "windows" {
			t.Skip(err)
		}
		t.Fatal(err)
	}
	if _, cleanup, err := startInstallationProofAs(maintenanceTestIdentityAt(home)); err == nil {
		cleanup()
		t.Fatal("published through directory symlink")
	}
}

func TestMaintenanceDescriptorPrivatePermissionsAndChangedContentCleanup(t *testing.T) {
	identity := maintenanceTestIdentityAt(maintenanceTestHome(t))
	_, closeProof, err := startInstallationProofAs(identity)
	if err != nil {
		t.Fatal(err)
	}
	defer closeProof()
	current, _ := identity()
	path := installationDescriptorPath(current)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && (info.Mode().Perm() != 0600 || dir.Mode().Perm() != 0700) {
		t.Fatalf("non-private descriptor permissions: %v, directory: %v", info.Mode(), dir.Mode())
	}
	// Truncate the existing inode rather than replacing it: inode identity alone
	// cannot establish that this instance still owns the descriptor contents.
	if err := os.WriteFile(path, []byte("changed"), 0600); err != nil {
		t.Fatal(err)
	}
	closeProof()
	raw, err := os.ReadFile(path)
	if err != nil || string(raw) != "changed" {
		t.Fatalf("cleanup removed changed descriptor: %q, %v", raw, err)
	}
	if err := verifyInstallationProofAs(context.Background(), "", identity); err == nil {
		t.Fatal("changed descriptor accepted")
	}
}
