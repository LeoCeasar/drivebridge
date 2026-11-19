import AppKit
import Foundation
import Darwin

enum LinkServiceError: LocalizedError {
    case message(String)
    var errorDescription: String? { if case .message(let value) = self { return value }; return nil }
}

func selectedLinkPath(_ urls: [URL]) throws -> String {
    guard urls.count == 1, let url = urls.first, url.isFileURL,
          url.path.hasPrefix("/"), !url.path.contains("\0") else {
        throw LinkServiceError.message("请只选择一个 DriveBridge 文件或文件夹。")
    }
    return url.path
}

// The graphical installer wraps a complete payload. The pkg/portable layout
// puts the app next to bin instead. Never rely on Finder's PATH.
func linkCLIURL(_ app: URL) -> URL {
    let nested = app.appendingPathComponent("Contents/Resources/payload/bin/drivebridge")
    if FileManager.default.fileExists(atPath: nested.path) { return nested }
    if app.lastPathComponent == "DriveBridge Link Service.app" && app.deletingLastPathComponent().lastPathComponent == "libexec" {
        return app.deletingLastPathComponent().deletingLastPathComponent().appendingPathComponent("bin/drivebridge")
    }
    return app.deletingLastPathComponent().appendingPathComponent("bin/drivebridge")
}

func launcherDesktopURL(_ app: URL) -> URL {
    let nested = app.appendingPathComponent("Contents/Resources/payload/DriveBridge.app/Contents/MacOS/drivebridge-desktop")
    if FileManager.default.fileExists(atPath: nested.path) { return nested }
    return app.appendingPathComponent("Contents/MacOS/drivebridge-desktop")
}

private struct LinkReply: Decodable {
    struct Failure: Decodable { let code: String; let message: String? }
    let schema_version: Int
    let url: String?
    let cached: Bool?
    let pending_changes: Bool?
    let error: Failure?
}

@MainActor func copyLinkReply(_ data: Data, to pasteboard: NSPasteboard, expectedChangeCount: Int? = nil) throws -> String {
    guard let reply = try? JSONDecoder().decode(LinkReply.self, from: data), reply.schema_version == 1 else {
        throw LinkServiceError.message("获取飞书链接失败，请检查 DriveBridge 状态后重试。")
    }
    if let error = reply.error {
        let messages = [
            "link_unsupported": "此项目不属于可分享的飞书文件或文件夹。",
            "link_not_uploaded": "首次上传完成后可复制飞书分享链接。",
            "link_unavailable": "该项目暂无可用的飞书链接，请检查云端状态或访问权限后重试。",
            "link_lookup_timeout": "首次获取此文件的飞书链接超时，缓存尚未补齐，请稍后重试。",
            "unauthorized": "请在 DriveBridge 中检查飞书账号授权。",
            "credential_unavailable": "请在 DriveBridge 中检查飞书账号授权。",
            "identity_changed": "文件或账号已变化，请重新选择后重试。",
            "network": "查询超时或连接失败，请稍后重试。"
        ]
        throw LinkServiceError.message(messages[error.code] ?? "获取飞书链接失败，请在 DriveBridge 中检查挂载和同步状态。")
    }
    guard let raw = reply.url, !raw.contains("\\"),
          raw.rangeOfCharacter(from: .whitespacesAndNewlines.union(.controlCharacters)) == nil,
          let decoded = raw.removingPercentEncoding, !decoded.contains("\\"),
          decoded.rangeOfCharacter(from: .controlCharacters) == nil,
          let url = URLComponents(string: raw), url.scheme == "https", let host = url.host, !host.isEmpty,
          url.user == nil, url.password == nil else {
        throw LinkServiceError.message("飞书未返回有效的 HTTPS 链接，剪贴板未修改。")
    }
    if let expectedChangeCount, pasteboard.changeCount != expectedChangeCount {
        throw LinkServiceError.message("剪贴板已变化，本次结果未覆盖新内容，请重新复制链接。")
    }
    pasteboard.clearContents()
    guard pasteboard.setString(raw, forType: .string) else { throw LinkServiceError.message("无法写入剪贴板，请稍后重试。") }
    var message = "飞书分享链接已复制，访问权限保持不变。"
    if reply.pending_changes == true { message += "此项目有待同步修改，链接指向云端当前内容。" }
    if reply.cached == true { message += "使用已缓存链接，当前访问状态未核验。" }
    return message
}

