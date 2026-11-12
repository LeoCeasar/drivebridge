import AppKit
import Foundation

@MainActor final class InstallerDelegate: NSObject, NSApplicationDelegate, NSWindowDelegate {
    private var window: NSWindow!
    private let location = NSPopUpButton()
    private let login = NSButton(checkboxWithTitle: "登录 Mac 时自动启动 DriveBridge", target: nil, action: nil)
    private let launch = NSButton(checkboxWithTitle: "安装完成后打开 DriveBridge", target: nil, action: nil)
    private let status = NSTextField(wrappingLabelWithString: "账号、待上传文件和冲突副本都会保留。")
    private let button = NSButton(title: "安装", target: nil, action: nil)
    private let progress = NSProgressIndicator()
    private var busy = false
    private var installed: URL?

    func applicationDidFinishLaunching(_ notification: Notification) {
        window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 560, height: 350), styleMask: [.titled, .closable, .miniaturizable], backing: .buffered, defer: false)
        window.title = "安装 DriveBridge"
        window.delegate = self
        window.isReleasedWhenClosed = false
        let title = NSTextField(labelWithString: "安装 DriveBridge")
        title.font = .boldSystemFont(ofSize: 23)
        let subtitle = NSTextField(wrappingLabelWithString: "安装后可从应用程序目录打开，运行状态显示在菜单栏。")
        location.addItems(withTitles: ["应用程序（/Applications）", "当前用户的应用程序（~/Applications）"])
        if !FileManager.default.isWritableFile(atPath: "/Applications") { location.selectItem(at: 1) }
        login.state = .on
        launch.state = .on
        button.target = self
        button.action = #selector(install)
        button.bezelStyle = .rounded
        button.keyEquivalent = "\r"
        progress.style = .spinning
        progress.controlSize = .small
        progress.isDisplayedWhenStopped = false
        let stack = NSStackView(views: [title, subtitle, location, login, launch, status])
        stack.orientation = .vertical
        stack.alignment = .leading
        stack.spacing = 17
        let footer = NSStackView(views: [progress, button])
        footer.spacing = 14
        for view in [stack, footer] {
            view.translatesAutoresizingMaskIntoConstraints = false
            window.contentView!.addSubview(view)
        }
        NSLayoutConstraint.activate([
            stack.topAnchor.constraint(equalTo: window.contentView!.topAnchor, constant: 28),
            stack.leadingAnchor.constraint(equalTo: window.contentView!.leadingAnchor, constant: 30),
            stack.trailingAnchor.constraint(equalTo: window.contentView!.trailingAnchor, constant: -30),
            footer.trailingAnchor.constraint(equalTo: stack.trailingAnchor),
            footer.bottomAnchor.constraint(equalTo: window.contentView!.bottomAnchor, constant: -22)
        ])
        window.center()
        window.makeKeyAndOrderFront(nil)
        NSApp.activate(ignoringOtherApps: true)
    }

    @objc private func install() {
        if installed != nil { NSApp.terminate(nil); return }
        guard !busy, let resources = Bundle.main.resourceURL, let executable = Bundle.main.executableURL else { return }
        let source = resources.appendingPathComponent("DriveBridge.app")
        let inventory = resources.appendingPathComponent("application-files.json")
        let parent = location.indexOfSelectedItem == 0 ? URL(fileURLWithPath: "/Applications") : FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent("Applications")
        let target = parent.appendingPathComponent("DriveBridge.app")
        let enableLogin = login.state == .on
        let openAfter = launch.state == .on
        busy = true
        button.isEnabled = false
        location.isEnabled = false
        login.isEnabled = false
        launch.isEnabled = false
        progress.startAnimation(nil)
        status.stringValue = "正在安全停止后台任务并安装，请稍候…"
        Task {
            do {
                let result = try await Task.detached {
                    try verifyApplication(source, inventory: inventory)
                    let payload = payloadURL(source)
                    _ = try runCommand(URL(fileURLWithPath: "/bin/sh"), [payload.appendingPathComponent("upgrade.sh").path,
                        payload.appendingPathComponent("bin/drivebridge").path, "--", executable.path, "--install-worker", source.path, target.path])
                    // The maintenance subprocess has exited, so it no longer
                    // holds the lifecycle/config locks required by these commands.
                    var warning = ""
                    do { try configureStartup(app: target, enabled: enableLogin) }
                    catch { warning = "应用已安装，但登录启动设置未完成：\(error.localizedDescription)" }
                    if openAfter {
                        do { _ = try runCommand(desktopURL(target), ["desktop", "start"]) }
                        catch { warning += "\n应用已安装，但自动打开失败：\(error.localizedDescription)" }
                    }
                    return warning
                }.value
                installed = target
                status.stringValue = "已安装到 \(target.path)" + (result.isEmpty ? (enableLogin ? "\n已开启登录自启动。" : "\n登录自启动已关闭。") : "\n" + result)
                button.title = "完成"
                button.isEnabled = true
                if !result.isEmpty { showError(result) }
            } catch {
                status.stringValue = "安装未完成，原有账号与同步数据已保留。"
                button.isEnabled = true
                location.isEnabled = true
                login.isEnabled = true
                launch.isEnabled = true
                showError(error.localizedDescription)
            }
            busy = false
            progress.stopAnimation(nil)
        }
    }

    private func showError(_ message: String) {
        let alert = NSAlert()
        alert.messageText = installed == nil ? "安装未完成" : "需要检查应用设置"
        alert.informativeText = message
        alert.addButton(withTitle: "好")
        alert.beginSheetModal(for: window)
    }
    func applicationShouldTerminate(_ sender: NSApplication) -> NSApplication.TerminateReply { busy ? .terminateCancel : .terminateNow }
    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool { !busy }
    func windowShouldClose(_ sender: NSWindow) -> Bool { !busy }
}

@main struct Installer {
    @MainActor static func main() {
        if CommandLine.arguments.count > 1 {
            do {
                guard CommandLine.arguments.count == 4, CommandLine.arguments[1] == "--install-worker", getuid() != 0, getuid() == geteuid() else { throw InstallError.unsafeTarget }
                let source = URL(fileURLWithPath: CommandLine.arguments[2])
                let target = URL(fileURLWithPath: CommandLine.arguments[3])
                guard let inventory = Bundle.main.resourceURL?.appendingPathComponent("application-files.json") else { throw InstallError.invalidPackage("缺少文件清单") }
                let userApps = FileManager.default.homeDirectoryForCurrentUser.appendingPathComponent("Applications")
                try FileManager.default.createDirectory(at: userApps, withIntermediateDirectories: true)
                try installApplication(source: source, target: target, allowedParents: [URL(fileURLWithPath: "/Applications"), userApps], verify: { staged in
                    try verifyApplication(staged, inventory: inventory)
                    _ = try runCommand(payloadURL(source).appendingPathComponent("bin/drivebridge"), ["feishu", "maintenance", "verify"])
                })
                print("DriveBridge application installed: \(target.path)")
            } catch {
                FileHandle.standardError.write(Data((error.localizedDescription + "\n").utf8))
                exit(1)
            }
            return
        }
        let app = NSApplication.shared
        let delegate = InstallerDelegate()
        app.delegate = delegate
        app.run()
        withExtendedLifetime(delegate) {}
    }
}
