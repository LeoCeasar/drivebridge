import Foundation
import CryptoKit
import Darwin
import AppKit
import CoreServices

enum InstallError: LocalizedError {
    case unsafeTarget, invalidPackage(String), command(String)
    var errorDescription: String? {
        switch self {
        case .unsafeTarget: return "安装位置不可用，或现有应用属于其他用户。请选择当前用户的应用程序目录。"
        case .invalidPackage(let detail): return "安装文件校验失败：\(detail)"
        case .command(let detail): return detail
        }
    }
}

func runCommand(_ executable: URL, _ arguments: [String]) throws -> String {
    let process = Process()
    process.executableURL = executable
    process.arguments = arguments
    let pipe = Pipe()
    process.standardOutput = pipe
    process.standardError = pipe
    try process.run()
    let data = pipe.fileHandleForReading.readDataToEndOfFile()
    process.waitUntilExit()
    let output = String(decoding: data, as: UTF8.self)
    guard process.terminationStatus == 0 else {
        throw InstallError.command(String(output.suffix(6000)))
    }
    return output
}

func payloadURL(_ app: URL) -> URL { app.appendingPathComponent("Contents/Resources/payload") }
func desktopURL(_ app: URL) -> URL {
    payloadURL(app).appendingPathComponent("DriveBridge.app/Contents/MacOS/drivebridge-desktop")
}

func linkServiceRegistrationURLs(_ app: URL) -> [URL] {
    if applicationIdentifier(app) == "org.drivebridge.linkservice" { return [app] }
    let provider = app.appendingPathComponent("Contents/Resources/payload/DriveBridge.app")
    var urls = [app]
    var root = app.deletingLastPathComponent()
    if applicationIdentifier(provider) == "org.drivebridge.desktop" {
        urls.append(provider); root = payloadURL(app)
    }
    let helper = root.appendingPathComponent("libexec/DriveBridge Link Service.app")
    if applicationIdentifier(helper) == "org.drivebridge.linkservice" { urls.append(helper) }
    return urls
}

private func applicationIdentifier(_ app: URL) -> String? {
    guard let data = try? Data(contentsOf: app.appendingPathComponent("Contents/Info.plist")),
          let info = try? PropertyListSerialization.propertyList(from: data, format: nil) as? [String: Any] else { return nil }
    return info["CFBundleIdentifier"] as? String
}

@MainActor func refreshInstalledServices(_ app: URL,
    register: (URL, Bool) -> OSStatus = { LSRegisterURL($0 as CFURL, $1) },
    update: () -> Void = { NSUpdateDynamicServices() }) throws {
    for url in linkServiceRegistrationURLs(app) {
        let status = register(url, true)
        guard status == noErr else { throw InstallError.command("应用已安装，但右键服务注册刷新失败（\(status)），请重新打开 DriveBridge。") }
    }
    update()
}

func isInstalledServiceLauncher(identifier: String?, bundle: URL?, executable: URL?, target: URL) -> Bool {
    guard let bundle, let executable else { return false }
    let helperRoot = applicationIdentifier(target) == "org.drivebridge.desktop" ? target.deletingLastPathComponent() : payloadURL(target)
    let candidates = [(target, applicationIdentifier(target) == "org.drivebridge.desktop" ? "org.drivebridge.desktop" : "org.drivebridge.launcher", "drivebridge-launcher"),
                      (target.appendingPathComponent("Contents/Resources/payload/DriveBridge.app"), "org.drivebridge.desktop", "drivebridge-launcher"),
                      (helperRoot.appendingPathComponent("libexec/DriveBridge Link Service.app"), "org.drivebridge.linkservice", "drivebridge-link-service")]
    return candidates.contains { app, id, executableName in
        identifier == id && bundle.standardizedFileURL.path == app.standardizedFileURL.path &&
        executable.standardizedFileURL.path == app.appendingPathComponent("Contents/MacOS/" + executableName).standardizedFileURL.path
    }
}

