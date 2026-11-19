import AppKit
import Foundation

@MainActor final class LauncherDelegate: NSObject, NSApplicationDelegate {
    private let services = LinkServiceProvider()
    func applicationWillFinishLaunching(_ notification: Notification) {
        if Bundle.main.bundleIdentifier == "org.drivebridge.linkservice" {
            NSApp.servicesProvider = services
            NSRegisterServicesProvider(services, "org.drivebridge.linkservice")
            NSLog("DriveBridge link service port ready")
        }
    }
    func applicationDidFinishLaunching(_ notification: Notification) {
        // Services launches must not open the management page or restart sync.
        if Bundle.main.bundleIdentifier == "org.drivebridge.linkservice" { return }
        if CommandLine.arguments.contains("-NSService") { NSApp.terminate(nil); return }
        do { try refreshInstalledServices(Bundle.main.bundleURL) }
        catch { NSLog("DriveBridge: %@", error.localizedDescription) }
        startDesktop()
    }
    func applicationShouldHandleReopen(_ sender: NSApplication, hasVisibleWindows flag: Bool) -> Bool {
        if Bundle.main.bundleIdentifier != "org.drivebridge.linkservice" { startDesktop() }
        return false
    }
    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool { false }
    private func startDesktop() {
        let app = Bundle.main.bundleURL
        Task {
            do {
                _ = try await Task.detached { try runCommand(launcherDesktopURL(app), ["desktop", "start"]) }.value
            } catch {
                NSApp.activate(ignoringOtherApps: true)
                let alert = NSAlert()
                alert.messageText = "DriveBridge 未能启动"
                alert.informativeText = error.localizedDescription
                alert.addButton(withTitle: "好")
                alert.runModal()
            }
            // Only the payload service needs to stay alive. The wrapper exits
            // after starting Go so future opens cannot return to an old wrapper.
            if Bundle.main.object(forInfoDictionaryKey: "NSServices") == nil { NSApp.terminate(nil) }
        }
    }
}

@main struct Launcher {
    @MainActor static func main() {
        if CommandLine.arguments.dropFirst().first == "--register-pkg-services" {
            do {
                guard CommandLine.arguments.count == 2, getuid() != 0, getuid() == geteuid() else { throw InstallError.unsafeTarget }
                let target = try pkgServicesApplication()
                try retireServiceLaunchers(installedServiceLaunchers(target))
                try refreshInstalledServices(target)
                print("DriveBridge package Services registration updated")
            } catch {
                FileHandle.standardError.write(Data((error.localizedDescription + "\n").utf8))
                exit(1)
            }
            return
        }
        let app = NSApplication.shared
        app.setActivationPolicy(.accessory)
        let delegate = LauncherDelegate()
        app.delegate = delegate
        app.run()
        withExtendedLifetime(delegate) {}
    }
}
