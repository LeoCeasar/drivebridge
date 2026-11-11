import Cocoa
import WebKit
import Foundation

// All directory requests stay inside this app's persistent WebKit store.
// The private JSONL channel accepts fixed UI commands and read-only paths only.
let bundleIdentifier = "org.drivebridge.sessionhost"
let arguments = CommandLine.arguments
func output(_ value: [String: Any]) {
    guard let data = try? JSONSerialization.data(withJSONObject: value, options: [.sortedKeys]) else { return }
    if data.count > 33_554_432 {
        if let id = value["id"] as? String { output(["id": id, "ok": false, "error": "protocol"]) }
        return
    }
    FileHandle.standardOutput.write(data)
    FileHandle.standardOutput.write(Data([10]))
}
if arguments.count == 2 && arguments[1] == "--capabilities" {
    output(["protocol": 1, "bundle_id": bundleIdentifier, "minimum_macos": 14,
            "persistent_per_account": true, "operations": ["show", "hide", "fetch", "complete", "shutdown"]])
    exit(0)
}
func argument(_ name: String) -> String? {
    guard arguments.filter({ $0 == name }).count == 1,
          let i = arguments.firstIndex(of: name), i + 1 < arguments.count else { return nil }
    return arguments[i + 1]
}
guard arguments.count == 5, Bundle.main.bundleIdentifier == bundleIdentifier,
      let origin = argument("--origin"), let originURL = URL(string: origin),
      originURL.scheme == "https", originURL.user == nil, originURL.password == nil,
      originURL.port == nil, originURL.query == nil, originURL.fragment == nil,
      originURL.path.isEmpty, let host = originURL.host,
      host.hasSuffix(".feishu.cn") || host.hasSuffix(".larksuite.com"),
      let profile = argument("--profile-id"), let profileUUID = UUID(uuidString: profile),
      profileUUID.uuidString.lowercased() == profile
else { exit(2) }

@MainActor
final class SessionHost: NSObject, NSApplicationDelegate, NSWindowDelegate, WKNavigationDelegate, WKUIDelegate {
    let origin: String
    let originURL: URL
    let profileUUID: UUID
    init(origin: String, originURL: URL, profileUUID: UUID) {
        self.origin = origin; self.originURL = originURL; self.profileUUID = profileUUID
        super.init()
    }
    var window: NSWindow!
    var web: WKWebView!
    var top: NSView!
    var address: NSTextField!
    var status: NSTextField!
    var local: NSView!
    var icon: NSImageView!
    var headline: NSTextField!
    var subtitle: NSTextField!
    var detail: NSTextField!
    var doneButton: NSButton!
    var observation: NSKeyValueObservation?
    var navigated = false
    var completed = false
    var stopping = false
    var fetching = false
    var queue: [[String: Any]] = []
    let paths: Set<String> = ["/space/api/explorer/v3/my_space/folder/", "/space/api/explorer/v3/my_space/obj/", "/space/api/explorer/v2/share/folder/list/"]