func fetchLinkReply(cli: URL, path: String, timeout: TimeInterval = 20) throws -> Data {
    let began = Date()
    var outcome = "native_error"
    var cacheState = -1
    defer {
        NSLog("DriveBridge link query elapsed_ms=%.0f result=%@ cached=%d", Date().timeIntervalSince(began) * 1000, outcome, cacheState)
    }
    let process = Process()
    process.executableURL = cli
    process.arguments = ["feishu", "link", "--path", path, "--json", "--timeout", "15s"]
    // A private file avoids pipe backpressure and EOF waits on inherited child
    // descriptors. It is bounded on read and removed on every return path.
    let outputURL = FileManager.default.temporaryDirectory.appendingPathComponent(".drivebridge-link-\(UUID().uuidString)")
    let fd = Darwin.open(outputURL.path, O_CREAT | O_EXCL | O_WRONLY, 0o600)
    guard fd >= 0 else { throw LinkServiceError.message("无法创建链接查询临时文件。") }
    let output = FileHandle(fileDescriptor: fd, closeOnDealloc: true)
    defer { try? output.close(); try? FileManager.default.removeItem(at: outputURL) }
    process.standardOutput = output
    process.standardError = FileHandle.nullDevice
    let finished = DispatchSemaphore(value: 0)
    process.terminationHandler = { _ in finished.signal() }
    try process.run()
    if finished.wait(timeout: .now() + timeout) == .timedOut {
        outcome = "native_timeout"
        if process.isRunning { process.terminate() }
        if finished.wait(timeout: .now() + 0.2) == .timedOut {
            if process.isRunning { Darwin.kill(process.processIdentifier, SIGKILL) }
            _ = finished.wait(timeout: .now() + 1)
        }
        throw LinkServiceError.message("获取链接超时，请检查 DriveBridge 状态后重试。")
    }
    let attributes = try FileManager.default.attributesOfItem(atPath: outputURL.path)
    guard let size = attributes[.size] as? NSNumber, size.intValue <= 65536 else {
        throw LinkServiceError.message("链接查询响应无效。")
    }
    let data = try Data(contentsOf: outputURL)
    if let reply = try? JSONDecoder().decode(LinkReply.self, from: data) {
        let safeCodes: Set<String> = ["link_lookup_timeout", "link_unsupported", "link_not_uploaded", "link_unavailable", "network", "unauthorized", "credential_unavailable", "identity_changed", "version_changed", "worker_unavailable", "pending", "starting", "config", "operation_failed"]
        if let code = reply.error?.code { outcome = safeCodes.contains(code) ? code : "other_error" }
        else { outcome = process.terminationStatus == 0 ? "ok" : "child_exit" }
        cacheState = reply.cached.map { $0 ? 1 : 0 } ?? -1
    } else { outcome = "invalid_reply" }
    // A structured error envelope is intentionally returned to the main actor;
    // it is inspected before the clipboard can be touched.
    if process.terminationStatus != 0 {
        guard let reply = try? JSONDecoder().decode(LinkReply.self, from: data), reply.error != nil else {
            throw LinkServiceError.message("获取飞书链接失败，请检查 DriveBridge 状态后重试。")
        }
    }
    return data
}

@MainActor private enum LinkFailureNotice {
    static var panel: NSPanel?
    static var dismissal: Task<Void, Never>?

    static func show(_ message: String) {
        dismissal?.cancel(); panel?.close()
        guard let screen = NSScreen.main else { NSLog("DriveBridge link request failed"); return }
        let frame = screen.visibleFrame
        let notice = NSPanel(contentRect: NSRect(x: frame.maxX - 376, y: frame.maxY - 156, width: 360, height: 112),
                             styleMask: [.titled, .closable, .nonactivatingPanel], backing: .buffered, defer: false)
        notice.title = "无法复制飞书分享链接"
        notice.isReleasedWhenClosed = false
        notice.level = .floating
        notice.hidesOnDeactivate = false
        notice.becomesKeyOnlyIfNeeded = true
        let label = NSTextField(wrappingLabelWithString: message)
        label.frame = NSRect(x: 16, y: 16, width: 328, height: 80)
        notice.contentView?.addSubview(label)
        panel = notice
        notice.orderFrontRegardless()
        dismissal = Task {
            do { try await Task.sleep(for: .seconds(6)) } catch { return }
            notice.close()
            if panel === notice { panel = nil }
        }
    }
}

@MainActor final class LinkServiceProvider: NSObject {
    private var busy = false

    @objc func copyFeishuLink(_ pasteboard: NSPasteboard, userData: String?, error: AutoreleasingUnsafeMutablePointer<NSString?>) {
        do {
            guard !busy else { throw LinkServiceError.message("正在获取链接，请稍后重试。") }
            let urls = pasteboard.readObjects(forClasses: [NSURL.self], options: [.urlReadingFileURLsOnly: true]) as? [URL] ?? []
            let path = try selectedLinkPath(urls)
            let cli = linkCLIURL(Bundle.main.bundleURL)
            let clipboardVersion = NSPasteboard.general.changeCount
            busy = true
            Task {
                defer { busy = false }
                do {
                    let data = try await Task.detached { try fetchLinkReply(cli: cli, path: path) }.value
                    _ = try copyLinkReply(data, to: .general, expectedChangeCount: clipboardVersion)
                } catch {
                    LinkFailureNotice.show(error.localizedDescription)
                }
            }
        } catch let failure {
            error.pointee = failure.localizedDescription as NSString
        }
    }
}