func pkgServicesApplication(home: URL = FileManager.default.homeDirectoryForCurrentUser) throws -> URL {
    let apps = home.appendingPathComponent("Applications")
    let payload = apps.appendingPathComponent("DriveBridge")
    let target = payload.appendingPathComponent("DriveBridge.app")
    for directory in [apps, payload, target] {
        let attrs = try FileManager.default.attributesOfItem(atPath: directory.path)
        guard attrs[.type] as? FileAttributeType == .typeDirectory,
              (attrs[.ownerAccountID] as? NSNumber)?.uint32Value == getuid() else { throw InstallError.unsafeTarget }
    }
    guard applicationIdentifier(target) == "org.drivebridge.desktop" else { throw InstallError.unsafeTarget }
    return target
}

@MainActor func installedServiceLaunchers(_ target: URL) -> [NSRunningApplication] {
    NSWorkspace.shared.runningApplications.filter {
        isInstalledServiceLauncher(identifier: $0.bundleIdentifier, bundle: $0.bundleURL, executable: $0.executableURL, target: target)
    }
}

@MainActor func retireServiceLaunchers(_ launchers: [NSRunningApplication]) throws {
    for app in launchers where !app.isTerminated { _ = app.terminate() }
    let deadline = Date().addingTimeInterval(3)
    while launchers.contains(where: { !$0.isTerminated }) && Date() < deadline {
        RunLoop.current.run(until: Date().addingTimeInterval(0.05))
    }
    guard launchers.allSatisfy({ $0.isTerminated }) else {
        throw InstallError.command("应用已安装，但旧链接服务尚未退出。请退出旧 DriveBridge 应用后重新打开。")
    }
}

// Only application files are replaced. Configuration, credentials and caches are
// outside this tree and remain owned by the existing maintenance coordinator.
func installApplication(source: URL, target: URL, allowedParents: [URL], verify: (URL) throws -> Void) throws {
    let fm = FileManager.default
    let parent = target.deletingLastPathComponent()
    guard target.lastPathComponent == "DriveBridge.app",
          allowedParents.contains(where: { $0.standardizedFileURL == parent.standardizedFileURL }),
          try parent.resourceValues(forKeys: [.isSymbolicLinkKey]).isSymbolicLink != true,
          fm.isWritableFile(atPath: parent.path) else { throw InstallError.unsafeTarget }
    if let attributes = try? fm.attributesOfItem(atPath: target.path) {
        guard attributes[.type] as? FileAttributeType == .typeDirectory,
              (attributes[.ownerAccountID] as? NSNumber)?.uint32Value == getuid() else { throw InstallError.unsafeTarget }
    }
    try verify(source)
    let scratch = parent.appendingPathComponent(".drivebridge-install-\(UUID().uuidString)")
    try fm.createDirectory(at: scratch, withIntermediateDirectories: false, attributes: [.posixPermissions: 0o700])
    var retainBackup = false
    defer { if !retainBackup { try? fm.removeItem(at: scratch) } }
    let staged = scratch.appendingPathComponent("DriveBridge.app")
    let backup = scratch.appendingPathComponent("previous.app")
    try fm.copyItem(at: source, to: staged)
    try verify(staged)
    let hadPrevious = fm.fileExists(atPath: target.path)
    if hadPrevious { try fm.moveItem(at: target, to: backup) }
    do {
        try fm.moveItem(at: staged, to: target)
    } catch {
        if hadPrevious {
            // Never remove a previous version if restoring it fails.
            do { try fm.moveItem(at: backup, to: target) }
            catch {
                retainBackup = true
                throw InstallError.command("恢复旧版本失败，旧应用保留在：\(backup.path)")
            }
        }
        throw error
    }
}

