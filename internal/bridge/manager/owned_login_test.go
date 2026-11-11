package manager

import (
	"context"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rclone/rclone/internal/bridge/discovery"
	"github.com/rclone/rclone/internal/bridge/ownedlogin"
)

type disconnectedDirectorySession struct{}

func (disconnectedDirectorySession) Get(context.Context, string, url.Values) ([]byte, error) {
	return nil, discovery.ErrBrowserDisconnected
}
func (disconnectedDirectorySession) Login(context.Context) error {
	return discovery.ErrBrowserDisconnected
}
func (disconnectedDirectorySession) SealLogin(context.Context) error { return nil }
func (disconnectedDirectorySession) Close() error                    { return nil }

func TestDisconnectedBrowserKeepsDirectoryCatalog(t *testing.T) {
	b := discovery.Binding{ProfileID: "01234567-89ab-4cde-8fab-0123456789ab", IdentityHash: strings.Repeat("a", 64), UserID: "12345", Origin: "https://team.feishu.cn"}
	previous := discovery.Snapshot{Binding: b, Complete: true, Entries: []discovery.Entry{{Token: "kept", Name: "Saved folder", Manual: true, Memberships: []string{discovery.Manual}}}}
	settings := discoverySettings{Binding: b, Enabled: true, State: "complete"}
	next := scanDiscoveryState(context.Background(), &settings, disconnectedDirectorySession{}, previous)
	if settings.State != "browser_disconnected" || settings.Error != "browser_disconnected" || next.Complete || len(next.Entries) != 1 || !next.Entries[0].Manual {
		t.Fatalf("disconnected browser lost state: %s %s %+v", settings.State, settings.Error, next)
	}
}

func TestOwnedLoginHelperMissingIsActionable(t *testing.T) {
	m, _, _, cancel := fixture(t, nil)
	defer cancel()
	defer m.Close()
	c, err := m.LoadConfig(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.discoverySession(context.Background(), c.Profiles[0].ID, "https://team.feishu.cn")
	if err == nil {
		t.Fatal("started missing native helper")
	}
	view := m.browserBridgeView(c.Profiles[0].ID)
	if view.ExtensionID != "" || view.ExtensionPath != "" || view.Connected || view.HelperAvailable {
		t.Fatalf("unexpected owned login status: %+v", view)
	}
}

func TestMissingWebViewRuntimeHasRecoverableState(t *testing.T) {
	err := ownedlogin.ErrRuntimeUnavailable
	if got := discoveryStateError(err); got != "needs_runtime" {
		t.Fatal(got)
	}
	if got := automaticError(err); got != "needs_runtime" {
		t.Fatal(got)
	}
	if got := automaticDiscoveryCode(DiscoveryView{State: "needs_runtime"}); got != "needs_runtime" {
		t.Fatal(got)
	}
}
func TestOwnedLoginPlatformPaths(t *testing.T) {
	root := t.TempDir()
	for _, tc := range []struct{ goos, relative string }{
		{"windows", "libexec/drivebridge-session-host.exe"},
		{"darwin", "libexec/DriveBridge Session.app/Contents/MacOS/drivebridge-session-host"},
	} {
		got := ownedLoginPathForOS(tc.goos, filepath.Join(root, "bin", "drivebridge"))
		if got != filepath.Join(root, filepath.FromSlash(tc.relative)) {
			t.Fatalf("%s: %s", tc.goos, got)
		}
	}
}
