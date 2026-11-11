package manager

import (
	"context"
	"os"
	"path/filepath"
	"runtime"

	"github.com/rclone/rclone/internal/bridge/model"
	"github.com/rclone/rclone/internal/bridge/ownedlogin"
	"github.com/rclone/rclone/internal/bridge/platform"
)

func (m *Manager) ownedLoginPath() string {
	return ownedLoginPathForOS(runtime.GOOS, m.options.Executable)
}
func ownedLoginPathForOS(goos, executable string) string {
	root := filepath.Join(filepath.Dir(executable), "..", "libexec")
	if goos == "windows" {
		return filepath.Clean(filepath.Join(root, "drivebridge-session-host.exe"))
	}
	return filepath.Clean(filepath.Join(root, "DriveBridge Session.app", "Contents", "MacOS", "drivebridge-session-host"))
}
func (m *Manager) browserBridgeView(id model.ProfileID) *BrowserBridgeView {
	view := &BrowserBridgeView{}
	helper := m.ownedLoginPath()
	if (runtime.GOOS == "darwin" || runtime.GOOS == "windows") && platform.CheckNoSymlinks(helper) == nil {
		info, err := os.Stat(helper)
		view.HelperAvailable = err == nil && info.Mode().IsRegular() && (runtime.GOOS == "windows" || info.Mode().Perm()&0111 != 0)
	}
	m.discoveryMu.Lock()
	defer m.discoveryMu.Unlock()
	if session := m.discoverySessions[id]; session != nil {
		if native, ok := session.(interface{ Connected() bool }); ok && native.Connected() {
			view.Connected = true
		}
	}
	return view
}
func (m *Manager) ownedDiscoverySession(ctx context.Context, id model.ProfileID, origin string) (discoverySession, error) {
	return ownedlogin.Start(ctx, ownedlogin.Options{HelperPath: m.ownedLoginPath(), ProfileID: string(id), Origin: origin})
}