func verifyApplication(_ app: URL, inventory: URL) throws {
    let fm = FileManager.default
    let root = app.resolvingSymlinksInPath().standardizedFileURL
    guard let files = try JSONSerialization.jsonObject(with: Data(contentsOf: inventory)) as? [[String: Any]], !files.isEmpty else {
        throw InstallError.invalidPackage("缺少文件清单")
    }
    var expected = Set<String>()
    for file in files {
        guard let path = file["path"] as? String, !path.hasPrefix("/"),
              !path.split(separator: "/", omittingEmptySubsequences: false).contains(where: { $0 == ".." || $0 == "." || $0.isEmpty }),
              let sum = file["sha256"] as? String, expected.insert(path).inserted else {
            throw InstallError.invalidPackage("无效的文件路径")
        }
        let url = root.appendingPathComponent(path)
        let values = try url.resourceValues(forKeys: [.isRegularFileKey, .isSymbolicLinkKey])
        guard values.isRegularFile == true, values.isSymbolicLink != true else { throw InstallError.invalidPackage(path) }
        let data = try Data(contentsOf: url, options: .mappedIfSafe)
        let actual = SHA256.hash(data: data).map { String(format: "%02x", $0) }.joined()
        guard actual == sum else { throw InstallError.invalidPackage(path) }
    }
    guard let walker = fm.enumerator(atPath: root.path) else { throw InstallError.invalidPackage("无法读取应用") }
    for case let path as String in walker {
        let file = root.appendingPathComponent(path)
        let values = try file.resourceValues(forKeys: [.isRegularFileKey, .isSymbolicLinkKey])
        guard values.isSymbolicLink != true else { throw InstallError.invalidPackage("不允许符号链接") }
        if values.isRegularFile == true {
            guard expected.contains(path) else { throw InstallError.invalidPackage(path) }
        }
    }
}

// Preserve a previous custom config and retire only our known prior startup
// entry. Unknown executables/configurations are reported, never overwritten.
func configureStartup(app: URL, enabled: Bool, home: URL = FileManager.default.homeDirectoryForCurrentUser) throws {
    let fm = FileManager.default
    let plist = home.appendingPathComponent("Library/LaunchAgents/org.drivebridge.desktop.plist")
    let newDesktop = desktopURL(app)
    var configArgs: [String] = []
    var previous: (URL, [String])?
    if fm.fileExists(atPath: plist.path) {
        let attrs = try fm.attributesOfItem(atPath: plist.path)
        guard attrs[.type] as? FileAttributeType == .typeRegular,
              (attrs[.ownerAccountID] as? NSNumber)?.uint32Value == getuid(),
              let object = try PropertyListSerialization.propertyList(from: Data(contentsOf: plist), format: nil) as? [String: Any],
              object["Label"] as? String == "org.drivebridge.desktop",
              let args = object["ProgramArguments"] as? [String],
              args.count == 5 || args.count == 7,
              args[1...3] == ["desktop", "_run", "--bridge-config"], args[4].hasPrefix("/"),
              args.count == 5 || args[5] == "--lark-cli" else { throw InstallError.command("现有登录启动项格式不同，已保留。请在托盘菜单中检查登录自启动。") }
        let old = URL(fileURLWithPath: args[0])
        let legacy = home.appendingPathComponent("Applications/DriveBridge/DriveBridge.app/Contents/MacOS/drivebridge-desktop")
        let standard = desktopURL(URL(fileURLWithPath: "/Applications/DriveBridge.app"))
        let personal = desktopURL(home.appendingPathComponent("Applications/DriveBridge.app"))
        guard [legacy, standard, personal, newDesktop].contains(old) else { throw InstallError.command("登录启动项指向其他安装位置，已保留。") }
        configArgs = ["--bridge-config", args[4]]
        var oldArgs = configArgs
        if args.count == 7 { oldArgs += ["--lark-cli", args[6]] }
        _ = try runCommand(old, ["desktop", "disable-autostart"] + oldArgs)
        previous = (old, oldArgs)
    }
    if enabled {
        do { _ = try runCommand(newDesktop, ["desktop", "enable-autostart"] + configArgs) }
        catch {
            if let (old, args) = previous { _ = try? runCommand(old, ["desktop", "enable-autostart"] + args) }
            throw error
        }
    }
}
