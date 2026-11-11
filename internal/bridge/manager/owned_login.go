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
	return filepath.Clean(filepath.Join(filepath.Dir(m.options.Executable), "..", "libexec", "DriveBridge Session.app", "Contents", "MacOS", "drivebridge-session-host"))
}
func (m *Manager) browserBridgeView(id model.ProfileID) *BrowserBridgeView {
	view := &BrowserBridgeView{}
	helper := m.ownedLoginPath()
	if runtime.GOOS == "darwin" && platform.CheckNoSymlinks(helper) == nil {
		info, err := os.Stat(helper)
		view.HelperAvailable = err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0
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