    func applicationDidFinishLaunching(_ notification: Notification) {
        let configuration = WKWebViewConfiguration()
        configuration.websiteDataStore = WKWebsiteDataStore(forIdentifier: profileUUID)
        web = WKWebView(frame: .zero, configuration: configuration)
        web.navigationDelegate = self; web.uiDelegate = self
        web.translatesAutoresizingMaskIntoConstraints = false
        window = NSWindow(contentRect: NSRect(x: 0, y: 0, width: 1000, height: 760), styleMask: [.titled, .closable, .miniaturizable, .resizable], backing: .buffered, defer: false)
        window.title = "DriveBridge 飞书登录"
        window.minSize = NSSize(width: 680, height: 520)
        window.delegate = self; window.isReleasedWhenClosed = false; window.center()
        top = NSView(); top.translatesAutoresizingMaskIntoConstraints = false
        address = NSTextField(labelWithString: originURL.host ?? "飞书登录")
        address.translatesAutoresizingMaskIntoConstraints = false; address.lineBreakMode = .byTruncatingMiddle
        let reload = NSButton(title: "重新载入", target: self, action: #selector(reloadPage))
        reload.translatesAutoresizingMaskIntoConstraints = false
        let hide = NSButton(title: "隐藏到后台", target: self, action: #selector(hideWindow))
        hide.translatesAutoresizingMaskIntoConstraints = false
        top.addSubview(address); top.addSubview(reload); top.addSubview(hide)
        status = NSTextField(labelWithString: "登录后将自动核验账号并发现目录；若等待较久，可返回 DriveBridge 重新验证。")
        status.translatesAutoresizingMaskIntoConstraints = false; status.lineBreakMode = .byTruncatingTail
        let content = window.contentView!
        content.addSubview(top); content.addSubview(web); content.addSubview(status)
        NSLayoutConstraint.activate([
            top.leadingAnchor.constraint(equalTo: content.leadingAnchor, constant: 16), top.trailingAnchor.constraint(equalTo: content.trailingAnchor, constant: -16),
            top.topAnchor.constraint(equalTo: content.topAnchor, constant: 8), top.heightAnchor.constraint(equalToConstant: 36),
            address.leadingAnchor.constraint(equalTo: top.leadingAnchor), address.centerYAnchor.constraint(equalTo: top.centerYAnchor),
            address.trailingAnchor.constraint(lessThanOrEqualTo: reload.leadingAnchor, constant: -8),
            reload.trailingAnchor.constraint(equalTo: hide.leadingAnchor, constant: -8), reload.centerYAnchor.constraint(equalTo: top.centerYAnchor),
            hide.trailingAnchor.constraint(equalTo: top.trailingAnchor), hide.centerYAnchor.constraint(equalTo: top.centerYAnchor),
            status.leadingAnchor.constraint(equalTo: content.leadingAnchor, constant: 16), status.trailingAnchor.constraint(equalTo: content.trailingAnchor, constant: -16),
            status.bottomAnchor.constraint(equalTo: content.bottomAnchor, constant: -10), status.heightAnchor.constraint(equalToConstant: 24),
            web.leadingAnchor.constraint(equalTo: content.leadingAnchor), web.trailingAnchor.constraint(equalTo: content.trailingAnchor),
            web.topAnchor.constraint(equalTo: top.bottomAnchor, constant: 6), web.bottomAnchor.constraint(equalTo: status.topAnchor, constant: -6)
        ])
        buildLocalView(in: content)
        observation = web.observe(\.url, options: [.new]) { [weak self] view, _ in
            DispatchQueue.main.async {
                guard let self = self, let url = view.url else { return }
                self.address.stringValue = url.host ?? "飞书登录"
                if !self.completed && self.isAuthenticationURL(url) { self.showCloud() }
            }
        }
        reloadPage()
        output(["event": "ready", "protocol": 1, "bundle_id": bundleIdentifier, "persistent": configuration.websiteDataStore.isPersistent])
        let handler = self
        DispatchQueue.global(qos: .userInitiated).async {
            while let line = readLine() {
                guard line.utf8.count < 65536, let data = line.data(using: .utf8) else { continue }
                DispatchQueue.main.async {
                    guard let message = (try? JSONSerialization.jsonObject(with: data)) as? [String: Any] else { return }
                    handler.handle(message)
                }
            }
            // Parent crash/exit closes stdin. Never leave an orphaned WebView.
            DispatchQueue.main.async { handler.shutdown() }
        }
    }
    func buildLocalView(in content: NSView) {
        local = NSView(); local.translatesAutoresizingMaskIntoConstraints = false
        local.wantsLayer = true; local.layer?.backgroundColor = NSColor.windowBackgroundColor.cgColor
        local.isHidden = true; content.addSubview(local)
        let stack = NSStackView(); stack.orientation = .vertical; stack.alignment = .centerX; stack.spacing = 18
        stack.translatesAutoresizingMaskIntoConstraints = false
        icon = NSImageView(); icon.translatesAutoresizingMaskIntoConstraints = false
        headline = NSTextField(labelWithString: ""); headline.font = .systemFont(ofSize: 28, weight: .semibold); headline.alignment = .center
        subtitle = NSTextField(labelWithString: ""); subtitle.font = .systemFont(ofSize: 17, weight: .medium); subtitle.alignment = .center
        detail = NSTextField(wrappingLabelWithString: ""); detail.font = .systemFont(ofSize: 14); detail.textColor = .secondaryLabelColor
        detail.alignment = .center; detail.preferredMaxLayoutWidth = 560
        let buttons = NSStackView(); buttons.orientation = .horizontal; buttons.spacing = 14
        doneButton = NSButton(title: "返回 DriveBridge", target: self, action: #selector(hideWindow)); doneButton.bezelStyle = .rounded; doneButton.keyEquivalent = "\r"
        let retry = NSButton(title: "重新打开登录页", target: self, action: #selector(reloadPage)); retry.bezelStyle = .rounded
        buttons.addArrangedSubview(doneButton); buttons.addArrangedSubview(retry)
        for view in [icon!, headline!, subtitle!, detail!, buttons] { stack.addArrangedSubview(view) }
        local.addSubview(stack)
        NSLayoutConstraint.activate([
            local.leadingAnchor.constraint(equalTo: content.leadingAnchor), local.trailingAnchor.constraint(equalTo: content.trailingAnchor),
            local.topAnchor.constraint(equalTo: content.topAnchor), local.bottomAnchor.constraint(equalTo: content.bottomAnchor),
            stack.centerXAnchor.constraint(equalTo: local.centerXAnchor), stack.centerYAnchor.constraint(equalTo: local.centerYAnchor, constant: -16),
            stack.leadingAnchor.constraint(greaterThanOrEqualTo: local.leadingAnchor, constant: 40), stack.trailingAnchor.constraint(lessThanOrEqualTo: local.trailingAnchor, constant: -40),
            icon.widthAnchor.constraint(equalToConstant: 64), icon.heightAnchor.constraint(equalToConstant: 64), detail.widthAnchor.constraint(lessThanOrEqualToConstant: 560)
        ])
    }
    func showLocal(success: Bool) {
        local.isHidden = false; web.isHidden = true; top.isHidden = true; status.isHidden = true
        icon.image = NSImage(systemSymbolName: success ? "checkmark.circle.fill" : "person.crop.circle.badge.checkmark", accessibilityDescription: nil)
        icon.symbolConfiguration = NSImage.SymbolConfiguration(pointSize: 52, weight: .regular)
        icon.contentTintColor = success ? .systemGreen : .systemBlue
        headline.stringValue = success ? "网页登录已验证" : "正在核验网页登录"
        subtitle.stringValue = success ? "目录发现已完成" : "正在自动核验账号并发现目录"
        detail.stringValue = success ? "请返回 DriveBridge 设置查看账号盘和挂载状态。关闭此窗口后，目录发现仍可在后台继续。" : "若等待较久，可返回 DriveBridge 重新验证。"
        doneButton.title = success ? "完成并隐藏" : "先隐藏到后台"
        window.title = success ? "DriveBridge · 网页登录已验证" : "DriveBridge 飞书登录"
    }
    func showCloud() { local.isHidden = true; web.isHidden = false; top.isHidden = false; status.isHidden = false }
    @objc func reloadPage() {
        completed = false; navigated = false; showCloud()
        status.stringValue = "登录后将自动核验账号并发现目录；若等待较久，可返回 DriveBridge 重新验证。"
        web.load(URLRequest(url: URL(string: origin + "/drive/me/")!))
    }
    @objc func hideWindow() { window.orderOut(nil) }
    func windowShouldClose(_ sender: NSWindow) -> Bool { hideWindow(); return false }
    func applicationShouldTerminateAfterLastWindowClosed(_ sender: NSApplication) -> Bool { false }
    func sameOrigin(_ url: URL?) -> Bool { guard let url = url else { return false }; return url.scheme == "https" && url.host == originURL.host && url.port == nil }
    func isAuthenticationURL(_ url: URL) -> Bool {
        if !sameOrigin(url) { return true }
        let path = url.path.lowercased()
        return ["/accounts", "/account/", "/login", "/passport", "/auth"].contains(where: { path.hasPrefix($0) })
    }
    func handle(_ message: [String: Any]) {
        guard !stopping, let id = message["id"] as? String, UUID(uuidString: id)?.uuidString.lowercased() == id,
              let op = message["op"] as? String else { return }
        switch op {
        case "show":
            completed = false
            if let url = web.url, sameOrigin(url), !isAuthenticationURL(url), navigated { showLocal(success: false) } else { showCloud() }
            window.makeKeyAndOrderFront(nil); NSApp.activate(ignoringOtherApps: true)
            output(["id": id, "ok": true])
        case "hide": hideWindow(); output(["id": id, "ok": true])
        case "complete":
            completed = true; showLocal(success: true)
            window.makeKeyAndOrderFront(nil); NSApp.activate(ignoringOtherApps: true)
            output(["id": id, "ok": true])
        case "shutdown": output(["id": id, "ok": true]); shutdown()
        case "fetch":
            guard let path = message["path"] as? String, paths.contains(path),
                  let query = message["query"] as? String, query.utf8.count <= 8192,
                  let components = URLComponents(string: "https://probe.invalid/?" + query) else { output(["id": id, "ok": false, "error": "protocol"]); return }
            var seen = Set<String>()
            for item in components.queryItems ?? [] {
                guard ["length", "asc", "rank", "hidden", "last_label"].contains(item.name), !seen.contains(item.name), (item.value ?? "").utf8.count <= 4096,
                      !(item.value ?? "").unicodeScalars.contains(where: { CharacterSet.controlCharacters.contains($0) }) else { output(["id": id, "ok": false, "error": "protocol"]); return }
                seen.insert(item.name)
            }
            // A real authentication/SSO page requires login immediately. Fetch never
            // navigates, so automatic checks cannot reset a form being entered.
            if let url = web.url, url.absoluteString != "about:blank", isAuthenticationURL(url) {
                output(["id": id, "ok": true, "result": ["kind": "login", "status": 0, "contentType": "", "body": ""]]); return
            }
            // Initial hidden page restoration is distinct from expired login.
            // The Go host waits within its existing fetch deadline.
            guard navigated, !web.isLoading, let url = web.url, url.absoluteString != "about:blank" else {
                output(["id": id, "ok": true, "result": ["kind": "loading", "status": 0, "contentType": "", "body": ""]]); return
            }
            if fetching {
                guard queue.count < 8 else { output(["id": id, "ok": false, "error": "request_failed"]); return }
                queue.append(message)
            } else { fetch(message, path: path, query: query) }
        default: output(["id": id, "ok": false, "error": "protocol"])
        }
    }
    func fetch(_ message: [String: Any], path: String, query: String) {
        guard let id = message["id"] as? String else { return }; fetching = true
        let script = """
        if (location.origin !== origin || /^\\/(?:accounts?|login|passport|auth)(?:\\/|$)/i.test(location.pathname)) return {kind:'login',status:0,contentType:'',body:''};
        const abort = new AbortController(); const timer = setTimeout(() => abort.abort(),30000);
        try {
          const response = await fetch(target,{method:'GET',mode:'same-origin',credentials:'include',redirect:'error',signal:abort.signal,headers:{Accept:'application/json'}});
          const type = response.headers.get('content-type') || '';
          if (response.status === 401 || response.status === 403 || type.toLowerCase().includes('text/html')) return {kind:'login',status:response.status,contentType:type,body:''};
          if (!response.body) return {kind:'request_failed',status:response.status,contentType:type,body:''};
          const reader = response.body.getReader(); let total=0, body=''; const decoder=new TextDecoder('utf-8',{fatal:true});
          for (;;) { const part=await reader.read(); if(part.done) break; total+=part.value.byteLength;
            if(total>16777216){await reader.cancel();return {kind:'request_failed',status:0,contentType:'',body:''};}
            body+=decoder.decode(part.value,{stream:true}); }
          body+=decoder.decode(); return {kind:'',status:response.status,contentType:type,body};
        } catch (_) { return {kind:'request_failed',status:0,contentType:'',body:''}; }
        finally { clearTimeout(timer); }
        """
        web.callAsyncJavaScript(script, arguments: ["origin": origin, "target": path + (query.isEmpty ? "" : "?" + query)], in: nil, in: .defaultClient) { [weak self] outcome in
            switch outcome {
            case .success(let value):
                if let result = value as? [String: Any] { output(["id": id, "ok": true, "result": result]) }
                else { output(["id": id, "ok": false, "error": "protocol"]) }
            case .failure(_): output(["id": id, "ok": false, "error": "request_failed"])
            }
            self?.fetching = false
            if let self = self, !self.queue.isEmpty { let next = self.queue.removeFirst(); self.handle(next) }
        }
    }
    func shutdown() {
        if stopping { return }; stopping = true; queue.removeAll(); web.stopLoading(); hideWindow()
        DispatchQueue.main.asyncAfter(deadline: .now() + 1.0) { NSApp.terminate(nil) }
    }
    func webView(_ webView: WKWebView, didFinish navigation: WKNavigation!) {
        navigated = true
        if !completed, let url = webView.url, !isAuthenticationURL(url) { showLocal(success: false) }
        else if !completed { showCloud() }
    }
    func failedNavigation() { navigated = false; if !completed { showCloud(); status.stringValue = "页面暂时无法加载，请检查网络或点击重新载入。" } }
    func webView(_ webView: WKWebView, didFail navigation: WKNavigation!, withError error: Error) { failedNavigation() }
    func webView(_ webView: WKWebView, didFailProvisionalNavigation navigation: WKNavigation!, withError error: Error) { failedNavigation() }
    func webViewWebContentProcessDidTerminate(_ webView: WKWebView) { failedNavigation() }
    func webView(_ webView: WKWebView, decidePolicyFor action: WKNavigationAction, decisionHandler: @escaping @MainActor @Sendable (WKNavigationActionPolicy) -> Void) {
        guard let url = action.request.url, url.scheme == "https" || url.absoluteString == "about:blank" else {
            status.stringValue = "请使用飞书页面提供的网页登录或扫码登录。"; decisionHandler(.cancel); return
        }
        decisionHandler(.allow)
    }
    func webView(_ webView: WKWebView, createWebViewWith configuration: WKWebViewConfiguration, for action: WKNavigationAction, windowFeatures: WKWindowFeatures) -> WKWebView? {
        if action.targetFrame == nil, let url = action.request.url, url.scheme == "https" { webView.load(URLRequest(url: url)) }; return nil
    }
}
MainActor.assumeIsolated {
    let application = NSApplication.shared
    application.setActivationPolicy(.regular)
    let host = SessionHost(origin: origin, originURL: originURL, profileUUID: profileUUID)
    application.delegate = host
    withExtendedLifetime(host) { application.run() }
}
