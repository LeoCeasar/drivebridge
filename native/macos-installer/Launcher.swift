import AppKit
import Foundation

@MainActor final class LauncherDelegate: NSObject, NSApplicationDelegate {
    func applicationDidFinishLaunching(_ notification: Notification) {
        let app = Bundle.main.bundleURL
        Task {
            do {
                _ = try await Task.detached { try runCommand(desktopURL(app), ["desktop", "start"]) }.value
            } catch {
                NSApp.activate(ignoringOtherApps: true)
                let alert = NSAlert()
                alert.messageText = "DriveBridge 未能启动"
                alert.informativeText = error.localizedDescription
                alert.addButton(withTitle: "好")
                alert.runModal()
            }
            NSApp.terminate(nil)
        }
    }
}

@main struct Launcher {
    @MainActor static func main() {
        let app = NSApplication.shared
        app.setActivationPolicy(.accessory)
        let delegate = LauncherDelegate()
        app.delegate = delegate
        app.run()
        withExtendedLifetime(delegate) {}
    }
}
